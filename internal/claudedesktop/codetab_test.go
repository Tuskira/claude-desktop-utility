package claudedesktop

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const (
	codeEventsURL = "https://claude.ai/v1/code/sessions/cse_ABC123/events"
	codeWSURL     = "https://claude.ai/v1/sessions/ws/session_ABC123/subscribe?organization_uuid=org-1"
)

func TestCodeTab_TextOnlyTurn(t *testing.T) {
	conv, calls, logs := newTestConverter()
	t0 := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

	conv.Handle(postRecord(t0, codeEventsURL,
		`{"events":[{"payload":{"type":"user","session_id":"session_ABC123","message":{"role":"user","content":"hello"}},"device_attestation":{"kid":"k","signature":"s"}}]}`))

	conv.Handle(wsRecord(t, t0.Add(10*time.Millisecond), codeWSURL, "WS<",
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"msg_001","model":"claude-opus-5-5"}}}`))
	conv.Handle(wsRecord(t, t0.Add(400*time.Millisecond), codeWSURL, "WS<",
		`{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":1,"cache_creation_input_tokens":2}}}`))
	conv.Handle(wsRecord(t, t0.Add(410*time.Millisecond), codeWSURL, "WS<",
		`{"type":"assistant","message":{"id":"msg_001","role":"assistant","content":[{"type":"text","text":"hi there"}]}}`))
	conv.Handle(wsRecord(t, t0.Add(420*time.Millisecond), codeWSURL, "WS<",
		`{"type":"stream_event","event":{"type":"message_stop"}}`))

	if len(*logs) != 0 {
		t.Fatalf("expected no access logs, got %d", len(*logs))
	}
	if len(*calls) != 1 {
		t.Fatalf("expected 1 LLM call, got %d: %+v", len(*calls), *calls)
	}
	c := (*calls)[0]
	if c.RequestID != "icp_msg_001" {
		t.Errorf("request_id = %q, want icp_msg_001", c.RequestID)
	}
	if c.SessionID != "ABC123" {
		t.Errorf("session_id = %q, want ABC123", c.SessionID)
	}
	if c.Model != "claude-opus-5-5" || c.RequestedModel != "claude-opus-5-5" {
		t.Errorf("model = %q / %q", c.Model, c.RequestedModel)
	}
	if c.ClientName != "claude-desktop" || c.Provider != "anthropic" {
		t.Errorf("common fields wrong: client_name=%q provider=%q", c.ClientName, c.Provider)
	}
	if c.UserAgent != testUA {
		t.Errorf("user_agent = %q, want %q", c.UserAgent, testUA)
	}
	if !c.Stream {
		t.Errorf("stream = false, want true")
	}
	if c.InputTokens == nil || *c.InputTokens != 10 {
		t.Errorf("input_tokens = %v, want 10", c.InputTokens)
	}
	if c.OutputTokens == nil || *c.OutputTokens != 20 {
		t.Errorf("output_tokens = %v, want 20", c.OutputTokens)
	}
	if c.CacheReadTokens == nil || *c.CacheReadTokens != 1 {
		t.Errorf("cache_read_tokens = %v, want 1", c.CacheReadTokens)
	}
	if c.CacheCreateTokens == nil || *c.CacheCreateTokens != 2 {
		t.Errorf("cache_creation_tokens = %v, want 2", c.CacheCreateTokens)
	}
	if c.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q, want end_turn", c.StopReason)
	}
	if c.DurationMS == nil || *c.DurationMS != 420 {
		t.Errorf("duration_ms = %v, want 420", c.DurationMS)
	}
	if c.Timestamp != t0.Format(time.RFC3339Nano) {
		t.Errorf("timestamp = %q, want the triggering POST's time", c.Timestamp)
	}
	if !strings.Contains(c.ResponseBody, "hi there") {
		t.Errorf("response_body = %q, want it to contain the reply text", c.ResponseBody)
	}
}

