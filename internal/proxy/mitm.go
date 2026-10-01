package proxy

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Tuskira/claude-desktop-utility/internal/capture"
)

var errPassthrough = errors.New("passthrough host")

// session is one client connection carrying HTTP/1.x requests, either plain
// or inside a TLS layer we terminate.
type session struct {
	srv    *Server
	conn   net.Conn // where requests are read from and responses written to
	br     *bufio.Reader
	bw     *bufio.Writer
	client string
	mode   string
	scheme string // upstream scheme: "http" or "https"

	target       string // fixed upstream host:port; "" = from the request
	defPort      string // port to assume when the request names none
	requireAbs   bool   // explicit proxy: plain requests must be absolute-URI
	allowConnect bool
}

func newSession(s *Server, conn net.Conn, client, mode, scheme string) *session {
	return &session{
		srv: s, conn: conn, br: bufio.NewReader(conn), bw: bufio.NewWriterSize(conn, 16<<10),
		client: client, mode: mode, scheme: scheme,
	}
}

// ---- entry points ---------------------------------------------------------

func (s *Server) handleProxyConn(c net.Conn) {
	sess := newSession(s, c, c.RemoteAddr().String(), capture.ModeProxy, "http")
	sess.defPort = "80"
	sess.requireAbs = true
	sess.allowConnect = true
	sess.loop()
}

func (s *Server) handleTransparentHTTP(c net.Conn) {
	sess := newSession(s, c, c.RemoteAddr().String(), capture.ModeTransparentHTTP, "http")
	sess.defPort = s.cfg.HTTPPort
	sess.loop()
}

// recordConn remembers bytes read from the client so a ClientHello can be
// replayed upstream when the host turns out to be a passthrough host.
type recordConn struct {
	net.Conn
	recording bool
	muted     bool
	data      []byte
}

func (r *recordConn) Read(p []byte) (int, error) {
	n, err := r.Conn.Read(p)
	if r.recording && n > 0 {
		r.data = append(r.data, p[:n]...)
	}
	return n, err
}

func (r *recordConn) Write(p []byte) (int, error) {
	if r.muted {
		return len(p), nil
	}
	return r.Conn.Write(p)
}

func (s *Server) handleTransparentTLS(c net.Conn) {
	client := c.RemoteAddr().String()
	rc := &recordConn{Conn: c, recording: true}
	var sni string
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"},
		GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
			sni = strings.ToLower(strings.TrimSuffix(h.ServerName, "."))
			if sni != "" && s.isPassthrough(sni) {
				rc.muted = true // do not send a TLS alert to the client
				return nil, errPassthrough
			}
			return nil, nil
		},
		GetCertificate: func(h *tls.ClientHelloInfo) (*tls.Certificate, error) {
			if sni == "" {
				return nil, errors.New("no SNI in client hello")
			}
			return s.cfg.CA.LeafFor(sni)
		},
	}
	tc := tls.Server(rc, cfg)
	c.SetDeadline(time.Now().Add(handshakeTimeout))
	err := tc.Handshake()
	c.SetDeadline(time.Time{})
	if err != nil {
		switch {
		case errors.Is(err, errPassthrough):
			s.tunnel(c, c, client, net.JoinHostPort(sni, s.cfg.TLSPort), rc.data, false)
		case sni == "":
			s.logf("transparent: %s sent no SNI, closing", client)
		default:
			s.logf("transparent: TLS handshake with %s for %s failed: %v (is the CA trusted by this app?)", client, sni, err)
		}
		return
	}
	rc.recording, rc.data = false, nil
	host := tc.ConnectionState().ServerName
	sess := newSession(s, tc, client, capture.ModeTransparent, "https")
	sess.target = net.JoinHostPort(host, s.cfg.TLSPort)
	sess.loop()
}

