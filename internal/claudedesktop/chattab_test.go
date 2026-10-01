package claudedesktop

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Tuskira/claude-desktop-utility/internal/capture"
)

// Synthetic fixtures mirroring the shapes verified in the Sep 30, 2026
// capture (see chattab_fields.go). No real conversation text is used.

const (
	performActionURL  = "https://claude.ai/claudeai-rpc/anthropic.bard.api.v1alpha.ConversationService/PerformAction"
	streamTimelineURL = "https://claude.ai/claudeai-rpc/anthropic.bard.api.v1alpha.ConversationService/StreamTimeline"
	testConvID        = "11111111-1111-4111-8111-111111111111"
	testModel         = "claude-opus-5-5"
	testClient        = "127.0.0.1:50001"

	histHuman1 = "aaaaaaaa-0000-4000-8000-000000000001"
	histAsst1  = "aaaaaaaa-0000-4000-8000-000000000002"
	newHuman   = "bbbbbbbb-0000-4000-8000-000000000003"
	newAsst    = "bbbbbbbb-0000-4000-8000-000000000004"

	historyUserText  = "HISTORY-PROMPT synthetic earlier question"
	historyReplyText = "HISTORY-REPLY synthetic earlier answer"
	promptText       = "PROMPT-2 what next"
)

type obj = map[string]any

