package execution

// run_flow_test.go — a workflow run rendered as a FLOW of its steps, and the jump from a step to
// its execution.
//
// The operator: "I think we should change the view on workflow runs in the tui to mimic a similar
// look to our new workflow view where we have the steps, and it should show next to the steps if
// it succeeded, failed, how many retries, etc. and you should be able to see live runs and if you
// hit enter on a particular step (whether it's complete or still running), it should take you to
// the execution."

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/timestamppb"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// stepRun builds a step run with the fields the flow renders.
func stepRun(id, stepID, name string, kind apiv1.StepKind, status apiv1.StepRunStatus, attempt, iter int32, execID string) *apiv1.WorkflowStepRun {
	return &apiv1.WorkflowStepRun{
		Id: id, StepId: stepID, StepName: name, StepKind: kind, Status: status,
		Attempt: attempt, Iteration: iter, WorkerExecutionId: execID,
		StartedAt: timestamppb.Now(),
	}
}

// A run's steps render with their STATUS, KIND, RETRY COUNT and timing — the four things the
// operator asked to see "next to the steps".
func TestRunFlowShowsStatusKindRetriesAndTiming(t *testing.T) {
	rows := runStepRows([]*apiv1.WorkflowStepRun{
		stepRun("sr-1", "a", "Architect", apiv1.StepKind_STEP_KIND_TASK, apiv1.StepRunStatus_STEP_RUN_STATUS_SUCCEEDED, 0, 0, "exec-a"),
		stepRun("sr-2", "b", "Engineer", apiv1.StepKind_STEP_KIND_TASK, apiv1.StepRunStatus_STEP_RUN_STATUS_RUNNING, 2, 0, "exec-b"),
		stepRun("sr-3", "c", "Reviewer", apiv1.StepKind_STEP_KIND_APPROVAL, apiv1.StepRunStatus_STEP_RUN_STATUS_FAILED, 0, 0, ""),
	})
	body, offsets := renderRunFlow(rows, 100, "b")

	for _, want := range []string{
		"Architect", "Engineer", "Reviewer", // the names
		"succeeded", "running", "failed", // the statuses, as words not enum names
		"task", "approval", // the kinds
		"2 retries",   // the attempt count
		"✓", "●", "✗", // the status glyphs
		"enter → execution", // where enter goes
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the flow does not show %q:\n%s", want, body)
		}
	}
	// The raw proto enum must not leak.
	if strings.Contains(body, "STEP_RUN_STATUS_") || strings.Contains(body, "STEP_KIND_") {
		t.Errorf("the flow leaks proto enum names:\n%s", body)
	}
	// Every step has a line offset, so the pane can follow the cursor.
	for _, id := range []string{"a", "b", "c"} {
		if _, ok := offsets[id]; !ok {
			t.Errorf("step %q has no line offset — the pane cannot scroll to it", id)
		}
	}
}

// The CURSOR is marked, and it is the only mark (a second marker would make "which step is
// selected" ambiguous).
func TestRunFlowMarksOnlyTheCursor(t *testing.T) {
	rows := runStepRows([]*apiv1.WorkflowStepRun{
		stepRun("sr-1", "a", "One", apiv1.StepKind_STEP_KIND_TASK, apiv1.StepRunStatus_STEP_RUN_STATUS_SUCCEEDED, 0, 0, "e1"),
		stepRun("sr-2", "b", "Two", apiv1.StepKind_STEP_KIND_TASK, apiv1.StepRunStatus_STEP_RUN_STATUS_SUCCEEDED, 0, 0, "e2"),
	})
	body, _ := renderRunFlow(rows, 100, "b")
	if got := strings.Count(body, "▸"); got != 1 {
		t.Errorf("the marker appears %d times, want exactly once:\n%s", got, body)
	}
	// And it is on the SELECTED step's row, not merely present.
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, "▸") && !strings.Contains(line, "Two") {
			t.Errorf("the marker is on the wrong row: %q", line)
		}
	}
}

