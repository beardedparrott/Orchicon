package execution

// run_actions_test.go — the two run actions that could NEVER fire, and the delete that now exists.
//
// THE OPERATOR, mid-live-test: "Executions and Workflow Runs do not have a delete operation (single and
// bulk)." Investigating that turned up a SECOND, larger defect on the same pane: both of its existing
// actions were dead on real data.
//
//	fetchRuns put the RAW LOWERED ENUM in the row's Meta — "workflow_run_status_failed" — while
//	actionsForSelection gated the actions on the BARE WORD:
//
//	    if meta == "failed"      { retry run … }
//	    if meta == "running"     { force-progress … }
//
// so neither comparison could ever be true and NEITHER ACTION WAS EVER OFFERED. A pinned-but-dead
// feature, invisible to every test because no test drove a row built by the real fetch.
//
// Measured before the fix: apiv1.WorkflowRunStatus_WORKFLOW_RUN_STATUS_FAILED.String() lowers to
// "workflow_run_status_failed", and "workflow_run_status_failed" == "failed" is false.
//
// These tests build rows THROUGH THE REAL FETCH, which is the only layer at which the defect is true.

import (
	"context"
	"strings"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// runsPlane is a plane serving one workflow run with the given status.
func runsPlane(status apiv1.WorkflowRunStatus) *fakePlane {
	p := &fakePlane{}
	p.workflows = []*apiv1.Workflow{{Id: "wf-1", Name: "SDLC (Human)"}}
	p.runs = []*apiv1.WorkflowRun{{Id: "run-1", WorkflowId: "wf-1", Status: status}}
	return p
}

// runsPane focuses the Runs pane with rows delivered by the REAL fetch.
func runsPane(t *testing.T, status apiv1.WorkflowRunStatus) *Model {
	t.Helper()
	m := newModel(t, runsPlane(status))
	if !m.Base.SelectSource(srcRuns) {
		t.Fatal("fixture: could not focus the Runs pane")
	}
	items, _, err := m.fetchRuns(context.Background(), "")
	if err != nil {
		t.Fatalf("fetchRuns: %v", err)
	}
	m.Base.LoadItems(srcRuns, items, "")
	m.Base.SelectItem(srcRuns, "run-1")
	return m
}

// THE META IS THE BARE WORD — the value the chords compare against AND the value the row prints.
func TestRunRowMetaIsTheBareStatusWord(t *testing.T) {
	for _, tc := range []struct {
		status apiv1.WorkflowRunStatus
		want   string
	}{
		{apiv1.WorkflowRunStatus_WORKFLOW_RUN_STATUS_FAILED, "failed"},
		{apiv1.WorkflowRunStatus_WORKFLOW_RUN_STATUS_RUNNING, "running"},
		{apiv1.WorkflowRunStatus_WORKFLOW_RUN_STATUS_COMPLETED, "completed"},
	} {
		items, _, err := newModel(t, runsPlane(tc.status)).fetchRuns(context.Background(), "")
		_ = items
		if err != nil {
			t.Fatalf("fetchRuns: %v", err)
		}
		if got := items[0].Meta; got != tc.want {
			t.Errorf("a %s run's meta = %q, want %q — the row must print what the pane compares against",
				tc.status, got, tc.want)
		}
	}
}

// RETRY IS OFFERED ON A FAILED RUN. Before the fix this could never fire: the gate compared the bare
// word against the enum.
func TestFailedRunOffersRetry(t *testing.T) {
	m := runsPane(t, apiv1.WorkflowRunStatus_WORKFLOW_RUN_STATUS_FAILED)

	a, ok := m.actionByKey(keyRetryRun)
	if !ok {
		t.Fatalf("a FAILED run offers no retry — the action is gated on a status comparison that the "+
			"real row data never satisfies. Actions: %v", actionLabels(m.actionsForSelection()))
	}
	if !strings.Contains(a.Confirm, "run-1") {
		t.Errorf("the retry confirm does not name the run: %q", a.Confirm)
	}
}

// FORCE-PROGRESS IS OFFERED ON A RUNNING RUN, same reason.
func TestRunningRunOffersForceProgress(t *testing.T) {
	m := runsPane(t, apiv1.WorkflowRunStatus_WORKFLOW_RUN_STATUS_RUNNING)
	if _, ok := m.actionByKey(keyForceProgress); !ok {
		t.Fatalf("a RUNNING run offers no force-progress — its gate has the same defect. Actions: %v",
			actionLabels(m.actionsForSelection()))
	}
}

// THE DELETE EXISTS on a run in ANY state, and names what it destroys.
func TestRunDeleteExistsForEveryStatus(t *testing.T) {
	for _, st := range []apiv1.WorkflowRunStatus{
		apiv1.WorkflowRunStatus_WORKFLOW_RUN_STATUS_COMPLETED,
		apiv1.WorkflowRunStatus_WORKFLOW_RUN_STATUS_FAILED,
		apiv1.WorkflowRunStatus_WORKFLOW_RUN_STATUS_RUNNING,
		apiv1.WorkflowRunStatus_WORKFLOW_RUN_STATUS_PENDING,
	} {
		m := runsPane(t, st)
		a, ok := m.actionByKey(keyDelete)
		if !ok {
			t.Fatalf("a %s run offers no delete — the operator asked for exactly this", st)
		}
		if !strings.Contains(strings.ToUpper(a.Confirm), "CANNOT BE UNDONE") {
			t.Errorf("a %s run's delete confirm is not explicit about being irreversible: %q", st, a.Confirm)
		}
	}
}

// A LIVE RUN'S DELETE WARNS that it will be aborted first, and a terminal one does not.
func TestRunDeleteWarnsOnlyWhenLive(t *testing.T) {
	for _, tc := range []struct {
		status   apiv1.WorkflowRunStatus
		wantWarn bool
	}{
		{apiv1.WorkflowRunStatus_WORKFLOW_RUN_STATUS_RUNNING, true},
		{apiv1.WorkflowRunStatus_WORKFLOW_RUN_STATUS_PENDING, true},
		{apiv1.WorkflowRunStatus_WORKFLOW_RUN_STATUS_COMPLETED, false},
		{apiv1.WorkflowRunStatus_WORKFLOW_RUN_STATUS_FAILED, false},
	} {
		m := runsPane(t, tc.status)
		a, _ := m.actionByKey(keyDelete)
		got := strings.Contains(a.Confirm, "STILL RUNNING")
		if got != tc.wantWarn {
			t.Errorf("a %s run's delete confirm warns=%v, want %v: %q", tc.status, got, tc.wantWarn, a.Confirm)
		}
	}
}

// THE DELETE WRITES DeleteWorkflowRun for the focused id.
func TestRunDeleteWritesTheFocusedID(t *testing.T) {
	m := runsPane(t, apiv1.WorkflowRunStatus_WORKFLOW_RUN_STATUS_COMPLETED)
	var got []string
	m.rpcDeleteRun = func(_ context.Context, id string) error {
		got = append(got, id)
		return nil
	}
	a, _ := m.actionByKey(keyDelete)
	if err := a.Do(context.Background()); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(got) != 1 || got[0] != "run-1" {
		t.Errorf("deleted %v, want exactly [run-1]", got)
	}
}

// BULK: marking runs replaces the single-row actions with a counted delete.
func TestRunBulkDeleteAppearsForASelection(t *testing.T) {
	p := &fakePlane{}
	p.workflows = []*apiv1.Workflow{{Id: "wf-1", Name: "SDLC (Human)"}}
	p.runs = []*apiv1.WorkflowRun{
		{Id: "run-1", WorkflowId: "wf-1", Status: apiv1.WorkflowRunStatus_WORKFLOW_RUN_STATUS_COMPLETED},
		{Id: "run-2", WorkflowId: "wf-1", Status: apiv1.WorkflowRunStatus_WORKFLOW_RUN_STATUS_FAILED},
		{Id: "run-3", WorkflowId: "wf-1", Status: apiv1.WorkflowRunStatus_WORKFLOW_RUN_STATUS_RUNNING},
	}
	m := newModel(t, p)
	if !m.Base.SelectSource(srcRuns) {
		t.Fatal("fixture: could not focus the Runs pane")
	}
	items, _, err := m.fetchRuns(context.Background(), "")
	if err != nil {
		t.Fatalf("fetchRuns: %v", err)
	}
	m.Base.LoadItems(srcRuns, items, "")
	m.Base.SelectItem(srcRuns, "run-1")
	press(t, m, " ")
	press(t, m, " ")
	press(t, m, " ")

	a, ok := labelFor(m, "delete 3 selected")
	if !ok {
		t.Fatalf("marking three runs produced no bulk delete. Actions: %v",
			actionLabels(m.actionsForSelection()))
	}
	if a.Key != keyDelete {
		t.Errorf("the bulk run delete answers %q, want the shared %q", a.Key, keyDelete)
	}
	if !strings.Contains(a.Confirm, "3 workflow runs") {
		t.Errorf("the bulk confirm does not say how many it destroys: %q", a.Confirm)
	}
	if !strings.Contains(a.Confirm, "STILL RUNNING") {
		t.Errorf("the bulk confirm misses that one of the marked runs is live: %q", a.Confirm)
	}
}
