package e2e

// status_test.go — THE LAUNCH LOOP MUST RECOGNISE A FINISHED WORKFLOW RUN.
//
// Reported: "after the first task completed successfully our script didn't move onto the next task".
//
// The two status enums do not agree on the word. A finished WORK ITEM is SUCCEEDED; a finished WORKFLOW RUN
// is COMPLETED. terminalStatus matched SUCCEEDED but not COMPLETED, so a run that had finished never looked
// terminal: waitTask polled until its deadline, and only then did the chain advance — which reads exactly
// like the harness hanging after the first task.
//
// These pin the matcher for every final state either enum can produce, and the controls that keep a
// non-final state from being mistaken for one.

import "testing"

func TestTerminalStatusRecognisesACompletedWorkflowRun(t *testing.T) {
	// The exact strings the RPC returns; the run one is what was missing.
	for _, s := range []string{
		"WORKFLOW_RUN_STATUS_COMPLETED",
		"WORKFLOW_RUN_STATUS_FAILED",
		"WORKFLOW_RUN_STATUS_ABORTED",
		"WORK_ITEM_STATUS_SUCCEEDED",
		"WORK_ITEM_STATUS_FAILED",
		"WORK_ITEM_STATUS_CANCELLED",
		"WORK_ITEM_STATUS_SKIPPED",
		"WORK_ITEM_STATUS_ARCHIVED",
	} {
		if !terminalStatus(s) {
			t.Errorf("%s is a FINAL state and was not recognised — a wait on it spins to its deadline, and the "+
				"chain looks stuck", s)
		}
	}
}

// THE CONTROLS. A non-final state must not look terminal, or the chain advances past work still running.
func TestNonFinalStatusIsNotTerminal(t *testing.T) {
	for _, s := range []string{
		"WORKFLOW_RUN_STATUS_PENDING",
		"WORKFLOW_RUN_STATUS_RUNNING",
		"WORKFLOW_RUN_STATUS_PAUSED",
		"WORK_ITEM_STATUS_PENDING",
		"WORK_ITEM_STATUS_READY",
		"WORK_ITEM_STATUS_ASSIGNED",
		"WORK_ITEM_STATUS_RUNNING",
		"WORK_ITEM_STATUS_CHECKPOINTING",
		"WORK_ITEM_STATUS_RECOVERING",
		"",
	} {
		if terminalStatus(s) {
			t.Errorf("%s is NOT final and was treated as terminal — the chain would advance past live work", s)
		}
	}
}

// shortStatus is what the report prints, so a wrong trim would mislabel every row.
func TestShortStatusNamesTheState(t *testing.T) {
	if got := shortStatus("WORK_ITEM_STATUS_SUCCEEDED"); got != "succeeded" {
		t.Errorf("shortStatus(WORK_ITEM_STATUS_SUCCEEDED) = %q, want %q", got, "succeeded")
	}
	if got := shortStatus("WORKFLOW_RUN_STATUS_COMPLETED"); got == "" {
		t.Error("shortStatus returned empty for a completed run")
	}
}
