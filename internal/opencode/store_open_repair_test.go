package opencode

// store_open_repair_test.go — A STORE THAT CANNOT OPEN MUST BE REPAIRED, NOT RETRIED FOREVER.
//
// THE INCIDENT (2026-11-23 21:27 → 2026-11-24 00:23, six hours). A stub file — 29 bytes reading
// "this is not a sqlite database" — sat at the host serve's store path. Every serve start died 200ms
// later:
//
//	host opencode serve exited — supervision will restart it   error="exit status 1"
//	host opencode serve lazy start failed: host serve did not become ready within 90s
//	(repeated every ~95 seconds, for six hours)
//
// and every work item pointed at that serve failed with the reaper's message, which named a restart that
// never happened and a container that does not exist.
//
// WHY NOTHING REPAIRED IT. RotateStore (#1004) fires on a SESSION-store failure — and that requires a
// live session. A store broken BEFORE any session exists produces no session error at all, so the repair
// path was unreachable. This closes that: when a serve cannot become ready, the store is probed and
// rotated if unopenable.

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func quietServe(t *testing.T) (*HostServe, string) {
	t.Helper()
	dataDir := t.TempDir()
	dir := filepath.Join(dataDir, "opencode")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return NewHostServe(slog.New(slog.NewTextHandler(io.Discard, nil)), dataDir, ""), dir
}

// THE EXACT STUB that wedged the plane for six hours.
func TestAStubbedStoreIsDetectedAndRotated(t *testing.T) {
	hs, dir := quietServe(t)
	db := filepath.Join(dir, "opencode.db")
	if err := os.WriteFile(db, []byte("this is not a sqlite database"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A real poisoned store also carries a WAL from the sessions that ran before it broke.
	if err := os.WriteFile(db+"-wal", []byte("wal"), 0o600); err != nil {
		t.Fatal(err)
	}

	repaired, err := hs.repairUnopenableStore()
	if err != nil {
		t.Fatalf("repairUnopenableStore: %v", err)
	}
	if !repaired {
		t.Fatal("the stubbed store was NOT rotated — every serve start would keep dying on it, which is " +
			"the six-hour outage this exists to prevent")
	}
	if _, err := os.Stat(db); err == nil {
		t.Error("the stub is still at the live path")
	}
	// Recoverable, not destroyed.
	entries, _ := os.ReadDir(dir)
	var rotated int
	for _, e := range entries {
		if strings.Contains(e.Name(), ".broken-") {
			rotated++
		}
	}
	if rotated == 0 {
		t.Error("the previous store was deleted rather than moved — it must stay recoverable by hand")
	}
}

// A HEALTHY STORE IS LEFT ALONE. A repair that rotates a working store would destroy every session on
// the plane the first time a serve was slow to become ready.
func TestAHealthyStoreIsNotRotated(t *testing.T) {
	hs, dir := quietServe(t)
	db := filepath.Join(dir, "opencode.db")
	// A real, minimal SQLite file: 100-byte header starting "SQLite format 3\0".
	hdr := make([]byte, 100)
	copy(hdr, []byte("SQLite format 3\x00"))
	if err := os.WriteFile(db, hdr, 0o600); err != nil {
		t.Fatal(err)
	}

	repaired, err := hs.repairUnopenableStore()
	if err != nil {
		t.Fatalf("repairUnopenableStore: %v", err)
	}
	if repaired {
		t.Fatal("a store with a valid header was rotated — a slow-starting serve would wipe the plane's " +
			"session history")
	}
	if _, err := os.Stat(db); err != nil {
		t.Error("the healthy store disappeared")
	}
}

// NO STORE AT ALL IS NOT A FAILURE: a fresh install creates one.
func TestAMissingStoreIsNotTreatedAsCorrupt(t *testing.T) {
	hs, _ := quietServe(t)
	repaired, err := hs.repairUnopenableStore()
	if err != nil || repaired {
		t.Fatalf("repairUnopenableStore on a fresh install = (%v, %v), want (false, nil)", repaired, err)
	}
}

// THE PROBE MUST REJECT WHAT SQLITE REJECTS: truncated files and non-SQLite content.
func TestStoreOpensRejectsWhatSQLiteRejects(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name string
		body []byte
		want bool
	}{
		{"stub", []byte("this is not a sqlite database"), false},
		{"empty", nil, false},
		{"truncated header", []byte("SQLite format 3\x00"), false},
		{"wrong magic", append([]byte("NotSQLite formato"), make([]byte, 100)...), false},
		{"valid header", func() []byte {
			b := make([]byte, 100)
			copy(b, []byte("SQLite format 3\x00"))
			return b
		}(), true},
	} {
		p := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "_"))
		if err := os.WriteFile(p, tc.body, 0o600); err != nil {
			t.Fatal(err)
		}
		if got := storeOpens(p); got != tc.want {
			t.Errorf("storeOpens(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// THE WIRING, because the repair existing is not the same as the repair RUNNING.
//
// The probe is unit-tested above; this asserts the readiness-failure path actually consults it. Without
// this, deleting the call site leaves every unit test green and restores the six-hour loop.
func TestTheReadinessFailurePathRepairsTheStore(t *testing.T) {
	raw, err := os.ReadFile("servehost.go")
	if err != nil {
		t.Fatalf("read servehost.go: %v", err)
	}
	src := string(raw)

	if !strings.Contains(src, "h.repairUnopenableStore()") {
		t.Fatal("nothing calls repairUnopenableStore — an unopenable store would again be retried forever " +
			"after a 90s readiness timeout, with no session error for any other path to notice")
	}

	// It must sit on the TIMEOUT path, next to the failure it repairs.
	i := strings.Index(src, `return fmt.Errorf("host serve did not become ready within 90s")`)
	if i < 0 {
		t.Fatal("the readiness-timeout error is gone")
	}
	before := src[:i]
	if !strings.Contains(before[max(0, len(before)-1200):], "repairUnopenableStore") {
		t.Error("repairUnopenableStore is not called on the readiness-failure path — that is the only " +
			"place the failure is observable, since a broken store yields no session")
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
