package scheduler

// fail_at_start_reason_test.go — A FAILED RUN MUST SAY WHY, IN THE ROW THE UI READS.
//
// REPORTED: "Windows local works, but runtimes fail with no errors in the UI."
//
// MEASURED on the live plane, four consecutive failed runs carried:
//
//	status           FAILED
//	error_message    (empty)
//	error_kind       (empty)
//	worktree_status  pending
//	runtime_image    (empty)
//	execution        NONE
//
// — a run that died before dispatch and left no statement of why. The reason WAS computed; failRunAtStart logged
// it at WARN and then wrote only `status` and `ended_at` to the row. The operator's run page, which reads the
// row, therefore showed nothing.
//
// error_message / error_kind were added precisely so a failure stops being invisible (a reason used to sit
// base64-encoded in run_context). This path was never updated to use them, and it is the single place five
// different fail-at-start causes report through: the image resolve, an empty step DAG, a missing workflow
// version, the container-create failover, and the serve gate.

import (
	"os"
	"strings"
	"testing"
)

func readSchedulerSrc(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// THE ROW CARRIES THE REASON — not only the log line.
func TestFailRunAtStartRecordsTheReasonOnTheRun(t *testing.T) {
	src := readSchedulerSrc(t, "workflow_reconciler.go")
	body := funcBody(t, src, "func (r *WorkflowReconciler) failRunAtStart(")

	// The update that terminalises the run must carry both fields.
	upd := body[strings.Index(body, "db.UpdateWorkflowRun("):]
	if end := strings.Index(upd, "}); err := nil"); end > 0 {
		upd = upd[:end]
	}
	if !strings.Contains(upd, "ErrorMessage:") {
		t.Error("failRunAtStart terminalises the run WITHOUT recording error_message. The reason only reaches a " +
			"WARN line, so the run page shows a failed run with no explanation — the reported 'runtimes fail " +
			"with no errors in the UI'.")
	}
	if !strings.Contains(upd, "ErrorKind:") {
		t.Error("failRunAtStart records no error_kind, so the UI cannot group or phrase the failure class")
	}
}

// AND THE KIND IS CLASSIFIED, not invented per caller.
func TestStartFailureKindClassifiesTheKnownCauses(t *testing.T) {
	cases := map[string]string{
		`runtime container could not be created (image "x", then base "y")`: "runtime",
		`the runtime image "orchicon-runtime:dev" is not downloaded`:       "runtime",
		`workflow version 3 not found`:                                     "preflight",
		`the workflow has no steps`:                                        "preflight",
		`something else entirely`:                                          "start",
	}
	for reason, want := range cases {
		if got := startFailureKind(reason); got != want {
			t.Errorf("startFailureKind(%q) = %q, want %q", reason, got, want)
		}
	}
}

// A FAILED RUN WITH NO MESSAGE IS STILL RENDERED — for rows written before the fix, and any future silent path.
func TestTheFailureCardRendersAFailedRunWithNoMessage(t *testing.T) {
	src := readSchedulerSrc(t, "../../frontend/src/components/workflow-runs/RunFailureCard.tsx")
	if !strings.Contains(src, "failed") {
		t.Fatal("RunFailureCard takes no `failed` prop")
	}
	if !strings.Contains(src, "This run failed before it could start") {
		t.Error("a FAILED run with no error message renders as nothing, which reads as 'nothing happened' " +
			"rather than 'this failed' — the exact symptom reported from the Windows UI")
	}
}
