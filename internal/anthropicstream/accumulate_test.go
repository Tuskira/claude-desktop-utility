package anthropicstream

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// ev is a small helper for building one raw stream event line in a table
// test.
func ev(s string) []byte { return []byte(s) }

func TestAccumulate_TextAndToolUseInterleaved(t *testing.T) {
	m := &Message{}
	events := []string{
		`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-5"}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Let me "}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"look."}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"command\":"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"ls\"}"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"input_tokens":3,"output_tokens":40}}`,
		`{"type":"message_stop"}`,
	}
	for i, e := range events {
		if err := m.Accumulate(ev(e)); err != nil {
			t.Fatalf("event %d (%s): %v", i, e, err)
		}
	}
	if m.Errors != 0 {
		t.Errorf("Errors = %d, want 0", m.Errors)
	}
	if len(m.Content) != 2 {
		t.Fatalf("len(Content) = %d, want 2", len(m.Content))
	}
	if m.Content[0].Type != "text" || m.Content[0].Text != "Let me look." {
		t.Errorf("block 0 = %+v", m.Content[0])
	}
	if m.Content[1].Type != "tool_use" || string(m.Content[1].Input) != `{"command":"ls"}` {
		t.Errorf("block 1 input = %s, want {\"command\":\"ls\"}", m.Content[1].Input)
	}
	if m.StopReason != "tool_use" {
		t.Errorf("StopReason = %q, want tool_use", m.StopReason)
	}
	if m.Usage.InputTokens == nil || *m.Usage.InputTokens != 3 {
		t.Errorf("InputTokens = %v, want 3", m.Usage.InputTokens)
	}
	if m.Usage.OutputTokens == nil || *m.Usage.OutputTokens != 40 {
		t.Errorf("OutputTokens = %v, want 40", m.Usage.OutputTokens)
	}
}

func TestAccumulate_ToolInputSplitAcrossManyFragments(t *testing.T) {
	m := &Message{}
	must(t, m, `{"type":"message_start","message":{"id":"msg_2"}}`)
	must(t, m, `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","name":"f","input":{}}}`)
	full := `{"a": "aaaaaaaaaa", "b": [1,2,3,4,5], "c": {"nested": true}}`
	for i := 0; i < len(full); i++ {
		frag := string(full[i])
		delta, err := json.Marshal(map[string]any{
			"type":  "content_block_delta",
			"index": 0,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": frag},
		})
		if err != nil {
			t.Fatal(err)
		}
		must(t, m, string(delta))
	}
	must(t, m, `{"type":"content_block_stop","index":0}`)

	var got, want any
	if err := json.Unmarshal(m.Content[0].Input, &got); err != nil {
		t.Fatalf("Input did not parse as JSON: %v (%s)", err, m.Content[0].Input)
	}
	if err := json.Unmarshal([]byte(full), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Input = %s, want %s", m.Content[0].Input, full)
	}
	if m.Errors != 0 {
		t.Errorf("Errors = %d, want 0", m.Errors)
	}
}

func TestAccumulate_ThinkingWithSignature(t *testing.T) {
	m := &Message{}
	must(t, m, `{"type":"message_start","message":{"id":"msg_3"}}`)
	must(t, m, `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`)
	must(t, m, `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"step one, "}}`)
	must(t, m, `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"step two."}}`)
	must(t, m, `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"abc"}}`)
	must(t, m, `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"def"}}`)
	must(t, m, `{"type":"content_block_stop","index":0}`)

	cb := m.Content[0]
	if cb.Thinking != "step one, step two." {
		t.Errorf("Thinking = %q", cb.Thinking)
	}
	if cb.Signature != "abcdef" {
		t.Errorf("Signature = %q, want abcdef", cb.Signature)
	}
	var raw struct {
		Thinking  string `json:"thinking"`
		Signature string `json:"signature"`
	}
	if err := json.Unmarshal(cb.Raw(), &raw); err != nil {
		t.Fatal(err)
	}
	if raw.Thinking != cb.Thinking || raw.Signature != cb.Signature {
		t.Errorf("Raw() = %s, want thinking/signature to match accumulated fields", cb.Raw())
	}
}

