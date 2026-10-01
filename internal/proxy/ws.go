package proxy

import (
	"bytes"
	"compress/flate"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Tuskira/claude-desktop-utility/internal/capture"
)

// WebSocket opcodes (RFC 6455).
const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xa
)

const (
	wsDeflateCap = 16 << 20 // max bytes retained/inflated per message when permessage-deflate is on
	wsWindow     = 32 << 10 // deflate window kept between messages
)

// wsFrame is one parsed frame. Payload is unmasked and may be shorter than
// Length when it exceeded the retention limit.
type wsFrame struct {
	Fin     bool
	RSV1    bool
	Opcode  byte
	Length  uint64
	Payload []byte
}

// frameParser is an incremental (push) RFC 6455 frame parser. It only
// observes bytes; it never affects what is forwarded.
type frameParser struct {
	retain int64 // max payload bytes kept per frame

	hdr       []byte
	inPayload bool
	cur       wsFrame
	masked    bool
	key       [4]byte
	pos       uint64
	remaining uint64
	err       error
}

// Err reports a protocol error after which parsing has stopped.
func (p *frameParser) Err() error { return p.err }

// Feed consumes b and returns the frames completed by it.
func (p *frameParser) Feed(b []byte) []wsFrame {
	var out []wsFrame
	for len(b) > 0 && p.err == nil {
		if !p.inPayload {
			p.hdr = append(p.hdr, b[0])
			b = b[1:]
			need := wsHeaderLen(p.hdr)
			if need == 0 || len(p.hdr) < need {
				continue
			}
			if p.err = p.start(); p.err != nil {
				break
			}
			if p.remaining == 0 {
				out = append(out, p.finish())
			}
			continue
		}
		n := uint64(len(b))
		if n > p.remaining {
			n = p.remaining
		}
		p.consume(b[:n])
		b = b[n:]
		p.remaining -= n
		if p.remaining == 0 {
			out = append(out, p.finish())
		}
	}
	return out
}

// wsHeaderLen returns the full header length once the first two bytes are
// known, else 0.
func wsHeaderLen(h []byte) int {
	if len(h) < 2 {
		return 0
	}
	n := 2
	switch h[1] & 0x7f {
	case 126:
		n += 2
	case 127:
		n += 8
	}
	if h[1]&0x80 != 0 {
		n += 4
	}
	return n
}

func (p *frameParser) start() error {
	h := p.hdr
	b0, b1 := h[0], h[1]
	if b0&0x30 != 0 {
		return errors.New("reserved bits RSV2/RSV3 set")
	}
	op := b0 & 0x0f
	switch op {
	case opContinuation, opText, opBinary, opClose, opPing, opPong:
	default:
		return fmt.Errorf("unknown opcode %d", op)
	}
	length := uint64(b1 & 0x7f)
	i := 2
	switch length {
	case 126:
		length = uint64(binary.BigEndian.Uint16(h[2:4]))
		i = 4
	case 127:
		length = binary.BigEndian.Uint64(h[2:10])
		if length>>63 != 0 {
			return errors.New("64-bit length has top bit set")
		}
		i = 10
	}
	fin := b0&0x80 != 0
	if op >= opClose && (!fin || length > 125) {
		return errors.New("invalid control frame")
	}
	p.masked = b1&0x80 != 0
	if p.masked {
		copy(p.key[:], h[i:i+4])
	}
	p.cur = wsFrame{Fin: fin, RSV1: b0&0x40 != 0, Opcode: op, Length: length}
	p.pos, p.remaining, p.inPayload = 0, length, true
	return nil
}

func (p *frameParser) consume(chunk []byte) {
	room := int(p.retain) - len(p.cur.Payload)
	if room < 0 {
		room = 0
	}
	k := len(chunk)
	if k > room {
		k = room
	}
	for i := 0; i < k; i++ {
		c := chunk[i]
		if p.masked {
			c ^= p.key[(p.pos+uint64(i))&3]
		}
		p.cur.Payload = append(p.cur.Payload, c)
	}
	p.pos += uint64(len(chunk))
}

func (p *frameParser) finish() wsFrame {
	f := p.cur
	p.hdr = p.hdr[:0]
	p.cur = wsFrame{}
	p.inPayload = false
	return f
}

// parseWSExtensions reads the permessage-deflate parameters accepted in a
// 101 response.
func parseWSExtensions(h http.Header) (deflate, clientNoCtx, serverNoCtx bool) {
	for _, v := range h.Values("Sec-WebSocket-Extensions") {
		for _, ext := range strings.Split(v, ",") {
			parts := strings.Split(ext, ";")
			if strings.ToLower(strings.TrimSpace(parts[0])) != "permessage-deflate" {
				continue
			}
			deflate = true
			for _, prm := range parts[1:] {
				switch strings.ToLower(strings.TrimSpace(strings.SplitN(prm, "=", 2)[0])) {
				case "client_no_context_takeover":
					clientNoCtx = true
				case "server_no_context_takeover":
					serverNoCtx = true
				}
			}
			return
		}
	}
	return
}

// wsTap watches one direction of a WebSocket tunnel. It is an io.Writer fed
// with the bytes being forwarded; it always reports success so forwarding is
// never disturbed, and it disables itself on any parse problem or panic.
type wsTap struct {
	log    *capture.Logger
	client string
	url    string
	dir    string // "WS>" client to server, "WS<" server to client
	max    int64

	deflate bool
	noCtx   bool
	window  []byte

	parser frameParser
	dead   bool

	inMsg   bool
	msgOp   byte
	msgRSV1 bool
	msgData []byte
	msgLen  uint64
}

