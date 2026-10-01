package proxy

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tuskira/claude-desktop-utility/internal/capture"
)

func bodies(us []streamUnit) []string {
	var out []string
	for _, u := range us {
		out = append(out, string(u.Body))
	}
	return out
}

func TestSSESplitter(t *testing.T) {
	s := &sseSplitter{}
	var got []streamUnit
	feed := func(p string) {
		us, err := s.Feed([]byte(p))
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, us...)
	}
	// Events split at awkward places, CRLF endings, comments and keep-alives.
	feed("event: msg\nda")
	feed("ta: {\"a\":1}\n")
	if len(got) != 0 {
		t.Fatalf("event emitted before its blank line: %v", bodies(got))
	}
	feed("\n: heartbeat\n\n") // comment-only event: dropped
	feed("data: two\r\ndata: lines\r\n\r")
	feed("\ndata: third\n\n")
	want := []string{"event: msg\ndata: {\"a\":1}", "data: two\ndata: lines", "data: third"}
	if g := bodies(got); strings.Join(g, "|") != strings.Join(want, "|") {
		t.Fatalf("events = %q, want %q", g, want)
	}
	if string(got[0].Data) != `{"a":1}` || string(got[1].Data) != "two\nlines" {
		t.Errorf("data payloads: %q %q", got[0].Data, got[1].Data)
	}
	// A trailing event without the final blank line is emitted on Flush.
	feed("data: tail")
	if fl := s.Flush(); len(fl) != 1 || string(fl[0].Body) != "data: tail" {
		t.Errorf("flush = %v", bodies(fl))
	}
}

func TestEnvelopeSplitterHeaderSplitAcrossWrites(t *testing.T) {
	e1 := append([]byte{0, 0, 0, 0, 3}, "abc"...)
	e2 := append([]byte{2, 0, 0, 0, 2}, "{}"...)
	all := append(append([]byte(nil), e1...), e2...)
	for cut := 1; cut < len(all); cut++ { // every possible split point
		s := &envSplitter{}
		a, err1 := s.Feed(all[:cut])
		b, err2 := s.Feed(all[cut:])
		if err1 != nil || err2 != nil {
			t.Fatalf("cut %d: %v %v", cut, err1, err2)
		}
		us := append(a, b...)
		if len(us) != 2 || string(us[0].Body) != "abc" || us[0].Flag != 0 || string(us[1].Body) != "{}" || us[1].Flag != 2 {
			t.Fatalf("cut %d: %+v", cut, us)
		}
	}
	// Absurd length: error, no unbounded buffering.
	if _, err := (&envSplitter{}).Feed([]byte{0, 0xff, 0xff, 0xff, 0xff}); err == nil {
		t.Error("expected an error for a huge envelope")
	}
}

func TestLineSplitter(t *testing.T) {
	s := &lineSplitter{}
	a, _ := s.Feed([]byte("{\"a\":1}\r\n\n{\"b\""))
	b, _ := s.Feed([]byte(":2}\n{\"c\":3}"))
	fl := s.Flush()
	got := strings.Join(append(append(bodies(a), bodies(b)...), bodies(fl)...), "|")
	if got != `{"a":1}|{"b":2}|{"c":3}` {
		t.Errorf("lines = %s", got)
	}
}

func streamRecs(recs []capture.Record) (units []capture.Record, final *capture.Record) {
	for i, r := range recs {
		if r.Mode == "stream" {
			units = append(units, r)
		} else if r.StreamUnits > 0 || r.Mode == "proxy" {
			final = &recs[i]
		}
	}
	return
}

func TestSSEUnitsAreLoggedBeforeStreamEnds(t *testing.T) {
	gate := make(chan struct{})
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprint(w, "event: a\ndata: {\"n\":1}\n\n")
		fl.Flush()
		time.Sleep(50 * time.Millisecond)
		fmt.Fprint(w, "data: plain two\n\n")
		fl.Flush()
		select { // held until the test has looked at the file
		case <-gate:
		case <-time.After(8 * time.Second):
		}
		fmt.Fprint(w, ": keepalive\n\ndata: three\n\n")
		fl.Flush()
	}))
	defer upstream.Close()

	h := newHarness(t, nil)
	client, _ := h.client(h.pool)
	resp, err := client.Get(upstream.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	var got strings.Builder
	for i := 0; i < 2; i++ { // read events 1 and 2 (each ends with a blank line)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			got.WriteString(line)
			if line == "\n" {
				break
			}
		}
	}

	// The stream is still open (gate closed): unit records must already exist.
	recs := h.waitFor("2 SSE< records", func(r []capture.Record) bool { u, _ := streamRecs(r); return len(u) >= 2 })
	units, final := streamRecs(recs)
	if final != nil {
		t.Fatal("final record written before the stream ended")
	}
	if units[0].Method != "SSE<" || units[0].Seq != 1 || units[1].Seq != 2 || units[0].Mode != "stream" {
		t.Errorf("unit records: %+v", units)
	}
	if units[0].RespBody != "event: a\ndata: {\"n\":1}" || string(units[0].RespBodyDecoded) != `{"n":1}` {
		t.Errorf("unit 1: body=%q decoded=%s", units[0].RespBody, units[0].RespBodyDecoded)
	}
	if units[1].RespBody != "data: plain two" || units[1].RespBodyDecoded != nil {
		t.Errorf("unit 2: %+v", units[1])
	}
	if len(units[0].ReqHeaders) != 0 || len(units[0].RespHeaders) != 0 || units[0].Status != 200 {
		t.Errorf("unit records should carry no headers: %+v", units[0])
	}
	if units[1].DurationMS < 30 {
		t.Errorf("duration_ms should be time since request start, got %d", units[1].DurationMS)
	}

	close(gate)
	rest, _ := io.ReadAll(br)
	if !strings.Contains(string(rest), "data: three") {
		t.Fatalf("rest = %q", rest)
	}
	recs = h.waitFor("final record", func(r []capture.Record) bool { _, f := streamRecs(r); return f != nil })
	units, final = streamRecs(recs)
	if len(units) != 3 || units[2].RespBody != "data: three" || units[2].Seq != 3 {
		t.Errorf("units: %+v", units)
	}
	if final.StreamUnits != 3 || final.Status != 200 || !strings.Contains(final.RespBody, "data: three") || len(final.RespHeaders) == 0 {
		t.Errorf("final record: %+v", final)
	}
	// The final record is written after every unit record.
	if recs[len(recs)-1].Mode != "proxy" {
		t.Error("final record should come last")
	}
}

