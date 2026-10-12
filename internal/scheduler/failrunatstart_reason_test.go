package scheduler

// failrunatstart_reason_test.go — THE REASON MUST SURVIVE THIS FUNCTION'S OWN FAILURE.
//
// `failRunAtStart` is the single place five different run-start failures report through: an
// unresolvable runtime image, an empty step DAG, a missing workflow version, the container-create
// failover, and the serve gate.
//
// Its shape was: attempt the status update, and if THAT fails, return ITS error. The `reason` — the
// only statement of why the run could not start — was discarded on that path, so the reconcile pass
// logged nothing but a failure of the failure handler:
//
//	WARN workflow run reconcile failed ... error="fail run at start: db: not found"
//
// A sentence about the UPDATE, not about the run. It cost several rounds of archaeology: the real
// cause (a container that could not be created — whose Docker error had just been made available by
// errorOrNone, one commit earlier) was computed on the immediately preceding line and thrown away
// here. The operator's report — "failed immediately complaining about the runtime image" — is exactly
// what a message about the runtime image looks like when the image error never reaches a log.
//
// The update CAN still fail: `UpdateWorkflowRun` matches on the snapshot's version, and a stale
// snapshot matches no row. That race is real and separate (a later pass re-fails the run with a fresh
// snapshot, which is why the run does end up terminal). What must not be possible is for it to erase
// the cause.
//
// This is a SOURCE-ORDER assertion, and it is honest about being one: a behaviour test needs a pgx.Tx
// fake of ~20 methods and would pin the crypto of a plumbing interface rather than the property that
// matters — which is that the reason is recorded BEFORE anything that can fail. The ordering IS the
// fix.

import (
	"os"
	"strings"
	"testing"
)

func TestFailRunAtStartLogsTheReasonBeforeAnythingThatCanFail(t *testing.T) {
	src, err := os.ReadFile("workflow_reconciler.go")
	if err != nil {
		t.Fatalf("read workflow_reconciler.go: %v", err)
	}
	s := string(src)

	i := strings.Index(s, "func (r *WorkflowReconciler) failRunAtStart(")
	if i < 0 {
		t.Fatal("failRunAtStart is gone — re-establish the reason-logging property or explain its replacement")
	}
	body := s[i:]
	if j := strings.Index(body, "\nfunc "); j >= 0 {
		body = body[:j]
	}

	logAt := strings.Index(body, `r.log.Warn("workflow run failed at start"`)
	updateAt := strings.Index(body, "db.UpdateWorkflowRun(")

	if logAt < 0 {
		t.Error("failRunAtStart no longer logs the reason. Without it, this function's own failure " +
			"(a stale-snapshot version race) hides the cause completely, and the operator sees only " +
			"'fail run at start: db: not found' — which is a statement about the UPDATE")
	}
	if updateAt < 0 {
		t.Fatal("failRunAtStart no longer updates the run — the shape changed; re-establish both properties")
	}
	if logAt >= 0 && logAt > updateAt {
		t.Error("the reason is logged AFTER the status update. The update is the step that can fail, so " +
			"logging afterwards means the failure it causes discards the reason — which is the bug this " +
			"ordering exists to prevent")
	}

	// And it must be carried in the error too, for a caller that propagates rather than logging.
	if !strings.Contains(body, `"fail run at start (%s): %w"`) {
		t.Error("the returned error must carry the reason as well as the cause — a caller that reports " +
			"the error (rather than logging its own line) otherwise states only that the update failed")
	}
}
