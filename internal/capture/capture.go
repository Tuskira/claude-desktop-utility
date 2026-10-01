// Package capture records proxied exchanges as JSONL and prints summaries.
package capture

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Capture modes.
const (
	ModeProxy           = "proxy"
	ModeTransparent     = "transparent"
	ModeTransparentHTTP = "transparent-http"
	ModePassthrough     = "passthrough"
	ModeWebSocket       = "websocket"
	ModeStream          = "stream"
)

// Record is one captured exchange (one JSONL line).
type Record struct {
	TS                time.Time           `json:"ts"`
	Mode              string              `json:"mode"`
	Client            string              `json:"client"`
	Method            string              `json:"method"`
	URL               string              `json:"url"`
	ReqHeaders        map[string][]string `json:"req_headers"`
	ReqBody           string              `json:"req_body"`
	ReqBodyTruncated  bool                `json:"req_body_truncated"`
	ReqBodyBase64     bool                `json:"req_body_base64"`
	Status            int                 `json:"status"`
	RespHeaders       map[string][]string `json:"resp_headers"`
	RespBody          string              `json:"resp_body"`
	RespBodyTruncated bool                `json:"resp_body_truncated"`
	RespBodyBase64    bool                `json:"resp_body_base64"`
	DurationMS        int64               `json:"duration_ms"`
	Error             string              `json:"error"`

	// Schema-less protobuf decoding of proto content types (display only).
	ReqBodyDecoded  json.RawMessage `json:"req_body_decoded,omitempty"`
	RespBodyDecoded json.RawMessage `json:"resp_body_decoded,omitempty"`
	DecodeError     string          `json:"decode_error,omitempty"`

	// Streaming responses: Seq numbers the per-unit records (mode "stream");
	// StreamUnits on the final record counts how many units were seen.
	Seq         int `json:"seq,omitempty"`
	StreamUnits int `json:"stream_units,omitempty"`

	// Extra fields: total bytes seen (not just captured). For passthrough
	// tunnels these are client-to-upstream and upstream-to-client bytes.
	ReqBytes  int64 `json:"req_bytes"`
	RespBytes int64 `json:"resp_bytes"`
}

func (r *Record) hostport() string {
	u, err := url.Parse(r.URL)
	if err != nil {
		return ""
	}
	return u.Host
}

// Writer appends records to a JSONL file. A nil *Writer discards records.
type Writer struct {
	mu sync.Mutex
	f  *os.File
}

// OpenWriter opens path for appending. An empty path returns a nil Writer.
func OpenWriter(path string) (*Writer, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &Writer{f: f}, nil
}

// Write appends one record as a single line.
func (w *Writer) Write(r *Record) error {
	if w == nil {
		return nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(r); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_, err := w.f.Write(buf.Bytes())
	return err
}

// Close closes the file.
func (w *Writer) Close() error {
	if w == nil {
		return nil
	}
	return w.f.Close()
}

// Logger filters records and sends them to the JSONL file and stdout.
// A nil *Logger discards everything.
type Logger struct {
	Out         *Writer
	Stdout      io.Writer
	Verbose     bool
	FilterHosts []string

	// Consumer, when set, is called with every record Log writes (after the
	// file and stdout writes), on whatever goroutine called Log. It exists so
	// another package (e.g. one that turns traffic into gateway-shaped
	// records for forwarding) can observe traffic without a channel or a
	// second copy of the filtering logic. It runs synchronously and directly
	// affects proxy latency if it is slow, so an implementation must return
	// quickly: do real work (parsing, network I/O) elsewhere and only
	// enqueue here. A nil Consumer is a no-op. Log recovers a panic from
	// Consumer so a bug in it can never take down the proxy connection.
	Consumer func(*Record)

	mu sync.Mutex
}

// Log records r unless its host is excluded by FilterHosts.
func (l *Logger) Log(r *Record) {
	if l == nil {
		return
	}
	if len(l.FilterHosts) > 0 && !MatchHost(l.FilterHosts, r.hostport()) {
		return
	}
	if r.ReqHeaders == nil {
		r.ReqHeaders = map[string][]string{}
	}
	if r.RespHeaders == nil {
		r.RespHeaders = map[string][]string{}
	}
	if l.Out != nil {
		if err := l.Out.Write(r); err != nil {
			fmt.Fprintln(os.Stderr, "interceptor: write capture:", err)
		}
	}
	if l.Stdout != nil {
		s := l.Format(r)
		l.mu.Lock()
		io.WriteString(l.Stdout, s)
		l.mu.Unlock()
	}
	l.callConsumer(r)
}

func (l *Logger) callConsumer(r *Record) {
	if l.Consumer == nil {
		return
	}
	defer func() {
		if rec := recover(); rec != nil {
			fmt.Fprintln(os.Stderr, "interceptor: capture consumer panic (ignored):", rec)
		}
	}()
	l.Consumer(r)
}

// Console prints r to stdout only (not to the JSONL file), if Verbose.
func (l *Logger) Console(r *Record) {
	if l == nil || l.Stdout == nil || !l.Verbose {
		return
	}
	if len(l.FilterHosts) > 0 && !MatchHost(l.FilterHosts, r.hostport()) {
		return
	}
	s := l.Format(r)
	l.mu.Lock()
	io.WriteString(l.Stdout, s)
	l.mu.Unlock()
}

func isWSMessage(r *Record) bool {
	return r.Mode == ModeWebSocket && (strings.HasPrefix(r.Method, "WS>") || strings.HasPrefix(r.Method, "WS<"))
}

func wsURL(u string) string {
	switch {
	case strings.HasPrefix(u, "https://"):
		return "wss://" + u[len("https://"):]
	case strings.HasPrefix(u, "http://"):
		return "ws://" + u[len("http://"):]
	}
	return u
}

// formatWS renders one WebSocket message.
func (l *Logger) formatWS(r *Record) string {
	var b strings.Builder
	n, body, isB64, trunc := r.RespBytes, r.RespBody, r.RespBodyBase64, r.RespBodyTruncated
	if strings.HasPrefix(r.Method, "WS>") {
		n, body, isB64, trunc = r.ReqBytes, r.ReqBody, r.ReqBodyBase64, r.ReqBodyTruncated
	}
	fmt.Fprintf(&b, "%s  %s  %s  %d bytes", r.TS.Format("15:04:05.000"), r.Method, wsURL(r.URL), n)
	if r.Error != "" {
		b.WriteString("  ERR: " + r.Error)
	}
	b.WriteByte('\n')
	if l.Verbose {
		if s := renderBody(body, isB64, trunc); s != "" {
			b.WriteString(s + "\n\n")
		}
	}
	return b.String()
}

// formatStream renders one streamed unit (SSE event, RPC envelope, NDJSON line).
func (l *Logger) formatStream(r *Record) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s  %s  #%d  %d bytes", r.TS.Format("15:04:05.000"), r.Method, r.URL, r.Seq, r.RespBytes)
	if r.Error != "" {
		b.WriteString("  ERR: " + r.Error)
	}
	b.WriteByte('\n')
	if l.Verbose {
		if s := renderBodyOrDecoded(r.RespBody, r.RespBodyBase64, r.RespBodyTruncated, r.RespBodyDecoded); s != "" {
			b.WriteString(s + "\n\n")
		}
	}
	return b.String()
}

