package claudedesktop

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// Chat tab protobuf field paths.
//
// protodec decodes these messages without a .proto file, so field numbers
// are all it can name. The meanings below were reverse-engineered from
// Claude Desktop 2.9939.4 traffic (anthropic.bard.api.v1alpha.
// ConversationService) and VERIFIED on Sep 30, 2026 against every message
// send in the local capture (6 of 6 sends, 4 conversations, including
// replies whose stream was closed before they finished). Anthropic can
// renumber these in any release; a missing field is skipped and counted,
// never guessed at.
//
// PerformAction request (a message send is the variant that has field 2;
// the variant sent when a conversation is opened has no field 2):
//
//	1.2  conversation UUID (matches the chat_conversations/{uuid} URL)
//	2.1  UUID of the new human message
//	2.2  UUID of the assistant message that will hold the reply
//	2.3  the prompt text, exactly as typed
//
// Field 2.7.2 (model), documented from an earlier capture, did not appear
// in any of the 6 verified sends; the model is read from the stream's
// conversation header (1.1.2.6.2) instead.
const (
	fieldPerformActionConversationID = "1.2"
	fieldPerformActionSend           = "2"
	fieldPerformActionHumanMsgID     = "2.1"
	fieldPerformActionReplyMsgID     = "2.2"
	fieldPerformActionPromptText     = "2.3"
)

// StreamTimeline stream units. Each unit is one envelope whose field 1 is an
// event. The same shapes carry both live updates and the history replay a
// stream sends when it opens; in a replay unit the repeated fields below are
// arrays holding the whole conversation, in a live unit they are usually
// single objects. Verified paths:
//
//	1.1.2        conversation header; .1 conversation UUID, .6.2 model
//	1.1.3        message record(s); .1 message UUID, .2 conversation UUID,
//	             .3 sender (1 = human, 2 = assistant), .4 index in the
//	             conversation, .6 == 1 once the message is complete (absent
//	             while it is still streaming), .11 parent message UUID
//	1.1.4        display group(s); .1 "dgrp_..." id, .2 owning message UUID
//	1.1.5        content block(s); .1 "cblk_..." id, .2 owning dgrp id,
//	             .9 full text so far (text blocks only; thinking blocks
//	             have no field 9)
//	1.2          text delta; .1 cblk id, .2 text to append to that block
//
// The discriminator between history and a new reply is the message UUID:
// a send names its reply's UUID (PerformAction 2.2), the reply's groups
// point at that UUID (1.1.4.2), and its blocks point at those groups
// (1.1.5.2). Arrival order is not used.
//
// Block ids are NOT stable: a history replay re-issues a finished message's
// blocks under new cblk ids. Group ids ARE stable across replays, and each
// group held exactly one block in every live reply seen, so reply text is
// kept per group: a field-9 snapshot replaces the group's text, a delta for
// the group's current block appends to it.
const (
	pathStreamConvHeader = "1.1.2"
	pathStreamMessages   = "1.1.3"
	pathStreamGroups     = "1.1.4"
	pathStreamBlocks     = "1.1.5"
	pathStreamDelta      = "1.2"

	senderAssistant = 2
)

// modelPattern validates the model string read from 1.1.2.6.2.
var modelPattern = regexp.MustCompile(`^claude-[a-z0-9]+(-[a-z0-9]+)*$`)

const (
	cblkPrefix = "cblk_"
	dgrpPrefix = "dgrp_"
)

// protoTree is protodec's decoded-message shape: a JSON object keyed by
// decimal field number, where a value is a string, a number, a nested
// object, or an array of any of those (a repeated field).
type protoTree = map[string]json.RawMessage

// decodeProtoTree unmarshals raw protodec JSON into a protoTree. ok is false
// if raw isn't a JSON object (e.g. it's an array, a bare string, or empty).
func decodeProtoTree(raw json.RawMessage) (protoTree, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return nil, false
	}
	var t protoTree
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, false
	}
	return t, true
}

// fieldPath looks up a dotted field path (e.g. "1.1.5") in a decoded proto
// tree, descending into nested objects. It returns ok=false the moment a
// segment is missing or not an object, without guessing.
func fieldPath(root json.RawMessage, path string) (json.RawMessage, bool) {
	cur := root
	for _, seg := range strings.Split(path, ".") {
		tree, ok := decodeProtoTree(cur)
		if !ok {
			return nil, false
		}
		v, ok := tree[seg]
		if !ok {
			return nil, false
		}
		cur = v
	}
	return cur, true
}