// handleConnect serves a CONNECT request: either a blind tunnel for
// passthrough hosts, or a MITM session.
func (s *Server) handleConnect(sess *session, req *http.Request) {
	c := sess.conn
	authority := req.Host
	if authority == "" {
		authority = req.URL.Host
	}
	target := ensurePort(authority, "443")
	if s.isPassthrough(target) {
		s.tunnel(c, sess.br, sess.client, target, nil, true)
		return
	}
	if _, err := io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	// Peek at the first byte: TLS handshakes start with 0x16. Anything else
	// (or a server-speaks-first protocol) is tunneled untouched.
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	b, err := sess.br.Peek(1)
	c.SetReadDeadline(time.Time{})
	if err != nil {
		if isTimeout(err) {
			s.tunnel(c, sess.br, sess.client, target, nil, false)
		}
		return
	}
	if b[0] != 0x16 {
		s.tunnel(c, sess.br, sess.client, target, nil, false)
		return
	}
	tc := tls.Server(&bufConn{Conn: c, r: sess.br}, s.tlsConfig(hostOnly(target)))
	c.SetDeadline(time.Now().Add(handshakeTimeout))
	err = tc.Handshake()
	c.SetDeadline(time.Time{})
	if err != nil {
		s.logf("proxy: TLS handshake with %s for %s failed: %v (is the CA trusted by this app?)", sess.client, target, err)
		return
	}
	sub := newSession(s, tc, sess.client, capture.ModeProxy, "https")
	sub.target = target
	sub.loop()
}

// tlsConfig builds the client-facing TLS config. fallback is used when the
// client sends no SNI (e.g. it connected to an IP address).
func (s *Server) tlsConfig(fallback string) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"}, // never negotiate h2 with the client
		GetCertificate: func(h *tls.ClientHelloInfo) (*tls.Certificate, error) {
			name := h.ServerName
			if name == "" {
				name = fallback
			}
			if name == "" {
				return nil, errors.New("no SNI in client hello")
			}
			return s.cfg.CA.LeafFor(name)
		},
	}
}

// ---- request loop ---------------------------------------------------------

func (sess *session) loop() {
	for {
		sess.conn.SetReadDeadline(time.Now().Add(idleTimeout))
		req, err := http.ReadRequest(sess.br)
		if err != nil {
			if !quietErr(err) {
				sess.srv.logf("%s: reading request: %v", sess.client, err)
			}
			return
		}
		sess.conn.SetReadDeadline(time.Time{})
		if sess.allowConnect && req.Method == http.MethodConnect {
			sess.srv.handleConnect(sess, req)
			return
		}
		if !sess.handle(req) {
			return
		}
	}
}

func (sess *session) targetFor(req *http.Request) (string, error) {
	if sess.target != "" {
		return sess.target, nil
	}
	h := req.URL.Host
	if h == "" {
		h = req.Host
	}
	if h == "" {
		return "", errors.New("no host in request")
	}
	return ensurePort(h, sess.defPort), nil
}