func TestCodeTab_ConnectorAndBuiltinTool(t *testing.T) {
	conv, calls, logs := newTestConverter()
	t0 := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

	conv.Handle(postRecord(t0, codeEventsURL,
		`{"events":[{"payload":{"type":"user","session_id":"session_ABC123","message":{"role":"user","content":"do it"}}}]}`))
	conv.Handle(wsRecord(t, t0.Add(5*time.Millisecond), codeWSURL, "WS<",
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"msg_002","model":"claude-opus-5-5"}}}`))
	conv.Handle(wsRecord(t, t0.Add(10*time.Millisecond), codeWSURL, "WS<",
		`{"type":"assistant","message":{"id":"msg_002","role":"assistant","content":[`+
			`{"type":"tool_use","id":"toolu_conn","name":"aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee:getJiraIssue","input":{"key":"X-1"}},`+
			`{"type":"tool_use","id":"toolu_bash","name":"Bash","input":{"command":"ls"}}`+
			`]}}`))
	conv.Handle(wsRecord(t, t0.Add(20*time.Millisecond), codeWSURL, "WS<",
		`{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"input_tokens":5,"output_tokens":6,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`))
	conv.Handle(wsRecord(t, t0.Add(25*time.Millisecond), codeWSURL, "WS<",
		`{"type":"stream_event","event":{"type":"message_stop"}}`))

	// Tool results echoed back for both tools.
	conv.Handle(wsRecord(t, t0.Add(200*time.Millisecond), codeWSURL, "WS<",
		`{"type":"user","message":{"role":"user","content":[`+
			`{"type":"tool_result","tool_use_id":"toolu_conn","content":"issue found","is_error":false},`+
			`{"type":"tool_result","tool_use_id":"toolu_bash","content":"file1\nfile2","is_error":false}`+
			`]}}`))

	if len(*calls) != 1 {
		t.Fatalf("expected 1 LLM call, got %d", len(*calls))
	}
	if len(*logs) != 1 {
		t.Fatalf("expected 1 access log (connector only, builtin skipped), got %d: %+v", len(*logs), *logs)
	}
	a := (*logs)[0]
	if a.ConnectorID != "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee" {
		t.Errorf("connector_id = %q", a.ConnectorID)
	}
	if a.ToolName != "getJiraIssue" {
		t.Errorf("tool_name = %q, want getJiraIssue", a.ToolName)
	}
	if a.Method != "tools/call" {
		t.Errorf("method = %q, want tools/call", a.Method)
	}
	if a.JSONRPCID != "toolu_conn" || a.RequestID != "icp_toolu_conn" {
		t.Errorf("json_rpc_id/request_id = %q/%q", a.JSONRPCID, a.RequestID)
	}
	if a.StatusCode != 200 {
		t.Errorf("status_code = %d, want 200", a.StatusCode)
	}
	if a.SessionID != "ABC123" || a.ClientSessionID != "ABC123" {
		t.Errorf("session_id/client_session_id = %q/%q", a.SessionID, a.ClientSessionID)
	}
	if a.DurationMS == nil || *a.DurationMS != 190 {
		t.Errorf("duration_ms = %v, want ~190", a.DurationMS)
	}
	if a.BytesIn == nil || *a.BytesIn == 0 {
		t.Errorf("bytes_in = %v, want > 0", a.BytesIn)
	}
	if a.Bytes == nil || *a.Bytes == 0 {
		t.Errorf("bytes = %v, want > 0", a.Bytes)
	}
}

func TestCodeTab_ToolError(t *testing.T) {
	conv, _, logs := newTestConverter()
	t0 := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

	conv.Handle(postRecord(t0, codeEventsURL,
		`{"events":[{"payload":{"type":"user","session_id":"session_ABC123","message":{"role":"user","content":"do it"}}}]}`))
	conv.Handle(wsRecord(t, t0.Add(5*time.Millisecond), codeWSURL, "WS<",
		`{"type":"assistant","message":{"id":"msg_003","role":"assistant","content":[`+
			`{"type":"tool_use","id":"toolu_err","name":"mcp__jira__getIssue","input":{"key":"nope"}}`+
			`]}}`))
	conv.Handle(wsRecord(t, t0.Add(50*time.Millisecond), codeWSURL, "WS<",
		`{"type":"user","message":{"role":"user","content":[`+
			`{"type":"tool_result","tool_use_id":"toolu_err","content":"not found","is_error":true}`+
			`]}}`))

	if len(*logs) != 1 {
		t.Fatalf("expected 1 access log, got %d", len(*logs))
	}
	a := (*logs)[0]
	if a.StatusCode != 500 {
		t.Errorf("status_code = %d, want 500 for is_error tool_result", a.StatusCode)
	}
	if a.ConnectorID != "jira" || a.ToolName != "getIssue" {
		t.Errorf("mcp__ name split wrong: connector_id=%q tool_name=%q", a.ConnectorID, a.ToolName)
	}
}

func TestCodeTab_SkillTool(t *testing.T) {
	conv, _, logs := newTestConverter()
	t0 := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

	conv.Handle(postRecord(t0, codeEventsURL,
		`{"events":[{"payload":{"type":"user","session_id":"session_ABC123","message":{"role":"user","content":"do it"}}}]}`))
	conv.Handle(wsRecord(t, t0.Add(5*time.Millisecond), codeWSURL, "WS<",
		`{"type":"assistant","message":{"id":"msg_004","role":"assistant","content":[`+
			`{"type":"tool_use","id":"toolu_skill","name":"Skill","input":{"command":"deploy-helper"}}`+
			`]}}`))
	conv.Handle(wsRecord(t, t0.Add(50*time.Millisecond), codeWSURL, "WS<",
		`{"type":"user","message":{"role":"user","content":[`+
			`{"type":"tool_result","tool_use_id":"toolu_skill","content":"done","is_error":false}`+
			`]}}`))

	if len(*logs) != 1 {
		t.Fatalf("expected 1 access log for the Skill tool, got %d", len(*logs))
	}
	a := (*logs)[0]
	if a.SkillName != "deploy-helper" {
		t.Errorf("skill_name = %q, want deploy-helper", a.SkillName)
	}
	if a.ConnectorID != "" || a.ToolName != "" {
		t.Errorf("Skill call should not set connector_id/tool_name, got %q/%q", a.ConnectorID, a.ToolName)
	}
}

func TestCodeTab_WSKeepAliveAndUnknownAreDropped(t *testing.T) {
	conv, calls, logs := newTestConverter()
	t0 := time.Now()
	conv.Handle(wsRecord(t, t0, codeWSURL, "WS>", `{"type":"keep_alive"}`))
	conv.Handle(wsRecord(t, t0, codeWSURL, "WS<", `{"type":"system","subtype":"thinking_tokens"}`))
	conv.Handle(wsRecord(t, t0, codeWSURL, "WS<", `{"type":"result","subtype":"success","is_error":false}`))
	if len(*calls) != 0 || len(*logs) != 0 {
		t.Fatalf("expected nothing emitted, got %d calls / %d logs", len(*calls), len(*logs))
	}
	if got := conv.Stats.Snapshot()[ReasonKeepAlive]; got != 1 {
		t.Errorf("keepalive skip count = %d, want 1", got)
	}
}

func TestBareID_SharedSuffixAcrossPrefixes(t *testing.T) {
	if bareID("cse_017N9V1ZHzdoJ7BFyAL5CTu7") != bareID("session_017N9V1ZHzdoJ7BFyAL5CTu7") {
		t.Fatal("cse_ and session_ prefixed IDs with the same suffix must map to the same session key")
	}
}

// Mirrors the live Code tab shape verified on Sep 30, 2026: the prompt
// arrives in the events POST, each WS "assistant" message carries one block
// and the last one arrives AFTER message_stop, and tool results come back
// only as WS "user" echoes. Before the fix the record had no request side
// at all and its response held only the block snapshot seen before
// message_stop.
func TestCodeTab_AnthropicShapedBodies(t *testing.T) {
	conv, calls, _ := newTestConverter()
	t0 := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	ws := func(at time.Duration, body string) {
		conv.Handle(wsRecord(t, t0.Add(at), codeWSURL, "WS<", body))
	}
	se := func(at time.Duration, event string) { ws(at, `{"type":"stream_event","event":`+event+`}`) }

	conv.Handle(postRecord(t0, codeEventsURL,
		`{"events":[{"payload":{"type":"user","message":{"role":"user","content":"list files"}},"device_attestation":{"kid":"k","signature":"s"}}]}`))
	ws(100*time.Millisecond, `{"type":"user","message":{"role":"user","content":"list files"}}`) // plain echo: not a trigger

	// Turn 1: thinking (redacted), text, tool_use.
	se(1*time.Second, `{"type":"message_start","message":{"id":"msg_A","model":"claude-opus-5-5"}}`)
	se(1010*time.Millisecond, `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`)
	se(1020*time.Millisecond, `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig"}}`)
	se(1030*time.Millisecond, `{"type":"content_block_stop","index":0}`)
	ws(1040*time.Millisecond, `{"type":"assistant","message":{"id":"msg_A","role":"assistant","content":[{"type":"thinking","thinking":"","signature":"sig"}]}}`)
	se(1050*time.Millisecond, `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`)
	se(1060*time.Millisecond, `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Let me "}}`)
	se(1070*time.Millisecond, `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"look."}}`)
	se(1080*time.Millisecond, `{"type":"content_block_stop","index":1}`)
	se(1090*time.Millisecond, `{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}}`)
	se(1100*time.Millisecond, `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"command\":"}}`)
	se(1110*time.Millisecond, `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"ls\"}"}}`)
	se(1120*time.Millisecond, `{"type":"content_block_stop","index":2}`)
	se(1130*time.Millisecond, `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"input_tokens":3,"output_tokens":40,"cache_read_input_tokens":100,"cache_creation_input_tokens":7}}`)
	se(1140*time.Millisecond, `{"type":"message_stop"}`)
	ws(1150*time.Millisecond, `{"type":"assistant","message":{"id":"msg_A","role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"ls"}}]}}`)

	// Tool result echo, then turn 2 (text only).
	ws(2*time.Second, `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"a.txt","is_error":false}]}}`)
	se(3*time.Second, `{"type":"message_start","message":{"id":"msg_B","model":"claude-opus-5-5"}}`)
	se(3010*time.Millisecond, `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	se(3020*time.Millisecond, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"One file."}}`)
	se(3030*time.Millisecond, `{"type":"content_block_stop","index":0}`)
	se(3040*time.Millisecond, `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":2,"output_tokens":5}}`)
	se(3050*time.Millisecond, `{"type":"message_stop"}`)

	if len(*calls) != 2 {
		t.Fatalf("got %d records, want 2", len(*calls))
	}
	type block struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Input     json.RawMessage `json:"input"`
		ToolUseID string          `json:"tool_use_id"`
	}
	type message struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	var req1, req2 struct {
		Model    string    `json:"model"`
		Messages []message `json:"messages"`
	}
	var resp1 struct {
		Type       string  `json:"type"`
		Role       string  `json:"role"`
		ID         string  `json:"id"`
		Model      string  `json:"model"`
		StopReason string  `json:"stop_reason"`
		Content    []block `json:"content"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			CacheRead    int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	c1, c2 := (*calls)[0], (*calls)[1]
	if err := json.Unmarshal([]byte(c1.RequestBody), &req1); err != nil {
		t.Fatalf("turn 1 request_body: %v: %q", err, c1.RequestBody)
	}
	if req1.Model != "claude-opus-5-5" || len(req1.Messages) != 1 || req1.Messages[0].Role != "user" || string(req1.Messages[0].Content) != `"list files"` {
		t.Errorf("turn 1 request = %+v, want one user message with the POSTed content", req1)
	}
	if c1.Messages != `[{"content":"list files","role":"user"}]` {
		t.Errorf("turn 1 messages = %q", c1.Messages)
	}
	if err := json.Unmarshal([]byte(c1.ResponseBody), &resp1); err != nil {
		t.Fatalf("turn 1 response_body: %v: %q", err, c1.ResponseBody)
	}
	if resp1.Type != "message" || resp1.Role != "assistant" || resp1.ID != "msg_A" || resp1.StopReason != "tool_use" {
		t.Errorf("turn 1 response header = %+v", resp1)
	}
	if resp1.Usage.InputTokens != 3 || resp1.Usage.OutputTokens != 40 || resp1.Usage.CacheRead != 100 {
		t.Errorf("turn 1 usage = %+v", resp1.Usage)
	}
	if len(resp1.Content) != 2 || resp1.Content[0].Type != "text" || resp1.Content[0].Text != "Let me look." ||
		resp1.Content[1].Type != "tool_use" || resp1.Content[1].ID != "toolu_1" || string(resp1.Content[1].Input) != `{"command":"ls"}` {
		t.Errorf("turn 1 content = %+v, want [text, tool_use] (empty thinking dropped)", resp1.Content)
	}

	if err := json.Unmarshal([]byte(c2.RequestBody), &req2); err != nil {
		t.Fatalf("turn 2 request_body: %v", err)
	}
	if len(req2.Messages) != 3 || req2.Messages[0].Role != "user" || req2.Messages[1].Role != "assistant" || req2.Messages[2].Role != "user" {
		t.Fatalf("turn 2 messages = %+v, want user, assistant, user(tool_result)", req2.Messages)
	}
	var tr []block
	if err := json.Unmarshal(req2.Messages[2].Content, &tr); err != nil || len(tr) != 1 || tr[0].Type != "tool_result" || tr[0].ToolUseID != "toolu_1" {
		t.Errorf("turn 2 tool_result turn = %s", req2.Messages[2].Content)
	}
	if !strings.Contains(string(req2.Messages[1].Content), `"toolu_1"`) {
		t.Errorf("turn 2 assistant turn = %s, want the previous tool_use", req2.Messages[1].Content)
	}
	if want := t0.Add(2 * time.Second).Format(time.RFC3339Nano); c2.Timestamp != want {
		t.Errorf("turn 2 timestamp = %q, want the tool result time %q", c2.Timestamp, want)
	}
	if !strings.Contains(c2.ResponseBody, `"text":"One file."`) {
		t.Errorf("turn 2 response_body = %q", c2.ResponseBody)
	}
}
