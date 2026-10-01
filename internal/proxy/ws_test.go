package proxy

import (
	"bufio"
	"bytes"
	"compress/flate"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Tuskira/claude-desktop-utility/internal/capture"
)

var testKey = [4]byte{0x37, 0xfa, 0x21, 0x3d}

// wsEncode builds a frame. A non-nil key masks the payload.
func wsEncode(fin, rsv1 bool, op byte, payload []byte, key *[4]byte) []byte {
	var b bytes.Buffer
	b0 := op
	if fin {
		b0 |= 0x80
	}
	if rsv1 {
		b0 |= 0x40
	}
	b.WriteByte(b0)
	var mbit byte
	if key != nil {
		mbit = 0x80
	}
	switch n := len(payload); {
	case n < 126:
		b.WriteByte(mbit | byte(n))
	case n < 1<<16:
		b.WriteByte(mbit | 126)
		binary.Write(&b, binary.BigEndian, uint16(n))
	default:
		b.WriteByte(mbit | 127)
		binary.Write(&b, binary.BigEndian, uint64(n))
	}
	if key != nil {
		b.Write(key[:])
		for i, c := range payload {
			b.WriteByte(c ^ key[i&3])
		}
	} else {
		b.Write(payload)
	}
	return b.Bytes()
}

// wsRead reads one frame with exact reads (so a TeeReader sees only its bytes).
func wsRead(r io.Reader) (fin, rsv1 bool, op byte, payload []byte, err error) {
	var h [2]byte
	if _, err = io.ReadFull(r, h[:]); err != nil {
		return
	}
	fin, rsv1, op = h[0]&0x80 != 0, h[0]&0x40 != 0, h[0]&0x0f
	n := uint64(h[1] & 0x7f)
	switch n {
	case 126:
		var e [2]byte
		if _, err = io.ReadFull(r, e[:]); err != nil {
			return
		}
		n = uint64(binary.BigEndian.Uint16(e[:]))
	case 127:
		var e [8]byte
		if _, err = io.ReadFull(r, e[:]); err != nil {
			return
		}
		n = binary.BigEndian.Uint64(e[:])
	}
	var key [4]byte
	if h[1]&0x80 != 0 {
		if _, err = io.ReadFull(r, key[:]); err != nil {
			return
		}
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(r, payload); err != nil {
		return
	}
	if h[1]&0x80 != 0 {
		for i := range payload {
			payload[i] ^= key[i&3]
		}
	}
	return
}

func TestFrameParser(t *testing.T) {
	big16 := bytes.Repeat([]byte("x"), 300)
	big64 := bytes.Repeat([]byte("yz"), 35000) // 70000 bytes: 64-bit length form is used by wsEncode
	var stream []byte
	stream = append(stream, wsEncode(true, false, opText, []byte("hi"), nil)...)
	stream = append(stream, wsEncode(true, false, opText, []byte("masked"), &testKey)...)
	stream = append(stream, wsEncode(false, false, opBinary, big16, &testKey)...)
	stream = append(stream, wsEncode(true, false, opContinuation, big64, nil)...)
	stream = append(stream, wsEncode(true, false, opPing, nil, &testKey)...)
	stream = append(stream, wsEncode(true, true, opText, []byte("z"), nil)...)

	check := func(name string, feed func(p *frameParser) []wsFrame) {
		p := &frameParser{retain: 1 << 20}
		fr := feed(p)
		if p.Err() != nil || len(fr) != 6 {
			t.Fatalf("%s: err=%v frames=%d", name, p.Err(), len(fr))
		}
		if string(fr[0].Payload) != "hi" || !fr[0].Fin || fr[0].Opcode != opText {
			t.Errorf("%s: frame 0 %+v", name, fr[0])
		}
		if string(fr[1].Payload) != "masked" {
			t.Errorf("%s: masked payload = %q", name, fr[1].Payload)
		}
		if fr[2].Fin || fr[2].Opcode != opBinary || !bytes.Equal(fr[2].Payload, big16) || fr[2].Length != 300 {
			t.Errorf("%s: 16-bit frame wrong (len %d)", name, len(fr[2].Payload))
		}
		if fr[3].Opcode != opContinuation || !bytes.Equal(fr[3].Payload, big64) || fr[3].Length != 70000 {
			t.Errorf("%s: 64-bit frame wrong (len %d)", name, len(fr[3].Payload))
		}
		if fr[4].Opcode != opPing || len(fr[4].Payload) != 0 {
			t.Errorf("%s: ping %+v", name, fr[4])
		}
		if !fr[5].RSV1 {
			t.Errorf("%s: RSV1 lost", name)
		}
	}
	check("whole", func(p *frameParser) []wsFrame { return p.Feed(stream) })
	check("bytewise", func(p *frameParser) []wsFrame {
		var out []wsFrame
		for i := range stream {
			out = append(out, p.Feed(stream[i:i+1])...)
		}
		return out
	})
}

func TestFrameParserRetainLimitStaysInSync(t *testing.T) {
	p := &frameParser{retain: 10}
	stream := append(wsEncode(true, false, opText, bytes.Repeat([]byte("a"), 100), &testKey),
		wsEncode(true, false, opText, []byte("next"), nil)...)
	fr := p.Feed(stream)
	if len(fr) != 2 || len(fr[0].Payload) != 10 || fr[0].Length != 100 || string(fr[1].Payload) != "next" {
		t.Fatalf("frames: %+v err=%v", fr, p.Err())
	}
}

func TestFrameParserRejectsGarbage(t *testing.T) {
	p := &frameParser{retain: 10}
	p.Feed([]byte{0x83, 0x00}) // opcode 3 is reserved
	if p.Err() == nil {
		t.Fatal("expected a protocol error")
	}
}

// dialWS performs CONNECT + TLS + upgrade through the proxy.
func dialWS(t *testing.T, h *harness, upstream *httptest.Server, path string) (net.Conn, *bufio.Reader, *http.Response) {
	t.Helper()
	u, _ := url.Parse(upstream.URL)
	pc, err := net.Dial("tcp", h.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	fmt.Fprintf(pc, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", u.Host, u.Host)
	cresp, err := http.ReadResponse(bufio.NewReader(pc), &http.Request{Method: "CONNECT"})
	if err != nil || cresp.StatusCode != 200 {
		t.Fatalf("CONNECT: %v %v", cresp, err)
	}
	tc := tls.Client(pc, &tls.Config{RootCAs: h.pool, ServerName: "127.0.0.1"})
	fmt.Fprintf(tc, "GET %s HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Key: dGVzdA==\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Extensions: permessage-deflate\r\n\r\n", path, u.Host)
	br := bufio.NewReader(tc)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 101 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	return tc, br, resp
}

func wsRecords(recs []capture.Record) (out, in []capture.Record) {
	for _, r := range recs {
		switch r.Method {
		case "WS>":
			out = append(out, r)
		case "WS<":
			in = append(in, r)
		}
	}
	return
}

func TestWebSocketFramesLogged(t *testing.T) {
	binPayload := []byte{0x00, 0x01, 0xfe, 0xff}
	serverBurst := bytes.Join([][]byte{
		wsEncode(true, false, opText, []byte("unsolicited"), nil),
		wsEncode(false, false, opText, []byte("frag-"), nil),
		wsEncode(true, false, opPing, []byte("p"), nil), // control frame interleaved mid-message
		wsEncode(true, false, opContinuation, []byte("ment"), nil),
		wsEncode(true, false, opBinary, binPayload, nil),
	}, nil)
	clientFrame := wsEncode(true, false, opText, []byte(`{"q":"client-msg"}`), &testKey)
	echoFrame := wsEncode(true, false, opText, []byte("echo:client-msg"), nil)

	gotClient := make(chan []byte, 1)
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, brw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		brw.Flush()
		conn.Write(serverBurst)
		raw := make([]byte, len(clientFrame))
		io.ReadFull(brw, raw)
		gotClient <- raw
		conn.Write(echoFrame)
	}))
	defer upstream.Close()

	h := newHarness(t, nil)
	tc, br, _ := dialWS(t, h, upstream, "/ws")
	if _, err := tc.Write(clientFrame); err != nil {
		t.Fatal(err)
	}
	want := append(append([]byte(nil), serverBurst...), echoFrame...)
	got := make([]byte, len(want))
	tc.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(br, got); err != nil {
		t.Fatalf("reading server frames: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("bytes relayed to the client differ from what the server sent")
	}
	if raw := <-gotClient; !bytes.Equal(raw, clientFrame) {
		t.Fatal("bytes relayed to the server differ from what the client sent")
	}
	tc.Close() // ends the tunnel; the closing record follows

	// handshake + 1 client msg + 4 server msgs + close-summary record
	recs := h.records(7)
	if recs[0].Status != 101 || recs[0].Method != "GET" || recs[0].Mode != "proxy" {
		t.Errorf("first record should be the handshake: %+v", recs[0])
	}
	out, in := wsRecords(recs)
	if len(out) != 1 || len(in) != 4 {
		t.Fatalf("got %d WS> and %d WS< records: %+v", len(out), len(in), recs)
	}
	if out[0].ReqBody != `{"q":"client-msg"}` || out[0].Mode != "websocket" || out[0].Status != 101 || out[0].URL != upstream.URL+"/ws" || out[0].ReqBytes != 18 {
		t.Errorf("WS> record wrong: %+v", out[0])
	}
	wantBodies := []struct {
		body string
		b64  bool
	}{
		{"unsolicited", false},
		{"frag-ment", false}, // two frames, one record
		{base64.StdEncoding.EncodeToString(binPayload), true},
		{"echo:client-msg", false},
	}
	for i, w := range wantBodies {
		if in[i].RespBody != w.body || in[i].RespBodyBase64 != w.b64 || in[i].DurationMS != 0 {
			t.Errorf("WS< #%d = %q b64=%v, want %q b64=%v", i, in[i].RespBody, in[i].RespBodyBase64, w.body, w.b64)
		}
	}
	if in[1].RespBytes != 9 {
		t.Errorf("fragmented message bytes = %d, want 9", in[1].RespBytes)
	}
	last := recs[len(recs)-1]
	if last.Method != "WS-CLOSE" || last.ReqBytes != int64(len(clientFrame)) || last.RespBytes != int64(len(want)) {
		t.Errorf("close record: %+v (want %d/%d)", last, len(clientFrame), len(want))
	}
}

// pmdWriter compresses messages the way a permessage-deflate sender does,
// keeping its window across messages (context takeover).
type pmdWriter struct {
	buf bytes.Buffer
	fw  *flate.Writer
}

func newPMD() *pmdWriter {
	p := &pmdWriter{}
	p.fw, _ = flate.NewWriter(&p.buf, flate.DefaultCompression)
	return p
}

func (p *pmdWriter) compress(msg string) []byte {
	p.buf.Reset()
	p.fw.Write([]byte(msg))
	p.fw.Flush()
	b := p.buf.Bytes()
	return append([]byte(nil), b[:len(b)-4]...) // strip the 00 00 ff ff tail
}

func TestWebSocketPermessageDeflate(t *testing.T) {
	m1 := strings.Repeat("hello deflate ", 10)
	m2 := strings.Repeat("hello deflate ", 5) + "and again"
	pmd := newPMD()
	frames := bytes.Join([][]byte{
		wsEncode(true, true, opText, pmd.compress(m1), nil),
		wsEncode(true, true, opText, pmd.compress(m2), nil),               // needs the window from m1
		wsEncode(true, true, opText, []byte{0xff, 0xff, 0xff, 0xff}, nil), // not valid deflate
	}, nil)

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, brw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Extensions: permessage-deflate\r\n\r\n")
		brw.Flush()
		conn.Write(frames)
		io.Copy(io.Discard, brw) // hold open until the client goes away
	}))
	defer upstream.Close()

	h := newHarness(t, nil)
	tc, br, _ := dialWS(t, h, upstream, "/pmd")
	got := make([]byte, len(frames))
	tc.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(br, got); err != nil || !bytes.Equal(got, frames) {
		t.Fatalf("client did not receive the compressed frames unchanged: %v", err)
	}
	tc.Close()

	recs := h.records(5) // handshake, 3 messages, close summary
	_, in := wsRecords(recs)
	if len(in) != 3 {
		t.Fatalf("WS< records: %+v", recs)
	}
	if in[0].RespBody != m1 || in[0].Error != "" {
		t.Errorf("message 1 = %q err=%q", in[0].RespBody, in[0].Error)
	}
	if in[1].RespBody != m2 || in[1].Error != "" {
		t.Errorf("message 2 (context takeover) = %q err=%q", in[1].RespBody, in[1].Error)
	}
	if in[2].Error != "permessage-deflate decode failed" || !in[2].RespBodyBase64 ||
		in[2].RespBody != base64.StdEncoding.EncodeToString([]byte{0xff, 0xff, 0xff, 0xff}) {
		t.Errorf("bad message record: %+v", in[2])
	}
}
