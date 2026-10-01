package claudedesktop

import (
	"net/url"
	"strings"
	"sync"

	"github.com/Tuskira/claude-desktop-utility/internal/capture"
)

// Config configures a Converter.
type Config struct {
	Sink        Sink   // required: where finished records go
	ForwardUser string // --forward-user override; wins over the learned account email
	// UserLabelFile, when set, persists the learned account email (mode
	// 0600) so it survives restarts; see userLabel.
	UserLabelFile string
	Logf          func(string, ...any)
}

// Converter turns capture.Record values (HTTP exchanges, WebSocket
// messages, and HTTP stream units) into LLMCallIn / AccessLogIn records and
// hands them to a Sink. Create one with New, then call Handle for every
// capture.Record — typically by assigning it to capture.Logger.Consumer.
//
// A Converter is safe for concurrent use. A running interceptor calls
// Handle from several goroutines at once: the request-handling goroutine
// for ordinary HTTP exchanges, a per-stream-tap goroutine for StreamTimeline
// units, and a goroutine per WebSocket direction for the Code tab's
// subscribe socket. Handle serialises all of that behind one mutex; see the
// package doc comment for why that is fine (all the work here is small,
// in-memory bookkeeping, not I/O).
type Converter struct {
	sink Sink
	user *userLabel
	logf func(string, ...any)

	Stats Stats

	mu   sync.Mutex
	code codeState
	chat chatState
}

// New creates a Converter. cfg.Sink must be non-nil.
func New(cfg Config) *Converter {
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	c := &Converter{sink: cfg.Sink, user: newUserLabel(cfg.ForwardUser, cfg.UserLabelFile, logf), logf: logf}
	c.code.init()
	c.chat.init()
	return c
}

// UserLabel returns the current "user" value to send with a batch: the
// --forward-user override if one was given, else the Claude account email
// last seen in a captured oauth/profile response, else "".
func (c *Converter) UserLabel() string { return c.user.Get() }

// Handle is the converter's entry point. It never panics (a panic anywhere
// below is recovered and counted) and never performs I/O or blocks on
// anything but Sink, which by contract must not block either.
func (c *Converter) Handle(rec *capture.Record) {
	defer func() {
		if r := recover(); r != nil {
			c.Stats.skip(ReasonPanicRecovered)
			c.logf("claudedesktop: recovered panic: %v", r)
		}
	}()
	if rec == nil {
		return
	}
	c.Stats.Handled.Add(1)

	u, err := url.Parse(rec.URL)
	if err != nil || u.Hostname() == "" {
		c.Stats.skip(ReasonUnhandledHost)
		return
	}
	host := strings.ToLower(u.Hostname())
	if isDroppedHost(host) {
		c.Stats.skip(ReasonUnhandledHost)
		return
	}
	if isDroppedPath(u.Path) {
		c.Stats.skip(ReasonDroppedTelemetry)
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if host == "api.anthropic.com" {
		if u.Path == oauthProfilePath && rec.Method == "GET" && rec.Status == 200 {
			c.user.observeOAuthProfile([]byte(rec.RespBody))
		}
		return
	}

	switch {
	case rec.Method == "POST" && isCodeSessionEventsPath(u.Path):
		c.handleCodeEventsPost(rec, u)
	case isCodeSessionWS(u.Path):
		c.handleCodeWSMessage(rec, u)
	case rec.Method == "POST" && strings.HasSuffix(u.Path, "/PerformAction"):
		c.handlePerformAction(rec)
	case rec.Mode == capture.ModeStream && strings.HasSuffix(u.Path, "/StreamTimeline"):
		c.handleStreamTimelineUnit(rec)
	case rec.Method == "POST" && strings.HasSuffix(u.Path, "/StreamTimeline"):
		// The exchange's own record, logged after all its units once the
		// stream has closed: finishes a reply that got no trailer.
		c.handleStreamTimelineEnd(rec)
	default:
		c.Stats.skip(ReasonUnhandledHost)
	}
}

// emitLLMCall fills in the fields common to every Code/Chat tab LLM record
// (design section 5.1, "Common fields") and hands the record to the sink.
func (c *Converter) emitLLMCall(call LLMCallIn) {
	call.ClientName = "claude-desktop"
	call.Provider = "anthropic"
	if call.RequestID == "" || call.Timestamp == "" {
		c.Stats.skip(ReasonMissingFields)
		return
	}
	c.Stats.LLMCalls.Add(1)
	c.sink.LLMCall(call)
}

func (c *Converter) emitAccessLog(a AccessLogIn) {
	if a.RequestID == "" || a.Timestamp == "" {
		c.Stats.skip(ReasonMissingFields)
		return
	}
	c.Stats.AccessLogs.Add(1)
	c.sink.AccessLog(a)
}