func mustJSONT(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func sendDecoded(t *testing.T, convID, humanID, replyID, prompt string) string {
	return mustJSONT(t, obj{
		"1": obj{"1": obj{"1": "req-1", "2": 1}, "2": convID, "3": 3, "14": "en-US"},
		"2": obj{"1": humanID, "2": replyID, "3": prompt, "12": "America/X", "18": 2},
	})
}

func openDecoded(t *testing.T, convID string) string {
	return mustJSONT(t, obj{"1": obj{"2": convID, "3": 3}, "15": obj{"1": obj{"12": "x"}}})
}

func header(convID string) obj {
	return obj{"1": convID, "3": 2, "6": obj{"2": testModel}, "7": "leaf"}
}

func msgRec(id, parent string, sender int, complete bool) obj {
	m := obj{"1": id, "2": testConvID, "3": sender, "11": parent}
	if complete {
		m["6"] = 1
	}
	return m
}

func eventUnit(t *testing.T, inner obj) string {
	return mustJSONT(t, obj{"1": obj{"1": inner, "10": obj{"1": "evt"}, "11": 5}})
}

func deltaUnit(t *testing.T, cblk, text string) string {
	return mustJSONT(t, obj{"1": obj{"2": obj{"1": cblk, "2": text}, "11": 5}})
}

const keepAliveUnit = `{"1":{"6":""}}`

// historyUnit is the replay a StreamTimeline sends when it opens: the
// whole conversation so far, complete, in arrays.
func historyUnit(t *testing.T) string {
	return eventUnit(t, obj{
		"2": header(testConvID),
		"3": []any{msgRec(histHuman1, "", 1, true), msgRec(histAsst1, histHuman1, 2, true)},
		"4": []any{obj{"1": "dgrp_h1", "2": histHuman1}, obj{"1": "dgrp_a1", "2": histAsst1}},
		"5": []any{
			obj{"1": "cblk_h1", "2": "dgrp_h1", "9": historyUserText, "10": obj{"1": 2}},
			obj{"1": "cblk_a1", "2": "dgrp_a1", "9": historyReplyText, "10": obj{"1": 2}, "24": 3},
		},
	})
}

type chatFeed struct {
	t    *testing.T
	conv *Converter
	t0   time.Time
	seq  map[string]int
}

func (f *chatFeed) unit(at time.Duration, client, decoded string) {
	f.seq[client]++
	f.conv.Handle(&capture.Record{
		TS: f.t0.Add(at), Mode: capture.ModeStream, Client: client, Method: "RPC<",
		URL: streamTimelineURL, Status: 200, Seq: f.seq[client], RespBodyDecoded: []byte(decoded),
	})
}

func (f *chatFeed) streamEnd(at time.Duration, client string) {
	f.conv.Handle(&capture.Record{
		TS: f.t0.Add(at), Mode: capture.ModeProxy, Client: client, Method: "POST",
		URL: streamTimelineURL, Status: 200, StreamUnits: f.seq[client],
	})
	f.seq[client] = 0
}

func (f *chatFeed) performAction(at time.Duration, decoded string) {
	pa := postRecord(f.t0.Add(at), performActionURL, "")
	pa.ReqBodyDecoded = []byte(decoded)
	f.conv.Handle(pa)
}

// liveReply feeds the verified live sequence for one reply: the human
// message's own record and block (an echo of the prompt), the assistant
// message opening, a thinking block (no text), a text snapshot, deltas, a
// fuller snapshot, one more delta, then the completion marker.
func (f *chatFeed) liveReply(start time.Duration, client, humanID, replyID, dgrp, cblk string) {
	t := f.t
	f.unit(start, client, eventUnit(t, obj{
		"2": header(testConvID),
		"3": msgRec(humanID, histAsst1, 1, true),
		"4": obj{"1": "dgrp_echo_" + dgrp, "2": humanID},
		"5": obj{"1": "cblk_echo_" + cblk, "2": "dgrp_echo_" + dgrp, "9": promptText, "10": obj{"1": 2}},
	}))
	f.unit(start+10*time.Millisecond, client, eventUnit(t, obj{"2": header(testConvID), "3": msgRec(replyID, humanID, 2, false)}))
	f.unit(start+20*time.Millisecond, client, eventUnit(t, obj{
		"4": obj{"1": "dgrp_think_" + dgrp, "2": replyID},
		"5": obj{"1": "cblk_think_" + cblk, "2": "dgrp_think_" + dgrp, "5": "Thinking", "10": obj{"1": 1}},
	}))
	f.unit(start+30*time.Millisecond, client, eventUnit(t, obj{
		"4": obj{"1": dgrp, "2": replyID},
		"5": obj{"1": cblk, "2": dgrp, "9": "Re", "10": obj{"1": 2}},
	}))
	f.unit(start+40*time.Millisecond, client, deltaUnit(t, cblk, "ply "))
	f.unit(start+50*time.Millisecond, client, keepAliveUnit)
	f.unit(start+60*time.Millisecond, client, deltaUnit(t, cblk, "text"))
	f.unit(start+70*time.Millisecond, client, eventUnit(t, obj{"5": obj{"1": cblk, "2": dgrp, "9": "Reply text", "10": obj{"1": 2}, "24": 3}}))
	f.unit(start+80*time.Millisecond, client, deltaUnit(t, cblk, " for "+replyID[:8]))
	f.unit(start+90*time.Millisecond, client, eventUnit(t, obj{"2": header(testConvID), "3": msgRec(replyID, humanID, 2, true), "4": obj{"1": dgrp, "2": replyID}}))
}

func newChatFeed(t *testing.T) (*chatFeed, *[]LLMCallIn) {
	conv, calls, _ := newTestConverter()
	return &chatFeed{t: t, conv: conv, t0: time.Date(2026, 9, 30, 11, 46, 56, 0, time.UTC), seq: map[string]int{}}, calls
}

func wantReply(replyID string) string { return "Reply text for " + replyID[:8] }

func replyTexts(t *testing.T, c LLMCallIn) string {
	t.Helper()
	var msg struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Model   string `json:"model"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(c.ResponseBody), &msg); err != nil {
		t.Fatalf("response_body is not an Anthropic message JSON: %v: %q", err, c.ResponseBody)
	}
	if msg.Type != "message" || msg.Role != "assistant" {
		t.Errorf("response_body type/role = %q/%q, want message/assistant", msg.Type, msg.Role)
	}
	var sb strings.Builder
	for _, b := range msg.Content {
		if b.Type != "text" {
			t.Errorf("unexpected content block type %q", b.Type)
		}
		sb.WriteString(b.Text)
	}
	return sb.String()
}

func requestPrompt(t *testing.T, c LLMCallIn) string {
	t.Helper()
	var req struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(c.RequestBody), &req); err != nil {
		t.Fatalf("request_body is not an Anthropic request JSON: %v: %q", err, c.RequestBody)
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != "user" {
		t.Fatalf("request_body messages = %+v, want exactly one user message", req.Messages)
	}
	var msgs []map[string]any
	if err := json.Unmarshal([]byte(c.Messages), &msgs); err != nil || len(msgs) != 1 {
		t.Errorf("messages = %q, want a one-element JSON array", c.Messages)
	}
	return req.Messages[0].Content
}

// Regression for the Sep 30, 2026 live bug: the stream opened on send
// replays history, closes with a trailer, and the reply arrives on a NEW
// stream after the send. fe2500d emitted the history as a record with no
// prompt and never emitted the reply.
func TestChatTab_HistoryReplayThenSendThenReplyOnNewStream(t *testing.T) {
	f, calls := newChatFeed(t)
	f.performAction(0, openDecoded(t, testConvID)) // conversation opened
	f.unit(500*time.Millisecond, testClient, historyUnit(t))
	f.unit(600*time.Millisecond, testClient, keepAliveUnit)
	f.unit(5*time.Second, testClient, `{"trailer":{"metadata":{"Stream-Close-Reason":["closed"]}}}`)
	f.streamEnd(5*time.Second, testClient)
	f.performAction(5100*time.Millisecond, sendDecoded(t, testConvID, newHuman, newAsst, promptText))
	f.unit(5800*time.Millisecond, testClient, historyUnit(t)) // the new stream replays history again
	f.liveReply(6*time.Second, testClient, newHuman, newAsst, "dgrp_r", "cblk_r")
	f.unit(30*time.Second, testClient, keepAliveUnit) // stream stays open

	if len(*calls) != 1 {
		t.Fatalf("got %d records, want exactly 1 (the reply): %+v", len(*calls), *calls)
	}
	c := (*calls)[0]
	if got := replyTexts(t, c); got != wantReply(newAsst) {
		t.Errorf("reply = %q, want %q", got, wantReply(newAsst))
	}
	if strings.Contains(c.ResponseBody, "HISTORY") || strings.Contains(c.RequestBody, "HISTORY") {
		t.Errorf("history text leaked into the record: %+v", c)
	}
	if got := requestPrompt(t, c); got != promptText {
		t.Errorf("prompt = %q, want %q", got, promptText)
	}
	if c.RequestID != "icp_chat_"+newAsst || c.SessionID != testConvID || c.Model != testModel {
		t.Errorf("request_id/session/model = %q/%q/%q", c.RequestID, c.SessionID, c.Model)
	}
	if want := f.t0.Add(5100 * time.Millisecond).Format(time.RFC3339Nano); c.Timestamp != want {
		t.Errorf("timestamp = %q, want the send time %q", c.Timestamp, want)
	}
	// Send at 5.1s, last live text at 6.08s.
	if c.DurationMS == nil || *c.DurationMS != 980 {
		t.Errorf("duration_ms = %v, want 980", c.DurationMS)
	}
}

// The send's record can be logged after the reply's first units (its
// response returns late): pairing is by UUID, so order does not matter.
func TestChatTab_ReplyUnitsBeforeSendIsLogged(t *testing.T) {
	f, calls := newChatFeed(t)
	f.unit(0, testClient, historyUnit(t))
	f.liveReply(time.Second, testClient, newHuman, newAsst, "dgrp_r", "cblk_r")
	if len(*calls) != 0 {
		t.Fatalf("emitted %d records before the send was known", len(*calls))
	}
	f.performAction(900*time.Millisecond, sendDecoded(t, testConvID, newHuman, newAsst, promptText))
	if len(*calls) != 1 {
		t.Fatalf("got %d records, want 1", len(*calls))
	}
	if got := replyTexts(t, (*calls)[0]); got != wantReply(newAsst) {
		t.Errorf("reply = %q", got)
	}
}

// The client closed the stream mid-reply. The reply is recorded once a
// later stream replays it complete, under new block ids but the same group
// id, without duplicating the part seen live.
func TestChatTab_StreamClosedMidReplyCompletedByLaterReplay(t *testing.T) {
	f, calls := newChatFeed(t)
	f.performAction(0, sendDecoded(t, testConvID, newHuman, newAsst, promptText))
	f.unit(100*time.Millisecond, "c1", eventUnit(t, obj{"3": msgRec(newAsst, newHuman, 2, false)}))
	f.unit(200*time.Millisecond, "c1", eventUnit(t, obj{"4": obj{"1": "dgrp_r", "2": newAsst}, "5": obj{"1": "cblk_live", "2": "dgrp_r", "9": "Partial"}}))
	f.streamEnd(300*time.Millisecond, "c1")
	if len(*calls) != 0 {
		t.Fatalf("emitted a partial reply on stream close")
	}
	full := "Partial and then the rest"
	f.unit(time.Minute, "c2", eventUnit(t, obj{
		"2": header(testConvID),
		"3": []any{msgRec(newHuman, histAsst1, 1, true), msgRec(newAsst, newHuman, 2, true)},
		"4": []any{obj{"1": "dgrp_q", "2": newHuman}, obj{"1": "dgrp_r", "2": newAsst}},
		"5": []any{obj{"1": "cblk_q2", "2": "dgrp_q", "9": promptText}, obj{"1": "cblk_replay", "2": "dgrp_r", "9": full}},
	}))
	if len(*calls) != 1 {
		t.Fatalf("got %d records, want 1", len(*calls))
	}
	if got := replyTexts(t, (*calls)[0]); got != full {
		t.Errorf("reply = %q, want %q", got, full)
	}
}

// History and a live reply with no send seen: nothing is emitted, and the
// orphan is counted once it ages out.
func TestChatTab_NoSendNoRecord(t *testing.T) {
	f, calls := newChatFeed(t)
	f.unit(0, testClient, historyUnit(t))
	f.liveReply(time.Second, testClient, newHuman, newAsst, "dgrp_r", "cblk_r")
	f.unit(chatTriggerMaxAge+time.Minute, testClient, keepAliveUnit)
	if len(*calls) != 0 {
		t.Fatalf("got %d records, want 0: %+v", len(*calls), *calls)
	}
	if n := f.conv.Stats.Snapshot()[ReasonNoTrigger]; n != 1 {
		t.Errorf("no_trigger = %d, want 1", n)
	}
}

func TestChatTab_SendWhoseReplyNeverCompletesIsCounted(t *testing.T) {
	f, calls := newChatFeed(t)
	f.performAction(0, sendDecoded(t, testConvID, newHuman, newAsst, promptText))
	f.unit(time.Second, testClient, eventUnit(t, obj{"3": msgRec(newAsst, newHuman, 2, false)}))
	f.unit(chatTriggerMaxAge+time.Minute, testClient, keepAliveUnit)
	if len(*calls) != 0 {
		t.Fatalf("got %d records, want 0", len(*calls))
	}
	if n := f.conv.Stats.Snapshot()[ReasonChatNoReply]; n != 1 {
		t.Errorf("chat_no_reply = %d, want 1", n)
	}
}

// The stream says the reply's parent is a different human message than
// the send's: never emit a wrong pairing.
func TestChatTab_ParentMismatchIsSkipped(t *testing.T) {
	f, calls := newChatFeed(t)
	f.performAction(0, sendDecoded(t, testConvID, newHuman, newAsst, promptText))
	f.liveReply(time.Second, testClient, histHuman1, newAsst, "dgrp_r", "cblk_r")
	if len(*calls) != 0 {
		t.Fatalf("got %d records, want 0", len(*calls))
	}
	if n := f.conv.Stats.Snapshot()[ReasonChatPairMismatch]; n != 1 {
		t.Errorf("chat_pair_mismatch = %d, want 1", n)
	}
}

func TestChatTab_TwoSendsOnOneOpenStream(t *testing.T) {
	const human2, asst2 = "cccccccc-0000-4000-8000-000000000005", "cccccccc-0000-4000-8000-000000000006"
	f, calls := newChatFeed(t)
	f.unit(0, testClient, historyUnit(t))
	f.performAction(time.Second, sendDecoded(t, testConvID, newHuman, newAsst, promptText))
	f.liveReply(1100*time.Millisecond, testClient, newHuman, newAsst, "dgrp_r1", "cblk_r1")
	f.performAction(10*time.Second, sendDecoded(t, testConvID, human2, asst2, promptText))
	f.liveReply(10100*time.Millisecond, testClient, human2, asst2, "dgrp_r2", "cblk_r2")
	if len(*calls) != 2 {
		t.Fatalf("got %d records, want 2", len(*calls))
	}
	for i, id := range []string{newAsst, asst2} {
		c := (*calls)[i]
		if c.RequestID != "icp_chat_"+id {
			t.Errorf("record %d request_id = %q", i, c.RequestID)
		}
		if got := replyTexts(t, c); got != wantReply(id) {
			t.Errorf("record %d reply = %q, want %q", i, got, wantReply(id))
		}
	}
}

func TestChatTab_SendMissingFieldsIsCounted(t *testing.T) {
	f, calls := newChatFeed(t)
	f.performAction(0, mustJSONT(t, obj{"1": obj{"2": testConvID}, "2": obj{"3": promptText}})) // no reply UUID
	f.liveReply(time.Second, testClient, newHuman, newAsst, "dgrp_r", "cblk_r")
	if len(*calls) != 0 {
		t.Fatalf("got %d records, want 0", len(*calls))
	}
	if n := f.conv.Stats.Snapshot()[ReasonProtoFieldNotFound]; n != 1 {
		t.Errorf("proto_field_not_found = %d, want 1", n)
	}
}
