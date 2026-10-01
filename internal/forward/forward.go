// Package forward sends the interceptor's converted Claude Desktop records
// (see internal/claudedesktop) to the gateway's POST /api/v1/ingest, per
// docs/design/interceptor-ingest.md sections 3 and 5.2 in the
// ai-agent-gateway repo.
//
// A Forwarder implements claudedesktop.Sink: LLMCall and AccessLog redact
// the record and enqueue it, both non-blocking, so they are safe to call
// from the proxy's hot goroutines (see capture.Logger.Consumer). A separate
// goroutine (Run) batches, gzips, sends, retries, and spools.
package forward

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Tuskira/claude-desktop-utility/internal/claudedesktop"
)

const (
	maxBatchSize   = 500
	batchInterval  = 2 * time.Second
	statusInterval = 60 * time.Second
	queueCapacity  = 20000 // ~40 batches of headroom before the queue drops oldest

	minBackoff = 1 * time.Second
	maxBackoff = 2 * time.Minute

	retryTickInterval    = 5 * time.Second
	maxSpoolDrainPerTick = 10
)

// Config configures a Forwarder. GatewayURL is required; New returns an
// error for anything else that is missing or invalid. Forwarding is
// entirely optional at the call site: cmd/main only constructs a Forwarder
// when --gateway-url was given.
type Config struct {
	GatewayURL string // control-plane base URL, e.g. http://localhost:8081
	KeyFile    string // path to a mode-0600 file holding the gateway key
	KeyEnv     string // INTERCEPTOR_GATEWAY_KEY value, if the env var was set; alternative to KeyFile
	SpoolDir   string // default ~/.interceptor/spool

	// OwnAddrs are the host:port addresses this interceptor process itself
	// listens on (--proxy-addr, --transparent-addr, --transparent-http-addr).
	// New refuses to start if GatewayURL resolves to one of them, since that
	// would tunnel the interceptor's own uploads back into itself.
	OwnAddrs []string

	UserLabel func() string // e.g. Converter.UserLabel; may be nil
	Logf      func(format string, args ...any)
}

// Counters are cumulative, process-lifetime, per the status line design
// wants: "every 60 seconds, log counts of sent, duplicate, rejected,
// dropped, and spooled records."
type Counters struct {
	Sent      atomic.Int64 // accepted by the gateway (llm_calls + access_logs)
	Duplicate atomic.Int64 // gateway-reported duplicates
	Rejected  atomic.Int64 // gateway-reported rejections, plus batches dropped locally (400, encode failure)
	Dropped   atomic.Int64 // records dropped from the in-memory queue or evicted from the spool cap
	Spooled   atomic.Int64 // records currently resting in the disk spool, best-effort gauge
}

type item struct {
	llm *claudedesktop.LLMCallIn
	acc *claudedesktop.AccessLogIn
}

// Forwarder sends batches to the gateway. Create one with New, start its
// background loop with go f.Run(ctx), and feed it records via LLMCall /
// AccessLog (it implements claudedesktop.Sink).
type Forwarder struct {
	gatewayURL string
	key        string
	userLabel  func() string
	logf       func(string, ...any)
	client     *http.Client
	spool      *spool

	ch   chan item
	done chan struct{}

	Counters Counters
}

