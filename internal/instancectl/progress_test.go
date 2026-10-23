package instancectl

// progress_test.go — the setup record must be trustworthy, because it is the ONLY thing telling a GUI
// what a detached install is doing.
//
// The failure this prevents is specific: a detached setup that is killed outright cannot write "failed".
// The record is then a `running` line with a frozen timestamp, which renders as an eternally-spinning
// progress bar and tells the operator nothing. Two independent signals catch it — the recorded PID is
// gone, or the timestamp has not moved — and both are asserted here.

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// progressIn writes a record into a temp instance dir and returns the name.
func progressIn(t *testing.T, p SetupProgress) (string, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	const name = "dev"
	if err := WriteSetupProgress(name, p); err != nil {
		t.Fatalf("write setup progress: %v", err)
	}
	return name, SetupProgressPath(name)
}

func TestSetupProgressRoundTrips(t *testing.T) {
	name, path := progressIn(t, SetupProgress{
		State: SetupRunning, Phase: "downloading-postgres", Label: "Downloading PostgreSQL",
		Percent: 62, Detail: "62% of 32.4 MiB",
	})
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the record was not written where SetupProgressPath says: %v", err)
	}
	got, err := ReadSetupProgress(name)
	if err != nil || got == nil {
		t.Fatalf("read back: %v (nil=%v)", err, got == nil)
	}
	if got.Percent != 62 || got.Phase != "downloading-postgres" || got.Label != "Downloading PostgreSQL" {
		t.Errorf("round trip lost fields: %+v", got)
	}
	if got.StartedAt == "" || got.UpdatedAt == "" {
		t.Error("the record carries no timestamps, so staleness cannot be judged")
	}
}

// A MISSING RECORD IS NOT AN ERROR: a machine that never installed this instance has no progress file,
// and that is the ordinary state the list screen renders.
func TestMissingProgressIsNotAnError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	got, err := ReadSetupProgress("dev")
	if err != nil {
		t.Fatalf("a missing record must not be an error: %v", err)
	}
	if got != nil {
		t.Fatalf("a missing record must read as nil, got %+v", got)
	}
	// And EffectiveState on nil is empty, not a panic.
	var p *SetupProgress
	if s := p.EffectiveState(); s != "" {
		t.Errorf("nil record EffectiveState() = %q, want empty", s)
	}
}

// A STALE "running" RECORD MUST READ AS STALE, NOT AS RUNNING.
//
// THE RECORD IS WRITTEN AS RAW JSON, deliberately. WriteSetupProgress stamps UpdatedAt itself — the writer
// is the passage of time, and letting a caller set it would let a caller lie about freshness. So a stale
// record cannot be produced through the writer; it can only be LEFT BEHIND by a process that died. Writing
// the bytes directly is what faithfully simulates that, and it is the only way to test the rule that
// catches it.
func TestStaleRunningRecordReadsAsStale(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	const name = "dev"
	path := SetupProgressPath(name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * setupProgressStaleAfter).UTC().Format(time.RFC3339)
	raw := `{"instance":"dev","state":"running","phase":"downloading-postgres","percent":62,` +
		`"started_at":"` + old + `","updated_at":"` + old + `","pid":0}`
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ReadSetupProgress(name)
	if err != nil || got == nil {
		t.Fatalf("read: %v", err)
	}
	if !got.Stale {
		t.Error("a record whose updated_at has not moved for 2× the window was not marked stale")
	}
	if s := got.EffectiveState(); s != SetupStale {
		t.Errorf("EffectiveState() = %q, want %q — a dead detached setup renders as an eternal spinner "+
			"otherwise", s, SetupStale)
	}
	if !got.Finished() {
		t.Error("a stale record must count as finished, so the UI offers a retry instead of waiting")
	}
}

// A FRESH "running" RECORD IS RUNNING, and a terminal state is never overridden by the timestamp rule.
func TestFreshAndTerminalRecordsAreNotStale(t *testing.T) {
	name, _ := progressIn(t, SetupProgress{State: SetupRunning})
	got, _ := ReadSetupProgress(name)
	if got.Stale || got.EffectiveState() != SetupRunning {
		t.Errorf("a just-written record must read as running, got stale=%v state=%q", got.Stale, got.EffectiveState())
	}
	if got.Finished() {
		t.Error("a running record must not count as finished")
	}

	// A DONE record with an old timestamp stays done: staleness is about a CLAIMED-RUNNING record whose
	// process vanished, not about age.
	old := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	name2, _ := progressIn(t, SetupProgress{State: SetupDone, UpdatedAt: old})
	got2, _ := ReadSetupProgress(name2)
	if got2.EffectiveState() != SetupDone {
		t.Errorf("a DONE record was reclassified as %q by age — that would turn a success into a "+
			"suspicious state on a machine that has been idle", got2.EffectiveState())
	}
}

// THE WRITE IS ATOMIC: a reader polling every second must never see a truncated file and call it corrupt.
func TestWriteIsAtomic(t *testing.T) {
	name, path := progressIn(t, SetupProgress{State: SetupRunning, Percent: 1})
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("a .tmp file survived the write, so the rename did not happen")
	}
	// The directory holds exactly the record (no stray temp files).
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "setup-progress.json" {
			t.Errorf("unexpected leftover file %q in the instance dir", e.Name())
		}
	}
	if got, err := ReadSetupProgress(name); err != nil || got.Percent != 1 {
		t.Errorf("the record is unreadable after the write: %v", err)
	}
}

// CLEARING IT MATTERS: a finished, older record must not be mistaken for this run's progress.
func TestClearSetupProgress(t *testing.T) {
	name, path := progressIn(t, SetupProgress{State: SetupDone})
	if err := ClearSetupProgress(name); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("the record survived ClearSetupProgress")
	}
	// Clearing what is not there is a success, so a caller never has to check first.
	if err := ClearSetupProgress("nothing-here"); err != nil {
		t.Errorf("clearing a missing record must not fail: %v", err)
	}
}
