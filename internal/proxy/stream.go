package proxy

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Tuskira/claude-desktop-utility/internal/capture"
	"github.com/Tuskira/claude-desktop-utility/internal/protodec"
)

const (
	maxSSELine  = 16 << 20
	maxEnvelope = 64 << 20
	tapBacklog  = 4096 // chunks queued for the splitter before it gives up
)

type streamKind int

const (
	streamNone streamKind = iota
	streamSSE
	streamRPC
	streamNDJSON
)

func (k streamKind) method() string {
	switch k {
	case streamSSE:
		return "SSE<"
	case streamRPC:
		return "RPC<"
	}
	return "NDJSON<"
}

// streamKindOf decides whether a response is logged unit by unit.
func streamKindOf(resp *http.Response) streamKind {
	// Wire bytes of a compressed stream cannot be split; the final record
	// still decodes gzip as a whole.
	if enc := strings.TrimSpace(resp.Header.Get("Content-Encoding")); enc != "" && !strings.EqualFold(enc, "identity") {
		return streamNone
	}
	mt := strings.ToLower(strings.TrimSpace(strings.SplitN(resp.Header.Get("Content-Type"), ";", 2)[0]))
	switch mt {
	case "text/event-stream":
		return streamSSE
	case "application/connect+proto", "application/grpc-web+proto", "application/grpc-web":
		return streamRPC
	case "application/x-ndjson", "application/ndjson", "application/jsonl", "application/x-jsonlines", "application/stream+json":
		if resp.ContentLength == -1 {
			return streamNDJSON
		}
	}
	return streamNone
}

// streamUnit is one event / envelope / line.
type streamUnit struct {
	Body []byte // SSE event text, envelope payload, or NDJSON line
	Data []byte // SSE: joined data: lines (nil if none)
	Flag byte   // envelope flag byte
}

// splitter incrementally cuts a byte stream into units.
type splitter interface {
	Feed(p []byte) ([]streamUnit, error)
	Flush() []streamUnit // called at end of stream for a trailing unit
}

// sseSplitter cuts text/event-stream into events at blank lines. Lines end
// with \n or \r\n (a lone \r terminator is not recognised). Comment lines
// (starting with ':') are dropped; comment-only events are not emitted.
type sseSplitter struct {
	buf []byte
	cur []string
}

func (s *sseSplitter) Feed(p []byte) ([]streamUnit, error) {
	s.buf = append(s.buf, p...)
	var out []streamUnit
	consumed := 0
	for {
		i := bytes.IndexByte(s.buf[consumed:], '\n')
		if i < 0 {
			break
		}
		line := s.buf[consumed : consumed+i]
		consumed += i + 1
		line = bytes.TrimSuffix(line, []byte("\r"))
		if len(line) == 0 {
			if u, ok := s.endEvent(); ok {
				out = append(out, u)
			}
			continue
		}
		s.cur = append(s.cur, string(line))
	}
	if consumed > 0 {
		s.buf = append([]byte(nil), s.buf[consumed:]...)
	}
	if len(s.buf) > maxSSELine {
		return out, errors.New("SSE line too long")
	}
	return out, nil
}

func (s *sseSplitter) endEvent() (streamUnit, bool) {
	lines := s.cur
	s.cur = nil
	var keep, data []string
	hasData := false
	for _, l := range lines {
		if strings.HasPrefix(l, ":") {
			continue
		}
		keep = append(keep, l)
		if l == "data" || strings.HasPrefix(l, "data:") {
			hasData = true
			v := strings.TrimPrefix(strings.TrimPrefix(l, "data"), ":")
			data = append(data, strings.TrimPrefix(v, " "))
		}
	}
	if len(keep) == 0 {
		return streamUnit{}, false
	}
	u := streamUnit{Body: []byte(strings.Join(keep, "\n"))}
	if hasData {
		u.Data = []byte(strings.Join(data, "\n"))
	}
	return u, true
}

func (s *sseSplitter) Flush() []streamUnit {
	if len(s.buf) > 0 {
		s.cur = append(s.cur, strings.TrimSuffix(string(s.buf), "\r"))
		s.buf = nil
	}
	if u, ok := s.endEvent(); ok {
		return []streamUnit{u}
	}
	return nil
}

// envSplitter cuts Connect / gRPC-Web envelopes: flag(1) + length(4, BE) + payload.
type envSplitter struct{ buf []byte }

func (s *envSplitter) Feed(p []byte) ([]streamUnit, error) {
	s.buf = append(s.buf, p...)
	var out []streamUnit
	consumed := 0
	for len(s.buf)-consumed >= 5 {
		h := s.buf[consumed:]
		n := binary.BigEndian.Uint32(h[1:5])
		if n > maxEnvelope {
			return out, fmt.Errorf("envelope of %d bytes is implausible", n)
		}
		if len(h)-5 < int(n) {
			break
		}
		out = append(out, streamUnit{Flag: h[0], Body: append([]byte(nil), h[5:5+int(n)]...)})
		consumed += 5 + int(n)
	}
	if consumed > 0 {
		s.buf = append([]byte(nil), s.buf[consumed:]...)
	}
	return out, nil
}

func (s *envSplitter) Flush() []streamUnit { return nil }

