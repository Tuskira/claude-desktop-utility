package forward

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Redaction (design section 5.2, "always on, not configurable off"),
// applied to every record before it is queued (see Forwarder.LLMCall /
// Forwarder.AccessLog), so spooled files on disk are already redacted.

// secretPatterns mask strings that look like credentials wherever they
// appear in a body, whether or not the body is JSON.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]+`),
	regexp.MustCompile(`gk_[A-Za-z0-9_-]+`),
	regexp.MustCompile(`(?i)Bearer\s+[A-Za-z0-9._~+/=-]+`),
	regexp.MustCompile(`sessionKey[A-Za-z0-9._~+/=-]*`),
}

func maskSecrets(s string) string {
	for _, re := range secretPatterns {
		s = re.ReplaceAllString(s, "[redacted]")
	}
	return s
}

// redactBody strips any "device_attestation" object (present in the Code
// tab's raw events POST body, which becomes LLMCallIn.Messages) and masks
// secret-shaped substrings. If body is not JSON, only masking applies;
// non-JSON bodies (SSE text, plain strings) are left otherwise unchanged.
func redactBody(body string) string {
	if body == "" {
		return body
	}
	var v any
	if err := json.Unmarshal([]byte(body), &v); err == nil {
		stripKey(v, "device_attestation")
		if b, err := json.Marshal(v); err == nil {
			body = string(b)
		}
	}
	return maskSecrets(body)
}

// stripKey removes every occurrence of key anywhere in a decoded JSON value
// (a tree of map[string]any / []any / scalars, as produced by
// json.Unmarshal into an any).
func stripKey(v any, key string) {
	switch t := v.(type) {
	case map[string]any:
		delete(t, key)
		for _, vv := range t {
			stripKey(vv, key)
		}
	case []any:
		for _, vv := range t {
			stripKey(vv, key)
		}
	}
}

// sensitiveHeaderNames are dropped outright; any other header whose name
// contains one of these substrings is also dropped (design: "any header
// containing token, session, or key").
var sensitiveHeaderNames = map[string]bool{
	"cookie":              true,
	"set-cookie":          true,
	"authorization":       true,
	"proxy-authorization": true,
	"x-api-key":           true,
}

var sensitiveHeaderSubstrings = []string{"token", "session", "key"}

// redactHeaders drops sensitive headers. No LLMCallIn/AccessLogIn field
// carries a raw header map today (the ingest schema only has scalar fields
// like user_agent, pulled from one specific, harmless header), so this has
// no call site yet; it exists, tested, so any future field that does copy
// headers through has a ready-made safe helper instead of reinventing one
// under time pressure.
func redactHeaders(h map[string][]string) map[string][]string {
	out := make(map[string][]string, len(h))
	for k, v := range h {
		lk := strings.ToLower(k)
		if sensitiveHeaderNames[lk] {
			continue
		}
		drop := false
		for _, s := range sensitiveHeaderSubstrings {
			if strings.Contains(lk, s) {
				drop = true
				break
			}
		}
		if drop {
			continue
		}
		out[k] = v
	}
	return out
}
