package claudedesktop

import "strings"

// hostsWeHandle are the only hosts Convert looks at. Everything else
// (intercom, sentry, datadog, cdn hosts, the local device bridge, ...) is
// dropped before any parsing, cheaply, by a single map lookup.
var hostsWeHandle = map[string]bool{
	"claude.ai":         true,
	"api.anthropic.com": true, // only for GET /api/oauth/profile, see userlabel.go
}

// droppedPathSubstrings: a request whose URL path contains any of these is
// telemetry/polling noise, per the design's "drop entirely" list. Matched
// as a case-insensitive substring of the path, which is simpler and no less
// precise than exact routes here because these substrings are specific
// enough not to collide with the endpoints we do handle.
var droppedPathSubstrings = []string{
	"/presence",
	"/heartbeat",
	"/mark_read",
	"/mark-read",
	"branch-status",
	"/event_logging",
	"reflections/time_spent",
	"cowork/remote_devices",
	"/marketplace",
	"/plugins",
	"/update", // app update checks
	"ReportViewing",
	"GetNewConversationDefaults",
}

// isDroppedHost reports whether host should be skipped outright.
func isDroppedHost(host string) bool {
	host = strings.ToLower(host)
	// The image beacon and Datadog are reachable at hosts we otherwise never
	// see, but keep the check explicit and documented rather than relying
	// only on hostsWeHandle, in case that set grows later.
	if strings.Contains(host, "datadoghq.com") || host == "s-cdn.anthropic.com" {
		return true
	}
	return !hostsWeHandle[host]
}

// isDroppedPath reports whether a request path is telemetry/polling noise
// we intentionally never turn into records.
func isDroppedPath(path string) bool {
	for _, s := range droppedPathSubstrings {
		if strings.Contains(path, s) {
			return true
		}
	}
	return false
}

// wsKeepAliveTypes are Claude Desktop's WebSocket keep-alive message types,
// both directions. Bodies for these are otherwise valid JSON we could parse,
// but the design says to drop them entirely.
var wsKeepAliveTypes = map[string]bool{
	"hb":         true,
	"hb_ack":     true,
	"keep_alive": true,
}
