package protodec

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"
)

// Hand-encoding helpers.
func varint(v uint64) []byte          { return binary.AppendUvarint(nil, v) }
func tag(num, wt uint64) []byte       { return varint(num<<3 | wt) }
func str(num uint64, s string) []byte { return ld(num, []byte(s)) }
func ld(num uint64, b []byte) []byte {
	return bytes.Join([][]byte{tag(num, 2), varint(uint64(len(b))), b}, nil)
}
func vint(num, v uint64) []byte  { return append(tag(num, 0), varint(v)...) }
func cat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
func env(flag byte, payload []byte) []byte {
	h := []byte{flag, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(h[1:], uint32(len(payload)))
	return append(h, payload...)
}

func js(t *testing.T, v any) string {
	t.Helper()
	b, err := marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func decode(t *testing.T, b []byte) string {
	t.Helper()
	m, err := Decode(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return js(t, m)
}

func TestStringNestedRepeatedAndScalars(t *testing.T) {
	msg := cat(
		str(1, "hello"),
		ld(2, str(1, "x")),
		ld(2, str(1, "y")),
		vint(3, 42),
	)
	if got, want := decode(t, msg), `{"1":"hello","2":[{"1":"x"},{"1":"y"}],"3":42}`; got != want {
		t.Errorf("got %s\nwant %s", got, want)
	}
}

func TestFixedAndBigVarint(t *testing.T) {
	f32 := binary.LittleEndian.AppendUint32(tag(4, 5), 7)
	f64 := binary.LittleEndian.AppendUint64(tag(5, 1), 1<<40)
	big := vint(6, 1<<60)
	edge := vint(7, 1<<53) // exactly 2^53 is still a number
	if got, want := decode(t, cat(f32, f64, big, edge)),
		`{"4":7,"5":1099511627776,"6":"1152921504606846976","7":9007199254740992}`; got != want {
		t.Errorf("got %s\nwant %s", got, want)
	}
}

func TestNonUTF8BytesFallback(t *testing.T) {
	raw := []byte{0xff, 0xfe, 0x00, 0x80}
	want := `{"1":{"bytes":"` + base64.StdEncoding.EncodeToString(raw) + `","len":4}}`
	if got := decode(t, ld(1, raw)); got != want {
		t.Errorf("got %s\nwant %s", got, want)
	}
}

func TestAmbiguityPrefersStringOrMessage(t *testing.T) {
	// "AAAAAAAAA" is printable text and also parses as a message (field 8
	// fixed64): text wins.
	if got := decode(t, ld(1, []byte("AAAAAAAAA"))); got != `{"1":"AAAAAAAAA"}` {
		t.Errorf("text case: %s", got)
	}
	// Inner message {1:"ééééé"} is valid UTF-8 text but mostly non-ASCII: message wins.
	inner := str(1, strings.Repeat("é", 5))
	if got := decode(t, ld(1, inner)); got != `{"1":{"1":"ééééé"}}` {
		t.Errorf("message case: %s", got)
	}
}

func TestRejects(t *testing.T) {
	for name, b := range map[string][]byte{
		"group start":      tag(1, 3),
		"group end":        tag(1, 4),
		"wire type 6":      tag(1, 6),
		"field zero":       {0x00, 0x01},
		"truncated string": {0x0a, 0x05, 'a'},
		"truncated fixed":  cat(tag(1, 1), []byte{1, 2}),
		"huge field num":   varint((1<<29)<<3 | 0),
	} {
		if _, err := Decode(b); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestDepthLimit(t *testing.T) {
	b := str(1, "leaf")
	for i := 0; i < 40; i++ {
		b = ld(1, b)
	}
	m, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	out := js(t, m)
	if !strings.Contains(out, `"bytes"`) {
		t.Errorf("expected deep nesting to fall back to bytes: %s", out)
	}
}

func TestConnectStream(t *testing.T) {
	stream := cat(
		env(0, str(1, "first")),
		env(0, cat(str(1, "second"), vint(2, 9))),
		env(2, []byte(`{"metadata":{}}`)),
	)
	got := js(t, DecodeStream(stream))
	want := `[{"1":"first"},{"1":"second","2":9},{"trailer":{"metadata":{}}}]`
	if got != want {
		t.Errorf("got %s\nwant %s", got, want)
	}
	// gRPC-Web text trailer
	got = js(t, DecodeStream(env(0x80, []byte("grpc-status: 0\r\n"))))
	if got != `[{"trailer":"grpc-status: 0\r\n"}]` {
		t.Errorf("grpc-web trailer: %s", got)
	}
}

func TestTruncatedStream(t *testing.T) {
	full := cat(env(0, str(1, "first")), env(0, str(1, "second")))
	cut := full[:len(full)-3]
	if got := js(t, DecodeStream(cut)); got != `[{"1":"first"},{"truncated":true}]` {
		t.Errorf("cut payload: %s", got)
	}
	cut = append(env(0, str(1, "first")), 0, 0, 0)
	if got := js(t, DecodeStream(cut)); got != `[{"1":"first"},{"truncated":true}]` {
		t.Errorf("cut header: %s", got)
	}
}

func TestKindOf(t *testing.T) {
	for ct, want := range map[string]Kind{
		"application/proto":                     KindMessage,
		"application/x-protobuf; charset=utf-8": KindMessage,
		"Application/Protobuf":                  KindMessage,
		"application/connect+proto":             KindStream,
		"application/grpc-web+proto":            KindStream,
		"application/grpc-web":                  KindStream,
		"application/json":                      KindNone,
		"":                                      KindNone,
	} {
		if got := KindOf(ct); got != want {
			t.Errorf("KindOf(%q) = %v, want %v", ct, got, want)
		}
	}
}

func TestDecodeBodyTruncatedAndInvalid(t *testing.T) {
	full := cat(str(1, "hello"), str(2, "world"))
	raw, msg := DecodeBody(KindMessage, full[:len(full)-2], true)
	if string(raw) != `{"1":"hello"}` || msg == "" {
		t.Errorf("partial: %s %q", raw, msg)
	}
	if raw, msg := DecodeBody(KindMessage, []byte{0x00}, false); raw != nil || msg == "" {
		t.Errorf("invalid: %s %q", raw, msg)
	}
	var v any
	raw, _ = DecodeBody(KindStream, env(0, str(1, "a")), false)
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Errorf("stream output is not JSON: %v", err)
	}
}