// A LOOPED step has several iterations. The ACTIVE one represents the step; the superseded ones
// are still shown (history is worth seeing) but are marked as history.
func TestRunFlowGroupsIterationsUnderTheStep(t *testing.T) {
	active := stepRun("sr-2", "a", "DevOps", apiv1.StepKind_STEP_KIND_TASK, apiv1.StepRunStatus_STEP_RUN_STATUS_RUNNING, 0, 1, "e2")
	old := stepRun("sr-1", "a", "DevOps", apiv1.StepKind_STEP_KIND_TASK, apiv1.StepRunStatus_STEP_RUN_STATUS_FAILED, 0, 0, "e1")
	old.SupersededBy = "sr-2"

	rows := runStepRows([]*apiv1.WorkflowStepRun{old, active})
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want both iterations", len(rows))
	}
	if !rows[0].active {
		t.Error("the ACTIVE iteration must come first — it is what the step IS right now")
	}
	if rows[1].active {
		t.Error("a superseded iteration must not be marked active")
	}
	// The cursor resolves to the ACTIVE row, and the cursor can only walk active rows.
	s := &runFlowState{}
	s.setRows(rows, "run-1")
	if cur := s.current(); cur == nil || cur.runID != "sr-2" {
		t.Fatalf("cursor = %+v, want the active iteration", cur)
	}
	s.moveSteps(1) // only one navigable row, so it clamps
	if cur := s.current(); cur == nil || cur.runID != "sr-2" {
		t.Errorf("the cursor moved onto a superseded iteration: %+v", cur)
	}
	// The superseded row says so, so the operator is not left wondering why the step has two
	// rows.
	body, _ := renderRunFlow(rows, 100, "a")
	if !strings.Contains(body, "superseded") {
		t.Errorf("a superseded iteration is not labelled:\n%s", body)
	}
}

// The cursor MOVES over the steps, clamps at both ends, and repaints.
func TestRunFlowCursorWalksAndClamps(t *testing.T) {
	s := &runFlowState{}
	s.setRows(runStepRows([]*apiv1.WorkflowStepRun{
		stepRun("sr-1", "a", "One", apiv1.StepKind_STEP_KIND_TASK, apiv1.StepRunStatus_STEP_RUN_STATUS_SUCCEEDED, 0, 0, "e1"),
		stepRun("sr-2", "b", "Two", apiv1.StepKind_STEP_KIND_TASK, apiv1.StepRunStatus_STEP_RUN_STATUS_RUNNING, 0, 0, "e2"),
		stepRun("sr-3", "c", "Three", apiv1.StepKind_STEP_KIND_TASK, apiv1.StepRunStatus_STEP_RUN_STATUS_PENDING, 0, 0, ""),
	}), "run-1")

	if got := s.sel(); got != "a" {
		t.Fatalf("the cursor starts on %q, want the first step", got)
	}
	s.moveSteps(1)
	if got := s.sel(); got != "b" {
		t.Fatalf("after down the cursor is %q, want b", got)
	}
	s.moveSteps(-1)
	if got := s.sel(); got != "a" {
		t.Fatalf("after up the cursor is %q, want a", got)
	}
	s.moveSteps(-5)
	if got := s.sel(); got != "a" {
		t.Errorf("the cursor overshot the start: %q", got)
	}
	s.moveSteps(100)
	if got := s.sel(); got != "c" {
		t.Errorf("the cursor overshot the end: %q", got)
	}
}

