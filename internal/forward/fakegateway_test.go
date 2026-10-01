package forward

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// Strict mirrors of the ingest API contract, docs/api.md (Ingest
// section) in the gateway repo. Deliberately NOT reusing
// claudedesktop.Batch/LLMCallIn/AccessLogIn: decoding into a second,
// independently-written struct with DisallowUnknownFields means a field
// added to (or renamed in) the production types without updating this file,
// or vice versa, fails the test instead of silently passing — the whole
// point being to catch drift from the documented contract.
type strictLLMCall struct {
	Timestamp           string `json:"timestamp"`
	RequestID           string `json:"request_id"`
	StatusCode          int    `json:"status_code"`
	SessionID           string `json:"session_id,omitempty"`
	ClientIP            string `json:"client_ip,omitempty"`
	ClientName          string `json:"client_name,omitempty"`
	UserAgent           string `json:"user_agent,omitempty"`
	Provider            string `json:"provider,omitempty"`
	UpstreamHost        string `json:"upstream_host,omitempty"`
	Model               string `json:"model,omitempty"`
	RequestedModel      string `json:"requested_model,omitempty"`
	Path                string `json:"path,omitempty"`
	DurationMS          *int64 `json:"duration_ms,omitempty"`
	Stream              bool   `json:"stream,omitempty"`
	InputTokens         *int   `json:"input_tokens,omitempty"`
	OutputTokens        *int   `json:"output_tokens,omitempty"`
	CacheReadTokens     *int   `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens *int   `json:"cache_creation_tokens,omitempty"`
	StopReason          string `json:"stop_reason,omitempty"`
	ProviderRequestID   string `json:"provider_request_id,omitempty"`
	Error               string `json:"error,omitempty"`
	Messages            string `json:"messages,omitempty"`
	RequestBody         string `json:"request_body,omitempty"`
	ResponseBody        string `json:"response_body,omitempty"`
	Truncated           bool   `json:"truncated,omitempty"`
}

type strictAccessLog struct {
	Timestamp       string `json:"timestamp"`
	RequestID       string `json:"request_id"`
	StatusCode      int    `json:"status_code"`
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

type strictBatch struct {
	SchemaVersion int               `json:"schema_version"`
	User          string            `json:"user,omitempty"`
	LLMCalls      []strictLLMCall   `json:"llm_calls,omitempty"`
	AccessLogs    []strictAccessLog `json:"access_logs,omitempty"`
}

const testGatewayKey = "gk_test_key_123"

// fakeGateway is an httptest server standing in for the gateway's ingest
// endpoint. Each received batch is strictly validated against strictBatch
// (unknown fields, or a record missing a required field, fail the test
// immediately via t.Fatal from the handler goroutine). behaviors, if set,
// queues one HTTP status per request in order; requests past the end of the
// queue get a default 200 that accepts everything.
type fakeGateway struct {
	t   *testing.T
	srv *httptest.Server
	key string

	mu        sync.Mutex
	received  []strictBatch
	behaviors []int
}

func newFakeGateway(t *testing.T) *fakeGateway {
	t.Helper()
	fg := &fakeGateway{t: t, key: testGatewayKey}
	fg.srv = httptest.NewServer(http.HandlerFunc(fg.handle))
	t.Cleanup(fg.srv.Close)
	return fg
}

func (fg *fakeGateway) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fg.t.Errorf("fake gateway: method = %s, want POST", r.Method)
	}
	if r.URL.Path != ingestPath {
		fg.t.Errorf("fake gateway: path = %s, want %s", r.URL.Path, ingestPath)
	}
	if got := r.Header.Get("X-Gateway-Key"); got != fg.key {
		fg.t.Errorf("fake gateway: X-Gateway-Key = %q, want %q", got, fg.key)
	}
	if got := r.Header.Get("Content-Encoding"); got != "gzip" {
		fg.t.Errorf("fake gateway: Content-Encoding = %q, want gzip", got)
	}
	zr, err := gzip.NewReader(r.Body)
	if err != nil {
		fg.t.Errorf("fake gateway: request body is not gzip: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	dec := json.NewDecoder(zr)
	dec.DisallowUnknownFields()
	var batch strictBatch
	if err := dec.Decode(&batch); err != nil {
		// t.Fatal must only be called from the test goroutine, and this
		// handler runs on the httptest server's own goroutine, so this is
		// an Error (which is goroutine-safe), not a Fatal; a malformed
		// batch also gets a 400 back so the caller under test does not
		// wait for a timeout.
		fg.t.Errorf("fake gateway: batch does not strictly match the ingest API contract (section 3): %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if batch.SchemaVersion != 1 {
		fg.t.Errorf("fake gateway: schema_version = %d, want 1", batch.SchemaVersion)
	}
	for i, c := range batch.LLMCalls {
		if c.Timestamp == "" || c.RequestID == "" || c.StatusCode == 0 {
			fg.t.Errorf("fake gateway: llm_calls[%d] missing a required field: %+v", i, c)
		}
	}
	for i, a := range batch.AccessLogs {
		if a.Timestamp == "" || a.RequestID == "" || a.StatusCode == 0 {
			fg.t.Errorf("fake gateway: access_logs[%d] missing a required field: %+v", i, a)
		}
	}

	fg.mu.Lock()
	fg.received = append(fg.received, batch)
	n := len(fg.received) - 1
	status := 200
	if n < len(fg.behaviors) {
		status = fg.behaviors[n]
	}
	fg.mu.Unlock()

	switch status {
	case 200:
		fmt.Fprintf(w, `{"accepted":{"llm_calls":%d,"access_logs":%d},"duplicates":{"llm_calls":0,"access_logs":0},"rejected":[]}`,
			len(batch.LLMCalls), len(batch.AccessLogs))
	case 429:
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(429)
	default:
		w.WriteHeader(status)
	}
}

func (fg *fakeGateway) Received() []strictBatch {
	fg.mu.Lock()
	defer fg.mu.Unlock()
	out := make([]strictBatch, len(fg.received))
	copy(out, fg.received)
	return out
}

func (fg *fakeGateway) Count() int {
	fg.mu.Lock()
	defer fg.mu.Unlock()
	return len(fg.received)
}