func TestConnectRPCUnitsAreDecoded(t *testing.T) {
	env := func(flag byte, payload []byte) []byte {
		return append([]byte{flag, 0, 0, 0, byte(len(payload))}, payload...)
	}
	m1 := append([]byte{0x0a, 0x05}, "hello"...)
	m2 := append(append([]byte{0x0a, 0x05}, "world"...), 0x10, 42)
	parts := [][]byte{env(0, m1), env(0, m2), env(2, []byte(`{"metadata":{}}`))}

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/connect+proto")
		fl := w.(http.Flusher)
		for _, p := range parts {
			// Split each envelope mid-header to exercise reassembly.
			w.Write(p[:2])
			fl.Flush()
			time.Sleep(20 * time.Millisecond)
			w.Write(p[2:])
			fl.Flush()
			time.Sleep(20 * time.Millisecond)
		}
	}))
	defer upstream.Close()

	h := newHarness(t, nil)
	client, _ := h.client(h.pool)
	resp, err := client.Post(upstream.URL+"/rpc", "application/connect+proto", bytes.NewReader(env(0, nil)))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Equal(body, bytes.Join(parts, nil)) {
		t.Fatal("stream altered in transit")
	}
	recs := h.waitFor("final record", func(r []capture.Record) bool { _, f := streamRecs(r); return f != nil })
	units, final := streamRecs(recs)
	want := []string{`{"1":"hello"}`, `{"1":"world","2":42}`, `{"trailer":{"metadata":{}}}`}
	if len(units) != 3 {
		t.Fatalf("units: %+v", units)
	}
	for i, u := range units {
		if u.Method != "RPC<" || u.Seq != i+1 || string(u.RespBodyDecoded) != want[i] || !u.RespBodyBase64 {
			t.Errorf("unit %d: %+v (decoded %s)", i, u, u.RespBodyDecoded)
		}
	}
	if b, _ := base64.StdEncoding.DecodeString(units[0].RespBody); !bytes.Equal(b, m1) {
		t.Errorf("unit 1 body should be base64 of the payload: %q", units[0].RespBody)
	}
	if final.StreamUnits != 3 || string(final.RespBodyDecoded) != "["+strings.Join(want, ",")+"]" {
		t.Errorf("final: units=%d decoded=%s", final.StreamUnits, final.RespBodyDecoded)
	}
}

func TestShutdownMidStreamKeepsUnitsAndWritesFinalWithError(t *testing.T) {
	var once sync.Once
	sent := make(chan struct{})
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprint(w, "data: one\n\ndata: two\n\n")
		fl.Flush()
		once.Do(func() { close(sent) })
		<-r.Context().Done() // never ends on its own
	}))
	defer upstream.Close()

	h := newHarness(t, nil)
	client, _ := h.client(h.pool)
	resp, err := client.Get(upstream.URL + "/forever")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	for i := 0; i < 4; i++ { // two events: "data: x", blank, "data: y", blank
		if _, err := br.ReadString('\n'); err != nil {
			t.Fatal(err)
		}
	}
	<-sent
	h.waitFor("unit records", func(r []capture.Record) bool { u, _ := streamRecs(r); return len(u) == 2 })

	h.srv.Shutdown(100 * time.Millisecond) // cancels the in-flight stream

	recs := h.waitFor("final record", func(r []capture.Record) bool { _, f := streamRecs(r); return f != nil })
	units, final := streamRecs(recs)
	if len(units) != 2 || units[0].RespBody != "data: one" || units[1].RespBody != "data: two" {
		t.Errorf("units: %+v", units)
	}
	if final.Error == "" || final.StreamUnits != 2 || !strings.Contains(final.RespBody, "data: two") {
		t.Errorf("final record should carry the error and the partial body: %+v", final)
	}
}
