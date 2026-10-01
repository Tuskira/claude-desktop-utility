package claudedesktop

// Code tab: Claude Desktop 2.9939.4's Anthropic-Messages-API-shaped traffic.
//
// A user message is POSTed to
//   https://claude.ai/v1/code/sessions/{cse_...}/events
// and the model's reply arrives as JSON messages on a WebSocket:
//   wss://claude.ai/v1/sessions/ws/{session_...}/subscribe
//
// The two IDs in those URLs are NOT the same string, but they share a
// suffix: confirmed against a real capture, "cse_017N9V1ZHzdoJ7BFyAL5CTu7"
// (the events POST) and "session_017N9V1ZHzdoJ7BFyAL5CTu7" (the WS
// subscribe) are the same underlying code session. bareID strips the
// "cse_"/"session_" prefix so both sides key into the same session map
// entry.
//
// One LLM record is emitted per message_start...message_stop. Its
// timestamp is "when the triggering user message or tool result was sent",
// tracked here as the session's lastTriggerAt, updated
// on every user-message POST to its /events endpoint and on every
// tool_result the subscribe socket echoes back (verified Sep 30, 2026: in
// the Code tab the tools run remotely, so tool results arrive only as WS
// "user" messages, not as events POSTs).
//
// Request and response bodies use the Anthropic Messages shape (see
// bodies.go). The request is the user message from the events POST; for a
// turn triggered by tool results it is that message, then the previous
// turn's assistant content, then the tool_result user turn (the part of the
// real request that caused this call; earlier turns are left out to keep
// records bounded). The response content is rebuilt from the turn's
// stream events (content_block_start/delta/stop), because the WS
// "assistant" messages each carry one block and the last one arrives after
// message_stop.

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/Tuskira/claude-desktop-utility/internal/anthropicstream"
	"github.com/Tuskira/claude-desktop-utility/internal/capture"
)

var (
	codeEventsPathRE = regexp.MustCompile(`^/v1/code/sessions/([^/]+)/events$`)
	codeWSPathRE     = regexp.MustCompile(`^/v1/sessions/ws/([^/]+)/subscribe$`)
)

func isCodeSessionEventsPath(path string) bool { return codeEventsPathRE.MatchString(path) }
func isCodeSessionWS(path string) bool         { return codeWSPathRE.MatchString(path) }

// codeSessionIDFromPath extracts and normalises the session ID from either
// URL shape above.
func codeSessionIDFromPath(re *regexp.Regexp, path string) (string, bool) {
	m := re.FindStringSubmatch(path)
	if m == nil {
		return "", false
	}
	return bareID(m[1]), true
}

// pendingEntryMaxAge bounds how long an unfinished turn or tool call is kept
// in memory before it is swept away as abandoned (e.g. the app was closed
// mid-turn). Session records themselves use a longer idle bound since a
// Code tab conversation can sit open for a long time between messages.
const (
	pendingEntryMaxAge = 15 * time.Minute
	sessionIdleMaxAge  = 4 * time.Hour
)

// codeState holds all in-memory Code tab bookkeeping. Guarded by
// Converter.mu; see codeSession for the per-session fields.
type codeState struct {
	sessions map[string]*codeSession
}

func (s *codeState) init() { s.sessions = map[string]*codeSession{} }

type codeSession struct {
	lastTriggerAt time.Time
	lastSeenAt    time.Time
	userAgent     string
	upstreamHost  string
	eventsPath    string // scrubbed path of the /events endpoint, for LLMCallIn.Path

	prompt        json.RawMessage   // content of the last user message POSTed to /events
	toolResults   []json.RawMessage // tool_result blocks echoed since the last model call started
	lastAssistant json.RawMessage   // content of the last finished turn, for the next request

	turns map[string]*codeTurn    // keyed by message ID (e.g. "msg_...")
	tools map[string]*pendingTool // keyed by tool_use ID
}

func newCodeSession() *codeSession {
	return &codeSession{turns: map[string]*codeTurn{}, tools: map[string]*pendingTool{}}
}

type codeTurn struct {
	createdAt       time.Time
	startedAt       time.Time                // = session.lastTriggerAt when message_start arrived
	requestMessages []map[string]any         // Anthropic messages that triggered this call
	acc             *anthropicstream.Message // rebuilt from message_start...message_stop
	snapshotBlocks  []json.RawMessage        // blocks from WS "assistant" messages (fallback)
	done            bool
}

type pendingTool struct {
	createdAt   time.Time
	startedAt   time.Time
	toolUseID   string
	connectorID string
	toolName    string
	skillName   string
	bytesIn     int64
	builtin     bool
}

