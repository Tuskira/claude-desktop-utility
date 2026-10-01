package claudedesktop

// Chat tab: Connect-RPC with binary protobuf, decoded field-number-only by
// internal/protodec (see chattab_fields.go for the verified field paths).
// Two calls matter:
//
//   POST .../ConversationService/PerformAction      (application/proto)
//   POST .../ConversationService/StreamTimeline      (application/connect+proto,
//                                                      streamed as RPC< units)
//
// One LLM record = one user prompt + the assistant reply to it, paired by
// message UUID, never by arrival order:
//
//   - A message-sending PerformAction carries the prompt (2.3) and the UUID
//     of the assistant message that will hold the reply (2.2).
//   - StreamTimeline units describe messages (1.1.3), their display groups
//     (1.1.4, pointing at a message UUID) and content blocks (1.1.5,
//     pointing at a group), plus text deltas (1.2, pointing at a block).
//   - The reply's text is the text of the blocks in groups owned by the
//     reply UUID. The record is emitted once that message is marked
//     complete (1.1.3.6 == 1) and the send is known, in either order.
//
// This is why the history a stream replays when it opens can never leak
// into a record: history messages have other UUIDs, and a message first
// seen already complete is only tracked if a send names it. It also means
// the reply may arrive on any connection or stream, and that a reply whose
// stream was closed before it finished is still recorded when a later
// stream replays it complete. A send whose reply is never seen complete
// within chatTriggerMaxAge is dropped and counted (ReasonChatNoReply);
// nothing is emitted with a guessed pairing.

import (
	"time"

	"github.com/Tuskira/claude-desktop-utility/internal/capture"
)

// chatTriggerMaxAge bounds how long a send waits for its reply to complete,
// and how long a live, not-yet-named reply waits for its send.
const chatTriggerMaxAge = 20 * time.Minute

type chatState struct {
	replies   map[string]*chatReply // keyed by assistant message UUID
	groupMsg  map[string]string     // dgrp id -> message UUID, tracked replies only
	convModel map[string]modelSeen  // conversation UUID -> model from the stream header
}

type modelSeen struct {
	model string
	at    time.Time
}

func (s *chatState) init() {
	s.replies = map[string]*chatReply{}
	s.groupMsg = map[string]string{}
	s.convModel = map[string]modelSeen{}
}

type chatSend struct {
	at                      time.Time
	convID, humanID, prompt string
}

type chatReply struct {
	msgID     string
	firstSeen time.Time
	lastSeen  time.Time
	send      *chatSend

	sender   int64  // 0 until a message record is seen
	parentID string // "" until a message record is seen
	complete bool

	lastLiveTextAt time.Time         // last text change while the message was still streaming
	groups         []string          // text-bearing dgrp ids, first-seen order
	groupText      map[string]string // dgrp -> full text so far
	groupCblk      map[string]string // dgrp -> block id whose deltas extend groupText
	cblkGroup      map[string]string // cblk -> dgrp
}

func (c *Converter) chatReplyFor(id string, now time.Time) *chatReply {
	r, ok := c.chat.replies[id]
	if !ok {
		r = &chatReply{msgID: id, firstSeen: now, groupText: map[string]string{}, groupCblk: map[string]string{}, cblkGroup: map[string]string{}}
		c.chat.replies[id] = r
	}
	r.lastSeen = now
	return r
}

// sweepChat drops replies that went stale: a send whose reply never
// completed, or a live reply no send ever named. Counted, never emitted.
func (c *Converter) sweepChat(now time.Time) {
	for id, r := range c.chat.replies {
		last := r.lastSeen
		if r.send != nil && r.send.at.After(last) {
			last = r.send.at
		}
		if now.Sub(last) <= chatTriggerMaxAge {
			continue
		}
		if r.send != nil {
			c.Stats.skip(ReasonChatNoReply)
		} else {
			c.Stats.skip(ReasonNoTrigger)
		}
		c.dropChatReply(id)
	}
	for conv, m := range c.chat.convModel {
		if now.Sub(m.at) > 4*time.Hour {
			delete(c.chat.convModel, conv)
		}
	}
}

func (c *Converter) dropChatReply(id string) {
	r, ok := c.chat.replies[id]
	if !ok {
		return
	}
	for _, g := range r.groups {
		delete(c.chat.groupMsg, g)
	}
	for g, m := range c.chat.groupMsg {
		if m == id {
			delete(c.chat.groupMsg, g)
		}
	}
	delete(c.chat.replies, id)
}

func (c *Converter) handlePerformAction(rec *capture.Record) {
	if len(rec.ReqBodyDecoded) == 0 {
		c.Stats.skip(ReasonBadJSON)
		return
	}
	c.sweepChat(rec.TS)
	convID, convOK := stringField(rec.ReqBodyDecoded, fieldPerformActionConversationID)
	if _, isSend := fieldPath(rec.ReqBodyDecoded, fieldPerformActionSend); !isSend {
		// Not a message send (e.g. the action sent when a conversation is
		// opened): nothing to pair.
		if !convOK {
			c.Stats.skip(ReasonProtoFieldNotFound)
		}
		return
	}
	replyID, replyOK := stringField(rec.ReqBodyDecoded, fieldPerformActionReplyMsgID)
	prompt, promptOK := stringField(rec.ReqBodyDecoded, fieldPerformActionPromptText)
	humanID, _ := stringField(rec.ReqBodyDecoded, fieldPerformActionHumanMsgID)
	if !convOK || !replyOK || replyID == "" || !promptOK || prompt == "" {
		c.Stats.skip(ReasonProtoFieldNotFound)
		return
	}
	r := c.chatReplyFor(replyID, rec.TS)
	r.send = &chatSend{at: rec.TS, convID: convID, humanID: humanID, prompt: prompt}
	c.maybeFinishChat(r, rec.TS)
}

