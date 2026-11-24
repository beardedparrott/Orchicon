package scheduler

import (
	"strings"
	"testing"
)

// A STALLED OR TIMED-OUT RUN MUST REACH RECOVERY, NOT DIE AS "FAILED".
//
// The two mechanisms that broke it, and what each test pins:
//
//  1. OnStall terminated the execution as `unhealthy`, a status pollTaskStep's switch does not
//     recognise. It fell to `default: return false, false` — "still in flight" — so the step wedged in
//     `recovering` forever and NO recovery was ever triggered. The work item was then failed underneath
//     it. That is the operator's symptom verbatim: marked failed, never resumed.
//
//  2. A stall or a wall-clock timeout spent a max_attempts slot like any other failure, so three
//     timeouts consumed a 3-attempt budget and the run died before the recovery engine's own budget was
//     touched.
//
// The bound is the RECOVERY ENGINE's per-run retry budget (5, escalating to L3 = a human block). These
// tests must never grow a second bound here.

func TestOnStallTerminatesTheExecutionAsFailed(t *testing.T) {
	src := readSource(t, "reconciler.go")
	body := funcBody(t, src, "func (r *TaskReconciler) OnStall(")
	if !strings.Contains(body, "domain.ExecutionFailed") {
		t.Errorf("OnStall must terminate the execution as Failed — `unhealthy` is invisible to the step " +
			"poller, so the step wedges in recovering and no recovery ever runs")
	}
	if strings.Contains(body, "r.updateExecStatus(ctx, execID, domain.ExecutionUnhealthy") {
		t.Errorf("OnStall still writes ExecutionUnhealthy as the terminal status")
	}
	// A late stall signal must not rewrite a verdict OnResult already delivered.
	if !strings.Contains(body, "executionReachedTerminalStatus") {
		t.Errorf("OnStall must not overwrite an execution that already reached a terminal status")
	}
}

func TestStepPollerTreatsUnhealthyAsTerminal(t *testing.T) {
	src := readSource(t, "workflow_reconciler.go")
	body := funcBody(t, src, "func (r *WorkflowReconciler) pollTaskStep(")
	i := strings.Index(body, "switch exec.Status {")
	if i < 0 {
		t.Fatal("pollTaskStep no longer switches on exec.Status")
	}
	head := body[i:]
	// Only the region up to the recovery fall-through matters.
	if j := strings.Index(head, "fall through to the recovery block below"); j > 0 {
		head = head[:j]
	}
	if !strings.Contains(head, "domain.ExecutionUnhealthy") {
		t.Errorf("pollTaskStep must treat ExecutionUnhealthy as terminal-to-recovery: executions started " +
			"by the previously-deployed plane are in that status on a fatal stall, and without it they " +
			"fall through as 'still in flight' and wedge their step")
	}
}

func TestLivenessFaultReachesRecoveryDespiteTheAttemptCap(t *testing.T) {
	src := readSource(t, "workflow_reconciler.go")
	body := funcBody(t, src, "func (r *WorkflowReconciler) pollTaskStep(")
	if !strings.Contains(body, "ResumableFaultReason") {
		t.Fatalf("pollTaskStep no longer classifies a liveness fault, so a stall/timeout spends the step's " +
			"attempt budget and the run dies before recovery's own budget is touched")
	}
	// The cap must consult the classification rather than returning unconditionally.
	i := strings.Index(body, "sr.Attempt >= rc.MaxAttempts-1")
	if i < 0 {
		t.Fatal("the attempt cap is gone — if that was deliberate, this test needs rewriting, not deleting")
	}
	tail := body[i : i+600]
	if !strings.Contains(tail, `if resumable == ""`) {
		t.Errorf("the attempt cap must still fail a NON-liveness exhaustion; only a stall/timeout may " +
			"bypass it. Otherwise every failure loops forever.")
	}
}

// This is the classification that decides whether a run is recovered or routed. If it is too broad, a
// worker's deliberate rejection gets "recovered" (re-running the same review against unchanged work,
// burning the whole budget and never reaching the fix branch). If it is too narrow, a stall dies.
func TestResumableFaultReason(t *testing.T) {
	resumable := []string{
		"stalled:no_progress",
		"stalled:no_progress:nudge_budget_spent",
		"stalled:liveness_probe_timeout",
		"claude session stalled: no_progress",
		"wall_clock_timeout",
		"  wall_clock_timeout  ",
	}
	for _, msg := range resumable {
		if got := resumableFaultReason(msg); got == "" {
			t.Errorf("%q is a liveness fault but was classified as a judgement — it would be routed "+
				"instead of recovered", msg)
		}
	}

	judgements := []string{
		"",
		"worker reported failure in its own ORCHICON WORKER SUMMARY: failure",
		"ORCHICON WORKER SUMMARY: failure",
		"claude session exited without a terminal result message (exit code 1)",
		"provider stream failed: 429 rate limited",
		"step-sse failed: acceptance criteria 5 unmet",
	}
	for _, msg := range judgements {
		if got := resumableFaultReason(msg); got != "" {
			t.Errorf("%q is a delivered judgement or an ordinary failure but was classified as a "+
				"liveness fault (%q) — recovery would re-run the same work against the same review",
				msg, got)
		}
	}
}
