package scheduler

// worker_verdict_test.go — the work item's outcome must follow the WORKER'S verdict, not the process exit.
//
// REPORTED: a run ended with "ORCHICON WORKER SUMMARY: failure — Nothing implemented or committed" (every
// write had been permission-refused) and was recorded Status: succeeded, workflow run completed, work item
// succeeded at v4 — where it sat in a success terminal state for ~6.5 hours. The summary word WAS captured
// the whole time (into `_decision`, for the run view); nothing consulted it before writing the status.

import (
	"testing"
)

// workerOutcome applies OnResult's downgrade rule without a database, which is the whole decision.
func workerOutcome(adapterSucceeded bool, output string) bool {
	if adapterSucceeded {
		if d := extractSummaryDecision(output); d == "failure" {
			return false
		}
	}
	return adapterSucceeded
}

// THE REPORTED CASE: the process exited cleanly, the worker said failure.
func TestWorkerFailureVerdictDowngradesASuccessfulProcess(t *testing.T) {
	out := "Work done.\n\nORCHICON WORKER SUMMARY: failure — Nothing implemented or committed. " +
		"Write/Edit permission was denied for all project files."
	if workerOutcome(true, out) {
		t.Fatal("a worker that reported FAILURE in its own summary was recorded as succeeded — the summary " +
			"word is captured for display but must also GATE the outcome, or a run that did nothing is " +
			"indistinguishable from a shipped one")
	}
}

// A successful worker stays successful.
func TestWorkerSuccessVerdictIsUnchanged(t *testing.T) {
	if !workerOutcome(true, "ORCHICON WORKER SUMMARY: success — implemented and committed") {
		t.Fatal("a success verdict was downgraded")
	}
}

// A FAILED PROCESS IS NEVER UPGRADED. A crash, a reclaimed container or a liveness reap outranks anything
// the model emitted, so text can only ever lower the outcome.
func TestAWorkerSummaryNeverUpgradesAFailedProcess(t *testing.T) {
	if workerOutcome(false, "ORCHICON WORKER SUMMARY: success — all done") {
		t.Fatal("a FAILED process was upgraded to success by worker text — the process outcome is " +
			"authoritative and text may only downgrade it")
	}
}

// A MISSING MARKER CHANGES NOTHING: output can be truncated, or an adapter may emit none.
func TestAbsentSummaryLeavesTheAdapterVerdictAlone(t *testing.T) {
	for _, out := range []string{"", "some output with no marker", "partial work, ran out of budget"} {
		if !workerOutcome(true, out) {
			t.Errorf("output %q downgraded a successful execution — absence of a marker is a reporting "+
				"omission, not a failure", out)
		}
	}
}

// A CUSTOM decision word (a reviewer's "approved"/"rejected") must not be read as failure: only the task
// vocabulary's `failure` downgrades, or review steps would invert.
func TestCustomDecisionWordsDoNotDowngrade(t *testing.T) {
	for _, out := range []string{
		"ORCHICON WORKER SUMMARY: approved — looks good",
		"ORCHICON WORKER SUMMARY: rejected — needs work",
		"ORCHICON WORKER SUMMARY: pending",
	} {
		if !workerOutcome(true, out) {
			t.Errorf("custom decision in %q was treated as a failure — firstWordAsDecision passes unknown "+
				"words through verbatim, so only an explicit `failure` may downgrade", out)
		}
	}
}
