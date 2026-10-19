package main

// host_services_recovery_test.go — A COMPONENT THAT CANNOT START IS REPAIRED, NOT RETRIED FOR EVER.
//
// THE FIELD FAILURE this exists for, read straight out of a live services.log:
//
//	level=WARN msg="component exited; restarting" component=loki restarts=71 backoff=32s error="exit status 1"
//
// Loki refuses to start when its WAL has a non-sequential segment:
//
//	get segment range: segments are not sequential
//	error initialising module: ingester
//
// That state is what an unclean kill leaves behind — a segment written but never filled, then a jump in the
// numbering — and it is PERMANENT. The supervisor's restart-with-backoff is exactly right for a transient
// crash and exactly wrong for this one: it retried every 32 seconds for hours while the Logs view reported
// "loki is unreachable", burning a process slot and filling the log with the same failure.
//
// So the property worth pinning is NOT "loki can be restarted" (it always could). It is that a component
// which has failed repeatedly has its UNSTARTABLE STATE QUARANTINED, and that the quarantine MOVES the bytes
// rather than destroying them.

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLokiRecoveryQuarantinesTheWALAndKeepsTheBytes pins the two halves that matter: the corrupt WAL is
// moved out of the way (so the next start can succeed) and it is NOT deleted (the bytes are the operator's
// telemetry, and a recovery that destroys data is worse than the crash-loop it fixes).
func TestLokiRecoveryQuarantinesTheWALAndKeepsTheBytes(t *testing.T) {
	dir := t.TempDir()
	s := &supervisor{log: testLogger(), dataDir: dir, infra: testInfra(), ports: servicePortsFromEnv()}

	wal := filepath.Join(dir, "loki", "wal")
	if err := os.MkdirAll(wal, 0o755); err != nil {
		t.Fatal(err)
	}
	// The shape the field showed: an unfilled segment beside a much larger one.
	for name, body := range map[string]string{
		"00014935": "filled",
		"00020544": "", // 0 bytes — the killed-mid-write artifact
	} {
		if err := os.WriteFile(filepath.Join(wal, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Loki's chunks are GOOD data and must survive untouched.
	chunks := filepath.Join(dir, "loki", "chunks")
	if err := os.MkdirAll(chunks, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chunks, "keep-me"), []byte("log data"), 0o644); err != nil {
		t.Fatal(err)
	}

	proc := s.lokiProc()
	if proc.recoverState == nil {
		t.Fatal("lokiProc has NO recoverState hook — a non-sequential WAL is permanent, so the component " +
			"would crash-loop for ever, which is the field failure this exists for")
	}
	if err := proc.recoverState(); err != nil {
		t.Fatalf("recoverState: %v", err)
	}

	// 1. THE WAL IS OUT OF THE WAY, so the next start begins clean.
	if _, err := os.Stat(wal); !os.IsNotExist(err) {
		t.Errorf("the WAL directory is still at %s — loki would fail to start again on the same segments", wal)
	}

	// 2. THE BYTES SURVIVED. They are moved to a timestamped sibling, never deleted.
	matches, err := filepath.Glob(wal + ".corrupt-*")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected exactly one quarantined WAL directory, found %d: %v", len(matches), matches)
	}
	segs, _ := os.ReadDir(matches[0])
	if len(segs) != 2 {
		t.Errorf("the quarantined WAL holds %d entries, want 2 — the segments must be KEPT, not discarded: "+
			"a recovery that destroys the operator's telemetry to fix a crash-loop is a worse failure than "+
			"the loop", len(segs))
	}
	if _, err := os.Stat(filepath.Join(matches[0], "00020544")); err != nil {
		t.Errorf("the 0-byte segment is missing from the quarantine: %v", err)
	}

	// 3. THE GOOD STATE IS UNTOUCHED. Loki's chunks are readable data that survives a restart; moving them
	// would discard logs that are perfectly good.
	if _, err := os.Stat(filepath.Join(chunks, "keep-me")); err != nil {
		t.Errorf("recovery disturbed loki's CHUNKS (not just its WAL): %v — only the write-ahead log is the "+
			"corrupt thing, so only it may be moved", err)
	}
}

// AND IT IS IDEMPOTENT-ENOUGH: a second call with no WAL present is a no-op rather than an error, so a
// component whose state was already quarantined does not fail the restart path.
func TestLokiRecoveryIsSafeWhenThereIsNoWAL(t *testing.T) {
	s := &supervisor{log: testLogger(), dataDir: t.TempDir(), infra: testInfra(), ports: servicePortsFromEnv()}
	if err := s.lokiProc().recoverState(); err != nil {
		t.Errorf("recoverState on a component with no WAL = %v, want nil (nothing to quarantine)", err)
	}
}

// THE THRESHOLD IS BOUNDED AND NON-ZERO. One crash is transient and two can be a race; three consecutive
// failures of the same child with the same backoff means the restart is not converging.
func TestRecoveryThresholdIsBounded(t *testing.T) {
	if recoveryThreshold < 2 {
		t.Errorf("recoveryThreshold = %d — quarantining state after a single crash would move aside GOOD "+
			"state on a transient fault", recoveryThreshold)
	}
	if recoveryThreshold > 10 {
		t.Errorf("recoveryThreshold = %d — too many retries before repair, which is the crash-loop this "+
			"exists to end (the field saw 71)", recoveryThreshold)
	}
}

var _ = time.Second