// asString reads a protodec leaf as a string. A single-element array (a
// repeated field seen once) is also accepted.
func asString(raw json.RawMessage) (string, bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, true
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err == nil && len(arr) > 0 {
		return asString(arr[0])
	}
	return "", false
}

// asInt reads a protodec numeric leaf.
func asInt(raw json.RawMessage) (int64, bool) {
	n, err := strconv.ParseInt(string(bytes.TrimSpace(raw)), 10, 64)
	return n, err == nil
}

// stringField looks up path in root and reads it as a string.
func stringField(root json.RawMessage, path string) (string, bool) {
	v, ok := fieldPath(root, path)
	if !ok {
		return "", false
	}
	return asString(v)
}

// treeString reads tree[key] as a string, if present.
func treeString(t protoTree, key string) (string, bool) {
	v, ok := t[key]
	if !ok {
		return "", false
	}
	return asString(v)
}

// treeInt reads tree[key] as an integer, if present.
func treeInt(t protoTree, key string) (int64, bool) {
	v, ok := t[key]
	if !ok {
		return 0, false
	}
	return asInt(v)
}

// objectsAt returns the object(s) at path: one object, or every object in
// an array (a repeated field). Non-object entries are ignored.
func objectsAt(root json.RawMessage, path string) []protoTree {
	v, ok := fieldPath(root, path)
	if !ok {
		return nil
	}
	if t, ok := decodeProtoTree(v); ok {
		return []protoTree{t}
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(v, &arr); err != nil {
		return nil
	}
	out := make([]protoTree, 0, len(arr))
	for _, e := range arr {
		if t, ok := decodeProtoTree(e); ok {
			out = append(out, t)
		}
	}
	return out
}

// streamMessage is one 1.1.3 message record.
type streamMessage struct {
	ID, ConvID, ParentID string
	Sender               int64
	Complete             bool
}

// streamGroup is one 1.1.4 display group.
type streamGroup struct{ DgrpID, MsgID string }

// streamBlock is one 1.1.5 content block. HasText is false for blocks with
// no field 9 (thinking and other non-text blocks).
type streamBlock struct {
	CblkID, DgrpID, Text string
	HasText              bool
}

// streamDelta is one 1.2 text delta.
type streamDelta struct{ CblkID, Text string }

// streamUnit is everything the Chat converter reads from one decoded
// StreamTimeline unit.
type streamUnit struct {
	ConvID, Model string
	Messages      []streamMessage
	Groups        []streamGroup
	Blocks        []streamBlock
	Delta         *streamDelta
}

// parseStreamUnit reads the verified paths above out of one decoded unit.
func parseStreamUnit(raw json.RawMessage) streamUnit {
	var u streamUnit
	for _, h := range objectsAt(raw, pathStreamConvHeader) {
		u.ConvID, _ = treeString(h, "1")
		if m, ok := stringField(h["6"], "2"); ok && modelPattern.MatchString(m) {
			u.Model = m
		}
	}
	for _, m := range objectsAt(raw, pathStreamMessages) {
		id, ok := treeString(m, "1")
		if !ok || id == "" {
			continue
		}
		sm := streamMessage{ID: id}
		sm.ConvID, _ = treeString(m, "2")
		sm.ParentID, _ = treeString(m, "11")
		sm.Sender, _ = treeInt(m, "3")
		done, _ := treeInt(m, "6")
		sm.Complete = done == 1
		u.Messages = append(u.Messages, sm)
	}
	for _, g := range objectsAt(raw, pathStreamGroups) {
		d, ok1 := treeString(g, "1")
		m, ok2 := treeString(g, "2")
		if ok1 && ok2 && strings.HasPrefix(d, dgrpPrefix) {
			u.Groups = append(u.Groups, streamGroup{DgrpID: d, MsgID: m})
		}
	}
	for _, b := range objectsAt(raw, pathStreamBlocks) {
		c, ok1 := treeString(b, "1")
		d, ok2 := treeString(b, "2")
		if !ok1 || !ok2 || !strings.HasPrefix(c, cblkPrefix) || !strings.HasPrefix(d, dgrpPrefix) {
			continue
		}
		text, hasText := treeString(b, "9")
		u.Blocks = append(u.Blocks, streamBlock{CblkID: c, DgrpID: d, Text: text, HasText: hasText})
	}
	for _, d := range objectsAt(raw, pathStreamDelta) {
		c, ok1 := treeString(d, "1")
		text, ok2 := treeString(d, "2")
		if ok1 && ok2 && strings.HasPrefix(c, cblkPrefix) {
			u.Delta = &streamDelta{CblkID: c, Text: text}
		}
	}
	return u
}
