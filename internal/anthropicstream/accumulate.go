// Package anthropicstream rebuilds an Anthropic Messages API response from
// the server's stream events, using only the Go standard library.
//
// It ports the accumulation semantics of anthropic.Message.Accumulate from
// the official Anthropic Go SDK, github.com/anthropics/anthropic-sdk-go
// v1.76.0 (messageutil.go's Accumulate, refreshContentBlockRaw and
// mergeRaw). The interceptor does not import that SDK (stdlib only); this
// file is an independent re-implementation of the same rules, checked
// against the SDK's own output in accumulate_test.go's golden-file
// comparison.
//
// Ported rules:
//
//   - message_start replaces the whole accumulated message (id, role,
//     model, content, stop fields, usage) with the one the event carries.
//
//   - content_block_start appends a new block at the event's index, which
//     must equal the number of blocks accumulated so far (blocks start in
//     order, with no gaps, even though later deltas and stops for still-
//     open blocks may interleave out of order). An unrecognized block
//     type is kept, not dropped: every field content_block_start sent is
//     preserved verbatim and only the fields below are ever patched.
//
//   - content_block_delta dispatches on delta.type:
//     text_delta and thinking_delta append to the block's text/thinking.
//     signature_delta appends to the block's signature.
//     citations_delta appends one citation to the block's citations.
//     input_json_delta appends a fragment of partial JSON to the block's
//     input buffer; the first non-empty fragment REPLACES the "{}"
//     sentinel a tool-input block starts with (matching the "input":{}
//     content_block_start always sends), later fragments are appended.
//     An unrecognized delta type is ignored (not applied) and counted in
//     Message.Errors, never causing a panic or a returned error.
//
//   - content_block_stop finalizes one block: its input buffer is parsed
//     into JSON if it now holds valid JSON; if it holds no bytes at all
//     it becomes "{}"; if it holds bytes that never became valid JSON
//     (a tool call cut off mid-argument), the interceptor keeps the raw
//     text in the input field as a JSON string, rather than the SDK's own
//     choice of discarding it to "{}", because these records exist for
//     forensic replay and a truncated call is exactly what an operator
//     needs to see. That substitution is counted in Message.Errors.
//
//   - message_delta updates stop_reason and stop_sequence unconditionally
//     (the API always sends both, using null for "no such detail") and
//     merges usage: output_tokens is always overwritten, because the API
//     sends a running cumulative total on every message_delta; the other
//     counters (input_tokens, cache_creation_input_tokens,
//     cache_read_input_tokens, server_tool_use,
//     output_tokens_details) are overwritten only when that event's usage
//     object actually carries them, so a field message_start already set
//     and this delta omits keeps its previous value.
//
//   - message_stop finalizes every content block exactly as
//     content_block_stop does, as a fallback for any block whose own stop
//     event never arrived.
//
//   - an event addressing a content block index outside the accumulated
//     range (before its content_block_start, or past the last block) is
//     counted in Message.Errors and returned as an error; the event is
//     not applied.
//
//   - any other top-level event type (for example "ping") is silently
//     ignored, matching the SDK, which only recognizes the six event
//     types above.
package anthropicstream

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Message is the Anthropic Messages API response being rebuilt from a
// turn's stream events. The zero value is ready to use: feed it events in
// order, starting with message_start, via Accumulate.
type Message struct {
	ID           string
	Type         string // "message"
	Role         string
	Model        string
	Content      []*ContentBlock
	StopReason   string
	StopSequence *string
	Usage        Usage

	// Errors counts every event this Message could not fully apply as the
	// server intended: a content-block index outside the accumulated
	// range, an unrecognized delta type, or tool input that never became
	// valid JSON. It never causes a panic, and most such events still
	// leave the rest of the message intact.
	Errors int
}

// Usage is the token/usage block of an Anthropic message, tracking every
// field the SDK's Usage type does. Pointer fields are nil when the field
// was never sent (as opposed to sent with a zero value), mirroring the
// SDK's respjson.Field.Valid() distinction.
type Usage struct {
	InputTokens              *int64
	OutputTokens             *int64
	CacheCreationInputTokens *int64
	CacheReadInputTokens     *int64
	CacheCreation            *CacheCreation
	OutputTokensDetails      *OutputTokensDetails
	ServerToolUse            *ServerToolUsage
	InferenceGeo             string
	ServiceTier              string
}

