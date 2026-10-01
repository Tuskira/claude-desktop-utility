// Package protodec decodes protobuf wire format without a schema, for
// display only. Fields are keyed by field number; the output is a JSON tree.
package protodec

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	maxDepth    = 32
	maxFieldNum = 536870911
	maxSafeInt  = 1 << 53
)

// Kind classifies a Content-Type.
type Kind int

const (
	KindNone    Kind = iota
	KindMessage      // a single protobuf message
	KindStream       // Connect / gRPC-Web envelopes
)

// KindOf maps a Content-Type header value (parameters ignored) to a Kind.
func KindOf(contentType string) Kind {
	mt := strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	switch mt {
	case "application/proto", "application/x-protobuf", "application/protobuf":
		return KindMessage
	case "application/connect+proto", "application/grpc-web+proto", "application/grpc-web":
		return KindStream
	}
	return KindNone
}

// Bytes is how an undecodable length-delimited value is shown.
type Bytes struct {
	B   string `json:"bytes"`
	Len int    `json:"len"`
}

// Message is a decoded message: field number -> values in wire order.
// It marshals with keys in numeric order; repeated fields become arrays.
type Message struct {
	order  []uint32
	fields map[uint32][]any
}

func (m *Message) add(n uint32, v any) {
	if m.fields == nil {
		m.fields = map[uint32][]any{}
	}
	if _, ok := m.fields[n]; !ok {
		m.order = append(m.order, n)
	}
	m.fields[n] = append(m.fields[n], v)
}

// Len is the number of distinct field numbers.
func (m *Message) Len() int { return len(m.order) }

// MarshalJSON implements json.Marshaler.
func (m *Message) MarshalJSON() ([]byte, error) {
	keys := append([]uint32(nil), m.order...)
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "\"%d\":", k)
		vals := m.fields[k]
		var enc []byte
		var err error
		if len(vals) == 1 {
			enc, err = marshal(vals[0])
		} else {
			enc, err = marshal(vals)
		}
		if err != nil {
			return nil, err
		}
		b.Write(enc)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

func numVal(u uint64) any {
	if u > maxSafeInt {
		return strconv.FormatUint(u, 10)
	}
	return u
}

// Decode parses b as one protobuf message. On error the fields parsed before
// the problem are still returned.
func Decode(b []byte) (*Message, error) { return parseMessage(b, 0) }

func parseMessage(b []byte, depth int) (*Message, error) {
	m := &Message{}
	for len(b) > 0 {
		tag, n := binary.Uvarint(b)
		if n <= 0 {
			return m, errors.New("bad field tag")
		}
		b = b[n:]
		num, wt := tag>>3, tag&7
		if num == 0 || num > maxFieldNum {
			return m, fmt.Errorf("invalid field number %d", num)
		}
		switch wt {
		case 0:
			v, n := binary.Uvarint(b)
			if n <= 0 {
				return m, errors.New("bad varint")
			}
			b = b[n:]
			m.add(uint32(num), numVal(v))
		case 1:
			if len(b) < 8 {
				return m, errors.New("truncated fixed64")
			}
			m.add(uint32(num), numVal(binary.LittleEndian.Uint64(b)))
			b = b[8:]
		case 5:
			if len(b) < 4 {
				return m, errors.New("truncated fixed32")
			}
			m.add(uint32(num), uint64(binary.LittleEndian.Uint32(b)))
			b = b[4:]
		case 2:
			l, n := binary.Uvarint(b)
			if n <= 0 {
				return m, errors.New("bad length")
			}
			b = b[n:]
			if l > uint64(len(b)) {
				return m, errors.New("truncated length-delimited field")
			}
			m.add(uint32(num), decodeLD(b[:l], depth))
			b = b[l:]
		default:
			return m, fmt.Errorf("unsupported wire type %d", wt)
		}
	}
	return m, nil
}

