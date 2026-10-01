package proxy

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tuskira/claude-desktop-utility/internal/ca"
	"github.com/Tuskira/claude-desktop-utility/internal/capture"
)

type harness struct {
	t      *testing.T
	srv    *Server
	pool   *x509.CertPool // trusts the interceptor CA
	out    string
	ln     net.Listener
	proxyU *url.URL
}

func newHarness(t *testing.T, mod func(*Config)) *harness {
	t.Helper()
	dir := t.TempDir()
	if err := ca.Init(dir, false); err != nil {
		t.Fatal(err)
	}
	authority, err := ca.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(authority.Cert)

	out := filepath.Join(dir, "cap.jsonl")
	w, err := capture.OpenWriter(out)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		CA:               authority,
		Logger:           &capture.Logger{Out: w, Stdout: io.Discard},
		MaxBody:          1 << 20,
		InsecureUpstream: true,
		Logf:             t.Logf,
	}
	if mod != nil {
		mod(&cfg)
	}
	srv := New(cfg)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.ServeProxy(ln)
	t.Cleanup(func() { srv.Shutdown(time.Second); w.Close() })
	u, _ := url.Parse("http://" + ln.Addr().String())
	return &harness{t: t, srv: srv, pool: pool, out: out, ln: ln, proxyU: u}
}

func (h *harness) client(roots *x509.CertPool) (*http.Client, *http.Transport) {
	tr := &http.Transport{Proxy: http.ProxyURL(h.proxyU), TLSClientConfig: &tls.Config{RootCAs: roots}}
	return &http.Client{Transport: tr, Timeout: 10 * time.Second}, tr
}

// readAll parses the capture file as it is right now.
func (h *harness) readAll() []capture.Record {
	h.t.Helper()
	data, _ := os.ReadFile(h.out)
	var recs []capture.Record
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var r capture.Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			h.t.Fatalf("bad JSONL line %q: %v", line, err)
		}
		recs = append(recs, r)
	}
	return recs
}

// records waits until the capture file holds at least n records and
// requires exactly n.
func (h *harness) records(n int) []capture.Record {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		recs := h.readAll()
		if len(recs) >= n || time.Now().After(deadline) {
			if len(recs) != n {
				h.t.Fatalf("got %d records, want %d: %+v", len(recs), n, recs)
			}
			return recs
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitFor polls until pred holds for the current records.
func (h *harness) waitFor(what string, pred func([]capture.Record) bool) []capture.Record {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		recs := h.readAll()
		if pred(recs) {
			return recs
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s; records: %+v", what, recs)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func findRec(t *testing.T, recs []capture.Record, url string) capture.Record {
	t.Helper()
	for _, r := range recs {
		if r.URL == url {
			return r
		}
	}
	t.Fatalf("no record for %s in %+v", url, recs)
	return capture.Record{}
}

func TestEndToEndProxyMITM(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc("/json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"hello":"world"}`)
	})
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprint(w, "data: 1\n\n")
		fl.Flush()
		select { // the client releases us after it saw event 1: proves streaming
		case <-release:
		case <-time.After(8 * time.Second):
		}
		for i := 2; i <= 3; i++ {
			time.Sleep(10 * time.Millisecond)
			fmt.Fprintf(w, "data: %d\n\n", i)
			fl.Flush()
		}
	})
	upstream := httptest.NewTLSServer(mux)
	defer upstream.Close()

	h := newHarness(t, func(c *Config) { c.NoStreamRecords = true }) // this test expects one record per exchange
	client, _ := h.client(h.pool)                                    // trusts only the interceptor CA, via IP SAN
	defer once.Do(func() { close(release) })

	resp, err := client.Get(upstream.URL + "/json")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != `{"hello":"world"}` {
		t.Fatalf("json body = %q", body)
	}

	begin := time.Now()
	resp, err = client.Get(upstream.URL + "/sse")
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(resp.Body)
	first, err := br.ReadString('\n')
	if err != nil || first != "data: 1\n" {
		t.Fatalf("first event = %q, %v", first, err)
	}
	if d := time.Since(begin); d > 4*time.Second {
		t.Fatalf("first event took %v: response is being buffered, not streamed", d)
	}
	once.Do(func() { close(release) })
	rest, err := io.ReadAll(br)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if got := first + string(rest); got != "data: 1\n\ndata: 2\n\ndata: 3\n\n" {
		t.Fatalf("sse body = %q", got)
	}

	recs := h.records(2)
	j := findRec(t, recs, upstream.URL+"/json")
	if j.Method != "GET" || j.Status != 200 || j.Mode != "proxy" || j.RespBody != `{"hello":"world"}` || j.RespBodyBase64 {
		t.Errorf("json record wrong: %+v", j)
	}
	s := findRec(t, recs, upstream.URL+"/sse")
	if s.Method != "GET" || s.Status != 200 {
		t.Errorf("sse record wrong: %+v", s)
	}
	for _, ev := range []string{"data: 1", "data: 2", "data: 3"} {
		if !strings.Contains(s.RespBody, ev) {
			t.Errorf("sse record body missing %q: %q", ev, s.RespBody)
		}
	}
	if j.RespHeaders["Content-Type"][0] != "application/json" {
		t.Errorf("resp headers: %v", j.RespHeaders)
	}
}