func (sess *session) writeSimple(code int, msg string) {
	body := msg + "\n"
	fmt.Fprintf(sess.bw, "HTTP/1.1 %d %s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		code, http.StatusText(code), len(body), body)
	sess.bw.Flush()
}

// teeBody passes reads through while copying them into a capture buffer.
type teeBody struct {
	r      io.Reader
	c      io.Closer
	before func() // called before every Read (used to flush pending output)
	eof    atomic.Bool
	err    error // only inspected by the goroutine that reads the body
}

func (t *teeBody) Read(p []byte) (int, error) {
	if t.before != nil {
		t.before()
	}
	n, err := t.r.Read(p)
	if err == io.EOF {
		t.eof.Store(true)
	} else if err != nil {
		t.err = err
	}
	return n, err
}

func (t *teeBody) Close() error { return t.c.Close() }

// handle forwards one request and streams the response back. It returns
// whether the client connection can be reused for another request.
func (sess *session) handle(req *http.Request) bool {
	s := sess.srv
	start := time.Now()
	if sess.requireAbs && req.URL.Host == "" {
		sess.writeSimple(http.StatusBadRequest, "interceptor: this is an HTTP proxy; send absolute-URI requests or use CONNECT")
		return false
	}
	hostport, err := sess.targetFor(req)
	if err != nil {
		sess.writeSimple(http.StatusBadRequest, "interceptor: "+err.Error())
		return false
	}
	req.URL.Scheme = sess.scheme
	req.URL.Host = hostport
	req.RequestURI = ""

	rec := &capture.Record{
		TS: start, Mode: sess.mode, Client: sess.client, Method: req.Method,
		URL:        sess.scheme + "://" + displayHost(sess.scheme, hostport) + req.URL.RequestURI(),
		ReqHeaders: headerMap(req.Header, req.Host),
	}
	reqEnc := req.Header.Get("Content-Encoding")
	ws := isWebSocket(req)
	prepareRequest(req)
	if strings.EqualFold(req.Header.Get("Expect"), "100-continue") {
		req.Header.Del("Expect")
		sess.bw.WriteString("HTTP/1.1 100 Continue\r\n\r\n")
		sess.bw.Flush()
	}

	reqCap := capture.NewBuffer(s.cfg.MaxBody)
	var reqBody *teeBody
	if req.Body != nil && req.Body != http.NoBody {
		reqBody = &teeBody{r: io.TeeReader(req.Body, reqCap), c: req.Body}
		req.Body = reqBody
	}
	req = req.WithContext(s.ctx)
	reqCT := req.Header.Get("Content-Type")
	fillReq := func() {
		raw := reqCap.Bytes()
		rec.ReqBody, rec.ReqBodyBase64, rec.ReqBodyTruncated = capture.EncodeBody(raw, reqEnc, reqCap.Truncated(), s.cfg.MaxBody)
		rec.ReqBytes = reqCap.Total()
		dec, derr := capture.DecodeProto(reqCT, reqEnc, raw, reqCap.Truncated(), s.cfg.MaxBody)
		rec.ReqBodyDecoded = dec
		rec.AddDecodeError("req", derr)
	}

	var (
		resp   *http.Response
		upConn net.Conn
		upR    *bufio.Reader
	)
	if ws {
		resp, upConn, upR, err = s.wsHandshake(req, sess.scheme)
	} else {
		resp, err = s.transport.RoundTrip(req)
	}
	if err != nil {
		rec.Status, rec.Error = http.StatusBadGateway, err.Error()
		fillReq()
		rec.DurationMS = time.Since(start).Milliseconds()
		s.cfg.Logger.Log(rec)
		sess.writeSimple(http.StatusBadGateway, "interceptor: upstream error: "+err.Error())
		return false
	}
	rec.Status = resp.StatusCode
	rec.RespHeaders = headerMap(resp.Header, "")

	if ws && resp.StatusCode == http.StatusSwitchingProtocols {
		fillReq()
		return sess.tunnelWebSocket(rec, resp, upConn, upR, start)
	}
	if upConn != nil { // websocket attempt that was refused: plain response
		defer func() { s.untrack(upConn); upConn.Close() }()
	}

	prepareResponse(resp, req.Method)
	respCap := capture.NewBuffer(s.cfg.MaxBody)
	var respSink io.Writer = respCap
	var tap *streamTap
	if !s.cfg.NoStreamRecords {
		if kind := streamKindOf(resp); kind != streamNone {
			tap = newStreamTap(s, kind, rec, resp.StatusCode)
			respSink = io.MultiWriter(respCap, tap)
		}
	}
	tb := &teeBody{
		r:      io.TeeReader(resp.Body, respSink),
		c:      resp.Body,
		before: func() { _ = sess.bw.Flush() }, // push out headers/chunks before blocking on upstream
	}
	resp.Body = tb
	// Hide bufio.Writer's ReadFrom: the before-read flush hook must not run
	// while bufio is filling its own buffer.
	werr := resp.Write(struct{ io.Writer }{sess.bw})
	if ferr := sess.bw.Flush(); werr == nil {
		werr = ferr
	}
	tb.Close()
	if tap != nil {
		rec.StreamUnits = tap.Close() // waits until all unit records are written
	}

	fillReq()
	respRaw, respEnc := respCap.Bytes(), resp.Header.Get("Content-Encoding")
	rec.RespBody, rec.RespBodyBase64, rec.RespBodyTruncated = capture.EncodeBody(respRaw, respEnc, respCap.Truncated(), s.cfg.MaxBody)
	rec.RespBytes = respCap.Total()
	dec, derr := capture.DecodeProto(resp.Header.Get("Content-Type"), respEnc, respRaw, respCap.Truncated(), s.cfg.MaxBody)
	rec.RespBodyDecoded = dec
	rec.AddDecodeError("resp", derr)
	switch {
	case tb.err != nil:
		rec.Error = "upstream body: " + tb.err.Error()
	case werr != nil:
		rec.Error = "client write: " + werr.Error()
	}
	rec.DurationMS = time.Since(start).Milliseconds()
	s.cfg.Logger.Log(rec)

	if werr != nil || tb.err != nil || req.Close || resp.Close {
		return false
	}
	// If the upstream answered before reading the whole request body, the
	// client stream is out of sync: do not reuse it.
	return reqBody == nil || reqBody.eof.Load()
}

// wsHandshake dials upstream directly, sends the upgrade request and reads
// the first response. The caller owns the returned connection.
func (s *Server) wsHandshake(req *http.Request, scheme string) (*http.Response, net.Conn, *bufio.Reader, error) {
	hostport := req.URL.Host
	d := &net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}
	var (
		up  net.Conn
		err error
	)
	if scheme == "https" {
		td := &tls.Dialer{NetDialer: d, Config: &tls.Config{
			ServerName:         hostOnly(hostport),
			InsecureSkipVerify: s.cfg.InsecureUpstream,
			MinVersion:         tls.VersionTLS12,
			NextProtos:         []string{"http/1.1"},
		}}
		up, err = td.DialContext(s.ctx, "tcp", hostport)
	} else {
		up, err = d.DialContext(s.ctx, "tcp", hostport)
	}
	if err != nil {
		return nil, nil, nil, err
	}
	s.track(up)
	fail := func(err error) (*http.Response, net.Conn, *bufio.Reader, error) {
		s.untrack(up)
		up.Close()
		return nil, nil, nil, err
	}
	up.SetDeadline(time.Now().Add(30 * time.Second))
	if err := req.Write(up); err != nil {
		return fail(err)
	}
	ur := bufio.NewReader(up)
	resp, err := http.ReadResponse(ur, req)
	if err != nil {
		return fail(err)
	}
	up.SetDeadline(time.Time{})
	return resp, up, ur, nil
}

