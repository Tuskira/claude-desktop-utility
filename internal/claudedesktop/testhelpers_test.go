package claudedesktop

import (
	"testing"
	"time"

	"github.com/Tuskira/claude-desktop-utility/internal/capture"
)

// newTestConverter returns a Converter wired to record every emitted call
// into the returned slices, for assertions.
func newTestConverter() (conv *Converter, calls *[]LLMCallIn, logs *[]AccessLogIn) {
	calls = &[]LLMCallIn{}
	logs = &[]AccessLogIn{}
	sink := SinkFuncs{
		OnLLMCall:   func(c LLMCallIn) { *calls = append(*calls, c) },
		OnAccessLog: func(a AccessLogIn) { *logs = append(*logs, a) },
	}
	conv = New(Config{Sink: sink, Logf: func(string, ...any) {}})
	return conv, calls, logs
}

const testUA = "TestAgent/1.0"

func wsRecord(t *testing.T, ts time.Time, url, dir, body string) *capture.Record {
	t.Helper()
	r := &capture.Record{
		TS: ts, Mode: capture.ModeWebSocket, Method: dir, URL: url,
		Status: 101, ReqHeaders: map[string][]string{}, RespHeaders: map[string][]string{},
	}
	if dir == "WS>" {
		r.ReqBody = body
	} else {
		r.RespBody = body
	}
	return r
}

func postRecord(ts time.Time, url, body string) *capture.Record {
	return &capture.Record{
		TS: ts, Mode: capture.ModeProxy, Method: "POST", URL: url,
		ReqBody: body, Status: 200,
		ReqHeaders:  map[string][]string{"User-Agent": {testUA}},
		RespHeaders: map[string][]string{},
	}
}
