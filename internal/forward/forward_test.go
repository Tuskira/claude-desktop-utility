package forward

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tuskira/claude-desktop-utility/internal/claudedesktop"
)

// writeKeyFile writes a mode-0600 key file in t.TempDir() and returns its path.
func writeKeyFile(t *testing.T, key string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gateway.key")
	if err := os.WriteFile(path, []byte(key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func newTestForwarder(t *testing.T, gatewayURL string, mod func(*Config)) *Forwarder {
	t.Helper()
	var mu sync.Mutex
	var logs []string
	cfg := Config{
		GatewayURL: gatewayURL,
		KeyFile:    writeKeyFile(t, testGatewayKey),
		SpoolDir:   filepath.Join(t.TempDir(), "spool"),
		UserLabel:  func() string { return "person@example.com" },
		Logf: func(f string, a ...any) {
			mu.Lock()
			defer mu.Unlock()
			logs = append(logs, fmt.Sprintf(f, a...))
		},
	}
	if mod != nil {
		mod(&cfg)
	}
	f, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Registered before any test-local defer that stops f.Run, so this runs
	// after Run has fully returned (t.Cleanup funcs run after the test body,
	// in LIFO order, once all of the test's own goroutines a caller waited
	// on have finished) — safe to read logs without racing Logf.
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, l := range logs {
			if strings.Contains(l, testGatewayKey) {
				t.Errorf("a log line leaked the gateway key: %q", l)
			}
		}
	})
	return f
}

func llmItem(id string) item {
	return item{llm: &claudedesktop.LLMCallIn{Timestamp: time.Now().Format(time.RFC3339Nano), RequestID: id, StatusCode: 200}}
}

func TestSendOrSpool_SuccessCountsSent(t *testing.T) {
	fg := newFakeGateway(t)
	f := newTestForwarder(t, fg.srv.URL, nil)

	f.sendOrSpool([]item{llmItem("icp_1"), llmItem("icp_2")})

	if got := f.Counters.Sent.Load(); got != 2 {
		t.Errorf("Sent = %d, want 2", got)
	}
	if got := f.Counters.Spooled.Load(); got != 0 {
		t.Errorf("Spooled = %d, want 0", got)
	}
	if fg.Count() != 1 {
		t.Fatalf("gateway received %d requests, want 1", fg.Count())
	}
	if got := fg.Received()[0].User; got != "person@example.com" {
		t.Errorf("batch user = %q, want person@example.com", got)
	}
}

func TestSendOrSpool_400DropsBatchWithoutSpooling(t *testing.T) {
	fg := newFakeGateway(t)
	fg.behaviors = []int{400}
	f := newTestForwarder(t, fg.srv.URL, nil)

	f.sendOrSpool([]item{llmItem("icp_1")})

	if got := f.Counters.Rejected.Load(); got != 1 {
		t.Errorf("Rejected = %d, want 1", got)
	}
	if got := f.Counters.Spooled.Load(); got != 0 {
		t.Errorf("Spooled = %d, want 0 (400 must not spool)", got)
	}
	entries, _ := f.spool.List()
	if len(entries) != 0 {
		t.Errorf("spool has %d files, want 0", len(entries))
	}
}

func TestSendOrSpool_503SpoolsThenDrainSucceeds(t *testing.T) {
	fg := newFakeGateway(t)
	fg.behaviors = []int{503} // only the first call fails; drainSpool's retry gets the default 200
	f := newTestForwarder(t, fg.srv.URL, nil)

	f.sendOrSpool([]item{llmItem("icp_1"), llmItem("icp_2"), llmItem("icp_3")})
	if got := f.Counters.Spooled.Load(); got != 3 {
		t.Fatalf("Spooled after 503 = %d, want 3", got)
	}
	entries, _ := f.spool.List()
	if len(entries) != 1 {
		t.Fatalf("spool has %d files, want 1", len(entries))
	}

	b := newBackoff() // Due() is true immediately for a fresh backoffState
	f.drainSpool(&b)

	if got := f.Counters.Sent.Load(); got != 3 {
		t.Errorf("Sent after drain = %d, want 3", got)
	}
	if got := f.Counters.Spooled.Load(); got != 0 {
		t.Errorf("Spooled after drain = %d, want 0", got)
	}
	entries, _ = f.spool.List()
	if len(entries) != 0 {
		t.Errorf("spool has %d files after successful drain, want 0", len(entries))
	}
	if fg.Count() != 2 {
		t.Errorf("gateway saw %d requests, want 2 (1 failed + 1 retry)", fg.Count())
	}
}

func TestDrainSpool_RetryableFailureLeavesFileInPlace(t *testing.T) {
	fg := newFakeGateway(t)
	fg.behaviors = []int{503, 503} // first send fails, and the retry also fails
	f := newTestForwarder(t, fg.srv.URL, nil)

	f.sendOrSpool([]item{llmItem("icp_1")})
	entries, _ := f.spool.List()
	if len(entries) != 1 {
		t.Fatalf("spool has %d files, want 1", len(entries))
	}
	originalPath := entries[0].path

	b := newBackoff()
	f.drainSpool(&b)

	entries, _ = f.spool.List()
	if len(entries) != 1 || entries[0].path != originalPath {
		t.Fatalf("spool after a failed retry = %+v, want the same single original file untouched", entries)
	}
	if got := f.Counters.Spooled.Load(); got != 1 {
		t.Errorf("Spooled = %d, want 1 (still pending)", got)
	}
	if b.Due(time.Now()) {
		t.Errorf("backoff should not be due immediately after a failure")
	}
}

func TestSpoolSurvivesRestart(t *testing.T) {
	fg := newFakeGateway(t)
	fg.behaviors = []int{503} // the pre-"restart" send fails and spools
	spoolDir := filepath.Join(t.TempDir(), "spool")

	f1 := newTestForwarder(t, fg.srv.URL, func(c *Config) { c.SpoolDir = spoolDir })
	f1.sendOrSpool([]item{llmItem("icp_restart_1"), llmItem("icp_restart_2")})
	entries, _ := f1.spool.List()
	if len(entries) != 1 {
		t.Fatalf("spool has %d files before restart, want 1", len(entries))
	}

	// Simulate a process restart: a brand new Forwarder over the same
	// spool directory, which the fake gateway will now accept.
	f2 := newTestForwarder(t, fg.srv.URL, func(c *Config) { c.SpoolDir = spoolDir })
	b := newBackoff()
	f2.drainSpool(&b)

	if got := f2.Counters.Sent.Load(); got != 2 {
		t.Errorf("Sent after restart+drain = %d, want 2", got)
	}
	entries, _ = f2.spool.List()
	if len(entries) != 0 {
		t.Errorf("spool after restart+drain has %d files, want 0", len(entries))
	}
	// No duplicates: the gateway saw exactly 2 requests total (1 failed, 1
	// successful retry), never re-sending the same records twice over.
	if fg.Count() != 2 {
		t.Errorf("gateway saw %d requests total, want 2", fg.Count())
	}
}

func TestRedaction_DeviceAttestationAndSecretsStripped(t *testing.T) {
	fg := newFakeGateway(t)
	f := newTestForwarder(t, fg.srv.URL, nil)

	f.LLMCall(claudedesktop.LLMCallIn{
		Timestamp: time.Now().Format(time.RFC3339Nano), RequestID: "icp_redact", StatusCode: 200,
		Messages:     `{"events":[{"payload":{"type":"user"},"device_attestation":{"kid":"abc","signature":"xyz"}}]}`,
		ResponseBody: "here is a key sk-ant-api03-verysecretvalue and Bearer abc.def.ghi and sessionKeyXYZ123 in the text",
	})

	var it item
	select {
	case it = <-f.ch:
	default:
		t.Fatal("expected one queued item")
	}
	if strings.Contains(it.llm.Messages, "device_attestation") {
		t.Errorf("messages still contains device_attestation: %s", it.llm.Messages)
	}
	if strings.Contains(it.llm.Messages, "abc") && strings.Contains(it.llm.Messages, "xyz") {
		t.Errorf("messages still contains the attestation kid/signature values: %s", it.llm.Messages)
	}
	body := it.llm.ResponseBody
	for _, secret := range []string{"sk-ant-api03-verysecretvalue", "Bearer abc.def.ghi", "sessionKeyXYZ123"} {
		if strings.Contains(body, secret) {
			t.Errorf("response_body still contains a secret-shaped string %q: %s", secret, body)
		}
	}
	if !strings.Contains(body, "[redacted]") {
		t.Errorf("response_body has no [redacted] marker: %s", body)
	}
}

func TestRedactHeaders(t *testing.T) {
	in := map[string][]string{
		"Cookie":          {"a=b"},
		"Authorization":   {"Bearer x"},
		"X-Session-Token": {"t"},
		"User-Agent":      {"test/1.0"},
		"X-Request-Id":    {"r1"},
	}
	out := redactHeaders(in)
	for _, k := range []string{"Cookie", "Authorization", "X-Session-Token"} {
		if _, ok := out[k]; ok {
			t.Errorf("redactHeaders kept sensitive header %q", k)
		}
	}
	for _, k := range []string{"User-Agent", "X-Request-Id"} {
		if _, ok := out[k]; !ok {
			t.Errorf("redactHeaders dropped harmless header %q", k)
		}
	}
}

func TestQueue_DropsOldestWhenFull(t *testing.T) {
	fg := newFakeGateway(t)
	f := newTestForwarder(t, fg.srv.URL, nil)
	// Fill the queue completely, then push one more: the oldest must be
	// dropped and counted, never blocking.
	for i := 0; i < queueCapacity; i++ {
		f.push(llmItem("icp_fill"))
	}
	done := make(chan struct{})
	go func() { f.push(llmItem("icp_overflow")); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("push blocked instead of dropping the oldest queued item")
	}
	if got := f.Counters.Dropped.Load(); got != 1 {
		t.Errorf("Dropped = %d, want 1", got)
	}
	if got := len(f.ch); got != queueCapacity {
		t.Errorf("queue length = %d, want %d (still full)", got, queueCapacity)
	}
}

func TestRun_FlushesOnBatchSizeTrigger(t *testing.T) {
	fg := newFakeGateway(t)
	f := newTestForwarder(t, fg.srv.URL, nil)
	ctx, cancel := context.WithCancel(context.Background())
	go f.Run(ctx)
	defer func() {
		cancel()
		f.Close(2 * time.Second) // wait for Run to fully stop before the test returns
	}()

	for i := 0; i < maxBatchSize+1; i++ {
		f.LLMCall(claudedesktop.LLMCallIn{Timestamp: time.Now().Format(time.RFC3339Nano), RequestID: fmt.Sprintf("icp_%d", i), StatusCode: 200})
	}

	deadline := time.Now().Add(3 * time.Second)
	for fg.Count() < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if fg.Count() < 1 {
		t.Fatal("no batch received within 3s of pushing more than maxBatchSize records")
	}
	if got := len(fg.Received()[0].LLMCalls); got != maxBatchSize {
		t.Errorf("first batch size = %d, want %d (size trigger, not the 2s ticker)", got, maxBatchSize)
	}
}

func TestNew_RequiresGatewayURL(t *testing.T) {
	_, err := New(Config{})
	if err == nil {
		t.Fatal("expected an error for an empty GatewayURL")
	}
}

func TestNew_RefusesOwnListenAddress(t *testing.T) {
	_, err := New(Config{
		GatewayURL: "http://127.0.0.1:9090",
		KeyFile:    writeKeyFile(t, testGatewayKey),
		SpoolDir:   t.TempDir(),
		OwnAddrs:   []string{"127.0.0.1:9090"},
	})
	if err == nil {
		t.Fatal("expected an error when --gateway-url is the interceptor's own listen address")
	}
}

func TestNew_KeyFileWrongModeRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.key")
	if err := os.WriteFile(path, []byte(testGatewayKey), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := New(Config{GatewayURL: "http://127.0.0.1:1", KeyFile: path, SpoolDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("err = %v, want a message about the required 0600 mode", err)
	}
}

func TestNew_KeyEnvFallback(t *testing.T) {
	f, err := New(Config{GatewayURL: "http://127.0.0.1:1", KeyEnv: testGatewayKey, SpoolDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if f.key != testGatewayKey {
		t.Errorf("key = %q, want %q", f.key, testGatewayKey)
	}
}

func TestNew_NoKeyIsAnError(t *testing.T) {
	_, err := New(Config{GatewayURL: "http://127.0.0.1:1", SpoolDir: t.TempDir()})
	if err == nil {
		t.Fatal("expected an error when neither --gateway-key-file nor INTERCEPTOR_GATEWAY_KEY is set")
	}
}

func TestIgnoresProxyEnvironment(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1") // nothing listens here
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	fg := newFakeGateway(t)
	f := newTestForwarder(t, fg.srv.URL, nil)
	f.sendOrSpool([]item{llmItem("icp_direct")})
	if got := f.Counters.Sent.Load(); got != 1 {
		t.Errorf("Sent = %d, want 1 (the client must ignore HTTP(S)_PROXY and dial the gateway directly)", got)
	}
}
