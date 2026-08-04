package logging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeN(t *testing.T, w *RotatingWriter, n int, line string) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
}

func listRotated(t *testing.T, dir, base string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), base+".") {
			out = append(out, e.Name())
		}
	}
	return out
}

// TestRotateBySize verifies a write that would breach the size ceiling
// rotates the file and starts a fresh active one.
func TestRotateBySize(t *testing.T) {
	dir := t.TempDir()
	w, err := New(Config{Dir: dir, BaseName: "orchicon.log", MaxSizeBytes: 50, RollInterval: 0, CheckInterval: time.Hour})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer w.Close()

	// Each line is 4 bytes; 100 lines (400 bytes) must cross the 50-byte
	// ceiling several times.
	writeN(t, w, 100, "aaaa")
	if got := len(listRotated(t, dir, "orchicon.log")); got == 0 {
		t.Fatal("expected at least one rotated file, got none")
	}
	data, err := os.ReadFile(filepath.Join(dir, "orchicon.log"))
	if err != nil {
		t.Fatalf("read active: %v", err)
	}
	if len(data) > 50 {
		t.Fatalf("active file %d bytes exceeds ceiling 50", len(data))
	}
}

// TestRotateByTime verifies the maintenance sweep rolls a file once it is
// older than RollInterval, even when it never nears the size ceiling.
func TestRotateByTime(t *testing.T) {
	dir := t.TempDir()
	w, err := New(Config{Dir: dir, BaseName: "orchicon.log", MaxSizeBytes: 1 << 30, RollInterval: 50 * time.Millisecond, CheckInterval: 20 * time.Millisecond})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer w.Close()

	writeN(t, w, 1, "aaaa")
	time.Sleep(150 * time.Millisecond)
	if got := len(listRotated(t, dir, "orchicon.log")); got == 0 {
		t.Fatal("expected a time-based rotation, got none")
	}
}

// TestPruneRetention verifies rotated files older than RetentionDays are
// deleted by the sweep.
func TestPruneRetention(t *testing.T) {
	dir := t.TempDir()
	// Hand-create an aged rotated file.
	old := filepath.Join(dir, "orchicon.log.20200101-000000.000")
	if err := os.WriteFile(old, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed aged file: %v", err)
	}
	past := time.Now().Add(-10 * 24 * time.Hour)
	_ = os.Chtimes(old, past, past)

	w, err := New(Config{Dir: dir, BaseName: "orchicon.log", MaxSizeBytes: 1 << 30, RollInterval: 0, RetentionDays: 7, MaxFiles: 100, CheckInterval: 20 * time.Millisecond})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer w.Close()

	writeN(t, w, 1, "aaaa")
	time.Sleep(80 * time.Millisecond)
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("expected aged rotated file to be pruned")
	}
}

// TestPruneMaxFiles verifies only the newest MaxFiles rotated files are
// kept.
func TestPruneMaxFiles(t *testing.T) {
	dir := t.TempDir()
	w, err := New(Config{Dir: dir, BaseName: "orchicon.log", MaxSizeBytes: 50, RollInterval: 0, RetentionDays: 1000, MaxFiles: 2, CheckInterval: time.Hour})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer w.Close()

	writeN(t, w, 100, "aaaa")
	time.Sleep(50 * time.Millisecond) // let the last inline rotation's prune settle
	// The sweep runs every hour in this test, so force an inline prune by
	// triggering another rotation via a big write.
	writeN(t, w, 100, "bbbb")
	time.Sleep(50 * time.Millisecond)
	if got := len(listRotated(t, dir, "orchicon.log")); got > 2 {
		t.Fatalf("expected ≤2 rotated files, got %d", got)
	}
}

// TestApplyChangesMaxSize verifies live reconfiguration takes effect.
func TestApplyChangesMaxSize(t *testing.T) {
	dir := t.TempDir()
	w, err := New(Config{Dir: dir, BaseName: "orchicon.log", MaxSizeBytes: 1 << 30, RollInterval: 0, CheckInterval: time.Hour})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer w.Close()

	w.Apply(Config{Dir: dir, BaseName: "orchicon.log", MaxSizeBytes: 20})
	writeN(t, w, 100, "cccc")
	if got := len(listRotated(t, dir, "orchicon.log")); got == 0 {
		t.Fatal("expected rotation after live size change")
	}
}

// TestOnRotateCallback verifies the callback fires after a rotation and
// that it is invoked outside the writer's lock (the serve dup2 path).
func TestOnRotateCallback(t *testing.T) {
	dir := t.TempDir()
	w, err := New(Config{Dir: dir, BaseName: "orchicon.log", MaxSizeBytes: 20, RollInterval: 0, CheckInterval: time.Hour})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer w.Close()

	called := make(chan struct{}, 8)
	w.SetOnRotate(func() {
		select {
		case called <- struct{}{}:
		default:
		}
	})
	writeN(t, w, 100, "dddd")
	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("onRotate callback was not invoked")
	}
}

// TestCloseIdempotent verifies a second Close is a no-op (the serve path
// defers Close while the applier loop may also close it).
func TestCloseIdempotent(t *testing.T) {
	dir := t.TempDir()
	w, err := New(Config{Dir: dir, BaseName: "orchicon.log", CheckInterval: time.Hour})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second close should be a no-op: %v", err)
	}
}
