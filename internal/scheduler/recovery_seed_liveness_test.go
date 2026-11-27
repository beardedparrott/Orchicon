package scheduler

import (
	"strings"
	"testing"
)

// The liveness/rejection split decides what a resumed worker is TOLD to do, so each case asserts the
// text the worker actually receives — not just that a predicate returned a bool.

func TestIsLivenessFailureReason(t *testing.T) {
	liveness := []string{
		"stalled:no_progress",
		"stalled:tool_hang:bash",
		"stalled:no_file_progress:liveness_probe_no_response",
		"stalled:missing_decision_signal:completion_probe_no_response",
		"stalled:repetition:bash|{}",
		"stalled:no_file_progress:nudge_budget_spent",
		"wall_clock_timeout",
		"cancelled",
		"timeout",
		// Case and surrounding whitespace must not change the verdict: the reason is recorded verbatim.
		"  Stalled:no_progress  ",
	}
	for _, r := range liveness {
		if !isLivenessFailureReason(r) {
			t.Errorf("isLivenessFailureReason(%q) = false, want true (a liveness stop must be treated as an interruption)", r)
		}
	}

	rejections := []string{
		"",
		"   ",
		"the worker reported success but the evidence contradicts it: AC3 unverified",
		"not substantiated: no file:line evidence for criterion 2",
		"worker reported failure in its own ORCHICON WORKER SUMMARY",
		"model ref \"\" has no provider/model",
	}
	for _, r := range rejections {
		if isLivenessFailureReason(r) {
			t.Errorf("isLivenessFailureReason(%q) = true, want false (a judgement on the work must NOT be treated as an interruption)", r)
		}
	}
}

// TestRecoveryCorrectionMatchesFailureClass is the regression guard for the defect that made every
// recovery start over: a `stalled:no_progress` resume was handed the "REJECTED — redo it" correction,
// so the worker re-ran finished work from scratch.
func TestRecoveryCorrectionMatchesFailureClass(t *testing.T) {
	got := recoveryRedoCorrection("stalled:no_progress")
	if got != recoveryInterruptedNotRejected {
		t.Fatalf("a stall must resume as an INTERRUPTION, got %q", got)
	}
	if strings.Contains(got, "REDO") || strings.Contains(got, "REJECTED") {
		t.Errorf("the interruption correction must not tell the worker its work was rejected/redo it: %q", got)
	}
	if !strings.Contains(got, "Do NOT start over") {
		t.Errorf("the interruption correction must explicitly forbid starting over: %q", got)
	}

	got = recoveryRedoCorrection("not substantiated: AC2 has no evidence")
	if got != recoveryRedoSupersedesAlreadyDone {
		t.Fatalf("a platform rejection must still tell the worker to CHANGE the work, got %q", got)
	}
}

// TestRecoveryFileContentCarriesLivenessCorrection asserts the correction that actually reaches the
// worker through the seed FILE (the DB-row path), since that file is what the prompt points at.
func TestRecoveryFileContentCarriesLivenessCorrection(t *testing.T) {
	seed := &recoverySeed{
		Summary:        "Execution ABC failed after 100 tokens.",
		FailedExecID:   "ABC",
		FailedWorkerID: "w_x",
		FailureReason:  "stalled:no_progress",
	}
	content := buildRecoveryFileContent("Someone", seed, "")
	if strings.Contains(content, "REJECTED by the platform") {
		t.Fatalf("a stalled resume's seed file must not claim the work was rejected:\n%s", content)
	}
	if !strings.Contains(content, recoveryInterruptedNotRejected) {
		t.Fatalf("a stalled resume's seed file must carry the interruption correction:\n%s", content)
	}
	// The reason itself must still be stated — it is the whole point of the resume.
	if !strings.Contains(content, "stalled:no_progress") {
		t.Errorf("the seed file must state the recorded failure reason:\n%s", content)
	}

	// A rejection keeps the redo correction.
	rejected := &recoverySeed{
		Summary:        "Execution DEF failed.",
		FailedExecID:   "DEF",
		FailedWorkerID: "w_x",
		FailureReason:  "not substantiated: AC3 has no evidence",
	}
	content = buildRecoveryFileContent("Someone", rejected, "")
	if !strings.Contains(content, recoveryRedoSupersedesAlreadyDone) {
		t.Fatalf("a rejected resume's seed file must keep the redo correction:\n%s", content)
	}
}

// TestRecoveryPromptBlockCarriesLivenessCorrection asserts the same split on the PROMPT block, which is
// the copy the agent sees when it never opens the file.
func TestRecoveryPromptBlockCarriesLivenessCorrection(t *testing.T) {
	stalled := &recoverySeed{FailureReason: "stalled:tool_hang:bash", Summary: "s"}
	block := recoveryFileReferenceBlock(stalled)
	if strings.Contains(block, "REJECTED by the platform") {
		t.Fatalf("the prompt block for a tool hang must not claim rejection:\n%s", block)
	}
	if !strings.Contains(block, recoveryInterruptedNotRejected) {
		t.Fatalf("the prompt block for a tool hang must carry the interruption correction:\n%s", block)
	}

	rejected := &recoverySeed{FailureReason: "the worker reported success but the evidence contradicts it", Summary: "s"}
	block = recoveryFileReferenceBlock(rejected)
	if !strings.Contains(block, recoveryRedoSupersedesAlreadyDone) {
		t.Fatalf("the prompt block for a rejection must keep the redo correction:\n%s", block)
	}
}