// tunnelWebSocket forwards the 101 response, logs the handshake, then relays
// raw bytes both ways until either side closes.
func (sess *session) tunnelWebSocket(rec *capture.Record, resp *http.Response, up net.Conn, upR *bufio.Reader, start time.Time) bool {
	s := sess.srv
	defer func() { s.untrack(up); up.Close() }()
	fmt.Fprintf(sess.bw, "HTTP/1.1 %s\r\n", resp.Status)
	resp.Header.Write(sess.bw)
	sess.bw.WriteString("\r\n")
	if err := sess.bw.Flush(); err != nil {
		rec.Error = "client write: " + err.Error()
	}
	rec.DurationMS = time.Since(start).Milliseconds()
	s.cfg.Logger.Log(rec)
	if rec.Error != "" {
		return false
	}
	deflate, clientNoCtx, serverNoCtx := parseWSExtensions(resp.Header)
	c2s := newWSTap(s.cfg.Logger, sess.client, rec.URL, "WS>", s.cfg.MaxBody, deflate, clientNoCtx)
	s2c := newWSTap(s.cfg.Logger, sess.client, rec.URL, "WS<", s.cfg.MaxBody, deflate, serverNoCtx)
	// The taps only observe the bytes being relayed; they never alter them.
	c2u, u2c := biCopy(sess.conn, io.TeeReader(sess.br, c2s), up, io.TeeReader(upR, s2c))
	s.cfg.Logger.Log(&capture.Record{
		TS: time.Now(), Mode: capture.ModeWebSocket, Client: sess.client, Method: "WS-CLOSE",
		URL: rec.URL, Status: http.StatusSwitchingProtocols, DurationMS: time.Since(start).Milliseconds(),
		ReqBytes: c2u, RespBytes: u2c,
	})
	return false
}

