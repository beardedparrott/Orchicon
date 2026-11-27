package workflow

import (
	"os"
	"strings"
	"testing"
)

// WHY THIS FILE EXISTS.
//
// Work item 01M84WD1G42BHD1HTCB2YMQ82Z was paused and sat in `checkpointing` indefinitely. Nothing
// was in flight — QA had failed 110 minutes earlier — so there was nothing to checkpoint, yet the
// item was stamped `checkpointing` anyway.
//
// That is a DEADLOCK, not a slow operation, because `checkpointing` is in IsActiveRunStatus:
//
//	RESUME   refuses ("cannot RESUME a work item that is running")
//	PAUSE    refuses (no run in flight)
//	RESTART  refuses (isInFlight is true)
//	reconciler ignores a `paused` run, so nothing revisits the item
//
// The operator sees "checkpointing for a while" with no error and no way out.
//
// These tests pin the two halves that break the deadlock: the pause must describe what ACTUALLY
// happened, and the resume guard must ask the RUN rather than the symptom status.

func readRunControl(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("run_control.go")
	if err != nil {
		t.Fatalf("read run_control.go: %v", err)
	}
	return string(b)
}

// TestPauseDoesNotStampCheckpointingWhenNothingWasCheckpointed is the writer half.
func TestPauseDoesNotStampCheckpointingWhenNothingWasCheckpointed(t *testing.T) {
	src := readRunControl(t)

	if !strings.Contains(src, "paused++") {
		t.Error("pauseRunInner does not count what it actually paused, so it cannot tell a real " +
			"checkpoint from an idle pause — and a paused-with-nothing-running item gets stamped " +
			"`checkpointing` and deadlocks")
	}
	// SUPERSEDED: the park status is now `paused`, because `pending` means "on deck" to the sequence
	// reconciler, which arms it. See pause_parks_test.go.
	if !strings.Contains(src, "status := domain.WorkItemPaused") {
		t.Error("pause no longer parks the item as `paused`")
	}
	// STRONGER RULE, superseding the "gate it on paused > 0" version: pause NEVER produces a real
	// checkpoint. It ends the session and resume re-establishes it, so `checkpointing` described a
	// state that never existed — and because that status is in IsActiveRunStatus, it locked every
	// gesture out. Twice: once with nothing in flight, once with an orphaned `terminating` row.
	if strings.Contains(src, "domain.WorkItemCheckpointing") {
		t.Error("pause stamps `checkpointing` again. Pause does not checkpoint: it ends the session " +
			"and resume re-dispatches. `checkpointing` is an active status, so stamping it makes the " +
			"item look busy forever and every gesture that could free it refuses.")
	}
}

// TestPauseIsNotPermanentWhenNothingIsInFlight documents the invariant the operator hit.
func TestPauseIsNotPermanentWhenNothingIsInFlight(t *testing.T) {
	src := readRunControl(t)
	// An idle pause must land the item somewhere RESUMABLE and NOT auto-armed: `paused` is both.
	if !strings.Contains(src, "WorkItemPaused") {
		t.Fatal("an idle pause does not park the item as `paused`")
	}
	if strings.Contains(src, "domain.WorkItemCheckpointing") {
		t.Fatal("pause stamps checkpointing — an ACTIVE status, which locks every gesture out")
	}
}

// TestPauseSignalsTheLiveSession is the defect the operator actually hit the second time.
//
// The execution row was flipped to `terminating` and the LIVE WORKER WAS NEVER TOLD, so it kept
// running while the row claimed to be stopping — indefinitely. AbortWorkflow had always called the
// abort hook; pause had no equivalent, which made Pause a status change that stopped nothing.
func TestPauseSignalsTheLiveSession(t *testing.T) {
	b, err := os.ReadFile("service.go")
	if err != nil {
		t.Fatalf("read service.go: %v", err)
	}
	src := strings.ReplaceAll(string(b), "\t", "")
	i := strings.Index(src, "func (s *Service) PauseWorkflowRun")
	if i < 0 {
		t.Fatal("PauseWorkflowRun is gone")
	}
	body := src[i:]
	if end := strings.Index(body, "\nfunc "); end > 0 {
		body = body[:end]
	}
	if !strings.Contains(body, "s.abortSession != nil") || !strings.Contains(body, "s.abortSession(ctx, eid") {
		t.Error("PauseWorkflowRun does not call the live-session abort hook. Setting the row to " +
			"`terminating` stops nothing: the worker keeps running and the row says terminating forever.")
	}
}