func TestPassthroughTunnelsRaw(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "pinned")
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)

	h := newHarness(t, func(c *Config) { c.Passthrough = []string{u.Host} }) // host:port pattern
	pool := x509.NewCertPool()
	pool.AddCert(upstream.Certificate()) // NOT the interceptor CA
	client, tr := h.client(pool)

	resp, err := client.Get(upstream.URL + "/x")
	if err != nil {
		t.Fatalf("client should verify the real upstream cert: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "pinned" {
		t.Fatalf("body = %q", body)
	}
	tr.CloseIdleConnections() // ends the tunnel so the record is written

	recs := h.records(1)
	r := recs[0]
	if r.Mode != "passthrough" || r.Method != "CONNECT" || !strings.Contains(r.URL, "127.0.0.1") || r.Status != 200 {
		t.Errorf("passthrough record wrong: %+v", r)
	}
	if r.ReqBytes == 0 || r.RespBytes == 0 {
		t.Errorf("tunnel byte counts not recorded: %+v", r)
	}
}

func TestPlainHTTPForward(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Seen-Proxy-Conn", r.Header.Get("Proxy-Connection"))
		fmt.Fprintf(w, "echo:%s", b)
	}))
	defer upstream.Close()

	h := newHarness(t, nil)
	client, _ := h.client(nil)
	resp, err := client.Post(upstream.URL+"/e?x=1", "text/plain", strings.NewReader("ping"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "echo:ping" {
		t.Fatalf("body = %q", body)
	}
	r := h.records(1)[0]
	if r.URL != upstream.URL+"/e?x=1" || r.Method != "POST" || r.ReqBody != "ping" || r.RespBody != "echo:ping" || r.Status != 200 {
		t.Errorf("record wrong: %+v", r)
	}
}

func TestBodyTruncationStillProxiesEverything(t *testing.T) {
	big := strings.Repeat("a", 5000)
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		io.WriteString(w, big)
	}))
	defer upstream.Close()

	h := newHarness(t, func(c *Config) { c.MaxBody = 100 })
	client, _ := h.client(h.pool)
	resp, err := client.Post(upstream.URL, "text/plain", strings.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if len(body) != 5000 {
		t.Fatalf("client got %d bytes, want 5000", len(body))
	}
	r := h.records(1)[0]
	if !r.ReqBodyTruncated || !r.RespBodyTruncated || len(r.ReqBody) != 100 || len(r.RespBody) != 100 {
		t.Errorf("truncation wrong: req %d/%v resp %d/%v", len(r.ReqBody), r.ReqBodyTruncated, len(r.RespBody), r.RespBodyTruncated)
	}
	if r.ReqBytes != 5000 || r.RespBytes != 5000 {
		t.Errorf("byte totals: %d/%d", r.ReqBytes, r.RespBytes)
	}
}

func TestTransparentTLS(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "host=%s", r.Host)
	}))
	defer upstream.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(upstream.URL, "https://"))

	h := newHarness(t, func(c *Config) { c.TLSPort = port })
	tln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go h.srv.ServeTransparentTLS(tln)

	conn, err := tls.Dial("tcp", tln.Addr().String(), &tls.Config{RootCAs: h.pool, ServerName: "localhost"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET /t HTTP/1.1\r\nHost: localhost:%s\r\nConnection: close\r\n\r\n", port)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "host=localhost:"+port {
		t.Fatalf("resp %d %q", resp.StatusCode, body)
	}
	r := h.records(1)[0]
	if r.Mode != "transparent" || r.URL != "https://localhost:"+port+"/t" || r.Status != 200 {
		t.Errorf("record wrong: %+v", r)
	}
}

