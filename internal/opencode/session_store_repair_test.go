package opencode

// session_store_repair_test.go — A BROKEN SESSION STORE MUST BE REPAIRED ON A LOCAL RUN TOO.
//
// THE INCIDENT. A local-mode work item produced six executions, all ending in
//
//	opencode_session_error: Failed to execute statement; Failed to execute statement
//
// — two of them after 5-11 minutes of real work (~6.5M tokens), three of them in 46ms with zero
// tokens. It never converged, and it never could have. Three things compounded:
//
//  1. isInfraModelTurnError matches only socket/transport terms, so a STORE failure was classified as
//     a generic model-layer error and fell through to recycleOnWedgedServe.
//  2. recycleOnWedgedServe was CONTAINER-ONLY (`RuntimeWorkflowID != "" && rt != nil`), so on a LOCAL
//     run — which has no container at all (runtimeContainerRouteEnabled, adapter.go:307) and is served
//     by the shared HOST serve — it returned immediately and killed nothing.
//  3. The process that had actually wedged was therefore never touched. Every retry re-hit the same
//     store. The 46ms/0-token signature is a serve failing at the first statement, over and over.
//
// These pin the repair existing, reaching the HOST serve, and rotating the store rather than reopening
// it (ARCHITECTURE.md:1212 — an in-place restart reuses an on-disk poison).

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/beardedparrott/orchicon/internal/scheduler"
)

func quietAdapter() *Adapter {
	return &Adapter{log: slog.New(slog.NewTextHandler(io.Discard, nil)), sessions: map[string]*sessionRun{}}
}

// THE SIGNATURE IS RECOGNISED. This is the exact field wording from the incident.
func TestSessionStoreFailureIsRecognised(t *testing.T) {
	for _, msg := range []string{
		"Failed to execute statement",
		"Failed to execute statement; Failed to execute statement",
		"failed to execute statement: SQLITE_CONSTRAINT",
		"database disk image is malformed",
		"no such table: session_v2",
		"database is locked",
	} {
		if !isSessionStoreFailure(msg) {
			t.Errorf("isSessionStoreFailure(%q) = false, want true — this is the incident's own wording", msg)
		}
	}
}

// AND AN ORDINARY MODEL FAILURE IS NOT. A store classifier that answers true everywhere would recycle
// the plane's only serve on every provider hiccup — far worse than the bug it fixes.
func TestOrdinaryModelFailuresAreNotStoreFailures(t *testing.T) {
	for _, msg := range []string{
		"",
		"Cannot connect to API: Unable to connect",
		"rate limit exceeded",
		"401 Unauthorized",
		"WebFetch is not allowed",
		"model returned an empty response",
	} {
		if isSessionStoreFailure(msg) {
			t.Errorf("isSessionStoreFailure(%q) = true, want false — an ordinary model failure must not "+
				"trigger a store rotation", msg)
		}
	}
}

// THE HOST SERVE IS REACHED. The bug was the repair not applying to the serve a local run talks to, so
// this asserts the selection rather than the restart (a restart needs a real CLI).
func TestTheRepairTargetsTheHostServeWhenNoContainerExists(t *testing.T) {
	a := quietAdapter()
	hs := NewHostServe(a.log, t.TempDir(), "")
	a.host = hs

	if got := a.activeHostServe(); got != hs {
		t.Fatalf("activeHostServe() = %v, want the wired host serve", got)
	}
	// A manifest with no runtime workflow cannot be container-served, which is what made the old guard
	// return early and leave the store unrepaired.
	m := scheduler.ExecutionManifest{ExecutionID: "exec-local", ExecutionMode: "local"}
	if runtimeContainerRouteEnabled(false, m) {
		t.Fatal("a local manifest with no daemon routed to a container — this test's premise is broken")
	}
}

// ROTATION MOVES THE STORE, IT DOES NOT DELETE IT. Operator-recoverable by construction.
func TestRotateStoreMovesTheStoreAside(t *testing.T) {
	dataDir := t.TempDir()
	dir := filepath.Join(dataDir, "opencode")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(dir, "opencode.db")
	for _, f := range []string{base, base + "-wal", base + "-shm"} {
		if err := os.WriteFile(f, []byte("stale"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	hs := NewHostServe(slog.New(slog.NewTextHandler(io.Discard, nil)), dataDir, "")
	if err := hs.RotateStore(); err != nil {
		t.Fatalf("RotateStore: %v", err)
	}

	// The live path is gone (a fresh start builds a new store here)...
	for _, f := range []string{base, base + "-wal", base + "-shm"} {
		if _, err := os.Stat(f); err == nil {
			t.Errorf("%s still exists at the live path — the next start would reopen the old store", f)
		}
	}
	// ...the directory itself survives, because the CLI will not create the XDG parent chain...
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Fatalf("the data dir was not left in place for the new store: %v", err)
	}
	// ...and the previous store is RECOVERABLE, not destroyed.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var rotated int
	for _, e := range entries {
		if strings.Contains(e.Name(), ".broken-") {
			rotated++
			b, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
			if rerr != nil || string(b) != "stale" {
				t.Errorf("rotated file %s does not carry the original contents", e.Name())
			}
		}
	}
	if rotated != 3 {
		t.Errorf("rotated %d files, want 3 (db + wal + shm) — got %v", rotated, entries)
	}
}

// A FRESH INSTALL HAS NO STORE TO ROTATE, and that must be a loud no-op rather than a doomed Mkdir.
func TestRotateStoreWithNothingToRotate(t *testing.T) {
	hs := NewHostServe(slog.New(slog.NewTextHandler(io.Discard, nil)), t.TempDir(), "")
	if err := hs.RotateStore(); err == nil {
		t.Fatal("RotateStore reported success with no store present — a repair would look like it worked " +
			"while changing nothing")
	}
}

// THE REPAIR IS BOUNDED. The host serve is shared by every in-process execution on the plane, so an
// unbounded recycle loop would restart it forever.
func TestHostServeRepairIsBounded(t *testing.T) {
	t.Setenv("ORCHICON_SESSION_REPAIR_ATTEMPTS", "2")
	a := quietAdapter()
	a.host = NewHostServe(a.log, t.TempDir(), "")

	// Each call stops and restarts a real serve, so drive the BUDGET directly rather than spawning.
	for i := 0; i < 2; i++ {
		a.mu.Lock()
		if a.hostServeRepairs >= sessionRepairBudget() {
			a.mu.Unlock()
			t.Fatalf("budget exhausted at attempt %d, want 2 available", i+1)
		}
		a.hostServeRepairs++
		a.mu.Unlock()
	}
	a.mu.Lock()
	got := a.hostServeRepairs
	a.mu.Unlock()
	if got != 2 {
		t.Fatalf("hostServeRepairs = %d, want 2", got)
	}
	if sessionRepairBudget() != 2 {
		t.Fatalf("repair budget = %d, want the configured 2", sessionRepairBudget())
	}
}

// A VISIBILITY GUARD: the store failure must NOT be classified as an infra model-turn error, or the
// container-recycle path would own it and the host-serve repair would never run.
func TestStoreFailureIsNotClassifiedAsInfraModelTurn(t *testing.T) {
	if isInfraModelTurnError("Failed to execute statement") {
		t.Fatal("a store failure is being classified as an infra model-turn error — that path is " +
			"container-only, which is exactly why the local-mode store was never repaired")
	}
}

var _ = context.Background
var _ fstest.MapFS
