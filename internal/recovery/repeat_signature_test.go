package recovery

// repeat_signature_test.go — THE SECOND IDENTICAL FAILURE IS NOT WORTH RETRYING.
//
// Reported: six executions of one claude run, 0 tokens each, ~280ms apart, every one with the same sentence —
// "claude session exited without a terminal result message (exit code 1)". Recovery resumed five times against
// a failure that a resume could not change, and the operator's budget and attention paid for it.
//
// The pattern list could not catch it: that sentence names a SYMPTOM and no cause, so it belonged to no class
// anyone had anticipated. What the repetition proves is not the cause but the futility — the same message twice
// means the second attempt did not change anything.

import (
	"strings"
	"testing"
)

// TestTheReportedSignatureIsStopClass — the exact text, because it is the one that looped.
func TestTheReportedSignatureIsStopClass(t *testing.T) {
	const reported = "claude session exited without a terminal result message (exit code 1)"
	if !isDeterministicStopError(reported) {
		t.Fatalf("%q is not classified as a deterministic stop, so recovery resumes a CLI that never reached "+
			"the model — five more times, which is what was reported", reported)
	}
}

// TestTheCredentialClassStillStops — the pre-existing classification must not have been disturbed.
func TestTheCredentialClassStillStops(t *testing.T) {
	for _, msg := range []string{
		"claude credentials not mounted",
		"authentication failed: invalid api key",
		"failed_to_start: adapter exited 1",
	} {
		if !isDeterministicStopError(msg) {
			t.Errorf("%q must remain stop-class", msg)
		}
	}
}

// TestAGenuineFailureIsStillRecoverable — the other direction, which a too-greedy pattern set would break.
func TestAGenuineFailureIsStillRecoverable(t *testing.T) {
	for _, msg := range []string{
		"context deadline exceeded while running go test",
		"worker reported failure: tests are red, 3 failing",
		"",
	} {
		if isDeterministicStopError(msg) {
			t.Errorf("%q must NOT be stop-class — it is a failure a resume can actually change", msg)
		}
	}
}

// TestNormaliseMakesRepetitionsComparable — formatting must not disguise a repeat.
func TestNormaliseMakesRepetitionsComparable(t *testing.T) {
	a := normaliseFailureText("claude  session EXITED\nwithout a terminal result message (exit code 1)")
	b := normaliseFailureText("claude session exited without a terminal result message (exit code 1)")
	if a != b {
		t.Errorf("the same failure formatted two ways compared unequal:\n  %q\n  %q", a, b)
	}
	// And different causes must NOT compare equal.
	c := normaliseFailureText("claude session exited without a terminal result message (exit code 2)")
	if a == c {
		t.Error("two different exit codes normalised to the same text — the repeat check would stop a run " +
			"whose failure had genuinely changed")
	}
	if strings.TrimSpace(normaliseFailureText("   ")) != "" {
		t.Error("a blank message must normalise to empty, so it never looks like a repeat")
	}
}