// A RELOAD must keep the cursor on the same STEP, or a live run's flow would re-seat the
// highlight every time an event arrived — which is exactly when the operator is watching it.
func TestRunFlowCursorSurvivesALiveReload(t *testing.T) {
	s := &runFlowState{}
	first := runStepRows([]*apiv1.WorkflowStepRun{
		stepRun("sr-1", "a", "One", apiv1.StepKind_STEP_KIND_TASK, apiv1.StepRunStatus_STEP_RUN_STATUS_SUCCEEDED, 0, 0, "e1"),
		stepRun("sr-2", "b", "Two", apiv1.StepKind_STEP_KIND_TASK, apiv1.StepRunStatus_STEP_RUN_STATUS_RUNNING, 0, 0, "e2"),
	})
	s.setRows(first, "run-1")
	s.moveSteps(1)
	if got := s.sel(); got != "b" {
		t.Fatalf("fixture: cursor = %q, want b", got)
	}

	// The run advanced: step a is now FAILED with a retry, and a third step appeared.
	second := runStepRows([]*apiv1.WorkflowStepRun{
		stepRun("sr-1", "a", "One", apiv1.StepKind_STEP_KIND_TASK, apiv1.StepRunStatus_STEP_RUN_STATUS_FAILED, 1, 0, "e1"),
		stepRun("sr-2", "b", "Two", apiv1.StepKind_STEP_KIND_TASK, apiv1.StepRunStatus_STEP_RUN_STATUS_SUCCEEDED, 0, 0, "e2"),
		stepRun("sr-3", "c", "Three", apiv1.StepKind_STEP_KIND_TASK, apiv1.StepRunStatus_STEP_RUN_STATUS_RUNNING, 0, 0, "e3"),
	})
	s.setRows(second, "run-1")
	if got := s.sel(); got != "b" {
		t.Errorf("the cursor moved to %q on a live reload — it must stay on the step it was on", got)
	}
	if cur := s.current(); cur == nil || cur.status != "succeeded" {
		t.Errorf("the cursor row is stale: %+v — the reload's new status must be visible", cur)
	}
}

// Selecting a DIFFERENT run re-seats the cursor: the step ids of one run mean nothing in another.
func TestRunFlowCursorResetsForANewRun(t *testing.T) {
	s := &runFlowState{}
	s.setRows(runStepRows([]*apiv1.WorkflowStepRun{
		stepRun("sr-1", "a", "One", apiv1.StepKind_STEP_KIND_TASK, apiv1.StepRunStatus_STEP_RUN_STATUS_SUCCEEDED, 0, 0, "e1"),
		stepRun("sr-2", "b", "Two", apiv1.StepKind_STEP_KIND_TASK, apiv1.StepRunStatus_STEP_RUN_STATUS_SUCCEEDED, 0, 0, "e2"),
	}), "run-1")
	s.moveSteps(1)

	s.setRows(runStepRows([]*apiv1.WorkflowStepRun{
		stepRun("sr-9", "b", "Two", apiv1.StepKind_STEP_KIND_TASK, apiv1.StepRunStatus_STEP_RUN_STATUS_RUNNING, 0, 0, "e9"),
	}), "run-2")
	if got := s.sel(); got != "b" {
		t.Fatalf("the cursor is %q; it must start at the first step of the NEW run", got)
	}
	// Even though the step id happens to match, the row is the new run's — proven by the
	// execution it points at.
	if cur := s.current(); cur == nil || cur.executionID != "e9" {
		t.Errorf("the cursor row belongs to the previous run: %+v", cur)
	}
}

// --- the jump ---------------------------------------------------------------

// ENTER on a step goes to that step's execution — the operator's central ask.
func TestEnterOnAStepJumpsToItsExecution(t *testing.T) {
	m := newModel(t, &fakePlane{})
	m.Base.SelectSource(srcRuns)
	m.runFlow.setRows(runStepRows([]*apiv1.WorkflowStepRun{
		stepRun("sr-1", "a", "One", apiv1.StepKind_STEP_KIND_TASK, apiv1.StepRunStatus_STEP_RUN_STATUS_SUCCEEDED, 0, 0, "exec-a"),
		stepRun("sr-2", "b", "Two", apiv1.StepKind_STEP_KIND_TASK, apiv1.StepRunStatus_STEP_RUN_STATUS_RUNNING, 0, 0, "exec-b"),
	}), "run-1")

	cmd := m.goToRunStepExecution()
	if m.Base.ActiveSourceName() != srcExecutions {
		t.Errorf("the jump left the pane on %q, want the Executions pane", m.Base.ActiveSourceName())
	}
	if cmd == nil {
		t.Error("the jump produced no command — it must reload and request the execution")
	}
}

