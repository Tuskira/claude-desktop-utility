package claudedesktop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUserLabel_LearnedFromProfile(t *testing.T) {
	u := newUserLabel("", "", nil)
	if got := u.Get(); got != "" {
		t.Fatalf("Get() before any profile = %q, want empty", got)
	}
	u.observeOAuthProfile([]byte(`{"account":{"email":"person@example.com","uuid":"x"}}`))
	if got := u.Get(); got != "person@example.com" {
		t.Fatalf("Get() = %q, want person@example.com", got)
	}
}

func TestUserLabel_OverrideWins(t *testing.T) {
	u := newUserLabel("override@example.com", "", nil)
	u.observeOAuthProfile([]byte(`{"account":{"email":"person@example.com"}}`))
	if got := u.Get(); got != "override@example.com" {
		t.Fatalf("Get() = %q, want the --forward-user override to win", got)
	}
}

func TestUserLabel_MalformedProfileIgnored(t *testing.T) {
	u := newUserLabel("", "", nil)
	u.observeOAuthProfile([]byte(`not json`))
	if got := u.Get(); got != "" {
		t.Fatalf("Get() = %q, want empty after malformed input", got)
	}
	u.observeOAuthProfile([]byte(`{"account":{}}`)) // no email
	if got := u.Get(); got != "" {
		t.Fatalf("Get() = %q, want empty when email is missing", got)
	}
}

func TestConverter_UserLabel(t *testing.T) {
	conv, _, _ := newTestConverter()
	if got := conv.UserLabel(); got != "" {
		t.Fatalf("UserLabel() = %q, want empty", got)
	}
	rec := postRecord(time.Now(), "https://api.anthropic.com/api/oauth/profile", "")
	rec.Method = "GET"
	rec.Status = 200
	rec.RespBody = `{"account":{"email":"a@b.com"}}`
	conv.Handle(rec)
	if got := conv.UserLabel(); got != "a@b.com" {
		t.Fatalf("UserLabel() = %q, want a@b.com", got)
	}
}

// A restart must not lose the label: Claude Desktop fetches the profile
// only at app start, so a restarted interceptor may never see it again.
func TestUserLabel_PersistedAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "user-label")
	u := newUserLabel("", path, nil)
	u.observeOAuthProfile([]byte(`{"account":{"email":"person@example.com"}}`))
	deadline := time.Now().Add(2 * time.Second)
	for {
		if b, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(b)) == "person@example.com" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("label was not persisted")
		}
		time.Sleep(5 * time.Millisecond)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("label file mode = %04o, want 0600", perm)
	}
	restarted := newUserLabel("", path, nil)
	if got := restarted.Get(); got != "person@example.com" {
		t.Errorf("Get() after restart = %q, want the persisted label", got)
	}
}

func TestUserLabel_MissingFileMeansEmptyNotHeld(t *testing.T) {
	u := newUserLabel("", filepath.Join(t.TempDir(), "absent"), nil)
	if got := u.Get(); got != "" {
		t.Errorf("Get() = %q, want empty when nothing is persisted", got)
	}
}