func TestAccumulate_Citations(t *testing.T) {
	m := &Message{}
	must(t, m, `{"type":"message_start","message":{"id":"msg_4"}}`)
	must(t, m, `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	must(t, m, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"cited fact"}}`)
	must(t, m, `{"type":"content_block_delta","index":0,"delta":{"type":"citations_delta","citation":{"type":"char_location","cited_text":"cited fact","document_index":0,"start_char_index":0,"end_char_index":10}}}`)
	must(t, m, `{"type":"content_block_delta","index":0,"delta":{"type":"citations_delta","citation":{"type":"char_location","cited_text":"cited fact","document_index":1,"start_char_index":0,"end_char_index":10}}}`)
	must(t, m, `{"type":"content_block_stop","index":0}`)

	cb := m.Content[0]
	if len(cb.Citations) != 2 {
		t.Fatalf("len(Citations) = %d, want 2", len(cb.Citations))
	}
	if !strings.Contains(string(cb.Citations[0]), `"document_index":0`) || !strings.Contains(string(cb.Citations[1]), `"document_index":1`) {
		t.Errorf("Citations = %s", cb.Citations)
	}
	if !strings.Contains(string(cb.Raw()), `"citations"`) {
		t.Errorf("Raw() = %s, want a citations array", cb.Raw())
	}
}

func TestAccumulate_WebSearchServerToolAndResult(t *testing.T) {
	m := &Message{}
	must(t, m, `{"type":"message_start","message":{"id":"msg_5"}}`)
	must(t, m, `{"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"srv_1","name":"web_search","input":{}}}`)
	must(t, m, `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"x\"}"}}`)
	must(t, m, `{"type":"content_block_stop","index":0}`)
	must(t, m, `{"type":"content_block_start","index":1,"content_block":{"type":"web_search_tool_result","tool_use_id":"srv_1","content":[{"type":"web_search_result","url":"https://example.com","title":"T","encrypted_content":"e"}]}}`)
	must(t, m, `{"type":"content_block_stop","index":1}`)

	if m.Content[0].Type != "server_tool_use" || string(m.Content[0].Input) != `{"query":"x"}` {
		t.Errorf("server_tool_use block = %+v", m.Content[0])
	}
	resultRaw := m.Content[1].Raw()
	if m.Content[1].Type != "web_search_tool_result" || !strings.Contains(string(resultRaw), `"tool_use_id":"srv_1"`) || !strings.Contains(string(resultRaw), "example.com") {
		t.Errorf("web_search_tool_result block Raw() = %s", resultRaw)
	}
}

func TestAccumulate_UnknownBlockTypePreserved(t *testing.T) {
	m := &Message{}
	must(t, m, `{"type":"message_start","message":{"id":"msg_6"}}`)
	must(t, m, `{"type":"content_block_start","index":0,"content_block":{"type":"future_block_type","widget_id":"w1","payload":{"a":1}}}`)
	must(t, m, `{"type":"content_block_stop","index":0}`)

	if m.Errors != 0 {
		t.Errorf("Errors = %d, want 0 (an unrecognized block type is preserved, not an error)", m.Errors)
	}
	raw := string(m.Content[0].Raw())
	if !strings.Contains(raw, `"future_block_type"`) || !strings.Contains(raw, `"widget_id":"w1"`) || !strings.Contains(raw, `"a":1`) {
		t.Errorf("Raw() = %s, want the unknown block's fields preserved verbatim", raw)
	}
}

