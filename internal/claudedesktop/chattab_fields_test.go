package claudedesktop

import (
	"encoding/json"
	"testing"
)

func TestFieldPath(t *testing.T) {
	raw := json.RawMessage(`{"1":{"2":"conv"},"2":{"2":"reply","3":"prompt"}}`)
	for path, want := range map[string]string{
		fieldPerformActionConversationID: "conv",
		fieldPerformActionReplyMsgID:     "reply",
		fieldPerformActionPromptText:     "prompt",
	} {
		if got, ok := stringField(raw, path); !ok || got != want {
			t.Errorf("stringField(%s) = %q, %v; want %q", path, got, ok, want)
		}
	}
	if _, ok := fieldPath(raw, "2.9.1"); ok {
		t.Errorf("missing path reported as found")
	}
}

func TestAsString_SingleElementArray(t *testing.T) {
	if s, ok := asString(json.RawMessage(`["x"]`)); !ok || s != "x" {
		t.Errorf("asString = %q, %v", s, ok)
	}
}

// A replay unit holds arrays; a live unit holds single objects. Both are
// read from the same paths.
func TestParseStreamUnit_ArraysAndObjects(t *testing.T) {
	replay := parseStreamUnit(json.RawMessage(`{"1":{"1":{
		"2":{"1":"conv","6":{"2":"claude-opus-5-5"}},
		"3":[{"1":"m1","2":"conv","3":1,"6":1},{"1":"m2","2":"conv","3":2,"6":1,"11":"m1"}],
		"4":[{"1":"dgrp_1","2":"m1"},{"1":"dgrp_2","2":"m2"}],
		"5":[{"1":"cblk_1","2":"dgrp_1","9":"q"},{"1":"cblk_2","2":"dgrp_2","9":"a"}]}}}`))
	if replay.ConvID != "conv" || replay.Model != "claude-opus-5-5" {
		t.Errorf("header = %q/%q", replay.ConvID, replay.Model)
	}
	if len(replay.Messages) != 2 || replay.Messages[1].Sender != 2 || !replay.Messages[1].Complete || replay.Messages[1].ParentID != "m1" {
		t.Errorf("messages = %+v", replay.Messages)
	}
	if len(replay.Groups) != 2 || len(replay.Blocks) != 2 || replay.Blocks[1].Text != "a" {
		t.Errorf("groups/blocks = %+v / %+v", replay.Groups, replay.Blocks)
	}

	live := parseStreamUnit(json.RawMessage(`{"1":{"1":{"3":{"1":"m3","3":2},"5":{"1":"cblk_t","2":"dgrp_t","10":{"1":1}}}}}`))
	if len(live.Messages) != 1 || live.Messages[0].Complete {
		t.Errorf("live message = %+v, want one incomplete message", live.Messages)
	}
	if len(live.Blocks) != 1 || live.Blocks[0].HasText {
		t.Errorf("thinking block = %+v, want no text", live.Blocks)
	}

	delta := parseStreamUnit(json.RawMessage(`{"1":{"2":{"1":"cblk_t","2":"more"}}}`))
	if delta.Delta == nil || delta.Delta.CblkID != "cblk_t" || delta.Delta.Text != "more" {
		t.Errorf("delta = %+v", delta.Delta)
	}
	if ka := parseStreamUnit(json.RawMessage(`{"1":{"6":""}}`)); ka.Delta != nil || len(ka.Messages) != 0 {
		t.Errorf("keep-alive parsed as content: %+v", ka)
	}
	if bad := parseStreamUnit(json.RawMessage(`{"1":{"1":{"2":{"1":"c","6":{"2":"not a model"}}}}}`)); bad.Model != "" {
		t.Errorf("model = %q, want empty for a non-model string", bad.Model)
	}
}
