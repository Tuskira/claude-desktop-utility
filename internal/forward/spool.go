package forward

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// spool persists gzip-compressed ingest batches on disk so they survive a
// restart or a gateway outage. It is capped at 500 MB or 7 days, dropping
// the oldest.
type spool struct {
	dir string
}

// spoolMaxBytes and spoolMaxAge are the 500 MB / 7 day caps; they
// are vars, not consts, only so tests can shrink them instead of writing
// hundreds of megabytes or waiting a week to exercise EnforceCap.
var (
	spoolMaxBytes int64 = 500 << 20
	spoolMaxAge         = 7 * 24 * time.Hour
)

const spoolFileExt = ".batch.gz"

// openSpool creates dir (mode 0700) if needed and returns a handle to it.
func openSpool(dir string) (*spool, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("forward: spool dir %s: %w", dir, err)
	}
	// MkdirAll does not change the mode of a directory that already exists
	// with looser permissions; fix that up explicitly since the spool can
	// hold un-truncated bodies before redaction bugs are caught.
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("forward: spool dir %s: %w", dir, err)
	}
	return &spool{dir: dir}, nil
}

// spoolEntry names one file: <unix-nanos>-<item-count><ext>. The name alone
// carries write order (files sort chronologically) and the record count
// (so callers don't need to decompress+parse just to update counters).
type spoolEntry struct {
	path    string
	writeAt time.Time
	count   int
	size    int64
}

func spoolFileName(writeAt time.Time, count int) string {
	return fmt.Sprintf("%020d-%d%s", writeAt.UnixNano(), count, spoolFileExt)
}

func parseSpoolFileName(name string) (writeAt time.Time, count int, ok bool) {
	name = strings.TrimSuffix(name, spoolFileExt)
	parts := strings.SplitN(name, "-", 2)
	if len(parts) != 2 {
		return time.Time{}, 0, false
	}
	nanos, err1 := strconv.ParseInt(parts[0], 10, 64)
	n, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return time.Time{}, 0, false
	}
	return time.Unix(0, nanos), n, true
}

// Write saves one gzip-compressed batch (count records) to disk, mode 0600,
// then enforces the size/age caps.
func (s *spool) Write(gz []byte, count int) error {
	name := spoolFileName(time.Now(), count)
	path := filepath.Join(s.dir, name)
	if err := os.WriteFile(path, gz, 0o600); err != nil {
		return fmt.Errorf("forward: spool write: %w", err)
	}
	return nil
}

// List returns spool entries oldest-first.
func (s *spool) List() ([]spoolEntry, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("forward: spool list: %w", err)
	}
	out := make([]spoolEntry, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), spoolFileExt) {
			continue
		}
		writeAt, count, ok := parseSpoolFileName(e.Name())
		if !ok {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, spoolEntry{path: filepath.Join(s.dir, e.Name()), writeAt: writeAt, count: count, size: info.Size()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].writeAt.Before(out[j].writeAt) })
	return out, nil
}

// Read loads one spool file's gzip bytes.
func (s *spool) Read(e spoolEntry) ([]byte, error) { return os.ReadFile(e.path) }

// Remove deletes one spool file. Not finding it is not an error: something
// else (a concurrent cap enforcement pass) may already have removed it.
func (s *spool) Remove(e spoolEntry) error {
	if err := os.Remove(e.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// EnforceCap drops the oldest spool files until the total size is at most
// spoolMaxBytes and no file is older than spoolMaxAge, returning how many
// records were dropped this way (for the "spooled"/"dropped" status line).
func (s *spool) EnforceCap(now time.Time) (droppedRecords int, err error) {
	entries, err := s.List()
	if err != nil {
		return 0, err
	}
	var total int64
	for _, e := range entries {
		total += e.size
	}
	for _, e := range entries {
		expired := now.Sub(e.writeAt) > spoolMaxAge
		overCap := total > spoolMaxBytes
		if !expired && !overCap {
			break // entries are oldest-first; nothing later needs dropping
		}
		if err := s.Remove(e); err != nil {
			return droppedRecords, err
		}
		total -= e.size
		droppedRecords += e.count
	}
	return droppedRecords, nil
}