func TestAccumulate_UnknownDeltaTypeIgnoredAndCounted(t *testing.T) {
	m := &Message{}
	must(t, m, `{"type":"message_start","message":{"id":"msg_7"}}`)
	must(t, m, `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	must(t, m, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`)
	must(t, m, `{"type":"content_block_delta","index":0,"delta":{"type":"future_delta_type","some_field":"x"}}`)
	must(t, m, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}`)
	must(t, m, `{"type":"content_block_stop","index":0}`)

	if m.Content[0].Text != "Hello world" {
		t.Errorf("Text = %q, want %q (unrecognized delta ignored, later deltas still applied)", m.Content[0].Text, "Hello world")
	}
	if m.Errors != 1 {
		t.Errorf("Errors = %d, want 1 (the one unrecognized delta)", m.Errors)
	}
}

func TestAccumulate_MalformedPartialJSON(t *testing.T) {
	// Diverges deliberately from the real SDK (which resets a tool call's
	// truncated input to "{}"): the interceptor keeps the raw text as a
	// JSON string instead, because these records exist for forensic
	// replay of exactly what the model sent, truncation included. See
	// testdata/stream/malformed_partial_json.golden.json for the SDK's
	// own "{}"-reset output on the same fixture.
	m := &Message{}
	must(t, m, `{"type":"message_start","message":{"id":"msg_8"}}`)
	must(t, m, `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","name":"run","input":{}}}`)
	must(t, m, `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"cmd\": \"do_someth"}}`)
	must(t, m, `{"type":"content_block_stop","index":0}`)

	cb := m.Content[0]
	var raw struct {
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(cb.Raw(), &raw); err != nil {
		t.Fatal(err)
	}
	var asString string
	if err := json.Unmarshal(raw.Input, &asString); err != nil {
		t.Fatalf("input field is not a JSON string: %s: %v", raw.Input, err)
	}
	if asString != `{"cmd": "do_someth` {
		t.Errorf("input string = %q, want the raw truncated text", asString)
	}
	if m.Errors != 1 {
		t.Errorf("Errors = %d, want 1", m.Errors)
	}
	// Calling Raw()/refresh again must not double-count or change the result.
	raw2 := cb.Raw()
	if string(raw2) != string(cb.Raw()) {
		t.Errorf("Raw() not idempotent: %s vs %s", raw2, cb.Raw())
	}
}

func TestAccumulate_EmptyInputBufferYieldsEmptyObject(t *testing.T) {
	m := &Message{}
	must(t, m, `{"type":"message_start","message":{"id":"msg_9"}}`)
	must(t, m, `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","name":"noop","input":{}}}`)
	must(t, m, `{"type":"content_block_stop","index":0}`)

	var raw struct {
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(m.Content[0].Raw(), &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw.Input) != "{}" {
		t.Errorf("input = %s, want {}", raw.Input)
	}
	if m.Errors != 0 {
		t.Errorf("Errors = %d, want 0", m.Errors)
	}
}

func TestAccumulate_OutOfRangeIndexIsErrorNotPanic(t *testing.T) {
	tests := []struct {
		name  string
		event string
	}{
		{"content_block_start skips ahead", `{"type":"content_block_start","index":5,"content_block":{"type":"text","text":""}}`},
		{"content_block_delta before any block", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"x"}}`},
		{"content_block_stop before any block", `{"type":"content_block_stop","index":0}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &Message{}
			must(t, m, `{"type":"message_start","message":{"id":"msg_10"}}`)
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("panicked: %v", r)
					}
				}()
				if err := m.Accumulate(ev(tt.event)); err == nil {
					t.Errorf("Accumulate(%s) returned nil error, want an out-of-range error", tt.event)
				}
			}()
			if m.Errors != 1 {
				t.Errorf("Errors = %d, want 1", m.Errors)
			}
			if len(m.Content) != 0 {
				t.Errorf("Content = %v, want unchanged (empty)", m.Content)
			}
		})
	}
}

func TestAccumulate_OutOfRangeThenRecovers(t *testing.T) {
	// A single bad event must not corrupt the rest of the accumulation:
	// later, correctly-indexed events still apply normally.
	m := &Message{}
	must(t, m, `{"type":"message_start","message":{"id":"msg_11"}}`)
	must(t, m, `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	if err := m.Accumulate(ev(`{"type":"content_block_delta","index":7,"delta":{"type":"text_delta","text":"lost"}}`)); err == nil {
		t.Fatal("expected an out-of-range error")
	}
	must(t, m, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"kept"}}`)
	must(t, m, `{"type":"content_block_stop","index":0}`)
	if m.Content[0].Text != "kept" {
		t.Errorf("Text = %q, want kept", m.Content[0].Text)
	}
	if m.Errors != 1 {
		t.Errorf("Errors = %d, want 1", m.Errors)
	}
}

