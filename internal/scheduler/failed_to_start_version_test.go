package scheduler

// failed_to_start_version_test.go — "db: not found" WAS A STALE VERSION, NOT A MISSING ROW.
//
// REPORTED, verbatim, six times in one run — once per failed execution:
//
//	01:12:06.939 ERROR adapter start failed   execution=…
//	01:12:06.940 ERROR mark failed_to_start   error="db: not found"
//
// `db: not found` is what UpdateExecution returns when its `AND version = $3` matches nothing, and the version
// used was the one the caller held BEFORE `bridge.Start` ran. Start marks the execution running, so the version
// had moved and the update could never land — a partial write, on the path that records a failure.
//
// It is also the string that once caused a 22-minute hot loop at a different call site, which is why it read as a
// regression rather than as a stale snapshot.
//
// The assertions below are SOURCE-LEVEL on the helper's contract plus a behavioural test of the terminal guard,
// because reproducing the race needs a live bridge whose Start mutates the row — the defect is the ORDER (read
// then write), which reads directly in the source.

import (
	"os"
	"strings"
	"testing"
)

// readReconcilerSource reads reconciler.go for the source-level half of these assertions.
func readReconcilerSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("reconciler.go")
	if err != nil {
		t.Fatalf("read reconciler.go: %v", err)
	}
	return string(b)
}

// TestTheMarkReReadsTheVersionBeforeUpdating is the regression assertion.
func TestTheMarkReReadsTheVersionBeforeUpdating(t *testing.T) {
	src := readReconcilerSource(t)

	// The shared helper must exist and be the only writer of failed_to_start.
	if !strings.Contains(src, "func (r *TaskReconciler) markExecutionFailedToStart(") {
		t.Fatal("markExecutionFailedToStart is missing — the two mark paths would each have to remember to " +
			"re-read the row, which is how the stale-version bug survived in both")
	}
	if got := strings.Count(src, "func (r *TaskReconciler) markExecutionFailedToStart("); got != 1 {
		t.Fatalf("%d definitions of markExecutionFailedToStart", got)
	}

	// It must READ the row inside the write path, before updating.
	i := strings.Index(src, "func (r *TaskReconciler) markExecutionFailedToStart(")
	j := strings.Index(src[i:], "\n// terminalExecutionStatus")
	if j < 0 {
		t.Fatal("could not bound the helper")
	}
	body := src[i : i+j]
	if !strings.Contains(body, "db.GetExecution(") {
		t.Error("the helper never re-reads the execution, so it still writes the version the caller happened to " +
			"hold — which is the reported `db: not found`")
	}
	if !strings.Contains(body, "cur.Version") {
		t.Error("the helper does not use the CURRENT row's version")
	}
	if !strings.Contains(body, "db.ErrNotFound") {
		t.Error("the helper does not treat a version conflict as retryable")
	}

	// AND NO CALL SITE MAY STILL PASS A SNAPSHOT VERSION.
	if strings.Contains(src, "exec.TenantID, exec.ID, exec.Version, db.UpdateExecutionFields{") {
		t.Error("a call site still writes failed_to_start with the caller's snapshot version — that is the bug")
	}
}

// TestTheMarkLeavesATerminalRowAlone — the guard that keeps this from overwriting history.
//
// An execution that already reached a terminal state was decided by something else. Writing failed_to_start over
// it would replace a real outcome (failed, terminated) with a shallower one.
func TestTheMarkLeavesATerminalRowAlone(t *testing.T) {
	for _, status := range []string{"succeeded", "failed", "failed_to_start", "terminated"} {
		if !terminalExecutionStatus(status) {
			t.Errorf("%q is not recognised as terminal, so the mark could overwrite a decided row", status)
		}
	}
	for _, status := range []string{"dispatching", "running", ""} {
		if terminalExecutionStatus(status) {
			t.Errorf("%q was treated as terminal, so a genuinely stuck execution would never be marked", status)
		}
	}
}