func (c *Converter) codeSessionFor(id string, now time.Time) *codeSession {
	sweepMap(c.code.sessions, func(s *codeSession) time.Time { return s.lastSeenAt }, sessionIdleMaxAge, now)
	s, ok := c.code.sessions[id]
	if !ok {
		s = newCodeSession()
		c.code.sessions[id] = s
	}
	s.lastSeenAt = now
	return s
}

// handleCodeEventsPost records the trigger time (and, opportunistically,
// user-agent/host) for a POST to a Code session's /events endpoint. It does
// not itself emit anything: the reply is what produces records.
func (c *Converter) handleCodeEventsPost(rec *capture.Record, u *url.URL) {
	id, ok := codeSessionIDFromPath(codeEventsPathRE, u.Path)
	if !ok {
		c.Stats.skip(ReasonBadJSON)
		return
	}
	var body struct {
		Events []struct {
			Payload struct {
				Type    string `json:"type"`
				Message struct {
					Content json.RawMessage `json:"content"`
				} `json:"message"`
			} `json:"payload"`
		} `json:"events"`
	}
	if err := json.Unmarshal([]byte(rec.ReqBody), &body); err != nil {
		c.Stats.skip(ReasonBadJSON)
		return
	}
	var prompt json.RawMessage
	isUserEvent := false
	for _, e := range body.Events {
		if e.Payload.Type == "user" {
			isUserEvent = true
			if len(e.Payload.Message.Content) > 0 {
				prompt = e.Payload.Message.Content
			}
		}
	}
	if !isUserEvent {
		return
	}
	sess := c.codeSessionFor(id, rec.TS)
	if len(prompt) > 0 {
		// A new user message starts a new exchange: earlier tool results
		// and assistant content no longer describe what triggers the next
		// model call.
		sess.prompt, sess.toolResults, sess.lastAssistant = prompt, nil, nil
	}
	sess.lastTriggerAt = rec.TS
	sess.userAgent = capture.HeaderValue(rec.ReqHeaders, "User-Agent")
	sess.upstreamHost = u.Hostname()
	sess.eventsPath = scrubPath(rec.URL)
}

// handleCodeWSMessage dispatches one JSON message from the Code tab's
// subscribe WebSocket, either direction.
func (c *Converter) handleCodeWSMessage(rec *capture.Record, u *url.URL) {
	if rec.Mode != capture.ModeWebSocket || (rec.Method != "WS<" && rec.Method != "WS>") {
		return // handshake / WS-CLOSE / ping-pong records: nothing to do
	}
	body := rec.RespBody
	if rec.Method == "WS>" {
		body = rec.ReqBody
	}
	if rec.RespBodyBase64 || rec.ReqBodyBase64 || body == "" {
		return // binary or empty frame, not a JSON protocol message
	}
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(body), &head); err != nil {
		c.Stats.skip(ReasonBadJSON)
		return
	}
	if wsKeepAliveTypes[head.Type] {
		c.Stats.skip(ReasonKeepAlive)
		return
	}

	id, ok := codeSessionIDFromPath(codeWSPathRE, u.Path)
	if !ok {
		c.Stats.skip(ReasonUnhandledHost)
		return
	}
	sess := c.codeSessionFor(id, rec.TS)
	if sess.userAgent == "" {
		sess.userAgent = capture.HeaderValue(rec.ReqHeaders, "User-Agent")
	}

	switch head.Type {
	case "stream_event":
		c.handleStreamEvent(sess, id, []byte(body), rec.TS)
	case "assistant":
		c.handleAssistantMessage(sess, []byte(body), rec.TS)
	case "user":
		c.handleUserEcho(sess, id, []byte(body), rec.TS)
	case "result", "system", "control_response", "control_request", "rate_limit_event":
		// Known, intentionally ignored: doc section 5.1 says system is
		// ignored, and result/control/rate-limit messages describe the
		// harness or connection, not a model call or a tool call.
	default:
		c.Stats.skip(ReasonUnknownWSType)
	}
}