func decodeLD(sub []byte, depth int) any {
	if len(sub) == 0 {
		return ""
	}
	isStr := validText(sub)
	var msg *Message
	if depth+1 <= maxDepth {
		if mm, err := parseMessage(sub, depth+1); err == nil && mm.Len() > 0 {
			msg = mm
		}
	}
	switch {
	case isStr && msg != nil:
		if preferString(sub) {
			return string(sub)
		}
		return msg
	case isStr:
		return string(sub)
	case msg != nil:
		return msg
	}
	return Bytes{B: base64.StdEncoding.EncodeToString(sub), Len: len(sub)}
}

// validText reports valid UTF-8 without control characters (except \n\r\t).
func validText(b []byte) bool {
	if !utf8.Valid(b) {
		return false
	}
	for _, r := range string(b) {
		if r == '\n' || r == '\r' || r == '\t' {
			continue
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return false
		}
	}
	return true
}

// preferString settles values that parse as both text and a message: text
// wins when it is at least 90% printable ASCII (letters, digits,
// punctuation, space) and not made only of bytes below 0x20.
func preferString(b []byte) bool {
	printable, allLow := 0, true
	for _, c := range b {
		if c >= 0x20 && c <= 0x7e || c == '\n' || c == '\r' || c == '\t' {
			printable++
		}
		if c >= 0x20 {
			allLow = false
		}
	}
	return !allLow && float64(printable) >= 0.9*float64(len(b))
}

// DecodeStream splits Connect / gRPC-Web envelopes (1 flag byte + 4-byte
// big-endian length + payload) and decodes each one.
func DecodeStream(b []byte) []any {
	out := []any{}
	for len(b) > 0 {
		if len(b) < 5 {
			return append(out, map[string]any{"truncated": true})
		}
		flag := b[0]
		n := binary.BigEndian.Uint32(b[1:5])
		if uint64(len(b)-5) < uint64(n) {
			return append(out, map[string]any{"truncated": true})
		}
		payload := b[5 : 5+int(n)]
		b = b[5+int(n):]
		out = append(out, DecodeEnvelope(flag, payload))
	}
	return out
}

// DecodeEnvelope renders one envelope: a decoded message, a
// {"trailer":...} object for Connect end-stream / gRPC-Web trailers, or a
// bytes fallback.
func DecodeEnvelope(flag byte, payload []byte) any {
	if flag&0x02 != 0 || flag&0x80 != 0 {
		return map[string]any{"trailer": trailerValue(payload)}
	}
	if flag&0x01 != 0 { // compressed message: only gzip is understood
		dec, err := gunzip(payload)
		if err != nil {
			return map[string]any{"compressed": true, "len": len(payload)}
		}
		payload = dec
	}
	m, err := Decode(payload)
	if err != nil {
		return Bytes{B: base64.StdEncoding.EncodeToString(payload), Len: len(payload)}
	}
	return m
}

// ToJSON marshals a decoded value as compact JSON without HTML escaping.
func ToJSON(v any) (json.RawMessage, error) { return marshal(v) }

func gunzip(b []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(io.LimitReader(zr, 64<<20))
}

func trailerValue(p []byte) any {
	if json.Valid(p) {
		return json.RawMessage(p)
	}
	if utf8.Valid(p) {
		return string(p)
	}
	return Bytes{B: base64.StdEncoding.EncodeToString(p), Len: len(p)}
}

// DecodeBody decodes a captured body of the given kind and returns compact
// JSON. truncated says the capture was cut short (as much as possible is
// still decoded). A non-empty error string explains a problem or partial
// result; the JSON may still be non-nil.
func DecodeBody(kind Kind, data []byte, truncated bool) (json.RawMessage, string) {
	switch kind {
	case KindMessage:
		m, err := Decode(data)
		if err != nil && (!truncated || m.Len() == 0) {
			return nil, "protobuf decode failed: " + err.Error()
		}
		raw, merr := marshal(m)
		if merr != nil {
			return nil, merr.Error()
		}
		if truncated {
			return raw, "body truncated by --max-body; decoded what was captured"
		}
		return raw, ""
	case KindStream:
		raw, err := marshal(DecodeStream(data))
		if err != nil {
			return nil, err.Error()
		}
		if truncated {
			return raw, "body truncated by --max-body; decoded what was captured"
		}
		return raw, ""
	}
	return nil, ""
}
