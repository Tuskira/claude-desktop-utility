package forward

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSpool_WriteListReadRemove(t *testing.T) {
	sp, err := openSpool(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := sp.Write([]byte("batch-a"), 3); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond) // ensure a distinct unix-nanos filename
	if err := sp.Write([]byte("batch-b"), 5); err != nil {
		t.Fatal(err)
	}
	entries, err := sp.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("List() returned %d entries, want 2", len(entries))
	}
	if entries[0].count != 3 || entries[1].count != 5 {
		t.Errorf("entries oldest-first counts = %d,%d, want 3,5", entries[0].count, entries[1].count)
	}
	got, err := sp.Read(entries[0])
	if err != nil || string(got) != "batch-a" {
		t.Errorf("Read(oldest) = %q, %v, want batch-a", got, err)
	}
	removed := entries[0]
	if err := sp.Remove(removed); err != nil {
		t.Fatal(err)
	}
	entries, _ = sp.List()
	if len(entries) != 1 {
		t.Fatalf("after Remove: %d entries, want 1", len(entries))
	}
	// Removing an already-removed entry is not an error (a concurrent cap
	// enforcement pass could race a send-succeeded removal).
	if err := sp.Remove(removed); err != nil {
		t.Errorf("Remove of an already-removed entry returned an error: %v", err)
	}
}

func TestSpool_DirModeAndFileMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")
	sp, err := openSpool(dir)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("spool dir mode = %04o, want 0700", info.Mode().Perm())
	}
	if err := sp.Write([]byte("x"), 1); err != nil {
		t.Fatal(err)
	}
	entries, _ := sp.List()
	fi, err := os.Stat(entries[0].path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("spool file mode = %04o, want 0600", fi.Mode().Perm())
	}
}

func TestSpool_EnforceCapByAge(t *testing.T) {
	dir := t.TempDir()
	sp, err := openSpool(dir)
	if err != nil {
		t.Fatal(err)
	}
	old := spoolFileName(time.Now().Add(-8*24*time.Hour), 7) // older than spoolMaxAge (7 days)
	fresh := spoolFileName(time.Now(), 9)
	if err := os.WriteFile(filepath.Join(dir, old), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fresh), []byte("fresh"), 0o600); err != nil {
		t.Fatal(err)
	}
	dropped, err := sp.EnforceCap(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 7 {
		t.Errorf("dropped = %d, want 7 (the old file's record count)", dropped)
	}
	entries, _ := sp.List()
	if len(entries) != 1 || entries[0].count != 9 {
		t.Errorf("remaining entries = %+v, want just the fresh one", entries)
	}
}

func TestSpool_EnforceCapByBytes(t *testing.T) {
	old := spoolMaxBytes
	spoolMaxBytes = 10 // shrink for this test only
	t.Cleanup(func() { spoolMaxBytes = old })

	sp, err := openSpool(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := sp.Write([]byte("0123456789"), 1); err != nil { // exactly at the cap
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	if err := sp.Write([]byte("0123456789"), 2); err != nil { // pushes total over the cap
		t.Fatal(err)
	}
	dropped, err := sp.EnforceCap(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 1 {
		t.Fatalf("dropped = %d, want 1 (the oldest file's record count)", dropped)
	}
	entries, _ := sp.List()
	if len(entries) != 1 || entries[0].count != 2 {
		t.Errorf("remaining entries = %+v, want just the newest one", entries)
	}
}

func TestParseSpoolFileName(t *testing.T) {
	name := spoolFileName(time.Unix(0, 1234567890), 42)
	ts, n, ok := parseSpoolFileName(name)
	if !ok || n != 42 || ts.UnixNano() != 1234567890 {
		t.Errorf("parseSpoolFileName(%q) = %v, %d, %v", name, ts, n, ok)
	}
	if _, _, ok := parseSpoolFileName("garbage" + spoolFileExt); ok {
		t.Error("expected ok=false for a non-conforming file name")
	}
}
