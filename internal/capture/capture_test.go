package capture

import (
	"bytes"
	"compress/gzip"
	"strings"
	"testing"
	"time"
)

func TestParseSize(t *testing.T) {
	cases := map[string]int64{"1MB": 1 << 20, "512KB": 512 << 10, "2mb": 2 << 20, "100": 100, "1GB": 1 << 30, "0": 0}
	for in, want := range cases {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "abc", "-1MB", "MB"} {
		if _, err := ParseSize(bad); err == nil {
			t.Errorf("ParseSize(%q) should fail", bad)
		}
	}
}

func TestBufferCaps(t *testing.T) {
	b := NewBuffer(4)
	b.Write([]byte("ab"))
	b.Write([]byte("cdef"))
	if string(b.Bytes()) != "abcd" || b.Total() != 6 || !b.Truncated() {
		t.Fatalf("got %q total=%d trunc=%v", b.Bytes(), b.Total(), b.Truncated())
	}
}

func TestEncodeBody(t *testing.T) {
	if s, b64, tr := EncodeBody([]byte("héllo"), "", false, 100); s != "héllo" || b64 || tr {
		t.Errorf("plain: %q %v %v", s, b64, tr)
	}
	if s, b64, _ := EncodeBody([]byte{0xff, 0xfe, 0x00}, "", false, 100); !b64 || s != "//4A" {
		t.Errorf("binary: %q %v", s, b64)
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write([]byte(`{"a":1}`))
	zw.Close()
	if s, b64, _ := EncodeBody(gz.Bytes(), "gzip", false, 100); s != `{"a":1}` || b64 {
		t.Errorf("gzip: %q %v", s, b64)
	}
	if s, _, tr := EncodeBody(gz.Bytes(), "gzip", false, 3); s != `{"a` || !tr {
		t.Errorf("gzip limit: %q %v", s, tr)
	}
	// Cut in the middle of a multi-byte rune stays text.
	if s, b64, tr := EncodeBody([]byte("aé")[:2], "", true, 100); s != "a" || b64 || !tr {
		t.Errorf("partial rune: %q %v %v", s, b64, tr)
	}
}

func TestMatchHost(t *testing.T) {
	pats := []string{"*.anthropic.com", "127.0.0.1:9000"}
	for hp, want := range map[string]bool{
		"api.anthropic.com":     true,
		"api.anthropic.com:443": true,
		"anthropic.com":         false,
		"127.0.0.1:9000":        true,
		"127.0.0.1:9001":        false,
		"evil.com":              false,
	} {
		if got := MatchHost(pats, hp); got != want {
			t.Errorf("MatchHost(%q) = %v, want %v", hp, got, want)
		}
	}
}

func TestLoggerFilterAndFormat(t *testing.T) {
	var out bytes.Buffer
	l := &Logger{Stdout: &out, Verbose: true, FilterHosts: []string{"*.anthropic.com"}}
	ts := time.Date(2026, 1, 2, 3, 4, 5, 6_000_000, time.UTC)
	l.Log(&Record{TS: ts, Method: "GET", URL: "https://example.com/x", Status: 200})
	if out.Len() != 0 {
		t.Fatalf("filtered host was logged: %q", out.String())
	}
	l.Log(&Record{
		TS: ts, Method: "POST", URL: "https://api.anthropic.com/v1/messages", Status: 200,
		DurationMS: 12, ReqBytes: 7, RespBytes: 9,
		ReqHeaders: map[string][]string{"Content-Type": {"application/json"}},
		ReqBody:    `{"a":1}`, RespBody: "AAE=", RespBodyBase64: true,
	})
	got := out.String()
	for _, want := range []string{
		"03:04:05.006  POST  https://api.anthropic.com/v1/messages  → 200  12ms  7/9",
		"> Content-Type: application/json",
		"\"a\": 1",
		"<2 bytes binary>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

func TestFormatWebSocketMessage(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 6_000_000, time.UTC)
	r := &Record{TS: ts, Mode: ModeWebSocket, Method: "WS<", URL: "https://claude.ai/v1/sessions/ws/x/subscribe",
		Status: 101, RespBody: `{"a":1}`, RespBytes: 7}
	plain := (&Logger{}).Format(r)
	if plain != "03:04:05.006  WS<  wss://claude.ai/v1/sessions/ws/x/subscribe  7 bytes\n" {
		t.Errorf("summary = %q", plain)
	}
	if v := (&Logger{Verbose: true}).Format(r); !strings.Contains(v, "\"a\": 1") {
		t.Errorf("verbose should pretty-print JSON: %q", v)
	}
	ping := &Record{TS: ts, Mode: ModeWebSocket, Method: "WS> ping", URL: "http://h/x", ReqBytes: 1}
	if got := (&Logger{}).Format(ping); !strings.Contains(got, "WS> ping  ws://h/x  1 bytes") {
		t.Errorf("ping = %q", got)
	}
}

func TestVerbosePrintsDecodedProtoInsteadOfBinary(t *testing.T) {
	r := &Record{
		TS: time.Now(), Mode: ModeProxy, Method: "POST", URL: "https://h/rpc", Status: 200,
		RespBody: "AAE=", RespBodyBase64: true, RespBodyDecoded: []byte(`{"1":"hi"}`),
	}
	out := (&Logger{Verbose: true}).Format(r)
	if !strings.Contains(out, `"1": "hi"`) || strings.Contains(out, "bytes binary") {
		t.Errorf("verbose output: %q", out)
	}
	r.RespBodyDecoded = nil
	if out := (&Logger{Verbose: true}).Format(r); !strings.Contains(out, "<2 bytes binary>") {
		t.Errorf("without decoding: %q", out)
	}
}

func TestFormatStreamUnit(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 6_000_000, time.UTC)
	r := &Record{TS: ts, Mode: ModeStream, Method: "RPC<", URL: "https://claude.ai/x/StreamTimeline", Status: 200,
		Seq: 3, RespBytes: 120, RespBody: "AAE=", RespBodyBase64: true, RespBodyDecoded: []byte(`{"9":"hi"}`)}
	if got := (&Logger{}).Format(r); got != "03:04:05.006  RPC<  https://claude.ai/x/StreamTimeline  #3  120 bytes\n" {
		t.Errorf("summary = %q", got)
	}
	if got := (&Logger{Verbose: true}).Format(r); !strings.Contains(got, `"9": "hi"`) {
		t.Errorf("verbose = %q", got)
	}
}
