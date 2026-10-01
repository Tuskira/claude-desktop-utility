package claudedesktop

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
)

// oauthProfileResponse is the subset of GET https://api.anthropic.com/api/oauth/profile
// this package reads, confirmed against a real capture (2026-09-30):
// {"account":{"email":"...", ...}, "organization":{...}, "application":{...}, ...}.
type oauthProfileResponse struct {
	Account struct {
		Email string `json:"email"`
	} `json:"account"`
}

// userLabel holds the "user" string sent with every batch: the Claude
// account email, learned from a captured oauth/profile response, or a fixed
// override from --forward-user. Safe for concurrent use.
//
// Claude Desktop fetches the profile rarely (at app start, not per request),
// so an interceptor restart would otherwise send every record with an empty
// user until the app happens to refetch it. When path is set, the learned
// label is therefore persisted there (mode 0600) and loaded at startup.
// Records are never held back waiting for a label: until one is known they
// go out with an empty user.
type userLabel struct {
	override string       // --forward-user; wins unconditionally when non-empty
	learned  atomic.Value // string, set from the captured profile response
	path     string       // persisted last-known label; "" disables
	logf     func(string, ...any)
	saveMu   sync.Mutex // serialises background saves
}

func newUserLabel(forwardUser, path string, logf func(string, ...any)) *userLabel {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	u := &userLabel{override: strings.TrimSpace(forwardUser), path: path, logf: logf}
	u.learned.Store("")
	if path != "" {
		if b, err := os.ReadFile(path); err == nil {
			u.learned.Store(strings.TrimSpace(string(b)))
		}
	}
	return u
}

// save writes label to u.path atomically with mode 0600. It runs on its own
// goroutine so the capture hot path never does file I/O.
func (u *userLabel) save(label string) {
	u.saveMu.Lock()
	defer u.saveMu.Unlock()
	if err := writeFile0600(u.path, []byte(label+"\n")); err != nil {
		u.logf("could not persist user label to %s: %v", u.path, err)
	}
}

func writeFile0600(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".user-label-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// observeOAuthProfile updates the learned label from a GET
// /api/oauth/profile response body, if it parses and carries an email.
// Never overrides an already-learned non-empty value with an empty one.
func (u *userLabel) observeOAuthProfile(body []byte) {
	var resp oauthProfileResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return
	}
	email := strings.TrimSpace(resp.Account.Email)
	if email == "" {
		return
	}
	prev, _ := u.learned.Swap(email).(string)
	if u.path != "" && email != prev {
		go u.save(email)
	}
}

// Get returns the label to send with the current batch: the override if
// set, else the learned email, else "".
func (u *userLabel) Get() string {
	if u.override != "" {
		return u.override
	}
	v, _ := u.learned.Load().(string)
	return v
}

const oauthProfilePath = "/api/oauth/profile"