// handleStreamEvent applies one "stream_event" WS message to the
// session's open turn, delegating the actual Anthropic stream semantics
// (message_start...message_stop) to anthropicstream.Message.Accumulate.
func (c *Converter) handleStreamEvent(sess *codeSession, sessID string, body []byte, ts time.Time) {
	var head struct {
		Type  string          `json:"type"`
		Event json.RawMessage `json:"event"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		c.Stats.skip(ReasonBadJSON)
		return
	}
	var evHead struct {
		Type    string `json:"type"`
		Message *struct {
			ID string `json:"id"`
		} `json:"message"`
	}
	if err := json.Unmarshal(head.Event, &evHead); err != nil {
		c.Stats.skip(ReasonBadJSON)
		return
	}
	sweepMap(sess.turns, func(t *codeTurn) time.Time { return t.createdAt }, pendingEntryMaxAge, ts)

	if evHead.Type == "message_start" {
		if evHead.Message == nil || evHead.Message.ID == "" {
			c.Stats.skip(ReasonMissingFields)
			return
		}
		t := &codeTurn{createdAt: ts, startedAt: sess.lastTriggerAt, acc: &anthropicstream.Message{}}
		if t.startedAt.IsZero() {
			t.startedAt = ts // no known trigger; better than a zero timestamp
			c.Stats.skip(ReasonNoTrigger)
		}
		t.requestMessages = sess.requestMessages()
		sess.toolResults = nil // consumed by this call
		if err := t.acc.Accumulate(head.Event); err != nil {
			c.Stats.skip(ReasonStreamAccumulate)
		}
		sess.turns[evHead.Message.ID] = t
		return
	}

	// Every other event type applies to whichever turn is still open.
	// Claude Desktop runs one model call at a time per session, so there
	// is normally at most one.
	t := c.openTurn(sess)
	if t == nil {
		return
	}
	if err := t.acc.Accumulate(head.Event); err != nil {
		c.Stats.skip(ReasonStreamAccumulate)
		// Fall through: a mid-stream event this turn could not apply
		// (e.g. a stray out-of-range index) should not suppress the
		// record once message_stop arrives.
	}
	if evHead.Type == "message_stop" {
		c.finishCodeTurn(sess, sessID, t, ts)
	}
}

// openTurn returns the most recently started, not-yet-finished turn in a
// session. Claude Desktop runs one model call at a time per session, so
// there is normally at most one.
func (c *Converter) openTurn(sess *codeSession) *codeTurn {
	var best *codeTurn
	for _, t := range sess.turns {
		if t.done {
			continue
		}
		if best == nil || t.createdAt.After(best.createdAt) {
			best = t
		}
	}
	return best
}

func (c *Converter) finishCodeTurn(sess *codeSession, sessID string, t *codeTurn, stoppedAt time.Time) {
	t.done = true
	dur := stoppedAt.Sub(t.startedAt).Milliseconds()
	if dur < 0 {
		dur = 0
	}
	var messageID string
	for id, tt := range sess.turns {
		if tt == t {
			messageID = id
			break
		}
	}
	content := t.responseContent()
	sess.lastAssistant = content
	usage := &anthropicUsage{
		InputTokens:              int64PtrToIntPtr(t.acc.Usage.InputTokens),
		OutputTokens:             int64PtrToIntPtr(t.acc.Usage.OutputTokens),
		CacheReadInputTokens:     int64PtrToIntPtr(t.acc.Usage.CacheReadInputTokens),
		CacheCreationInputTokens: int64PtrToIntPtr(t.acc.Usage.CacheCreationInputTokens),
	}
	call := LLMCallIn{
		Timestamp:         t.startedAt.UTC().Format(time.RFC3339Nano),
		RequestID:         icpRequestID(messageID),
		StatusCode:        200,
		SessionID:         sessID,
		UserAgent:         sess.userAgent,
		UpstreamHost:      sess.upstreamHost,
		Model:             t.acc.Model,
		RequestedModel:    t.acc.Model,
		Path:              sess.eventsPath,
		DurationMS:        &dur,
		Stream:            true,
		InputTokens:       usage.InputTokens,
		OutputTokens:      usage.OutputTokens,
		CacheReadTokens:   usage.CacheReadInputTokens,
		CacheCreateTokens: usage.CacheCreationInputTokens,
		StopReason:        t.acc.StopReason,
		ProviderRequestID: messageID,
		Messages:          mustJSON(t.requestMessages),
		RequestBody:       anthropicRequestJSON(t.acc.Model, t.requestMessages),
		ResponseBody:      anthropicResponseJSON(messageID, t.acc.Model, content, t.acc.StopReason, usage),
	}
	if len(t.requestMessages) == 0 {
		// No user message was seen for this call (e.g. the interceptor
		// started mid-conversation): send no request side rather than an
		// empty messages array.
		call.Messages, call.RequestBody = "", ""
	}
	c.emitLLMCall(call)
	// Remove finished, successfully-emitted turns promptly; sweepMap still
	// catches ones that never got this far (e.g. message_stop never seen).
	delete(sess.turns, messageID)
}

// int64PtrToIntPtr converts a usage counter from anthropicstream's *int64
// to the *int the gateway ingest API (LLMCallIn, anthropicUsage) uses.
func int64PtrToIntPtr(p *int64) *int {
	if p == nil {
		return nil
	}
	v := int(*p)
	return &v
}

// requestMessages builds the Anthropic messages that trigger the model call
// starting now: the user message, plus, when tool results arrived since
// the last call, the previous assistant content and those tool results.
func (s *codeSession) requestMessages() []map[string]any {
	var msgs []map[string]any
	if len(s.prompt) > 0 {
		msgs = append(msgs, map[string]any{"role": "user", "content": s.prompt})
	}
	if len(s.toolResults) > 0 {
		if len(s.lastAssistant) > 0 {
			msgs = append(msgs, map[string]any{"role": "assistant", "content": s.lastAssistant})
		}
		msgs = append(msgs, map[string]any{"role": "user", "content": s.toolResults})
	}
	return msgs
}

// responseContent returns the turn's content blocks as a JSON array. When
// the turn saw stream events, the blocks come from anthropicstream's
// accumulated Message (index order is guaranteed by Accumulate itself);
// otherwise they fall back to the WS "assistant" snapshots. Empty
// (redacted) thinking blocks are left out either way.
func (t *codeTurn) responseContent() json.RawMessage {
	var out []any
	if len(t.acc.Content) > 0 {
		for _, cb := range t.acc.Content {
			if cb.Type == "thinking" && cb.Thinking == "" {
				continue
			}
			out = append(out, cb)
		}
	} else {
		for _, raw := range t.snapshotBlocks {
			var head struct {
				Type     string `json:"type"`
				Thinking string `json:"thinking"`
			}
			if json.Unmarshal(raw, &head) == nil && head.Type == "thinking" && head.Thinking == "" {
				continue
			}
			out = append(out, raw)
		}
	}
	if out == nil {
		out = []any{}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return json.RawMessage("[]")
	}
	return b
}

type assistantOrUserMsg struct {
	Type    string `json:"type"`
	Message struct {
		ID      string          `json:"id"`
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type contentBlock struct {
	Type      string          `json:"type"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

func (c *Converter) handleAssistantMessage(sess *codeSession, body []byte, ts time.Time) {
	var m assistantOrUserMsg
	if err := json.Unmarshal(body, &m); err != nil {
		c.Stats.skip(ReasonBadJSON)
		return
	}
	if m.Message.ID != "" {
		t, ok := sess.turns[m.Message.ID]
		if !ok {
			// message_start for this id was missed; keep the content around
			// under a synthetic entry so a later message_stop can still use
			// it, best-effort.
			t = &codeTurn{createdAt: ts, startedAt: sess.lastTriggerAt, acc: &anthropicstream.Message{}}
			if t.startedAt.IsZero() {
				t.startedAt = ts
			}
			t.requestMessages = sess.requestMessages()
			sess.turns[m.Message.ID] = t
		}
		t.addSnapshotBlocks(m.Message.Content)
	}

	var blocks []contentBlock
	if err := json.Unmarshal(m.Message.Content, &blocks); err != nil {
		return
	}
	for _, b := range blocks {
		if b.Type != "tool_use" || b.ID == "" {
			continue
		}
		if _, exists := sess.tools[b.ID]; exists {
			continue // an earlier snapshot already registered it
		}
		sweepMap(sess.tools, func(p *pendingTool) time.Time { return p.createdAt }, pendingEntryMaxAge, ts)
		connectorID, toolName, skillName, builtin := classifyTool(b.Name, b.Input)
		sess.tools[b.ID] = &pendingTool{
			createdAt: ts, startedAt: ts, toolUseID: b.ID,
			connectorID: connectorID, toolName: toolName, skillName: skillName,
			bytesIn: int64(len(b.Input)), builtin: builtin,
		}
	}
}

// classifyTool splits a tool name into a connector ID and tool name:
// "<connector-uuid>:getJiraIssue" or
// "mcp__<server>__<tool>". A name with neither shape is a built-in tool
// (Bash, Edit, Skill, ...) and is skipped, except Skill, which gets its own
// record with skill_name set instead of connector_id/tool_name.
func classifyTool(name string, input json.RawMessage) (connectorID, toolName, skillName string, builtin bool) {
	if i := strings.IndexByte(name, ':'); i > 0 && looksLikeUUID(name[:i]) {
		return name[:i], name[i+1:], "", false
	}
	if strings.HasPrefix(name, "mcp__") {
		rest := strings.TrimPrefix(name, "mcp__")
		if i := strings.Index(rest, "__"); i > 0 {
			return rest[:i], rest[i+2:], "", false
		}
		return "", rest, "", false
	}
	if name == "Skill" {
		return "", "", skillNameFromInput(input), false
	}
	return "", "", "", true
}

func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
				return false
			}
		}
	}
	return true
}