// TestResumeReArmsRatherThanResurrects: flipping a dead execution back to `running` points at a
// process that no longer exists.
func TestResumeReArmsRatherThanResurrects(t *testing.T) {
	// The resume core lives in run_control.go, shared by the run RPC and the work item page.
	b, err := os.ReadFile("run_control.go")
	if err != nil {
		t.Fatalf("read run_control.go: %v", err)
	}
	src := strings.ReplaceAll(string(b), "\t", "")
	i := strings.Index(src, "func resumeRunInnerReturning")
	if i < 0 {
		t.Fatal("resumeRunInnerReturning is gone")
	}
	body := src[i:]
	if end := strings.Index(body, "\nfunc "); end > 0 {
		body = body[:end]
	}
	if !strings.Contains(body, "StepRunPending") {
		t.Error("resume does not re-arm the interrupted step, so nothing is dispatched again and the " +
			"session it claims to continue does not exist")
	}
	if strings.Contains(body, "running := domain.ExecutionRunning") {
		t.Error("resume resurrects the paused execution by writing `running` — that row points at a " +
			"session pause already ended")
	}
}

// TestResumeCoreMovesTheWorkItem is the bug the operator hit: the item stayed `paused` while the run
// was already `running`.
//
// The status update lived in the ResumeWorkflowRun RPC WRAPPER, not in the shared core — so the run
// page moved the item and the WORK ITEM page did not. The operator saw "kicked it off", then an item
// that had not moved, then a second click that failed because the RUN was running while the ITEM
// still said paused. Two surfaces, two answers — the same divergence that made leaf RESUME a no-op.
//
// It belongs in the core because it is not optional: a resumed run whose item still reads `paused`
// is a state no gesture can reconcile.
func TestResumeCoreMovesTheWorkItem(t *testing.T) {
	src := readRunControl(t)

	i := strings.Index(src, "func resumeRunInnerReturning")
	if i < 0 {
		t.Fatal("resumeRunInnerReturning is gone")
	}
	body := src[i:]
	if end := strings.Index(body, "\nfunc "); end > 0 {
		body = body[:end]
	}
	if !strings.Contains(body, "domain.WorkItemRunning") {
		t.Error("the shared resume core does not move the work item to running. It lives only in the " +
			"RPC wrapper, so resuming from the WORK ITEM page leaves the item `paused` while the run " +
			"is already running — and every subsequent gesture disagrees with the run.")
	}
	if !strings.Contains(body, "UpdateWorkItem") {
		t.Error("the shared resume core never writes the work item")
	}
}

// TestResumeRPCWrapperHasNoSecondCopy: one implementation, or the two drift again.
func TestResumeRPCWrapperHasNoSecondCopy(t *testing.T) {
	b, err := os.ReadFile("service.go")
	if err != nil {
		t.Fatalf("read service.go: %v", err)
	}
	src := strings.ReplaceAll(string(b), "\t", "")
	i := strings.Index(src, "func (s *Service) ResumeWorkflowRun")
	if i < 0 {
		t.Fatal("ResumeWorkflowRun is gone")
	}
	body := src[i:]
	if end := strings.Index(body, "\nfunc "); end > 0 {
		body = body[:end]
	}
	if strings.Contains(body, "status := domain.WorkItemRunning") {
		t.Error("the RPC wrapper sets the work item status itself again — that duplicate is what let " +
			"the work item page diverge from the run page. It belongs in the shared core only.")
	}
}

// TestResumeReArmsAFailedStepInsideAPausedRun — the operator's rule.
//
// "If you pause during a failure, hitting resume starts from the failed step and retries from that
// point."
//
// Resume previously re-armed only `running` and `recovering` steps. A run paused WHILE a step had
// failed (the loop had not yet routed it) therefore resumed with that step still `failed` and nothing
// for the reconciler to dispatch: running, with a dead step, forever.
func TestResumeReArmsAFailedStepInsideAPausedRun(t *testing.T) {
	src := readRunControl(t)
	i := strings.Index(src, "func resumeRunInnerReturning")
	if i < 0 {
		t.Fatal("resumeRunInnerReturning is gone")
	}
	body := src[i:]
	if end := strings.Index(body, "\nfunc "); end > 0 {
		body = body[:end]
	}

	if !strings.Contains(body, "domain.StepRunFailed") {
		t.Fatal("resume does not re-arm a FAILED step, so a run paused during a failure resumes with " +
			"nothing to dispatch — the exact state the operator hit")
	}
	// It must be in the re-arm switch alongside running/recovering, not somewhere unrelated.
	if !strings.Contains(body, "case domain.StepRunRunning, domain.StepRunRecovering, domain.StepRunFailed:") {
		t.Error("failed is not in the re-arm switch with running/recovering")
	}
	// And attempted count must survive: this is the same attempt continuing.
	if strings.Contains(body, "Attempt:") {
		t.Error("resume resets Attempt. A pause/resume is the same attempt continuing; only a genuine " +
			"retry (RetryRunInTx) resets it, and giving a resumed step a fresh budget is how a loop " +
			"burns its attempts unnoticed.")
	}
}