// CacheCreation is usage.cache_creation: the breakdown of cached input
// tokens by TTL.
type CacheCreation struct {
	Ephemeral1hInputTokens int64 `json:"ephemeral_1h_input_tokens"`
	Ephemeral5mInputTokens int64 `json:"ephemeral_5m_input_tokens"`
}

// OutputTokensDetails is usage.output_tokens_details.
type OutputTokensDetails struct {
	ThinkingTokens int64 `json:"thinking_tokens"`
}

// ServerToolUsage is usage.server_tool_use: counts of server-executed
// tool requests (web search, web fetch, ...).
type ServerToolUsage struct {
	WebFetchRequests  int64 `json:"web_fetch_requests"`
	WebSearchRequests int64 `json:"web_search_requests"`
}

// ContentBlock is one content block of the accumulated message. The
// fields deltas can mutate (Text, Thinking, Signature, Input, Citations)
// are typed; every other field content_block_start sent, including a
// whole content-block type this package does not otherwise know about
// (a server tool block, a *_tool_result block, or a future type), round-
// trips through raw and survives into MarshalJSON unchanged.
type ContentBlock struct {
	Type      string
	Text      string
	Thinking  string
	Signature string
	Input     json.RawMessage // "{}" until an input_json_delta arrives
	Citations []json.RawMessage

	raw          json.RawMessage // content_block_start's wire JSON, patched by refresh
	inputInvalid bool            // latched once Input's bytes failed to parse as JSON
}

// wire* types mirror the JSON the server sends for one stream event, one
// message and one delta, decoded permissively: every field is optional so
// a block or event this package does not specifically know about still
// parses (and, for a content block, round-trips via its raw bytes).
type wireEvent struct {
	Type         string          `json:"type"`
	Message      *wireMessage    `json:"message"`
	Index        *int64          `json:"index"`
	ContentBlock json.RawMessage `json:"content_block"`
	Delta        *wireDelta      `json:"delta"`
	Usage        *wireUsage      `json:"usage"`
}

type wireMessage struct {
	ID           string            `json:"id"`
	Type         string            `json:"type"`
	Role         string            `json:"role"`
	Model        string            `json:"model"`
	Content      []json.RawMessage `json:"content"`
	StopReason   string            `json:"stop_reason"`
	StopSequence *string           `json:"stop_sequence"`
	Usage        *wireUsage        `json:"usage"`
}

type wireDelta struct {
	Type         string          `json:"type"`
	Text         string          `json:"text"`
	Thinking     string          `json:"thinking"`
	Signature    string          `json:"signature"`
	PartialJSON  string          `json:"partial_json"`
	Citation     json.RawMessage `json:"citation"`
	StopReason   string          `json:"stop_reason"`
	StopSequence *string         `json:"stop_sequence"`
}