func newWSTap(log *capture.Logger, client, url, dir string, max int64, deflate, noCtx bool) *wsTap {
	retain := max
	if deflate {
		retain = wsDeflateCap
	}
	t := &wsTap{log: log, client: client, url: url, dir: dir, max: max, deflate: deflate, noCtx: noCtx}
	t.parser.retain = retain
	return t
}

func (t *wsTap) Write(p []byte) (n int, err error) {
	n = len(p)
	if t.dead {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			t.dead = true
		}
	}()
	for _, f := range t.parser.Feed(p) {
		t.onFrame(f)
	}
	if t.parser.Err() != nil {
		t.dead = true
	}
	return
}

func (t *wsTap) rec() *capture.Record {
	return &capture.Record{
		TS: time.Now(), Mode: capture.ModeWebSocket, Client: t.client,
		Method: t.dir, URL: t.url, Status: http.StatusSwitchingProtocols,
	}
}

func (t *wsTap) setBody(r *capture.Record, body string, b64, trunc bool, wire uint64, errStr string) {
	r.Error = errStr
	if t.dir == "WS>" {
		r.ReqBody, r.ReqBodyBase64, r.ReqBodyTruncated, r.ReqBytes = body, b64, trunc, int64(wire)
	} else {
		r.RespBody, r.RespBodyBase64, r.RespBodyTruncated, r.RespBytes = body, b64, trunc, int64(wire)
	}
}

func (t *wsTap) onFrame(f wsFrame) {
	switch f.Opcode {
	case opPing, opPong:
		if t.log != nil && t.log.Verbose {
			r := t.rec()
			r.Method = t.dir + " ping"
			if f.Opcode == opPong {
				r.Method = t.dir + " pong"
			}
			body, b64, tr := capture.EncodeBody(f.Payload, "", false, t.max)
			t.setBody(r, body, b64, tr, f.Length, "")
			t.log.Console(r)
		}
	case opClose:
		body := "close"
		if len(f.Payload) >= 2 {
			body = fmt.Sprintf("close %d %s", binary.BigEndian.Uint16(f.Payload[:2]), strings.ToValidUTF8(string(f.Payload[2:]), "?"))
		}
		r := t.rec()
		t.setBody(r, strings.TrimSpace(body), false, false, f.Length, "")
		t.log.Log(r)
	case opText, opBinary:
		t.inMsg, t.msgOp, t.msgRSV1, t.msgData, t.msgLen = true, f.Opcode, f.RSV1, nil, 0
		t.addFragment(f)
	case opContinuation:
		if t.inMsg {
			t.addFragment(f)
		}
	}
}

func (t *wsTap) addFragment(f wsFrame) {
	t.msgLen += f.Length
	room := int(t.parser.retain) - len(t.msgData)
	p := f.Payload
	if len(p) > room {
		p = p[:max(room, 0)]
	}
	t.msgData = append(t.msgData, p...)
	if f.Fin {
		t.emitMessage()
		t.inMsg, t.msgData = false, nil
	}
}

func (t *wsTap) emitMessage() {
	data := t.msgData
	trunc := uint64(len(data)) < t.msgLen
	errStr := ""
	if t.msgRSV1 && t.deflate {
		out, err := t.inflate(data)
		if err != nil {
			errStr = "permessage-deflate decode failed"
			t.window = nil
		} else {
			data, trunc = out, false
		}
	}
	if int64(len(data)) > t.max {
		data, trunc = data[:t.max], true
	}
	r := t.rec()
	if errStr != "" || t.msgOp == opBinary {
		t.setBody(r, base64.StdEncoding.EncodeToString(data), true, trunc, t.msgLen, errStr)
	} else {
		body, b64, tr := capture.EncodeBody(data, "", trunc, t.max)
		t.setBody(r, body, b64, tr, t.msgLen, "")
	}
	t.log.Log(r)
}

// inflate decodes a permessage-deflate message (RFC 7692), keeping the LZ77
// window between messages unless no_context_takeover was negotiated.
func (t *wsTap) inflate(data []byte) ([]byte, error) {
	src := make([]byte, 0, len(data)+9)
	src = append(src, data...)
	// Sync tail removed by the sender, plus an empty final block so the
	// stream ends cleanly.
	src = append(src, 0x00, 0x00, 0xff, 0xff, 0x01, 0x00, 0x00, 0xff, 0xff)
	var r io.ReadCloser
	if len(t.window) > 0 {
		r = flate.NewReaderDict(bytes.NewReader(src), t.window)
	} else {
		r = flate.NewReader(bytes.NewReader(src))
	}
	out, err := io.ReadAll(io.LimitReader(r, wsDeflateCap+1))
	if err != nil {
		return nil, err
	}
	if len(out) > wsDeflateCap {
		return nil, errors.New("inflated message too large")
	}
	if t.noCtx {
		t.window = nil
	} else {
		w := append(t.window, out...)
		if len(w) > wsWindow {
			w = w[len(w)-wsWindow:]
		}
		t.window = append([]byte(nil), w...)
	}
	return out, nil
}
