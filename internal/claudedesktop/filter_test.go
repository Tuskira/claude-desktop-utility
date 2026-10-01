package claudedesktop

import "testing"

func TestIsDroppedHost(t *testing.T) {
	cases := map[string]bool{
		"claude.ai":                          false,
		"api.anthropic.com":                  false,
		"s-cdn.anthropic.com":                true,
		"http-intake.logs.us5.datadoghq.com": true,
		"nexus-websocket-a.intercom.io":      true,
		"bridge.claudeusercontent.com":       true,
		"downloads.claude.ai":                true, // not in hostsWeHandle
	}
	for host, want := range cases {
		if got := isDroppedHost(host); got != want {
			t.Errorf("isDroppedHost(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestIsDroppedPath(t *testing.T) {
	cases := map[string]bool{
		"/v1/code/sessions/cse_x/events":                false,
		"/v1/code/sessions/cse_x/client/presence":       true,
		"/v1/code/sessions/heartbeat_check":             true,
		"/v1/code/sessions/session_x/mark_read":         true,
		"/v1/code/github/get-batch-branch-status":       true,
		"/api/event_logging/v2/batch":                   true,
		"/api/organizations/org/reflections/time_spent": true,
		"/api/organizations/org/cowork/remote_devices":  true,
		"/claudeai-rpc/.../PerformAction":               false,
	}
	for path, want := range cases {
		if got := isDroppedPath(path); got != want {
			t.Errorf("isDroppedPath(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestWSKeepAliveTypes(t *testing.T) {
	for _, ty := range []string{"hb", "hb_ack", "keep_alive"} {
		if !wsKeepAliveTypes[ty] {
			t.Errorf("wsKeepAliveTypes[%q] should be true", ty)
		}
	}
	if wsKeepAliveTypes["stream_event"] {
		t.Error("stream_event must not be treated as a keep-alive")
	}
}
