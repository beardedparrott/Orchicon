package scheduler

// worktree_provision_stages_test.go — A SLOW PROVISION MUST NAME ITS OWN STAGE.
//
// Reported: "worktree provision took 65,677 ms this run vs ~350-500ms previously" — a 130x regression with
// nothing to attribute it to, because the log gave ONE number for six phases that shell out to git. Guessing
// at the cause from outside (a missing origin, a network probe, a large repo) was unproductive precisely
// because the artifact did not say.

import (
	"os"
	"strings"
	"testing"
	"time"
)

// TestStageTimerAttributesTimeInOrder pins the rendering the provision log depends on.
func TestStageTimerAttributesTimeInOrder(t *testing.T) {
	st := newStageTimer()
	st.last = time.Now().Add(-50 * time.Millisecond) // pretend 50ms elapsed
	st.mark("base")
	got := st.String()

	if !strings.HasPrefix(got, "base=") {
		t.Fatalf("stages = %q, want it to start with the first mark", got)
	}
	if !strings.Contains(got, "base=5") && !strings.Contains(got, "base=4") && !strings.Contains(got, "base=50") {
		// the exact value is timing-dependent; what matters is that a duration is rendered
		if !strings.Contains(got, "ms") {
			t.Fatalf("stages = %q, want a duration", got)
		}
	}
	st.mark("add")
	if !strings.Contains(st.String(), "add=") {
		t.Fatalf("second mark missing: %q", st.String())
	}
	// ORDER IS PRESERVED, because the log is read left to right while diagnosing.
	if strings.Index(st.String(), "base=") > strings.Index(st.String(), "add=") {
		t.Fatalf("marks rendered out of order: %q", st.String())
	}
}

// TestStageTimerIsNilSafe keeps the defer path safe: logProvisionOutcome is called from a deferred closure
// that also runs on error paths, and a nil timer must render empty rather than panic.
func TestStageTimerIsNilSafe(t *testing.T) {
	var st *stageTimer
	st.mark("never") // must not panic
	if got := st.String(); got != "" {
		t.Fatalf("nil timer rendered %q, want empty", got)
	}
}

// TestTheProvisionLogCarriesTheStages is the wiring assertion: a timer that is never rendered is exactly the
// situation this change exists to fix, and a nil-safe no-op would hide it.
func TestTheProvisionLogCarriesTheStages(t *testing.T) {
	src := readSchedulerSource(t, "worktree_reconciler.go")
	i := strings.Index(src, "func (r *WorktreeReconciler) logProvisionOutcome")
	if i < 0 {
		t.Fatal("logProvisionOutcome is gone")
	}
	body := src[i:]
	if j := strings.Index(body, "\n}"); j > 0 {
		body = body[:j]
	}
	if !strings.Contains(body, `"stages", stages.String()`) {
		t.Error("the provision log no longer includes the stage breakdown — a 65-second provision would be " +
			"unattributable again, which is the report this change answers")
	}
	// And the timer must actually be MARKED, or the breakdown is always empty.
	if !strings.Contains(src, `stages.mark("base")`) || !strings.Contains(src, `stages.mark("list")`) {
		t.Error("the provision path no longer marks its phases, so `stages` would render empty")
	}
}

func readSchedulerSource(t *testing.T, name string) string {
	t.Helper()
	b, err := osReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return b
}

// osReadFile is a one-line indirection so the import list stays honest about why the file is read.
func osReadFile(name string) (string, error) {
	b, err := os.ReadFile(name)
	return string(b), err
}
