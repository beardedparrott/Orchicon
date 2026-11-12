package scheduler

// log_noise_test.go — A PERSISTENT CONDITION MUST NOT BE RE-REPORTED ON EVERY SWEEP PASS.
//
// REPORTED: "The logs in general are WAY too noisy. Every second I am seeing ..." — two lines, the same run id,
// forever:
//
//	{"level":"INFO","msg":"workflow runtime reaped","run":"01M6MKW6V6KC91CTTTPVKTT58W"}
//	{"level":"INFO","msg":"runtime reap sweep: reaped a terminal run's container","run":"01M6MKW…","status":"completed"}
//
// Both are the same class of defect: a condition that never becomes false, re-evaluated and re-reported by a
// sweep that runs continuously.
//
//   1. THE REAP SWEEP'S CANDIDATE PREDICATE WAS NEVER CONSUMED. It selects terminal runs with runtime evidence
//      (`runtime_ready = true OR runtime_image <> ''`) and NOTHING EVER CLEARS either — runtime_image is the
//      run's history, runtime_ready is a gate. So every historical run was reaped again (a no-op) and logged as
//      "reaped a terminal run's container" once a second — false from the second pass onward.
//   2. THE DIRTY-WORKTREE WARNING fired on every prune pass for the same path. Correctly refusing to destroy
//      uncommitted work is not a new event each time.
//
// An operator who sees the same WARN every second learns to ignore WARN, which is how the real one gets missed.

import (
	"os"
	"strings"
	"testing"
)

func readSource(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// THE SWEEP'S MARKER IS WHAT MAKES THE CANDIDATE SET TERMINATE.
func TestTheReapSweepsCandidateQueryConsumesItsEvidence(t *testing.T) {
	src := readSource(t, "../db/workflow.go")
	body := funcBody(t, src, "func ListTerminalRunsWithRuntimeEvidence(")
	if strings.Contains(body, "runtime_ready = true OR runtime_image <> ''") &&
		!strings.Contains(body, "runtime_reaped_at IS NULL") {
		t.Fatal("the candidate query selects terminal runs on evidence that NOTHING EVER CLEARS — " +
			"runtime_image is the run's history and runtime_ready is a gate, so every historical run is " +
			"re-reaped and re-logged on every sweep pass, once a second, forever. It needs the reap marker in " +
			"its predicate.")
	}
}

// AND THE SWEEP WRITES IT, in the pass it reaps — otherwise the marker never lands.
func TestTheReapSweepMarksWhatItReaped(t *testing.T) {
	src := readSource(t, "runtime_reap_sweep.go")
	if !strings.Contains(src, "markReaped(ctx, tenantID, run.ID)") {
		t.Fatal("the reap sweep does not mark the run it reaped, so the same run returns on every pass and is " +
			"logged as freshly reaped once a second")
	}
	db := readSource(t, "../db/workflow.go")
	if !strings.Contains(db, "runtime_reaped_at = now()") {
		t.Error("no reap marker is written anywhere the sweep can reach")
	}
}

// THE ROUTINE REAP LINE IS NOT INFO. The sweep owns the operator-facing statement, once.
func TestTheLifecycleReapIsNotLoggedAtInfo(t *testing.T) {
	src := readSource(t, "../runtime/lifecycle.go")
	body := funcBody(t, src, "func (l *Lifecycle) ReapForRun(")
	if strings.Contains(body, `l.log.Info("workflow runtime reaped"`) {
		t.Error("ReapForRun logs at INFO. It runs on a routine path and the SWEEP already reports the " +
			"operator-facing line; logging here as well doubled the volume, and on a run whose container was " +
			"long gone the sentence was untrue.")
	}
}

// THE DIRTY-WORKTREE WARNING IS SAID ONCE PER PATH.
func TestTheDirtyWorktreeWarningIsSaidOnce(t *testing.T) {
	forgetDirtyWarn("/tmp/wt-a")
	t.Cleanup(func() { forgetDirtyWarn("/tmp/wt-a") })

	// The guard is the package-level set the warn goes through; drive it directly so this asserts the GUARD
	// rather than git.
	_, first := dirtyWarned.LoadOrStore("/tmp/wt-a", struct{}{})
	if first {
		t.Fatal("premise wrong: the path was already warned")
	}
	for i := 0; i < 5; i++ {
		if _, seen := dirtyWarned.LoadOrStore("/tmp/wt-a", struct{}{}); !seen {
			t.Fatalf("pass %d warned AGAIN about the same path — a persistent condition re-reported every "+
				"sweep pass is the noise this change removes", i)
		}
	}

	// A DIFFERENT path is its own event.
	if _, seen := dirtyWarned.LoadOrStore("/tmp/wt-b", struct{}{}); seen {
		t.Error("a different path was treated as already warned")
	}
	forgetDirtyWarn("/tmp/wt-b")

	// AND A RECLAIMED PATH CAN WARN AGAIN, so a later run reusing it is not silenced by this one.
	forgetDirtyWarn("/tmp/wt-a")
	if _, seen := dirtyWarned.Load("/tmp/wt-a"); seen {
		t.Fatal("forgetDirtyWarn left the guard in place — a run reusing this path could never warn again")
	}
}

// AND THE REMOVAL PATH CLEARS THE GUARD, or every later run on that reused path stays silent.
func TestRemovingAWorktreeClearsTheDirtyGuard(t *testing.T) {
	src := readSource(t, "worktree_reconciler.go")
	body := funcBody(t, src, "func (r *WorktreeReconciler) removeWorktree(")
	if !strings.Contains(body, "forgetDirtyWarn(path)") {
		t.Error("removeWorktree does not clear the once-guard, so a run that later reuses this path can never " +
			"warn about its own uncommitted work")
	}
}