// A step with NO execution explains itself rather than silently doing nothing — and the two
// causes (not dispatched yet, not a worker step) both have no execution, so the message has to
// work for both.
func TestEnterOnAStepWithoutAnExecutionRefuses(t *testing.T) {
	m := newModel(t, &fakePlane{})
	m.Base.SelectSource(srcRuns)
	m.runFlow.setRows(runStepRows([]*apiv1.WorkflowStepRun{
		stepRun("sr-1", "a", "Approval", apiv1.StepKind_STEP_KIND_APPROVAL, apiv1.StepRunStatus_STEP_RUN_STATUS_PENDING, 0, 0, ""),
	}), "run-1")

	m.goToRunStepExecution()
	if !strings.Contains(m.notice, "no execution linked") {
		t.Errorf("notice = %q — a step with no execution must say so", m.notice)
	}
	if m.Base.ActiveSourceName() != srcRuns {
		t.Error("a refused jump must not move the pane")
	}
}

// The keys are reachable through the SCREEN's dispatch, and ONLY with the detail focused: with
// the list focused the same keys still move the LIST, which is the operator's navigation model.
func TestRunFlowKeysAreScopedToTheFocusedDetail(t *testing.T) {
	m := newModel(t, &fakePlane{})
	m.Base.SelectSource(srcRuns)
	m.runFlow.setRows(runStepRows([]*apiv1.WorkflowStepRun{
		stepRun("sr-1", "a", "One", apiv1.StepKind_STEP_KIND_TASK, apiv1.StepRunStatus_STEP_RUN_STATUS_SUCCEEDED, 0, 0, "e1"),
		stepRun("sr-2", "b", "Two", apiv1.StepKind_STEP_KIND_TASK, apiv1.StepRunStatus_STEP_RUN_STATUS_SUCCEEDED, 0, 0, "e2"),
	}), "run-1")

	// List focused: the vertical keys belong to the list, so the screen must NOT claim them.
	if _, handled := m.handleActionKey("down"); handled {
		t.Error("down was handled with the LIST focused — it must move the list, not the step cursor")
	}

	// Detail focused: they walk the steps.
	m.Base.SetFocusForTest("detail")
	if _, handled := m.handleActionKey("down"); !handled {
		t.Fatal("down was not handled with the detail focused")
	}
	if got := m.runFlow.sel(); got != "b" {
		t.Errorf("the step cursor is %q, want b", got)
	}
	if _, handled := m.handleActionKey("up"); !handled {
		t.Fatal("up was not handled with the detail focused")
	}
	if got := m.runFlow.sel(); got != "a" {
		t.Errorf("the step cursor is %q, want a", got)
	}
}

// The run DETAIL renders the flow, so the pane the operator opens shows the steps rather than a
// list of step names.
func TestRunDetailRendersTheStepFlow(t *testing.T) {
	p := namePlane()
	p.workflows = []*apiv1.Workflow{{Id: "wf-1", Name: "SDLC"}}
	p.run = &apiv1.WorkflowRun{Id: "run-1", WorkflowId: "wf-1", WorkItemId: "wi-1"}
	p.stepRuns = []*apiv1.WorkflowStepRun{
		stepRun("sr-1", "a", "Architect", apiv1.StepKind_STEP_KIND_TASK, apiv1.StepRunStatus_STEP_RUN_STATUS_SUCCEEDED, 0, 0, "exec-a"),
		stepRun("sr-2", "b", "Engineer", apiv1.StepKind_STEP_KIND_TASK, apiv1.StepRunStatus_STEP_RUN_STATUS_RUNNING, 0, 0, "exec-b"),
	}
	m := newModel(t, p)
	m.Base.SelectSource(srcRuns)

	title, fields, body, err := m.detail(context.Background(), srcRuns, "run-1")
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if !strings.Contains(title, "run-1") {
		t.Errorf("title = %q", title)
	}
	for _, want := range []string{"Architect", "Engineer", "succeeded", "running"} {
		if !strings.Contains(body, want) {
			t.Errorf("the run detail body does not show %q:\n%s", want, body)
		}
	}
	// The header says how many steps and WHERE enter will go, so the gesture is discoverable
	// from the pane rather than only from the hint line.
	got := map[string]string{}
	for _, f := range fields {
		got[f.Key] = f.Value
	}
	if got["steps"] != "2" {
		t.Errorf("steps = %q, want 2", got["steps"])
	}
	if !strings.Contains(got["selected"], "exec-a") {
		t.Errorf("selected = %q, want the cursor step's execution", got["selected"])
	}
}
