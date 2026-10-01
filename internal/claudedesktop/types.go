// Package claudedesktop turns the interceptor's capture records for Claude
// Desktop traffic (Code tab and Chat tab) into gateway-shaped LLM call and
// access log records, and hands them to a Sink for forwarding.
//
// It never blocks the proxy: Convert does only in-memory parsing and
// bookkeeping, recovers its own panics, and counts (rather than surfaces)
// every record it cannot make sense of. See doc/design/interceptor-ingest.md
// section 5.1 in the ai-agent-gateway repo for the design this
// implements.
package claudedesktop

// LLMCallIn is one LLM call record, field for field per the ingest API
// contract (gateway repo, docs/design/interceptor-ingest.md section 3).
// Bodies are plain strings of JSON or text, never base64.
type LLMCallIn struct {
	// Required.
	Timestamp  string `json:"timestamp"` // RFC 3339
	RequestID  string `json:"request_id"`
	StatusCode int    `json:"status_code"`

	// Optional.
	SessionID         string `json:"session_id,omitempty"`
	ClientIP          string `json:"client_ip,omitempty"`
	ClientName        string `json:"client_name,omitempty"`
	UserAgent         string `json:"user_agent,omitempty"`
	Provider          string `json:"provider,omitempty"`
	UpstreamHost      string `json:"upstream_host,omitempty"`
	Model             string `json:"model,omitempty"`
	RequestedModel    string `json:"requested_model,omitempty"`
	Path              string `json:"path,omitempty"`
	DurationMS        *int64 `json:"duration_ms,omitempty"`
	Stream            bool   `json:"stream,omitempty"`
	InputTokens       *int   `json:"input_tokens,omitempty"`
	OutputTokens      *int   `json:"output_tokens,omitempty"`
	CacheReadTokens   *int   `json:"cache_read_tokens,omitempty"`
	CacheCreateTokens *int   `json:"cache_creation_tokens,omitempty"`
	StopReason        string `json:"stop_reason,omitempty"`
	ProviderRequestID string `json:"provider_request_id,omitempty"`
	Error             string `json:"error,omitempty"`
	Messages          string `json:"messages,omitempty"`
	RequestBody       string `json:"request_body,omitempty"`
	ResponseBody      string `json:"response_body,omitempty"`
	Truncated         bool   `json:"truncated,omitempty"`
}

// AccessLogIn is one MCP access log record, field for field per the ingest
// API contract, section 3.
type AccessLogIn struct {
	// Required.
	Timestamp  string `json:"timestamp"` // RFC 3339
	RequestID  string `json:"request_id"`
	StatusCode int    `json:"status_code"`

	// Optional.
	SessionID       string `json:"session_id,omitempty"`
	ClientSessionID string `json:"client_session_id,omitempty"`
	Method          string `json:"method,omitempty"`
	JSONRPCID       string `json:"json_rpc_id,omitempty"`
	ConnectorID     string `json:"connector_id,omitempty"`
	ToolName        string `json:"tool_name,omitempty"`
	SkillName       string `json:"skill_name,omitempty"`
	ErrorCode       string `json:"error_code,omitempty"`
	DurationMS      *int64 `json:"duration_ms,omitempty"`
	BytesIn         *int64 `json:"bytes_in,omitempty"`
	Bytes           *int64 `json:"bytes,omitempty"`
	ClientIP        string `json:"client_ip,omitempty"`
	UserAgent       string `json:"user_agent,omitempty"`
	RequestBody     string `json:"request_body,omitempty"`
	ResponseBody    string `json:"response_body,omitempty"`
	Truncated       bool   `json:"truncated,omitempty"`
}

// Batch is the ingest request body: POST /api/v1/ingest.
type Batch struct {
	SchemaVersion int           `json:"schema_version"`
	User          string        `json:"user,omitempty"`
	LLMCalls      []LLMCallIn   `json:"llm_calls,omitempty"`
	AccessLogs    []AccessLogIn `json:"access_logs,omitempty"`
}

// SchemaVersion is the only schema_version this converter emits.
const SchemaVersion = 1

// Sink receives finished records. A Converter never talks to the network
// itself; something implementing Sink (see package forward) does. Both
// methods must return quickly and must not block: Convert calls them
// synchronously.
type Sink interface {
	LLMCall(LLMCallIn)
	AccessLog(AccessLogIn)
}

// SinkFunc pair adapts two functions to a Sink, mainly for tests.
type SinkFuncs struct {
	OnLLMCall   func(LLMCallIn)
	OnAccessLog func(AccessLogIn)
}

func (s SinkFuncs) LLMCall(c LLMCallIn) {
	if s.OnLLMCall != nil {
		s.OnLLMCall(c)
	}
}

func (s SinkFuncs) AccessLog(a AccessLogIn) {
	if s.OnAccessLog != nil {
		s.OnAccessLog(a)
	}
}
