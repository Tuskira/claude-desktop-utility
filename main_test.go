package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCmdRun_RequiresGatewayFlags is a table test over the ways gateway
// forwarding can be left unconfigured. interceptor run must refuse to start
// in every case, with an error that names the missing flag/file and points
// at the README step that fixes it. None of these cases may bind a port or
// touch a capture file; see TestCmdRun_FailsFastWithoutListening for the
// listening check.
func TestCmdRun_RequiresGatewayFlags(t *testing.T) {
	tests := []struct {
		name    string
		extra   []string // appended after --dir/--out/--proxy-addr
		wantErr string
	}{
		{
			name:    "no gateway flags at all",
			extra:   nil,
			wantErr: "--gateway-url is required",
		},
		{
			name:    "gateway-url set, but no key file and no env var",
			extra:   []string{"--gateway-url", "http://127.0.0.1:1"},
			wantErr: "no gateway key",
		},
		{
			name:    "gateway-key-file set, but gateway-url missing",
			extra:   []string{"--gateway-key-file", mustWriteKeyFile(t)},
			wantErr: "--gateway-url is required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := append([]string{
				"--dir", t.TempDir(),
				"--out", "",
				"--proxy-addr", "127.0.0.1:0",
			}, tt.extra...)
			err := cmdRun(args)
			if err == nil {
				t.Fatal("cmdRun: expected an error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("cmdRun err = %q, want it to contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

// TestCmdRun_FailsFastWithoutListening confirms that a run invocation missing
// gateway flags returns before any listener binds a port: dialing the
// requested proxy address afterward must fail, because nothing is there.
func TestCmdRun_FailsFastWithoutListening(t *testing.T) {
	addr := "127.0.0.1:19347" // unlikely to be in use; nothing should ever bind it here
	err := cmdRun([]string{
		"--dir", t.TempDir(),
		"--out", "",
		"--proxy-addr", addr,
	})
	if err == nil {
		t.Fatal("cmdRun: expected an error when --gateway-url is not set")
	}
	if !strings.Contains(err.Error(), "--gateway-url") {
		t.Errorf("cmdRun err = %q, want it to mention --gateway-url", err.Error())
	}
	conn, dialErr := net.DialTimeout("tcp", addr, 200*time.Millisecond)
	if dialErr == nil {
		conn.Close()
		t.Fatalf("expected nothing listening on %s after cmdRun failed, but connected", addr)
	}
}

// mustWriteKeyFile writes a mode-0600 dummy gateway key file under t.TempDir
// and returns its path.
func mustWriteKeyFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gateway.key")
	if err := os.WriteFile(path, []byte("gk_test_dummy_key"), 0o600); err != nil {
		t.Fatalf("writing dummy key file: %v", err)
	}
	return path
}