// Format renders r for the terminal: one summary line, plus headers and
// bodies when Verbose is set.
func (l *Logger) Format(r *Record) string {
	if isWSMessage(r) {
		return l.formatWS(r)
	}
	if r.Mode == ModeStream {
		return l.formatStream(r)
	}
	var b strings.Builder
	status := "-"
	if r.Status != 0 {
		status = strconv.Itoa(r.Status)
	}
	dur := (time.Duration(r.DurationMS) * time.Millisecond).String()
	fmt.Fprintf(&b, "%s  %s  %s  → %s  %s  %d/%d",
		r.TS.Format("15:04:05.000"), r.Method, r.URL, status, dur, r.ReqBytes, r.RespBytes)
	if r.Mode == ModePassthrough {
		b.WriteString("  [passthrough]")
	}
	if r.Error != "" {
		b.WriteString("  ERR: " + r.Error)
	}
	b.WriteByte('\n')
	if l.Verbose && r.Mode != ModePassthrough {
		writeHeaders(&b, "> ", r.ReqHeaders)
		if s := renderBodyOrDecoded(r.ReqBody, r.ReqBodyBase64, r.ReqBodyTruncated, r.ReqBodyDecoded); s != "" {
			b.WriteString("\n" + s + "\n")
		}
		writeHeaders(&b, "< ", r.RespHeaders)
		if s := renderBodyOrDecoded(r.RespBody, r.RespBodyBase64, r.RespBodyTruncated, r.RespBodyDecoded); s != "" {
			b.WriteString("\n" + s + "\n")
		}
		if r.DecodeError != "" {
			b.WriteString("[decode: " + r.DecodeError + "]\n")
		}
		b.WriteString("\n")
	}
	return b.String()
}

func renderBodyOrDecoded(body string, isBase64, truncated bool, decoded json.RawMessage) string {
	if len(decoded) > 0 {
		var buf bytes.Buffer
		if json.Indent(&buf, decoded, "", "  ") == nil {
			return buf.String()
		}
	}
	return renderBody(body, isBase64, truncated)
}

func writeHeaders(b *strings.Builder, prefix string, h map[string][]string) {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, v := range h[k] {
			fmt.Fprintf(b, "%s%s: %s\n", prefix, k, v)
		}
	}
}

func renderBody(body string, isBase64, truncated bool) string {
	if isBase64 {
		n := base64.StdEncoding.DecodedLen(len(body))
		for i := len(body) - 1; i >= 0 && body[i] == '='; i-- {
			n--
		}
		return fmt.Sprintf("<%d bytes binary>", n)
	}
	if body == "" {
		return ""
	}
	out := body
	if t := strings.TrimSpace(body); strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[") {
		var buf bytes.Buffer
		if json.Indent(&buf, []byte(t), "", "  ") == nil {
			out = buf.String()
		}
	}
	if truncated {
		out += "\n[truncated]"
	}
	return out
}