// lineSplitter cuts newline-delimited text (NDJSON), skipping blank lines.
type lineSplitter struct{ buf []byte }

func (s *lineSplitter) Feed(p []byte) ([]streamUnit, error) {
	s.buf = append(s.buf, p...)
	var out []streamUnit
	consumed := 0
	for {
		i := bytes.IndexByte(s.buf[consumed:], '\n')
		if i < 0 {
			break
		}
		line := bytes.TrimSuffix(s.buf[consumed:consumed+i], []byte("\r"))
		consumed += i + 1
		if len(bytes.TrimSpace(line)) > 0 {
			out = append(out, streamUnit{Body: append([]byte(nil), line...)})
		}
	}
	if consumed > 0 {
		s.buf = append([]byte(nil), s.buf[consumed:]...)
	}
	if len(s.buf) > maxSSELine {
		return out, errors.New("line too long")
	}
	return out, nil
}

func (s *lineSplitter) Flush() []streamUnit {
	line := bytes.TrimSpace(s.buf)
	s.buf = nil
	if len(line) == 0 {
		return nil
	}
	return []streamUnit{{Body: append([]byte(nil), line...)}}
}

func newSplitter(k streamKind) splitter {
	switch k {
	case streamSSE:
		return &sseSplitter{}
	case streamRPC:
		return &envSplitter{}
	}
	return &lineSplitter{}
}

// streamTap receives a copy of the response body bytes on the forwarding
// path. Write only queues a copy; a separate goroutine splits and logs, so
// the client is never delayed by logging. Any splitter problem stops
// splitting (logged once) and never affects forwarding.
type streamTap struct {
	srv    *Server
	kind   streamKind
	client string
	url    string
	status int
	start  time.Time
	max    int64

	split  splitter
	ch     chan tapChunk
	done   chan struct{}
	failed atomic.Bool
	seq    int // worker goroutine only; valid after Close
}

type tapChunk struct {
	at   time.Time
	data []byte
}

func newStreamTap(s *Server, kind streamKind, rec *capture.Record, status int) *streamTap {
	t := &streamTap{
		srv: s, kind: kind, client: rec.Client, url: rec.URL, status: status,
		start: rec.TS, max: s.cfg.MaxBody, split: newSplitter(kind),
		ch: make(chan tapChunk, tapBacklog), done: make(chan struct{}),
	}
	go t.run()
	return t
}

// Write implements io.Writer; it never blocks and never fails.
func (t *streamTap) Write(p []byte) (int, error) {
	if !t.failed.Load() && len(p) > 0 {
		select {
		case t.ch <- tapChunk{at: time.Now(), data: append([]byte(nil), p...)}:
		default:
			t.fail(errors.New("splitter fell too far behind"))
		}
	}
	return len(p), nil
}

func (t *streamTap) fail(err error) {
	if t.failed.CompareAndSwap(false, true) {
		t.srv.logf("stream records for %s stopped: %v (forwarding continues)", t.url, err)
	}
}

// Close ends the stream, waits for pending records to be written and
// returns how many units were seen.
func (t *streamTap) Close() int {
	close(t.ch)
	<-t.done
	return t.seq
}

func (t *streamTap) run() {
	defer close(t.done)
	var last time.Time
	for c := range t.ch {
		last = c.at
		if t.failed.Load() {
			continue
		}
		t.guarded(func() {
			units, err := t.split.Feed(c.data)
			for _, u := range units {
				t.emit(u, c.at)
			}
			if err != nil {
				t.fail(err)
			}
		})
	}
	if !t.failed.Load() {
		t.guarded(func() {
			for _, u := range t.split.Flush() {
				t.emit(u, last)
			}
		})
	}
}

func (t *streamTap) guarded(f func()) {
	defer func() {
		if r := recover(); r != nil {
			t.fail(fmt.Errorf("panic: %v", r))
		}
	}()
	f()
}

func (t *streamTap) emit(u streamUnit, at time.Time) {
	t.seq++
	r := &capture.Record{
		TS: at, Mode: capture.ModeStream, Client: t.client, Method: t.kind.method(),
		URL: t.url, Status: t.status, Seq: t.seq, DurationMS: at.Sub(t.start).Milliseconds(),
		RespBytes: int64(len(u.Body)),
	}
	body := u.Body
	trunc := false
	if int64(len(body)) > t.max {
		body, trunc = body[:t.max], true
	}
	switch t.kind {
	case streamRPC:
		r.RespBody, r.RespBodyBase64, r.RespBodyTruncated = base64.StdEncoding.EncodeToString(body), true, trunc
		if dec, err := protodec.ToJSON(protodec.DecodeEnvelope(u.Flag, u.Body)); err == nil {
			r.RespBodyDecoded = dec
		}
	default:
		r.RespBody, r.RespBodyBase64, r.RespBodyTruncated = capture.EncodeBody(body, "", trunc, t.max)
		payload := u.Body
		if t.kind == streamSSE {
			payload = u.Data
		}
		if len(payload) > 0 && json.Valid(payload) {
			var buf bytes.Buffer
			if json.Compact(&buf, payload) == nil {
				r.RespBodyDecoded = buf.Bytes()
			}
		}
	}
	t.srv.cfg.Logger.Log(r)
}
