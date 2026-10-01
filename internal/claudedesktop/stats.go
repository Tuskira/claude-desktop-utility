package claudedesktop

import (
	"sync"
	"sync/atomic"
)

// Reason names counted in Stats.Skipped. Kept as a closed set of constants
// (rather than free-form strings) so callers can build a stable status line.
const (
	ReasonUnhandledHost      = "unhandled_host"        // dropped: not a host/path we handle
	ReasonDroppedTelemetry   = "dropped_telemetry"     // presence/heartbeat/event_logging/etc.
	ReasonKeepAlive          = "keepalive"             // WS hb/hb_ack/keep_alive
	ReasonUnknownWSType      = "unknown_ws_type"       // WS message type we don't recognise
	ReasonBadJSON            = "bad_json"              // JSON body didn't parse as expected
	ReasonNoTrigger          = "no_trigger"            // reply with no matching user message/tool_use
	ReasonBuiltinTool        = "builtin_tool"          // tool_use with no connector part (expected, not an error)
	ReasonOrphanToolResult   = "orphan_tool_result"    // tool_result with no matching tool_use
	ReasonMissingFields      = "missing_fields"        // required output fields could not be filled in
	ReasonProtoFieldNotFound = "proto_field_not_found" // a documented field path was absent
	ReasonPanicRecovered     = "panic_recovered"       // Handle recovered from a panic
	ReasonChatNoReply        = "chat_no_reply"         // chat send whose complete reply text was never seen
	ReasonChatPairMismatch   = "chat_pair_mismatch"    // stream disagrees with the send about the reply (sender/parent)
	ReasonStreamAccumulate   = "stream_accumulate"     // a Code tab stream event could not be applied (see anthropicstream.Message.Errors)
)

// Stats are cumulative, process-lifetime counters. All operations are
// lock-free so they are cheap to bump from the proxy's hot goroutines.
type Stats struct {
	Handled    atomic.Int64 // capture records accepted for processing
	LLMCalls   atomic.Int64 // LLMCallIn records emitted
	AccessLogs atomic.Int64 // AccessLogIn records emitted
	skipped    sync.Map     // reason (string) -> *atomic.Int64, filled in lazily
}

func (s *Stats) skip(reason string) {
	v, _ := s.skipped.LoadOrStore(reason, new(atomic.Int64))
	v.(*atomic.Int64).Add(1)
}

// Snapshot returns the current skip counts by reason, for logging.
func (s *Stats) Snapshot() map[string]int64 {
	out := map[string]int64{}
	s.skipped.Range(func(k, v any) bool {
		out[k.(string)] = v.(*atomic.Int64).Load()
		return true
	})
	return out
}