// ---- helpers --------------------------------------------------------------

func headerMap(h http.Header, host string) map[string][]string {
	m := make(map[string][]string, len(h)+1)
	for k, v := range h {
		m[k] = append([]string(nil), v...)
	}
	if host != "" {
		if _, ok := m["Host"]; !ok {
			m["Host"] = []string{host}
		}
	}
	return m
}

// prepareRequest strips proxy-only headers and drops encodings we do not
// decode for display. Everything else is forwarded unchanged.
func prepareRequest(req *http.Request) {
	req.Header.Del("Proxy-Connection")
	req.Header.Del("Proxy-Authorization")
	filterAcceptEncoding(req.Header)
	// Do not let net/http inject its own User-Agent when the client sent none.
	if _, ok := req.Header["User-Agent"]; !ok {
		req.Header["User-Agent"] = nil
	}
}

func filterAcceptEncoding(h http.Header) {
	vals := h.Values("Accept-Encoding")
	if len(vals) == 0 {
		return
	}
	var keep []string
	for _, v := range vals {
		for _, part := range strings.Split(v, ",") {
			p := strings.TrimSpace(part)
			if p == "" {
				continue
			}
			name := strings.ToLower(strings.TrimSpace(strings.SplitN(p, ";", 2)[0]))
			if name == "br" || name == "zstd" {
				continue
			}
			keep = append(keep, p)
		}
	}
	if len(keep) == 0 {
		h.Set("Accept-Encoding", "identity")
		return
	}
	h.Set("Accept-Encoding", strings.Join(keep, ", "))
}

// prepareResponse makes an upstream response (possibly HTTP/2) writable to
// an HTTP/1.1 client while keeping the connection reusable.
func prepareResponse(resp *http.Response, method string) {
	resp.Proto, resp.ProtoMajor, resp.ProtoMinor = "HTTP/1.1", 1, 1
	if resp.ContentLength == -1 && len(resp.TransferEncoding) == 0 && !resp.Close &&
		method != http.MethodHead && bodyAllowed(resp.StatusCode) {
		resp.TransferEncoding = []string{"chunked"}
	}
}

func bodyAllowed(status int) bool {
	return !(status >= 100 && status < 200) && status != http.StatusNoContent && status != http.StatusNotModified
}

func isWebSocket(req *http.Request) bool {
	return strings.EqualFold(strings.TrimSpace(req.Header.Get("Upgrade")), "websocket") &&
		headerHasToken(req.Header, "Connection", "upgrade")
}

func headerHasToken(h http.Header, key, token string) bool {
	for _, v := range h.Values(key) {
		for _, p := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(p), token) {
				return true
			}
		}
	}
	return false
}

// ensurePort appends def when hostport has no port.
func ensurePort(hostport, def string) string {
	if _, _, err := net.SplitHostPort(hostport); err == nil {
		return hostport
	}
	return net.JoinHostPort(strings.TrimSuffix(strings.TrimPrefix(hostport, "["), "]"), def)
}

func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

// displayHost renders host:port without the scheme's default port.
func displayHost(scheme, hostport string) string {
	h, p, err := net.SplitHostPort(hostport)
	if err != nil {
		return hostport
	}
	def := "80"
	if scheme == "https" {
		def = "443"
	}
	if strings.Contains(h, ":") {
		h = "[" + h + "]"
	}
	if p == def {
		return h
	}
	return h + ":" + p
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func quietErr(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) ||
		isTimeout(err) || strings.Contains(err.Error(), "connection reset")
}
