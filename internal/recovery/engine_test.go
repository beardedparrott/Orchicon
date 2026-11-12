package recovery

import "testing"

// TestEvidenceRejectionIsRecognised pins which failures count as the PLATFORM refusing the work.
func TestEvidenceRejectionIsRecognised(t *testing.T) {
	rejections := []string{
		"the worker reported success but the evidence contradicts it — not substantiated: project_gate: the project's declared gate FAILED",
		"not substantiated: work_present: HEAD did not move",
		"the worker reported success but the evidence contradicts it — not substantiated: tree_clean: uncommitted changes remain",
	}
	for _, r := range rejections {
		if !isEvidenceRejection(r) {
			t.Errorf("%q must be recognised as an evidence rejection", r)
		}
	}
	notRejections := []string{
		"", // no message
		"stalled:no_progress; execution ended before the final model step completed",
		"container exited with code 137",
		"credential absent",
	}
	for _, r := range notRejections {
		if isEvidenceRejection(r) {
			t.Errorf("%q is NOT an evidence rejection — a stall or crash must keep normal recovery", r)
		}
	}
}

// TestEvidenceRejectionIsOneRetryThenStop pins the policy that replaced a six-attempt loop.
func TestEvidenceRejectionIsOneRetryThenStop(t *testing.T) {
	// The policy lives in recoveryStrategyFor, which needs a DB for countTaskChainRecoveries.
	if !isDeterministicStopError("the worker reported success but the evidence contradicts it — not substantiated: x") {
		t.Fatal("an evidence rejection must be in the deterministic stop-class pattern list, or it falls " +
			"through to summarize_restart and loops exactly as the reported run did")
	}
	if !isDeterministicStopError("not substantiated: work_present") {
		t.Fatal("the gate's own per-check prefix must also be stop-class")
	}
}

// A WORKER'S OWN VERDICT IS STOP-CLASS, for the same reason an evidence rejection is: the worker ran to
// completion and judged the work unacceptable, so a resume loop re-runs the same review against unchanged
// work. Run 01M70EPBN9ZK0FM1N6HJBGH6H5 burned six attempts on exactly that and never reached the loop's
// fix branch.
func TestAWorkerVerdictIsStopClass(t *testing.T) {
	reported := "worker reported failure in its own ORCHICON WORKER SUMMARY: The gate is green, but the " +
		"layout is broken and I reproduced it independently."
	if !isDeterministicStopError(reported) {
		t.Fatal("the worker-verdict failure is not in the deterministic stop-class list, so it falls through " +
			"to summarize_restart and recovers — the six-attempt burn")
	}
	// THE CONTROL: a genuine fault stays recoverable.
	if isDeterministicStopError("container exited 137") ||
		isDeterministicStopError("claude session exited without a terminal result message (exit code 1)") == false {
		t.Fatal("the stop-class list must keep ordinary faults recoverable")
	}
}
