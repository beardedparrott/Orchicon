package recovery

import "testing"

// TestEvidenceRejectionIsRecognised pins which failures count as the PLATFORM refusing the work.
//
// The distinction decides whether a resume is a retry or a repetition: a stalled session lost its transcript
// and may finish next time; a rejected report was read, judged, and refused, so only a CHANGED report can
// pass — and change requires knowing the objection.
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
//
// The pattern list makes a rejection stop-class (right in principle), and that alone would stop on the FIRST
// rejection — throwing away the one retry that can now succeed, because the seed carries WHY. So the first
// rejection retries WITH the reason and the second stops. That pair is the difference between repeating and
// correcting; run 01M6TB0MQ2EXDRW6EX8GAR0DT3 was six repetitions because neither half existed.
func TestEvidenceRejectionIsOneRetryThenStop(t *testing.T) {
	// The policy lives in recoveryStrategyFor, which needs a DB for countTaskChainRecoveries. The PART that
	// is decidable without one — that a rejection is stop-class — is asserted here, and the retry-once
	// branch is covered by the integration path.
	if !isDeterministicStopError("the worker reported success but the evidence contradicts it — not substantiated: x") {
		t.Fatal("an evidence rejection must be in the deterministic stop-class pattern list, or it falls " +
			"through to summarize_restart and loops exactly as the reported run did")
	}
	if !isDeterministicStopError("not substantiated: work_present") {
		t.Fatal("the gate's own per-check prefix must also be stop-class")
	}
}
