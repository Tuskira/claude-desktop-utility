// Package proxy implements a TLS-intercepting proxy that only observes
// traffic: an explicit HTTP proxy (with CONNECT), and transparent TLS/HTTP
// listeners for redirected connections.
package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime/debug"
	"sync"
	"time"

	"github.com/Tuskira/claude-desktop-utility/internal/ca"
	"github.com/Tuskira/claude-desktop-utility/internal/capture"
)

const (
	dialTimeout      = 15 * time.Second
	handshakeTimeout = 15 * time.Second
	idleTimeout      = 5 * time.Minute
)

// Config configures a Server.
type Config struct {
	CA               *ca.CA
	Logger           *capture.Logger
	MaxBody          int64    // captured bytes per message
	Passthrough      []string // host globs to tunnel without interception
	InsecureUpstream bool     // skip upstream certificate verification
	NoStreamRecords  bool     // disable per-unit records for streaming responses
	Logf             func(format string, args ...any)

	// Upstream ports for transparent modes (defaults 443 and 80).
	TLSPort  string
	HTTPPort string
}

// Server is the interceptor. Use ServeProxy, ServeTransparentTLS and
// ServeTransparentHTTP on listeners, and Shutdown to stop.
type Server struct {
	cfg       Config
	transport *http.Transport
	ctx       context.Context
	cancel    context.CancelFunc

	wg        sync.WaitGroup
	mu        sync.Mutex
	closing   bool
	listeners map[net.Listener]struct{}
	conns     map[net.Conn]struct{}
}

// New creates a Server.
func New(cfg Config) *Server {
	if cfg.TLSPort == "" {
		cfg.TLSPort = "443"
	}
	if cfg.HTTPPort == "" {
		cfg.HTTPPort = "80"
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		cfg:    cfg,
		ctx:    ctx,
		cancel: cancel,
		transport: &http.Transport{
			Proxy:               nil, // never chain to another proxy
			DisableCompression:  true,
			ForceAttemptHTTP2:   true,
			DialContext:         (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext,
			TLSClientConfig:     &tls.Config{InsecureSkipVerify: cfg.InsecureUpstream, MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout: handshakeTimeout,
			MaxIdleConnsPerHost: 16,
			IdleConnTimeout:     90 * time.Second,
		},
		listeners: map[net.Listener]struct{}{},
		conns:     map[net.Conn]struct{}{},
	}
}

func (s *Server) logf(format string, args ...any) {
	if s.cfg.Logf != nil {
		s.cfg.Logf(format, args...)
	}
}

// ServeProxy serves the explicit HTTP proxy on l until l is closed.
func (s *Server) ServeProxy(l net.Listener) error { return s.serve(l, s.handleProxyConn) }

// ServeTransparentTLS serves redirected raw TLS connections on l.
func (s *Server) ServeTransparentTLS(l net.Listener) error {
	return s.serve(l, s.handleTransparentTLS)
}

// ServeTransparentHTTP serves redirected raw plain-HTTP connections on l.
func (s *Server) ServeTransparentHTTP(l net.Listener) error {
	return s.serve(l, s.handleTransparentHTTP)
}

func (s *Server) serve(l net.Listener, handle func(net.Conn)) error {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		l.Close()
		return nil
	}
	s.listeners[l] = struct{}{}
	s.mu.Unlock()

	for {
		c, err := l.Accept()
		if err != nil {
			s.mu.Lock()
			closing := s.closing
			s.mu.Unlock()
			if closing || errors.Is(err, net.ErrClosed) {
				return nil
			}
			s.logf("accept on %s: %v", l.Addr(), err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		s.mu.Lock()
		if s.closing {
			s.mu.Unlock()
			c.Close()
			return nil
		}
		s.conns[c] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()
		go func() {
			defer s.wg.Done()
			defer s.untrack(c)
			defer c.Close()
			defer func() {
				if r := recover(); r != nil {
					s.logf("panic handling %s: %v\n%s", c.RemoteAddr(), r, debug.Stack())
				}
			}()
			handle(c)
		}()
	}
}

// track registers an extra connection (e.g. upstream) for forced shutdown.
func (s *Server) track(c net.Conn) {
	s.mu.Lock()
	s.conns[c] = struct{}{}
	s.mu.Unlock()
}

func (s *Server) untrack(c net.Conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
}

// Shutdown stops accepting, waits up to grace for connections to finish,
// then cancels in-flight work and closes what is left.
func (s *Server) Shutdown(grace time.Duration) {
	s.mu.Lock()
	s.closing = true
	for l := range s.listeners {
		l.Close()
	}
	s.mu.Unlock()

	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(grace):
		s.cancel()
		s.mu.Lock()
		for c := range s.conns {
			c.Close()
		}
		s.mu.Unlock()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}
	s.cancel()
	s.transport.CloseIdleConnections()
}

func (s *Server) dial(addr string) (net.Conn, error) {
	d := net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}
	c, err := d.DialContext(s.ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	s.track(c)
	return c, nil
}

func (s *Server) isPassthrough(hostport string) bool {
	return capture.MatchHost(s.cfg.Passthrough, hostport)
}

// bufConn lets a TLS server read bytes already buffered in r.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufConn) Read(p []byte) (int, error) { return b.r.Read(p) }

func halfClose(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}

// biCopy copies both directions and returns bytes moved client-to-upstream
// and upstream-to-client. It returns when both directions have ended.
func biCopy(client net.Conn, clientR io.Reader, up net.Conn, upR io.Reader) (c2u, u2c int64) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		c2u, _ = io.Copy(up, clientR)
		halfClose(up)
	}()
	go func() {
		defer wg.Done()
		u2c, _ = io.Copy(client, upR)
		halfClose(client)
	}()
	wg.Wait()
	return c2u, u2c
}

// tunnel blindly relays c to target and records a passthrough entry.
// prefix is written to the upstream first (already-read client bytes).
// With sendAck it answers a pending CONNECT (200 after dialing, 502 on failure).
func (s *Server) tunnel(c net.Conn, cr io.Reader, client, target string, prefix []byte, sendAck bool) {
	start := time.Now()
	rec := &capture.Record{
		TS: start, Mode: capture.ModePassthrough, Client: client,
		Method: "CONNECT", URL: "https://" + displayHost("https", target),
	}
	finish := func() {
		rec.DurationMS = time.Since(start).Milliseconds()
		s.cfg.Logger.Log(rec)
	}
	up, err := s.dial(target)
	if err != nil {
		rec.Status, rec.Error = http.StatusBadGateway, err.Error()
		if sendAck {
			fmt.Fprint(c, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		}
		finish()
		return
	}
	defer s.untrack(up)
	defer up.Close()
	if sendAck {
		if _, err := fmt.Fprint(c, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			rec.Error = err.Error()
			finish()
			return
		}
	}
	rec.Status = http.StatusOK
	if len(prefix) > 0 {
		if _, err := up.Write(prefix); err != nil {
			rec.Error = err.Error()
			finish()
			return
		}
	}
	c2u, u2c := biCopy(c, cr, up, up)
	rec.ReqBytes, rec.RespBytes = c2u+int64(len(prefix)), u2c
	finish()
}