func TestAccumulate_MessageDeltaUsageOverwriteVsKeep(t *testing.T) {
	m := &Message{}
	must(t, m, `{"type":"message_start","message":{"id":"msg_12","usage":{"input_tokens":100,"output_tokens":1,"cache_read_input_tokens":50,"cache_creation_input_tokens":5}}}`)
	// First delta: only output_tokens and input_tokens present.
	must(t, m, `{"type":"message_delta","delta":{"stop_reason":null},"usage":{"input_tokens":100,"output_tokens":20}}`)
	if m.Usage.CacheReadInputTokens == nil || *m.Usage.CacheReadInputTokens != 50 {
		t.Errorf("CacheReadInputTokens = %v, want kept at 50 (this delta omitted it)", m.Usage.CacheReadInputTokens)
	}
	if m.Usage.OutputTokens == nil || *m.Usage.OutputTokens != 20 {
		t.Errorf("OutputTokens = %v, want overwritten to 20", m.Usage.OutputTokens)
	}
	// Second delta: cache_read_input_tokens now present, overwrites.
	must(t, m, `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":100,"output_tokens":45,"cache_read_input_tokens":80}}`)
	if m.Usage.CacheReadInputTokens == nil || *m.Usage.CacheReadInputTokens != 80 {
		t.Errorf("CacheReadInputTokens = %v, want overwritten to 80", m.Usage.CacheReadInputTokens)
	}
	if m.Usage.CacheCreationInputTokens == nil || *m.Usage.CacheCreationInputTokens != 5 {
		t.Errorf("CacheCreationInputTokens = %v, want kept at 5 (never mentioned again)", m.Usage.CacheCreationInputTokens)
	}
	if m.StopReason != "end_turn" {
		t.Errorf("StopReason = %q, want end_turn", m.StopReason)
	}
}

func TestAccumulate_ServerToolUseUsageOverwrite(t *testing.T) {
	m := &Message{}
	must(t, m, `{"type":"message_start","message":{"id":"msg_13","usage":{"input_tokens":1,"output_tokens":1,"server_tool_use":{"web_search_requests":0,"web_fetch_requests":0}}}}`)
	must(t, m, `{"type":"message_delta","delta":{},"usage":{"input_tokens":1,"output_tokens":9,"server_tool_use":{"web_search_requests":1,"web_fetch_requests":0}}}`)
	if m.Usage.ServerToolUse == nil || m.Usage.ServerToolUse.WebSearchRequests != 1 {
		t.Errorf("ServerToolUse = %+v, want WebSearchRequests=1", m.Usage.ServerToolUse)
	}
}

func TestAccumulate_MessageStartReplacesWholeMessage(t *testing.T) {
	m := &Message{}
	must(t, m, `{"type":"message_start","message":{"id":"msg_14","model":"model-a"}}`)
	must(t, m, `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"x"}}`)
	// A second message_start (a fresh turn reusing the same Message value)
	// must wipe prior content, not append to it.
	must(t, m, `{"type":"message_start","message":{"id":"msg_15","model":"model-b"}}`)
	if m.ID != "msg_15" || m.Model != "model-b" {
		t.Errorf("ID/Model = %q/%q, want msg_15/model-b", m.ID, m.Model)
	}
	if len(m.Content) != 0 {
		t.Errorf("Content = %v, want reset to empty", m.Content)
	}
}

func TestAccumulate_NilMessageNeverPanics(t *testing.T) {
	var m *Message
	if err := m.Accumulate(ev(`{"type":"message_start","message":{"id":"x"}}`)); err == nil {
		t.Error("expected an error accumulating into a nil Message")
	}
}

func TestAccumulate_InvalidEventJSONNeverPanics(t *testing.T) {
	m := &Message{}
	if err := m.Accumulate(ev(`not json`)); err == nil {
		t.Error("expected an error for malformed event JSON")
	}
	if m.Errors != 1 {
		t.Errorf("Errors = %d, want 1", m.Errors)
	}
}