type wireUsage struct {
	InputTokens              *int64               `json:"input_tokens"`
	OutputTokens             *int64               `json:"output_tokens"`
	CacheCreationInputTokens *int64               `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     *int64               `json:"cache_read_input_tokens"`
	CacheCreation            *CacheCreation       `json:"cache_creation"`
	OutputTokensDetails      *OutputTokensDetails `json:"output_tokens_details"`
	ServerToolUse            *ServerToolUsage     `json:"server_tool_use"`
	InferenceGeo             *string              `json:"inference_geo"`
	ServiceTier              *string              `json:"service_tier"`
}

// Accumulate applies one raw stream event (the JSON object carried as the
// event's own payload -- what the wire calls "event" inside a
// stream_event message, e.g. {"type":"message_start","message":{...}})
// to m. It never panics; anomalies are counted in m.Errors, and returned
// as an error only when the event could not be applied at all (malformed
// event JSON, or a content-block index outside the accumulated range).
func (m *Message) Accumulate(event []byte) error {
	if m == nil {
		return fmt.Errorf("anthropicstream: cannot accumulate into nil Message")
	}
	var ev wireEvent
	if err := json.Unmarshal(event, &ev); err != nil {
		m.Errors++
		return fmt.Errorf("anthropicstream: invalid event JSON: %w", err)
	}

	switch ev.Type {
	case "message_start":
		if ev.Message == nil {
			m.Errors++
			return fmt.Errorf("anthropicstream: message_start with no message")
		}
		m.applyMessageStart(ev.Message)

	case "content_block_start":
		idx := int64(len(m.Content))
		if ev.Index == nil || *ev.Index != idx {
			m.Errors++
			got := "<none>"
			if ev.Index != nil {
				got = fmt.Sprintf("%d", *ev.Index)
			}
			return fmt.Errorf("anthropicstream: content_block_start at index %s, expected %d", got, idx)
		}
		cb, err := newContentBlock(ev.ContentBlock)
		if err != nil {
			m.Errors++
			return err
		}
		m.Content = append(m.Content, cb)

	case "content_block_delta":
		cb, err := m.blockAt(ev.Index)
		if err != nil {
			m.Errors++
			return err
		}
		if ev.Delta == nil {
			m.Errors++
			return fmt.Errorf("anthropicstream: content_block_delta with no delta")
		}
		if !cb.applyDelta(ev.Delta) {
			m.Errors++ // unrecognized delta type: ignored, not applied
		}

	case "content_block_stop":
		cb, err := m.blockAt(ev.Index)
		if err != nil {
			m.Errors++
			return err
		}
		if cb.refresh() {
			m.Errors++
		}

	case "message_delta":
		m.applyMessageDelta(ev.Delta, ev.Usage)

	case "message_stop":
		for _, cb := range m.Content {
			if cb.refresh() {
				m.Errors++
			}
		}

	default:
		// Any other event type (e.g. "ping" or a future addition) is
		// silently ignored, matching the SDK: it only recognizes the six
		// cases above.
	}
	return nil
}

// blockAt returns the content block index addresses, or an error if index
// is nil or outside the accumulated range. Delta and stop events may
// interleave across open content blocks, so they always address a block
// by index rather than implicitly meaning "the most recently started
// one".
func (m *Message) blockAt(index *int64) (*ContentBlock, error) {
	if index == nil {
		return nil, fmt.Errorf("anthropicstream: event with no index")
	}
	idx := *index
	if idx < 0 || idx >= int64(len(m.Content)) {
		return nil, fmt.Errorf("anthropicstream: index %d out of range (%d content blocks)", idx, len(m.Content))
	}
	return m.Content[idx], nil
}

func (m *Message) applyMessageStart(wm *wireMessage) {
	m.ID = wm.ID
	m.Type = wm.Type
	m.Role = wm.Role
	m.Model = wm.Model
	m.StopReason = wm.StopReason
	m.StopSequence = wm.StopSequence
	m.Content = nil
	for _, raw := range wm.Content {
		cb, err := newContentBlock(raw)
		if err != nil {
			m.Errors++
			continue
		}
		m.Content = append(m.Content, cb)
	}
	if wm.Usage != nil {
		m.Usage = usageFromWire(wm.Usage)
	} else {
		m.Usage = Usage{}
	}
}

// applyMessageDelta merges a message_delta event's stop fields and usage
// counters, per the overwrite-vs-keep rules documented on the package.
func (m *Message) applyMessageDelta(d *wireDelta, u *wireUsage) {
	if d != nil {
		m.StopReason = d.StopReason
		m.StopSequence = d.StopSequence
	}
	if u == nil {
		return
	}
	// output_tokens is a cumulative running total the API sends on every
	// message_delta, so it always overwrites, even if this particular
	// event were to omit it.
	m.Usage.OutputTokens = u.OutputTokens
	if u.InputTokens != nil {
		m.Usage.InputTokens = u.InputTokens
	}
	if u.CacheCreationInputTokens != nil {
		m.Usage.CacheCreationInputTokens = u.CacheCreationInputTokens
	}
	if u.CacheReadInputTokens != nil {
		m.Usage.CacheReadInputTokens = u.CacheReadInputTokens
	}
	if u.ServerToolUse != nil {
		m.Usage.ServerToolUse = u.ServerToolUse
	}
	if u.OutputTokensDetails != nil {
		m.Usage.OutputTokensDetails = u.OutputTokensDetails
	}
}

func usageFromWire(u *wireUsage) Usage {
	out := Usage{
		InputTokens:              u.InputTokens,
		OutputTokens:             u.OutputTokens,
		CacheCreationInputTokens: u.CacheCreationInputTokens,
		CacheReadInputTokens:     u.CacheReadInputTokens,
		CacheCreation:            u.CacheCreation,
		OutputTokensDetails:      u.OutputTokensDetails,
		ServerToolUse:            u.ServerToolUse,
	}
	if u.InferenceGeo != nil {
		out.InferenceGeo = *u.InferenceGeo
	}
	if u.ServiceTier != nil {
		out.ServiceTier = *u.ServiceTier
	}
	return out
}

// contentBlockHead is the subset of a content block's fields this package
// mutates via deltas. Every field content_block_start sent -- including
// ones not listed here, such as a server tool block's tool_use_id or a
// *_tool_result block's content -- is preserved separately in raw.
type contentBlockHead struct {
	Type      string            `json:"type"`
	Text      string            `json:"text"`
	Thinking  string            `json:"thinking"`
	Signature string            `json:"signature"`
	Input     json.RawMessage   `json:"input"`
	Citations []json.RawMessage `json:"citations"`
}

func newContentBlock(raw json.RawMessage) (*ContentBlock, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("anthropicstream: content_block_start with no content_block")
	}
	var head contentBlockHead
	if err := json.Unmarshal(raw, &head); err != nil {
		return nil, fmt.Errorf("anthropicstream: invalid content_block: %w", err)
	}
	return &ContentBlock{
		Type:      head.Type,
		Text:      head.Text,
		Thinking:  head.Thinking,
		Signature: head.Signature,
		Input:     head.Input,
		Citations: head.Citations,
		raw:       append(json.RawMessage(nil), raw...),
	}, nil
}

// applyDelta applies one content_block_delta to cb. It reports whether
// the delta type was recognized; an unrecognized type is left unapplied
// so the caller can count it, never causing an error or a panic.
func (cb *ContentBlock) applyDelta(d *wireDelta) bool {
	switch d.Type {
	case "text_delta":
		cb.Text += d.Text
	case "thinking_delta":
		cb.Thinking += d.Thinking
	case "signature_delta":
		cb.Signature += d.Signature
	case "input_json_delta":
		if d.PartialJSON != "" {
			if string(cb.Input) == "{}" {
				cb.Input = json.RawMessage(d.PartialJSON)
			} else {
				cb.Input = append(cb.Input, d.PartialJSON...)
			}
		}
	case "citations_delta":
		if len(d.Citation) > 0 {
			cb.Citations = append(cb.Citations, append(json.RawMessage(nil), d.Citation...))
		}
	default:
		return false
	}
	return true
}

// refresh overlays the delta-mutated fields (Text, Thinking, Signature,
// Input, Citations) onto the block's raw wire JSON, leaving a block that
// received no deltas byte-for-byte as content_block_start sent it. It is
// idempotent: calling it again (message_stop's fallback pass over every
// block) reproduces the same bytes. It reports whether Input held bytes
// that never became valid JSON, on the first call only, so the caller
// counts it exactly once per block.
func (cb *ContentBlock) refresh() (newlyInvalidInput bool) {
	m := map[string]json.RawMessage{}
	if len(cb.raw) > 0 {
		_ = json.Unmarshal(cb.raw, &m) // cb.raw is our own prior output or the original wire bytes; always an object
	}
	if cb.Text != "" {
		m["text"] = jsonString(cb.Text)
	}
	if cb.Thinking != "" {
		m["thinking"] = jsonString(cb.Thinking)
	}
	if cb.Signature != "" {
		m["signature"] = jsonString(cb.Signature)
	}
	if len(cb.Input) > 0 {
		if json.Valid(cb.Input) {
			m["input"] = cb.Input
		} else {
			// A tool call cut off mid-argument: unlike the SDK (which
			// discards it to "{}"), keep the raw text as a JSON string so
			// a forensic reader can still see what was sent. Counted once.
			if !cb.inputInvalid {
				cb.inputInvalid = true
				newlyInvalidInput = true
			}
			m["input"] = jsonString(string(cb.Input))
		}
	}
	if len(cb.Citations) > 0 {
		if arr, err := json.Marshal(cb.Citations); err == nil {
			m["citations"] = arr
		}
	}
	if b, err := json.Marshal(m); err == nil {
		cb.raw = b
	}
	return newlyInvalidInput
}

// jsonString encodes s the way the Anthropic API does, leaving <, > and &
// literal rather than the \u-escaping encoding/json applies by default,
// so text content round-trips unchanged.
func jsonString(s string) json.RawMessage {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		b, _ := json.Marshal(s)
		return b
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// Raw returns the block's current JSON. It reflects deltas applied up to
// the last content_block_stop or message_stop that refreshed it; deltas
// applied after that point (which the protocol never sends) would not
// yet be reflected.
func (cb *ContentBlock) Raw() json.RawMessage {
	if len(cb.raw) == 0 {
		return json.RawMessage(`{"type":"` + cb.Type + `"}`)
	}
	return append(json.RawMessage(nil), cb.raw...)
}

// MarshalJSON implements json.Marshaler so a *ContentBlock can be embedded
// directly in a []any passed to json.Marshal.
func (cb *ContentBlock) MarshalJSON() ([]byte, error) {
	return cb.Raw(), nil
}
