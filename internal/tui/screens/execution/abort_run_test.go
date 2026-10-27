package execution

// abort_run_test.go — THE TUI MUST BE ABLE TO ABORT A LIVE WORKFLOW RUN.
//
// REPORTED: "I didn't see the abort workflow run command in the TUI." It was not
// hidden behind a condition — it did not exist. The TUI had no AbortWorkflow
// caller at all:
//
//   - `c` on the Executions pane calls CancelExecution: that stops ONE WORKER, the
//     run stays `running`, and a run whose steps are not dispatching has no
//     execution to cancel, so it cannot be stopped that way either;
//   - `delete` on the Runs pane removes the row.
//
// Neither ends a live run, which is the gesture the GUI has always offered as
// "Stop <run>" on the workflow page (frontend/src/routes/workflows_.$id.tsx,
// gated on latestRunActive). The consequence was concrete: a run wedged
// `running` could only be terminalised from the GUI — and until it was terminal,
// the runtime-container sweep could not reap its container either.
//
// These tests assert the action EXISTS on a live run, that it reaches ABORT (not
// cancel), and that it is offered only where it can work.

import (
	"context"
	"strings"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// TestAbortRunIsOfferedOnALiveRun is the direct regression: the action exists.
func TestAbortRunIsOfferedOnALiveRun(t *testing.T) {
	m := runsPane(t, apiv1.WorkflowRunStatus_WORKFLOW_RUN_STATUS_RUNNING)

	a, ok := m.actionByKey(keyAbortRun)
	if !ok {
		t.Fatalf("a RUNNING run offers no abort (%q) — the TUI had no AbortWorkflow caller at all, "+
			"so a wedged run could only be stopped from the GUI. Actions: %v",
			keyAbortRun, actionLabels(m.actionsForSelection()))
	}
	if !a.Danger {
		t.Error("aborting a run must be marked dangerous — it fails in-flight steps and terminates " +
			"running workers")
	}
	if a.Confirm == "" {
		t.Error("aborting a run must be confirmed")
	}
}

// TestAbortRunCallsAbortNotCancel: the whole distinction. Cancel stops one worker and
// leaves the run running; abort is what makes the run terminal.
func TestAbortRunCallsAbortNotCancel(t *testing.T) {
	p := runsPlane(apiv1.WorkflowRunStatus_WORKFLOW_RUN_STATUS_RUNNING)
	m := newModel(t, p)
	if !m.Base.SelectSource(srcRuns) {
		t.Fatal("fixture: could not focus the Runs pane")
	}

	if err := m.rpcAbortRun(context.Background(), "run-1"); err != nil {
		t.Fatalf("rpcAbortRun: %v", err)
	}
	if len(p.aborted) != 1 || p.aborted[0] != "run-1" {
		t.Fatalf("AbortWorkflow calls = %v, want [run-1]", p.aborted)
	}
	if len(p.cancel) != 0 {
		t.Errorf("aborting a run also cancelled %d execution(s) directly — abort ends the RUN (the "+
			"server terminates its executions); issuing CancelExecution too would double-handle it",
			len(p.cancel))
	}
}

// TestAbortIsNotOfferedOnAFinishedRun: aborting a terminal run is a refusal the server
// would only echo, so the action is gated the way the GUI gates its Stop button.
func TestAbortIsNotOfferedOnAFinishedRun(t *testing.T) {
	for _, st := range []apiv1.WorkflowRunStatus{
		apiv1.WorkflowRunStatus_WORKFLOW_RUN_STATUS_COMPLETED,
		apiv1.WorkflowRunStatus_WORKFLOW_RUN_STATUS_FAILED,
		apiv1.WorkflowRunStatus_WORKFLOW_RUN_STATUS_ABORTED,
	} {
		m := runsPane(t, st)
		if _, ok := m.actionByKey(keyAbortRun); ok {
			t.Errorf("the abort action is offered on a %s run — the server refuses it, and a chord "+
				"that cannot work teaches the operator the row lies", st)
		}
	}
}

// TestTheAbortChordIsAdvertisedOnlyOnALiveRun pins the guidance: the composer's row
// names it while a live run is focused, and nowhere near a finished one.
func TestTheAbortChordIsAdvertisedOnlyOnALiveRun(t *testing.T) {
	live := runsPane(t, apiv1.WorkflowRunStatus_WORKFLOW_RUN_STATUS_RUNNING)
	if !live.runIsLive() {
		t.Fatal("runIsLive() = false on a running row")
	}
	if h := live.HintLine(); !strings.Contains(h, keyAbortRun+": abort run") {
		t.Errorf("the Runs pane's hint does not name %q on a live run: %q", keyAbortRun+": abort run", h)
	}

	done := runsPane(t, apiv1.WorkflowRunStatus_WORKFLOW_RUN_STATUS_COMPLETED)
	if done.runIsLive() {
		t.Fatal("runIsLive() = true on a completed row")
	}
	if h := done.HintLine(); strings.Contains(h, keyAbortRun+": abort run") {
		t.Errorf("the hint names the abort chord on a completed run: %q", h)
	}
}

// TestAbortRunHintAnswersForTheRunsPaneOnly: the composer names the abort chord where it
// works, and nowhere else. A hint is a promise about a key binding.
func TestAbortRunHintAnswersForTheRunsPaneOnly(t *testing.T) {
	live := runsPane(t, apiv1.WorkflowRunStatus_WORKFLOW_RUN_STATUS_RUNNING)
	if got := live.AbortRunHint(); !strings.Contains(got, keyAbortRun+" abort run") {
		t.Errorf("AbortRunHint() on a live run = %q, want it to name %q", got, keyAbortRun+" abort run")
	}

	done := runsPane(t, apiv1.WorkflowRunStatus_WORKFLOW_RUN_STATUS_COMPLETED)
	if got := done.AbortRunHint(); got != "" {
		t.Errorf("AbortRunHint() on a completed run = %q, want empty", got)
	}

	// A different pane contributes nothing even with a live run in scope: `S` is the Runs
	// pane's chord and would be inert elsewhere.
	live2 := runsPane(t, apiv1.WorkflowRunStatus_WORKFLOW_RUN_STATUS_RUNNING)
	if !live2.Base.SelectSource(srcExecutions) {
		t.Skip("this fixture does not expose the Executions pane")
	}
	if got := live2.AbortRunHint(); got != "" {
		t.Errorf("AbortRunHint() on the Executions pane = %q, want empty — the abort chord is not "+
			"bound there", got)
	}
}