func (c *Converter) handleStreamTimelineUnit(rec *capture.Record) {
	if len(rec.RespBodyDecoded) == 0 {
		return
	}
	c.sweepChat(rec.TS)
	u := parseStreamUnit(rec.RespBodyDecoded)
	if u.ConvID != "" && u.Model != "" {
		c.chat.convModel[u.ConvID] = modelSeen{model: u.Model, at: rec.TS}
	}

	// 1. Start tracking assistant messages seen still streaming: a live
	// reply may show up before its send is processed. A message first seen
	// complete (history) is tracked only if a send already named it.
	for _, m := range u.Messages {
		if r, ok := c.chat.replies[m.ID]; ok {
			r.sender, r.parentID = m.Sender, m.ParentID
			continue
		}
		if m.Sender == senderAssistant && !m.Complete {
			r := c.chatReplyFor(m.ID, rec.TS)
			r.sender, r.parentID = m.Sender, m.ParentID
		}
	}
	// 2. Map groups, then blocks and deltas, onto tracked replies.
	for _, g := range u.Groups {
		if _, ok := c.chat.replies[g.MsgID]; ok {
			c.chat.groupMsg[g.DgrpID] = g.MsgID
		}
	}
	for _, b := range u.Blocks {
		r := c.replyForGroup(b.DgrpID)
		if r == nil {
			continue
		}
		r.lastSeen = rec.TS
		r.cblkGroup[b.CblkID] = b.DgrpID
		if !b.HasText {
			continue
		}
		if _, seen := r.groupText[b.DgrpID]; !seen {
			r.groups = append(r.groups, b.DgrpID)
		}
		r.groupText[b.DgrpID] = b.Text // a snapshot is the block's full text so far
		r.groupCblk[b.DgrpID] = b.CblkID
		if !r.complete {
			r.lastLiveTextAt = rec.TS
		}
	}
	if d := u.Delta; d != nil {
		for _, r := range c.chat.replies {
			g, ok := r.cblkGroup[d.CblkID]
			if !ok || r.groupCblk[g] != d.CblkID {
				continue
			}
			r.groupText[g] += d.Text
			r.lastSeen = rec.TS
			if !r.complete {
				r.lastLiveTextAt = rec.TS
			}
		}
	}
	// 3. Completion, after this unit's text has been applied.
	for _, m := range u.Messages {
		if r, ok := c.chat.replies[m.ID]; ok && m.Complete {
			r.complete = true
			c.maybeFinishChat(r, rec.TS)
		}
	}
}

func (c *Converter) replyForGroup(dgrp string) *chatReply {
	id, ok := c.chat.groupMsg[dgrp]
	if !ok {
		return nil
	}
	return c.chat.replies[id]
}

// handleStreamTimelineEnd handles the final capture record of a
// StreamTimeline exchange. Nothing to do: records are keyed by message
// UUID, not by stream, so a stream closing neither finishes nor discards a
// reply (a later stream's history replay completes it).
func (c *Converter) handleStreamTimelineEnd(*capture.Record) {}

// maybeFinishChat emits r once both the send and the complete reply are
// known, after checking the stream agrees with the send about who is who.
func (c *Converter) maybeFinishChat(r *chatReply, now time.Time) {
	if r.send == nil || !r.complete {
		return
	}
	defer c.dropChatReply(r.msgID)
	if r.sender != 0 && r.sender != senderAssistant {
		c.Stats.skip(ReasonChatPairMismatch)
		return
	}
	if r.parentID != "" && r.send.humanID != "" && r.parentID != r.send.humanID {
		c.Stats.skip(ReasonChatPairMismatch)
		return
	}
	var texts []string
	for _, g := range r.groups {
		if t := r.groupText[g]; t != "" {
			texts = append(texts, t)
		}
	}
	if len(texts) == 0 {
		c.Stats.skip(ReasonChatNoReply)
		return
	}
	model := c.chat.convModel[r.send.convID].model
	var dur *int64
	if !r.lastLiveTextAt.IsZero() {
		d := r.lastLiveTextAt.Sub(r.send.at).Milliseconds()
		if d < 0 {
			d = 0
		}
		dur = &d
	}
	zero := 0
	content := make([]map[string]any, 0, len(texts))
	for _, t := range texts {
		content = append(content, map[string]any{"type": "text", "text": t})
	}
	userMsgs := []map[string]any{{"role": "user", "content": r.send.prompt}}
	c.emitLLMCall(LLMCallIn{
		Timestamp:         r.send.at.UTC().Format(time.RFC3339Nano),
		RequestID:         icpChatRequestID(r.msgID),
		StatusCode:        200,
		SessionID:         r.send.convID,
		Model:             model,
		RequestedModel:    model,
		DurationMS:        dur,
		Stream:            true,
		InputTokens:       &zero,
		OutputTokens:      &zero,
		CacheReadTokens:   &zero,
		CacheCreateTokens: &zero,
		Messages:          mustJSON(userMsgs),
		RequestBody:       anthropicRequestJSON(model, userMsgs),
		// No usage block: the chat stream carries no token counts.
		ResponseBody: anthropicResponseJSON(r.msgID, model, content, "", nil),
	})
}