// New validates cfg and constructs a Forwarder. It does not start the
// background loop; call go f.Run(ctx) for that.
func New(cfg Config) (*Forwarder, error) {
	if strings.TrimSpace(cfg.GatewayURL) == "" {
		return nil, errors.New("forward: GatewayURL is required")
	}
	u, err := url.Parse(cfg.GatewayURL)
	if err != nil {
		return nil, fmt.Errorf("forward: --gateway-url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("forward: --gateway-url must be http:// or https://, got %q", cfg.GatewayURL)
	}
	for _, own := range cfg.OwnAddrs {
		if own != "" && sameHostPort(u.Host, own) {
			return nil, fmt.Errorf("forward: --gateway-url (%s) is this interceptor's own listen address; refusing to avoid forwarding to itself", cfg.GatewayURL)
		}
	}
	key, err := resolveKey(cfg.KeyFile, cfg.KeyEnv)
	if err != nil {
		return nil, err
	}
	spoolDir := cfg.SpoolDir
	sp, err := openSpool(spoolDir)
	if err != nil {
		return nil, err
	}
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	userLabel := cfg.UserLabel
	if userLabel == nil {
		userLabel = func() string { return "" }
	}
	f := &Forwarder{
		gatewayURL: cfg.GatewayURL,
		key:        key,
		userLabel:  userLabel,
		logf:       logf,
		spool:      sp,
		ch:         make(chan item, queueCapacity),
		done:       make(chan struct{}),
		client: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				Proxy:               nil, // ignore HTTP(S)_PROXY: go straight to the gateway
				DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
				TLSHandshakeTimeout: 10 * time.Second,
				MaxIdleConnsPerHost: 4,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
	return f, nil
}

// resolveKey reads the gateway key from a mode-0600 file, or from the
// INTERCEPTOR_GATEWAY_KEY environment variable. It never logs the key.
func resolveKey(keyFile, envKey string) (string, error) {
	if keyFile != "" {
		info, err := os.Stat(keyFile)
		if err != nil {
			return "", fmt.Errorf("forward: --gateway-key-file: %w", err)
		}
		if info.Mode().Perm() != 0o600 {
			return "", fmt.Errorf("forward: --gateway-key-file %s must be mode 0600 (it is %04o); run chmod 600 %s", keyFile, info.Mode().Perm(), keyFile)
		}
		b, err := os.ReadFile(keyFile)
		if err != nil {
			return "", fmt.Errorf("forward: --gateway-key-file: %w", err)
		}
		key := strings.TrimSpace(string(b))
		if key == "" {
			return "", fmt.Errorf("forward: --gateway-key-file %s is empty", keyFile)
		}
		return key, nil
	}
	if strings.TrimSpace(envKey) != "" {
		return strings.TrimSpace(envKey), nil
	}
	return "", errors.New("forward: no gateway key: set --gateway-key-file (mode 0600) or INTERCEPTOR_GATEWAY_KEY; see deploy/macos/README.md Part 3 to create one")
}

func sameHostPort(a, b string) bool {
	na, pa := splitHostPortDefault(a)
	nb, pb := splitHostPortDefault(b)
	return strings.EqualFold(normalizeHost(na), normalizeHost(nb)) && pa == pb
}

func splitHostPortDefault(hostport string) (host, port string) {
	h, p, err := net.SplitHostPort(hostport)
	if err != nil {
		return hostport, ""
	}
	return h, p
}

func normalizeHost(h string) string {
	if h == "" || h == "0.0.0.0" || h == "::" {
		return "localhost"
	}
	if h == "127.0.0.1" || h == "::1" {
		return "localhost"
	}
	return h
}

// LLMCall implements claudedesktop.Sink. It redacts and enqueues; it never
// blocks and never performs I/O.
func (f *Forwarder) LLMCall(c claudedesktop.LLMCallIn) {
	c.Messages = redactBody(c.Messages)
	c.RequestBody = redactBody(c.RequestBody)
	c.ResponseBody = redactBody(c.ResponseBody)
	f.push(item{llm: &c})
}

// AccessLog implements claudedesktop.Sink.
func (f *Forwarder) AccessLog(a claudedesktop.AccessLogIn) {
	a.RequestBody = redactBody(a.RequestBody)
	a.ResponseBody = redactBody(a.ResponseBody)
	f.push(item{acc: &a})
}

// push enqueues it, dropping the oldest queued item if the queue is full.
// Never blocks (design: "a bounded in-memory queue that never blocks the
// proxy, dropping oldest records and counting the drops").
func (f *Forwarder) push(it item) {
	for {
		select {
		case f.ch <- it:
			return
		default:
			select {
			case <-f.ch:
				f.Counters.Dropped.Add(1)
			default:
				// Another goroutine won the race and drained/filled first;
				// loop and try the send again.
			}
		}
	}
}

// Run is the background loop: batches queued items, sends them, retries
// failed sends from the spool, and logs a status line every minute. It
// returns when ctx is cancelled, after a best-effort final flush.
func (f *Forwarder) Run(ctx context.Context) {
	defer close(f.done)
	statusTicker := time.NewTicker(statusInterval)
	defer statusTicker.Stop()
	batchTicker := time.NewTicker(batchInterval)
	defer batchTicker.Stop()
	retryTicker := time.NewTicker(retryTickInterval)
	defer retryTicker.Stop()

	backoff := newBackoff()

	var batch []item
	flush := func() {
		if len(batch) == 0 {
			return
		}
		b := batch
		batch = nil
		f.sendOrSpool(b)
	}

	for {
		select {
		case <-ctx.Done():
			f.drainQueue(&batch)
			flush()
			return
		case it := <-f.ch:
			batch = append(batch, it)
			if len(batch) >= maxBatchSize {
				flush()
			}
		case <-batchTicker.C:
			flush()
		case <-retryTicker.C:
			f.drainSpool(&backoff)
		case <-statusTicker.C:
			f.logStatus()
		}
	}
}

// drainQueue moves whatever is already queued into batch without blocking,
// for a clean shutdown flush.
func (f *Forwarder) drainQueue(batch *[]item) {
	for {
		select {
		case it := <-f.ch:
			*batch = append(*batch, it)
		default:
			return
		}
	}
}

// Close waits for Run to finish (its context must already be cancelled by
// the caller) or for timeout, whichever comes first.
func (f *Forwarder) Close(timeout time.Duration) {
	select {
	case <-f.done:
	case <-time.After(timeout):
	}
}

func (f *Forwarder) encode(batch []item) ([]byte, int, error) {
	b := claudedesktop.Batch{SchemaVersion: claudedesktop.SchemaVersion, User: f.userLabel()}
	for _, it := range batch {
		switch {
		case it.llm != nil:
			b.LLMCalls = append(b.LLMCalls, *it.llm)
		case it.acc != nil:
			b.AccessLogs = append(b.AccessLogs, *it.acc)
		}
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return nil, 0, err
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(raw); err != nil {
		return nil, 0, err
	}
	if err := gz.Close(); err != nil {
		return nil, 0, err
	}
	return buf.Bytes(), len(batch), nil
}

// outcome classifies one send() result. The three cases mirror the design's
// three branches exactly: design section 5.2's "on 429 or 503 or a network
// error, ... keeping the spool", "on 400, log it and drop that batch", and
// success.
type outcome int

const (
	outcomeSuccess outcome = iota
	outcomeBadRequest
	outcomeRetryable
)

func classify(status int, err error) outcome {
	switch {
	case err == nil && status == http.StatusOK:
		return outcomeSuccess
	case status == http.StatusBadRequest:
		return outcomeBadRequest
	default:
		return outcomeRetryable
	}
}

// countSuccess/countBadRequest apply the counter and log side effects
// shared by a fresh send and a spool replay; only what happens to the
// spool itself differs between the two callers, so that stays out of here.
func (f *Forwarder) countSuccess(resp *ingestResponse) {
	f.Counters.Sent.Add(int64(resp.Accepted.LLMCalls + resp.Accepted.AccessLogs))
	f.Counters.Duplicate.Add(int64(resp.Duplicates.LLMCalls + resp.Duplicates.AccessLogs))
	f.Counters.Rejected.Add(int64(len(resp.Rejected)))
	for _, r := range resp.Rejected {
		f.logf("interceptor: forward: gateway rejected %s #%d: %s", r.Kind, r.Index, r.Reason)
	}
}

func (f *Forwarder) countBadRequest(count int, err error) {
	f.logf("interceptor: forward: gateway returned 400 for a batch of %d records (dropped, not retried): %v", count, err)
	f.Counters.Rejected.Add(int64(count))
}

// sendOrSpool tries to send a freshly-batched (not yet on disk) set of
// records once. On success or a 400 there is nothing left to do. On a
// retryable failure it is written to the spool for drainSpool to retry
// later.
func (f *Forwarder) sendOrSpool(batch []item) {
	gz, n, err := f.encode(batch)
	if err != nil {
		f.logf("interceptor: forward: could not encode a batch of %d records (dropped): %v", n, err)
		f.Counters.Rejected.Add(int64(len(batch)))
		return
	}
	status, resp, sendErr := f.send(context.Background(), gz)
	switch classify(status, sendErr) {
	case outcomeSuccess:
		f.countSuccess(resp)
	case outcomeBadRequest:
		f.countBadRequest(n, sendErr)
	case outcomeRetryable:
		if err := f.spool.Write(gz, n); err != nil {
			f.logf("interceptor: forward: could not spool a batch of %d records (dropped): %v", n, err)
			f.Counters.Rejected.Add(int64(n))
			return
		}
		f.Counters.Spooled.Add(int64(n))
		f.enforceSpoolCap()
	}
}

func (f *Forwarder) enforceSpoolCap() {
	dropped, err := f.spool.EnforceCap(time.Now())
	if err != nil {
		f.logf("interceptor: forward: spool cap enforcement: %v", err)
		return
	}
	if dropped > 0 {
		f.Counters.Dropped.Add(int64(dropped))
		f.Counters.Spooled.Add(-int64(dropped))
		f.logf("interceptor: forward: spool over cap, dropped %d oldest records", dropped)
	}
}

// drainSpool attempts to resend the oldest spooled batches, applying
// exponential backoff between attempts and stopping at the first failure so
// the remaining, presumably still-failing, batches are left untouched on
// disk for a later tick — a failed retry must never re-spool (it is already
// there) or drop (spool eviction is EnforceCap's job, not a failed send's).
func (f *Forwarder) drainSpool(backoff *backoffState) {
	if !backoff.Due(time.Now()) {
		return
	}
	entries, err := f.spool.List()
	if err != nil {
		f.logf("interceptor: forward: spool list: %v", err)
		return
	}
	for i := 0; i < len(entries) && i < maxSpoolDrainPerTick; i++ {
		e := entries[i]
		gz, err := f.spool.Read(e)
		if err != nil {
			f.logf("interceptor: forward: spool read %s: %v", e.path, err)
			continue
		}
		status, resp, sendErr := f.send(context.Background(), gz)
		switch classify(status, sendErr) {
		case outcomeSuccess:
			f.countSuccess(resp)
		case outcomeBadRequest:
			f.countBadRequest(e.count, sendErr)
		case outcomeRetryable:
			backoff.Failure()
			return // stop draining; try the rest again after the backoff
		}
		f.Counters.Spooled.Add(-int64(e.count))
		if err := f.spool.Remove(e); err != nil {
			f.logf("interceptor: forward: spool remove %s: %v", e.path, err)
		}
		backoff.Success()
	}
}

func (f *Forwarder) logStatus() {
	f.logf("interceptor: forward status: sent=%d duplicate=%d rejected=%d dropped=%d spooled=%d",
		f.Counters.Sent.Load(), f.Counters.Duplicate.Load(), f.Counters.Rejected.Load(),
		f.Counters.Dropped.Load(), f.Counters.Spooled.Load())
}

type ingestResponse struct {
	Accepted struct {
		LLMCalls   int `json:"llm_calls"`
		AccessLogs int `json:"access_logs"`
	} `json:"accepted"`
	Duplicates struct {
		LLMCalls   int `json:"llm_calls"`
		AccessLogs int `json:"access_logs"`
	} `json:"duplicates"`
	Rejected []struct {
		Kind   string `json:"kind"`
		Index  int    `json:"index"`
		Reason string `json:"reason"`
	} `json:"rejected"`
}

// send POSTs one already-gzipped batch and returns the HTTP status (0 on a
// network error, alongside a non-nil err) and, on 200, the parsed response.
func (f *Forwarder) send(ctx context.Context, gz []byte) (status int, resp *ingestResponse, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.gatewayURL+ingestPath, bytes.NewReader(gz))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("X-Gateway-Key", f.key)

	httpResp, err := f.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer httpResp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(httpResp.Body, 1<<20))

	if httpResp.StatusCode != http.StatusOK {
		return httpResp.StatusCode, nil, fmt.Errorf("gateway responded %d: %s", httpResp.StatusCode, truncateForLog(body))
	}
	var parsed ingestResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return httpResp.StatusCode, nil, fmt.Errorf("gateway 200 with unparseable body: %w", err)
	}
	return httpResp.StatusCode, &parsed, nil
}

func truncateForLog(b []byte) string {
	const max = 300
	s := string(b)
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

const ingestPath = "/api/v1/ingest"

// backoffState implements exponential backoff with jitter for the spool
// retry loop, per design: "on 429 or 503 or a network error, exponential
// backoff with jitter, keeping the spool."
type backoffState struct {
	mu     sync.Mutex
	cur    time.Duration
	nextAt time.Time
}

func newBackoff() backoffState { return backoffState{cur: minBackoff} }

func (b *backoffState) Due(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return !now.Before(b.nextAt)
}

func (b *backoffState) Success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cur = minBackoff
	b.nextAt = time.Time{}
}

func (b *backoffState) Failure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	jitter := time.Duration(rand.Int63n(int64(b.cur) / 2)) // up to 50% jitter
	b.nextAt = time.Now().Add(b.cur/2 + jitter)
	b.cur *= 2
	if b.cur > maxBackoff {
		b.cur = maxBackoff
	}
}