func skillNameFromInput(input json.RawMessage) string {
	var v struct {
		Command string `json:"command"`
		Skill   string `json:"skill"`
		Name    string `json:"name"`
	}
	if json.Unmarshal(input, &v) != nil {
		return ""
	}
	for _, s := range []string{v.Skill, v.Name, v.Command} {
		if s != "" {
			return s
		}
	}
	return ""
}

// addSnapshotBlocks appends the blocks of one WS "assistant" message (each
// carries one new block of the same message id), skipping exact repeats.
func (t *codeTurn) addSnapshotBlocks(content json.RawMessage) {
	var blocks []json.RawMessage
	if json.Unmarshal(content, &blocks) != nil {
		return
	}
next:
	for _, b := range blocks {
		for _, have := range t.snapshotBlocks {
			if string(have) == string(b) {
				continue next
			}
		}
		t.snapshotBlocks = append(t.snapshotBlocks, b)
	}
}

func (c *Converter) handleUserEcho(sess *codeSession, sessID string, body []byte, ts time.Time) {
	var m assistantOrUserMsg
	if err := json.Unmarshal(body, &m); err != nil {
		c.Stats.skip(ReasonBadJSON)
		return
	}
	var raws []json.RawMessage
	if err := json.Unmarshal(m.Message.Content, &raws); err != nil {
		return // a plain-string echo of the user's own message: not a trigger
	}
	for _, raw := range raws {
		var b contentBlock
		if json.Unmarshal(raw, &b) != nil || b.Type != "tool_result" || b.ToolUseID == "" {
			continue
		}
		// A tool result is what triggers the next model call.
		sess.lastTriggerAt = ts
		sess.toolResults = append(sess.toolResults, raw)
		c.finishToolResult(sess, sessID, b, ts)
	}
}