func TestAccumulate_ContentBlockStartMustFollowIndexOrder(t *testing.T) {
	m := &Message{}
	must(t, m, `{"type":"message_start","message":{"id":"msg_16"}}`)
	// index 1 before any index 0 block exists: rejected.
	if err := m.Accumulate(ev(`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`)); err == nil {
		t.Error("expected an error for a content_block_start that skips index 0")
	}
	if len(m.Content) != 0 {
		t.Errorf("Content = %v, want still empty", m.Content)
	}
	must(t, m, `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	if len(m.Content) != 1 {
		t.Fatalf("Content = %v, want one block after the correctly-indexed start", m.Content)
	}
}

func must(t *testing.T, m *Message, event string) {
	t.Helper()
	if err := m.Accumulate(ev(event)); err != nil {
		t.Fatalf("Accumulate(%s): %v", event, err)
	}
}

// --- Golden comparison against the real Anthropic Go SDK ---
//
// The fixtures under ../claudedesktop/testdata/stream/*.jsonl were fed
// through anthropic.Message.Accumulate from
// github.com/anthropics/anthropic-sdk-go v1.76.0 (a throwaway program
// outside this module, since the SDK is intentionally not a dependency
// here) and its RawJSON() output saved as the matching *.golden.json.
// This test re-runs the same fixtures through this package and compares
// content, stop_reason and usage semantically (parsed and reflect.
// DeepEqual'd, not byte-for-byte, since key order and whitespace are not
// part of the contract). malformed_partial_json.jsonl is excluded: its
// golden documents the SDK's own behavior, which this package
// deliberately diverges from (see TestAccumulate_MalformedPartialJSON).
func TestAccumulate_MatchesSDKGolden(t *testing.T) {
	dir := filepath.Join("..", "claudedesktop", "testdata", "stream")
	fixtures, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(fixtures) == 0 {
		t.Fatalf("no fixtures found in %s", dir)
	}
	skip := map[string]bool{"malformed_partial_json.jsonl": true}
	ran := 0
	for _, fixture := range fixtures {
		name := filepath.Base(fixture)
		if skip[name] {
			continue
		}
		ran++
		t.Run(name, func(t *testing.T) {
			lines, err := readLines(fixture)
			if err != nil {
				t.Fatal(err)
			}
			m := &Message{}
			for i, line := range lines {
				if err := m.Accumulate([]byte(line)); err != nil {
					t.Fatalf("line %d: %v", i+1, err)
				}
			}

			goldenPath := strings.TrimSuffix(fixture, ".jsonl") + ".golden.json"
			goldenBytes, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatal(err)
			}
			var golden struct {
				Content    []map[string]any `json:"content"`
				StopReason any              `json:"stop_reason"`
				Usage      map[string]any   `json:"usage"`
			}
			if err := json.Unmarshal(goldenBytes, &golden); err != nil {
				t.Fatal(err)
			}

			gotContent := make([]map[string]any, 0, len(m.Content))
			for _, cb := range m.Content {
				var v map[string]any
				if err := json.Unmarshal(cb.Raw(), &v); err != nil {
					t.Fatalf("block did not marshal to a JSON object: %v: %s", err, cb.Raw())
				}
				gotContent = append(gotContent, v)
			}
			if !reflect.DeepEqual(gotContent, golden.Content) {
				t.Errorf("content mismatch\n got:  %s\n want: %s", mustMarshal(gotContent), mustMarshal(golden.Content))
			}

			var gotStopReason any
			if m.StopReason != "" {
				gotStopReason = m.StopReason
			}
			if !reflect.DeepEqual(gotStopReason, golden.StopReason) {
				t.Errorf("stop_reason = %v, want %v", gotStopReason, golden.StopReason)
			}

			gotUsage := usageToMap(m.Usage)
			if !mapSubsetEqual(gotUsage, golden.Usage) {
				t.Errorf("usage mismatch\n got:  %s\n want: %s", mustMarshal(gotUsage), mustMarshal(golden.Usage))
			}
		})
	}
	if ran == 0 {
		t.Fatal("every fixture was skipped; nothing was actually compared")
	}
}

func readLines(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out, nil
}

func usageToMap(u Usage) map[string]any {
	out := map[string]any{}
	if u.InputTokens != nil {
		out["input_tokens"] = float64(*u.InputTokens)
	}
	if u.OutputTokens != nil {
		out["output_tokens"] = float64(*u.OutputTokens)
	}
	if u.CacheCreationInputTokens != nil {
		out["cache_creation_input_tokens"] = float64(*u.CacheCreationInputTokens)
	}
	if u.CacheReadInputTokens != nil {
		out["cache_read_input_tokens"] = float64(*u.CacheReadInputTokens)
	}
	if u.ServerToolUse != nil {
		out["server_tool_use"] = map[string]any{
			"web_fetch_requests":  float64(u.ServerToolUse.WebFetchRequests),
			"web_search_requests": float64(u.ServerToolUse.WebSearchRequests),
		}
	}
	if u.OutputTokensDetails != nil {
		out["output_tokens_details"] = map[string]any{
			"thinking_tokens": float64(u.OutputTokensDetails.ThinkingTokens),
		}
	}
	return out
}

// mapSubsetEqual reports whether every key got has matches want's value;
// the golden usage objects may carry additional SDK-only zero-value
// fields (service_tier, cache_creation, ...) this package does not track,
// which is fine as long as none of the fields it does track disagree.
func mapSubsetEqual(got, want map[string]any) bool {
	for k, v := range got {
		wv, ok := want[k]
		if !ok || !reflect.DeepEqual(v, wv) {
			return false
		}
	}
	return true
}

func mustMarshal(v any) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}