func TestTransparentTLSPassthroughReplaysClientHello(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "pinned")
	}))
	defer upstream.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(upstream.URL, "https://"))

	h := newHarness(t, func(c *Config) { c.TLSPort = port; c.Passthrough = []string{"localhost"} })
	tln, _ := net.Listen("tcp", "127.0.0.1:0")
	go h.srv.ServeTransparentTLS(tln)

	pool := x509.NewCertPool()
	pool.AddCert(upstream.Certificate())
	// The httptest cert has no "localhost" SAN; verify as example.com but
	// send SNI localhost by dialing manually.
	conn, err := tls.Dial("tcp", tln.Addr().String(), &tls.Config{RootCAs: pool, ServerName: "localhost", InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("handshake through passthrough failed: %v", err)
	}
	if got := conn.ConnectionState().PeerCertificates[0].Raw; string(got) != string(upstream.Certificate().Raw) {
		t.Fatal("client did not receive the real upstream certificate")
	}
	fmt.Fprint(conn, "GET / HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	conn.Close()
	if string(body) != "pinned" {
		t.Fatalf("body = %q", body)
	}
	r := h.records(1)[0]
	if r.Mode != "passthrough" {
		t.Errorf("record: %+v", r)
	}
}

func TestTransparentHTTP(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "path=%s", r.URL.Path)
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)

	h := newHarness(t, func(c *Config) { c.HTTPPort = u.Port() })
	hln, _ := net.Listen("tcp", "127.0.0.1:0")
	go h.srv.ServeTransparentHTTP(hln)

	conn, err := net.Dial("tcp", hln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET /plain HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "path=/plain" {
		t.Fatalf("body = %q", body)
	}
	r := h.records(1)[0]
	if r.Mode != "transparent-http" || r.URL != "http://127.0.0.1:"+u.Port()+"/plain" {
		t.Errorf("record wrong: %+v", r)
	}
}

func TestUpstreamErrorGives502(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := l.Addr().String()
	l.Close()

	h := newHarness(t, nil)
	client, _ := h.client(h.pool)
	resp, err := client.Get("https://" + dead + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	r := h.records(1)[0]
	if r.Status != 502 || r.Error == "" {
		t.Errorf("record: %+v", r)
	}
}

func TestConnectProtoBodiesAreDecoded(t *testing.T) {
	env := func(flag byte, payload []byte) []byte {
		h := []byte{flag, 0, 0, 0, byte(len(payload))}
		return append(h, payload...)
	}
	msg1 := append([]byte{0x0a, 0x05}, "hello"...)                   // {1:"hello"}
	msg2 := append(append([]byte{0x0a, 0x05}, "world"...), 0x10, 42) // {1:"world",2:42}
	stream := bytes.Join([][]byte{env(0, msg1), env(0, msg2), env(2, []byte(`{"metadata":{}}`))}, nil)

	var gotReq []byte
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotReq, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/connect+proto")
		w.Write(stream)
	}))
	defer upstream.Close()

	h := newHarness(t, func(c *Config) { c.NoStreamRecords = true })
	client, _ := h.client(h.pool)
	reqBody := append([]byte{0x0a, 0x06}, "noodle"...) // {1:"noodle"}
	resp, err := client.Post(upstream.URL+"/rpc", "application/proto", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Equal(body, stream) || !bytes.Equal(gotReq, reqBody) {
		t.Fatal("proto bytes were altered in transit")
	}
	r := h.records(1)[0]
	if got := string(r.ReqBodyDecoded); got != `{"1":"noodle"}` {
		t.Errorf("req_body_decoded = %s", got)
	}
	want := `[{"1":"hello"},{"1":"world","2":42},{"trailer":{"metadata":{}}}]`
	if got := string(r.RespBodyDecoded); got != want {
		t.Errorf("resp_body_decoded = %s\nwant %s", got, want)
	}
	if r.DecodeError != "" || r.RespBody == "" {
		t.Errorf("decode_error=%q, resp_body should be kept", r.DecodeError)
	}
}
