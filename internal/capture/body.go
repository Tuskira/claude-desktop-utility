package capture

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"path"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// Buffer is an io.Writer that keeps at most max bytes but counts everything
// written. It is safe for concurrent use.
type Buffer struct {
	mu    sync.Mutex
	max   int64
	buf   bytes.Buffer
	total int64
}

// NewBuffer returns a Buffer that keeps up to max bytes.
func NewBuffer(max int64) *Buffer { return &Buffer{max: max} }

// Write implements io.Writer. It never fails and never short-writes.
func (b *Buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	b.total += int64(n)
	if room := b.max - int64(b.buf.Len()); room > 0 {
		if int64(len(p)) > room {
			p = p[:room]
		}
		b.buf.Write(p)
	}
	return n, nil
}

// Bytes returns a copy of the captured bytes.
func (b *Buffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}

// Total is the number of bytes written, including discarded ones.
func (b *Buffer) Total() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.total
}

// Truncated reports whether some written bytes were discarded.
func (b *Buffer) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.total > int64(b.buf.Len())
}

// EncodeBody turns captured bytes into the string stored in the log.
// A gzip Content-Encoding is decoded (at most limit decoded bytes are kept).
// Bodies that are not valid UTF-8 are base64 encoded. truncated says whether
// raw was already cut short; the returned flag also covers decode limits.
func EncodeBody(raw []byte, contentEncoding string, truncated bool, limit int64) (body string, isBase64, isTruncated bool) {
	isTruncated = truncated
	data := raw
	if strings.EqualFold(strings.TrimSpace(contentEncoding), "gzip") && len(raw) > 0 {
		if dec, more, ok := gunzip(raw, limit); ok {
			data = dec
			if more {
				isTruncated = true
			}
		}
	}
	if isTruncated {
		data = trimPartialRune(data)
	}
	if utf8.Valid(data) {
		return string(data), false, isTruncated
	}
	return base64.StdEncoding.EncodeToString(data), true, isTruncated
}

func gunzip(raw []byte, limit int64) (out []byte, more, ok bool) {
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, false, false
	}
	out, err = io.ReadAll(io.LimitReader(zr, limit+1))
	if int64(len(out)) > limit {
		out, more = out[:limit], true
	}
	if err != nil && len(out) == 0 {
		return nil, false, false
	}
	return out, more, true
}

// trimPartialRune drops up to 3 trailing bytes if that turns a cut-off
// UTF-8 sequence into valid text.
func trimPartialRune(b []byte) []byte {
	if utf8.Valid(b) {
		return b
	}
	for k := 1; k <= 3 && k <= len(b); k++ {
		if utf8.Valid(b[:len(b)-k]) {
			return b[:len(b)-k]
		}
	}
	return b
}

// ParseSize parses "512KB", "2MB", "1GB" or a plain byte count.
func ParseSize(s string) (int64, error) {
	t := strings.ToUpper(strings.TrimSpace(s))
	if t == "" {
		return 0, fmt.Errorf("empty size")
	}
	mult := int64(1)
	for _, u := range []struct {
		suffix string
		mult   int64
	}{{"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}, {"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}, {"B", 1}} {
		if strings.HasSuffix(t, u.suffix) {
			mult = u.mult
			t = strings.TrimSpace(strings.TrimSuffix(t, u.suffix))
			break
		}
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return int64(f * float64(mult)), nil
}

// MatchHost reports whether hostport ("host" or "host:port") matches any glob
// pattern. Patterns are tried against the bare host and the full host:port.
func MatchHost(patterns []string, hostport string) bool {
	if len(patterns) == 0 {
		return false
	}
	hp := strings.ToLower(hostport)
	h := hp
	if hh, _, err := net.SplitHostPort(hp); err == nil {
		h = hh
	}
	h = strings.TrimSuffix(h, ".")
	for _, p := range patterns {
		p = strings.ToLower(strings.TrimSpace(p))
		if ok, _ := path.Match(p, h); ok {
			return true
		}
		if ok, _ := path.Match(p, hp); ok {
			return true
		}
	}
	return false
}
