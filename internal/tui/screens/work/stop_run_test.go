package work

// stop_run_test.go — A RUNNING ITEM CAN BE STOPPED FROM THE PANE IT IS RUNNING IN,
// AND THE COMPOSER SAYS SO.
//
// REPORTED: an operator watching a run escalate could not stop it. There was no
// chord on the Work pane and nothing on the composer's affordance row. The only
// cancel lived on the Executions pane (`c`), described only on that pane's hint.
//
// These tests pin the three things that were missing: the action exists, it
// cancels the LIVE EXECUTION (not just the item's status), and the guidance is
// DERIVED — advertised on a running row, absent otherwise, so it never teaches a
// dead key.

import (
	"context"
	"strings"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// runningPlane seeds one RUNNING item plus a live execution for it.
func runningPlane(t *testing.T) (*fakePlane, *Model) {
	t.Helper()
	p := newPlane()
	p.seedProject("proj-1", "Orchicon")
	p.addItem(&apiv1.WorkItem{
		Id: "wi-run", Title: "Running thing", ProjectId: "proj-1",
		Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK, Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_RUNNING,
	})
	p.addExec(&apiv1.WorkerExecution{
		Id: "exec-live", TaskId: "wi-run", Status: apiv1.ExecutionStatus_EXECUTION_STATUS_RUNNING,
	})
	m := newModel(t, p)
	// Load the row explicitly: the pane's rows come from a fetch Cmd the test does not
	// run, so the fixture states the row (and its status pill) the operator would see.
	if !m.Base.SelectSource(srcWorkItems) {
		t.Fatal("fixture: could not focus the Work Items pane")
	}
	if !m.Base.LoadItems(srcWorkItems, []kit2.Item{
		{ID: "wi-run", Title: "Running thing", Meta: statusPill(apiv1.WorkItemStatus_WORK_ITEM_STATUS_RUNNING)},
	}, "") {
		t.Fatal("fixture: could not load the running row")
	}
	if !m.Base.SelectItem(srcWorkItems, "wi-run") {
		t.Fatal("fixture: could not select the running row")
	}
	return p, m
}

// TestStopRunIsOfferedOnARunningItem: the chord exists where the run is.
func TestStopRunIsOfferedOnARunningItem(t *testing.T) {
	_, m := runningPlane(t)
	found := false
	for _, a := range m.itemActions() {
		if a.Key == keyStopRun && strings.Contains(a.Label, "stop execution") {
			found = true
			if !a.Danger {
				t.Error("stopping a run must be marked dangerous — it abandons in-flight tool calls")
			}
			if a.Confirm == "" {
				t.Error("stopping a run must be confirmed: it is destructive and irreversible for the worker")
			}
		}
	}
	if !found {
		t.Fatalf("no %q action on a RUNNING item — this is the gap the operator reported: there was "+
			"no way to stop a run from the pane it runs in", keyStopRun)
	}
}

// TestStopRunCancelsTheExecutionNotJustTheItem: cancelling the ITEM is a status
// change; the worker keeps going and keeps spending. The operator's "stop" means
// the execution.
func TestStopRunCancelsTheExecutionNotJustTheItem(t *testing.T) {
	p, m := runningPlane(t)
	if err := m.rpcStopRun(context.Background(), "wi-run"); err != nil {
		t.Fatalf("rpcStopRun: %v", err)
	}
	if len(p.cancelled) != 1 {
		t.Fatalf("cancelled %d executions, want 1 — stopping the ITEM instead would leave the worker "+
			"running and spending (recorded: %v)", len(p.cancelled), p.cancelled)
	}
	if p.cancelled[0] != "exec-live" {
		t.Errorf("cancelled %q, want the live execution %q", p.cancelled[0], "exec-live")
	}
	// The item itself must NOT be soft-deleted as a side effect: "stop the run" is
	// not "cancel the work item".
	for _, id := range p.deleted {
		if id == "wi-run" {
			t.Error("stopping the run soft-deleted the work item — those are different gestures")
		}
	}
}

// TestStopRunWithNothingRunningSaysSo: an inert chord is indistinguishable from a
// broken one, so the no-op is a NAMED outcome.
func TestStopRunWithNothingRunningSaysSo(t *testing.T) {
	p := newPlane()
	p.seedProject("proj-1", "Orchicon")
	p.addItem(&apiv1.WorkItem{
		Id: "wi-idle", Title: "Idle", ProjectId: "proj-1",
		Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK, Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING,
	})
	m := newModel(t, p)
	err := m.rpcStopRun(context.Background(), "wi-idle")
	if err == nil {
		t.Fatal("stopping an item with no running execution reported success — the operator would read " +
			"that as 'stopped' and the worker would still be running")
	}
	if !strings.Contains(err.Error(), "nothing to stop") {
		t.Errorf("error = %q, want it to say there is nothing to stop", err)
	}
}

// TestStopHintIsAdvertisedOnlyWhileRunning is the anti-dead-key rule: the guidance
// appears on a running row and disappears otherwise.
func TestStopHintIsAdvertisedOnlyWhileRunning(t *testing.T) {
	_, running := runningPlane(t)
	if got := running.StopRunHint(); !strings.Contains(got, keyStopRun+" stop execution") {
		t.Errorf("StopRunHint() on a RUNNING item = %q, want it to name %q — the composer had no stop "+
			"guidance at all, which is what the operator reported", got, keyStopRun)
	}
	// ASSERT THE SEGMENT, NOT THE BARE KEY. keyStopRun is "x", which is a substring of the
	// unrelated "ctrl+x: delete" — a Contains(hint, "x") check would pass on a row that
	// advertises nothing (it did, on the first run of this test).
	if !strings.Contains(running.HintLine(), keyStopRun+": stop execution") {
		t.Errorf("the pane's hint line does not name %q on a running row: %q", keyStopRun+": stop execution", running.HintLine())
	}

	// A non-running row advertises nothing.
	p := newPlane()
	p.seedProject("proj-1", "Orchicon")
	p.addItem(&apiv1.WorkItem{
		Id: "wi-pending", Title: "Not running", ProjectId: "proj-1",
		Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK, Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING,
	})
	m := newModel(t, p)
	if !m.Base.SelectSource(srcWorkItems) {
		t.Fatal("fixture: could not focus the Work Items pane")
	}
	if !m.Base.LoadItems(srcWorkItems, []kit2.Item{
		{ID: "wi-pending", Title: "Not running", Meta: statusPill(apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING)},
	}, "") {
		t.Fatal("fixture: could not load the row")
	}
	if !m.Base.SelectItem(srcWorkItems, "wi-pending") {
		t.Fatal("fixture: could not select")
	}
	if got := m.StopRunHint(); got != "" {
		t.Errorf("StopRunHint() on a PENDING item = %q, want empty — advertising a chord that does "+
			"nothing is how a hint row stops being trusted", got)
	}
	if strings.Contains(m.HintLine(), keyStopRun+": stop execution") {
		t.Errorf("the hint line names the stop chord on a non-running row: %q", m.HintLine())
	}
}

// TestStopHintIgnoresOtherPanes: the probe answers only for the Work pane, so the
// composer never shows this chord while a different screen is active.
func TestStopHintIgnoresOtherPanes(t *testing.T) {
	_, m := runningPlane(t)
	m.SelectSource(srcProjects)
	if got := m.StopRunHint(); got != "" {
		t.Errorf("StopRunHint() on the Projects pane = %q, want empty — the chord would be inert there", got)
	}
}