func (c *Converter) finishToolResult(sess *codeSession, sessID string, b contentBlock, ts time.Time) {
	p, ok := sess.tools[b.ToolUseID]
	if !ok {
		c.Stats.skip(ReasonOrphanToolResult)
		return
	}
	delete(sess.tools, b.ToolUseID)
	if p.builtin {
		c.Stats.skip(ReasonBuiltinTool) // matched, but intentionally not reported
		return
	}
	status := 200
	if b.IsError {
		status = 500
	}
	dur := ts.Sub(p.startedAt).Milliseconds()
	if dur < 0 {
		dur = 0
	}
	// tool_result's own "content" field is not decoded into contentBlock
	// above (only tool_use_id/is_error are); measure size generically.
	bytesOut := toolResultSize(b)
	bytesIn := p.bytesIn
	rec := AccessLogIn{
		Timestamp:       p.startedAt.UTC().Format(time.RFC3339Nano),
		RequestID:       "icp_" + p.toolUseID,
		StatusCode:      status,
		SessionID:       sessID,
		ClientSessionID: sessID,
		Method:          "tools/call",
		JSONRPCID:       p.toolUseID,
		ConnectorID:     p.connectorID,
		ToolName:        p.toolName,
		SkillName:       p.skillName,
		DurationMS:      &dur,
		BytesIn:         &bytesIn,
		Bytes:           &bytesOut,
		UserAgent:       sess.userAgent,
	}
	c.emitAccessLog(rec)
}

// toolResultSize re-decodes the raw content block just for its byte size,
// since contentBlock does not carry tool_result's "content" payload (only
// the fields other block types need).
func toolResultSize(b contentBlock) int64 {
	raw, err := json.Marshal(b)
	if err != nil {
		return 0
	}
	return int64(len(raw))
}

// sweepMap removes entries older than maxAge from m, keyed by whatever
// timestamp ageOf reports. It is called with the Converter's mutex already
// held, on maps that stay small in normal operation, so a linear scan on
// each insert is cheap and keeps memory bounded without a separate ticker
// goroutine.
func sweepMap[K comparable, V any](m map[K]V, ageOf func(V) time.Time, maxAge time.Duration, now time.Time) {
	if len(m) == 0 {
		return
	}
	for k, v := range m {
		if now.Sub(ageOf(v)) > maxAge {
			delete(m, k)
		}
	}
}
