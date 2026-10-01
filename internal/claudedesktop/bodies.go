package claudedesktop

import "encoding/json"

// Both tabs send request_body and response_body in the Anthropic Messages
// API shape, so the gateway console shows interceptor rows the same way as
// gateway-proxied Anthropic calls:
//
//	request_body:  {"model": ..., "messages": [{"role": "user", "content": ...}, ...]}
//	response_body: {"type": "message", "role": "assistant", "model": ...,
//	                "content": [...], "stop_reason": ..., "usage": {...}}
//
// messages is also sent on its own (the gateway stores it separately and
// the console's Prompt section reads it from there).

// mustJSON marshals v, returning "" if it cannot be marshalled (it only
// ever receives maps/slices of strings and raw JSON built here).
func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// anthropicRequestJSON builds an Anthropic Messages request body.
func anthropicRequestJSON(model string, messages any) string {
	req := map[string]any{"messages": messages}
	if model != "" {
		req["model"] = model
	}
	return mustJSON(req)
}

// anthropicUsage is the usage block of an Anthropic message. Nil fields
// are left out.
type anthropicUsage struct {
	InputTokens              *int `json:"input_tokens,omitempty"`
	OutputTokens             *int `json:"output_tokens,omitempty"`
	CacheReadInputTokens     *int `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens *int `json:"cache_creation_input_tokens,omitempty"`
}

// anthropicResponseJSON builds an Anthropic message (response) body.
// content must marshal to a JSON array of content blocks.
func anthropicResponseJSON(id, model string, content any, stopReason string, usage *anthropicUsage) string {
	msg := map[string]any{"type": "message", "role": "assistant", "content": content}
	if id != "" {
		msg["id"] = id
	}
	if model != "" {
		msg["model"] = model
	}
	if stopReason != "" {
		msg["stop_reason"] = stopReason
	}
	if usage != nil {
		msg["usage"] = usage
	}
	return mustJSON(msg)
}
