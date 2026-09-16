package execution

// execution_pane_test.go — regression tests for three operator reports about the Executions pane and
// the step→execution jump. Each one pins a defect that was live in the shipped binary:
//
//  1. "On a workflow run page, hitting enter on a step is not taking you to the execution page for
//     that step."  TWO independent causes, so there are two tests: the key was gated behind a focus
//     the pane's own text did not ask for, and the jump was then reverted by the executions list
//     landing and auto-loading the row under the cursor.
//  2. "they are flicking repeatedly like they are refreshing poorly over and over" and "several of
//     them still look like the old ones and they don't have all of the same details". Both were the
//     same closed loop: a todo landing asked for the DETAIL again, and the detail landing issued the
//     next todo fetch — one round trip per lap, every lap repainting the pane. The flicker test
//     proves the loop terminates; the composition tests prove the pane keeps all its parts.

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
)

// --- report 1a: enter is the operator's TWO-STAGE gesture ---------------------------------------

// Enter on the run MOVES INTO the detail pane; enter AGAIN on a step jumps to its execution.
//
// The operator's model, verbatim: "hitting enter on the run immediately jumps to the execution.
// You should have to hit enter FIRST on the workflow run, THEN it moves to the detail pane and
// then from there enter should select steps."
//
// A previous revision claimed enter unconditionally while a flow was drawn — first press jumped —
// on the reasoning that every row advertises "enter → execution". That misread which enter the row
// advertises: the rows say what enter does once the DETAIL holds the focus, and the first enter is
// what GIVES it the focus. Claiming the key from the list also broke the list, because enter on a
// run row is how the run is opened.
//
// Both presses are asserted here, through the screen's own dispatch, because only the second is
// interesting on its own and a test of the second alone would pass with the first broken.
func TestEnterIsATwoStageGestureOnARun(t *testing.T) {
	p := namePlane()
	p.workflows = []*apiv1.Workflow{{Id: "wf-1", Name: "SDLC"}}
	p.run = &apiv1.WorkflowRun{Id: "run-1", WorkflowId: "wf-1", WorkItemId: "wi-1"}
	p.stepRuns = []*apiv1.WorkflowStepRun{
		stepRun("sr-1", "a", "Architect", apiv1.StepKind_STEP_KIND_TASK, apiv1.StepRunStatus_STEP_RUN_STATUS_SUCCEEDED, 0, 0, "exec-a"),
	}
	m := newModel(t, p)
	m.Base.SelectSource(srcRuns)

	// The run's detail lands, which is what draws the flow.
	title, fields, body, err := m.detail(context.Background(), srcRuns, "run-1")
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	m.Base.DeliverDetailForTest(srcRuns, "run-1", title, body, fields)
	if m.runFlow.count() == 0 {
		t.Fatal("the run drew no step flow — the fixture is wrong, so this test would pass vacuously")
	}
	if m.Base.DetailFocusedForTest() {
		t.Fatal("the fixture left the detail focused, which is not the state the gesture starts in")
	}

	// FIRST enter: into the detail. It must NOT have jumped — that is the report.
	press(t, m, "enter")
	if got := m.Base.ActiveSourceName(); got != srcRuns {
		t.Fatalf("the first enter left the pane on %q — it must move the FOCUS into the detail, not jump", got)
	}
	if !m.Base.DetailFocusedForTest() {
		t.Fatal("the first enter did not move the focus into the detail")
	}

	// SECOND enter: from the detail, on the step, out to its execution.
	cmd := press(t, m, "enter")
	if got := m.Base.ActiveSourceName(); got != srcExecutions {
		t.Fatalf("the second enter left the pane on %q — with the detail focused it must jump to the step's execution", got)
	}
	if cmd == nil {
		t.Error("the jump produced no command, so nothing was requested for the execution")
	}
}

// With NO flow drawn, enter keeps its generic meaning (activate the row / open its detail) — the
// jump takes over exactly where the pane is actually offering one.
func TestEnterWithNoFlowIsNotHijacked(t *testing.T) {
	m := newModel(t, namePlane())
	m.Base.SelectSource(srcRuns)
	m.Base.SetFocusForTest("detail")
	if _, handled := m.handleActionKey("enter"); handled {
		t.Error("enter was claimed with no step flow to jump from — it must fall through to the base")
	}
}

// --- report 1b: the jump must survive the list landing ------------------------------------------

// A jump to an execution that is NOT on the loaded page stays put when the list lands.
//
// The executions list is recent-first and paginated, so a step's execution frequently is not row 0.
// A list landing auto-loads the detail of the row under the cursor, and the jump's claim on the pane
// used to be consumed by the FIRST landing — after which the next refresh (a live event pokes one
// continuously) auto-loaded row 0 and slid the operator off the target they had just jumped to.
func TestJumpSurvivesTheExecutionsListLanding(t *testing.T) {
	p := namePlane()
	p.workflows = []*apiv1.Workflow{{Id: "wf-1", Name: "SDLC"}}
	p.run = &apiv1.WorkflowRun{Id: "run-1", WorkflowId: "wf-1"}
	p.stepRuns = []*apiv1.WorkflowStepRun{
		stepRun("sr-1", "a", "Architect", apiv1.StepKind_STEP_KIND_TASK, apiv1.StepRunStatus_STEP_RUN_STATUS_SUCCEEDED, 0, 0, "exec-target"),
	}
	// The step's execution is what the plane can serve, and the LIST PAGE that lands holds a
	// DIFFERENT execution at row 0 (the list is recent-first and paginated, so this is the normal
	// case, not a contrived one).
	p.exec = &apiv1.WorkerExecution{Id: "exec-target", Status: apiv1.ExecutionStatus_EXECUTION_STATUS_RUNNING}
	m := newModel(t, p)
	m.Base.SelectSource(srcRuns)

	title, fields, body, err := m.detail(context.Background(), srcRuns, "run-1")
	if err != nil {
		t.Fatalf("run detail: %v", err)
	}
	m.Base.DeliverDetailForTest(srcRuns, "run-1", title, body, fields)

	// The gesture: enter into the detail, then enter on the step. (The focus step is asserted on its
	// own above; here the point is that the JUMP survives the list landing that follows it.)
	press(t, m, "enter")
	cmd := press(t, m, "enter")
	if got := m.Base.ActiveSourceName(); got != srcExecutions {
		t.Fatalf("the jump did not switch panes (on %q)", got)
	}
	runAllCmds(t, m, cmd)
	if got, _, _ := m.Base.DetailForTest(); !strings.Contains(got, "exec-target") {
		t.Fatalf("the jump did not show the step's execution: %q", got)
	}

	// The list lands with the OTHER execution under the cursor. This must not take the pane.
	listCmd := m.Base.DeliverFetchForTest(srcExecutions, []screenkit.Item{{ID: "exec-other", Title: "exec-other"}}, "")
	runAllCmds(t, m, listCmd)
	if got, _, _ := m.Base.DetailForTest(); !strings.Contains(got, "exec-target") {
		t.Errorf("the list landing clobbered the jump target — the pane shows %q, not the step's execution", got)
	}
}

// --- report 2a: the refresh loop must terminate -------------------------------------------------

// A todo landing never asks for the detail again, so the fetch cycle has a base case.
//
// This is the flicker. The loop was: detail landing → todo fetch → todo landing → RequestDetail →
// detail landing → … with no exit, one round trip per lap, every lap repainting the pane.
func TestTodoLandingDoesNotReissueTheDetailFetch(t *testing.T) {
	p := namePlane()
	p.exec = &apiv1.WorkerExecution{Id: "exec-1", Status: apiv1.ExecutionStatus_EXECUTION_STATUS_RUNNING}
	p.todos = []*apiv1.TodoItem{{Content: "wire the loop detector", Status: apiv1.TodoStatus_TODO_STATUS_IN_PROGRESS}}
	m := newModel(t, p)
	m.Base.SelectSource(srcExecutions)
	m.Base.DeliverFetchForTest(srcExecutions, []screenkit.Item{{ID: "exec-1", Title: "exec-1", Meta: "running"}}, "")

	// The record lands, so the pane has something to recompose.
	title, fields, body, err := m.detail(context.Background(), srcExecutions, "exec-1")
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	m.Base.DeliverDetailForTest(srcExecutions, "exec-1", title, body, fields)

	// The todo landing's follow-up command is what used to be the next detail fetch.
	_, next := m.Update(execTodosMsg{execID: "exec-1", todos: p.todos})
	if next != nil {
		if msg := next(); msg != nil {
			t.Errorf("the todo landing produced follow-up work (%T) — a landing that requests work is a loop with no base case", msg)
		}
	}
}

// The fetch is issued when the cache is COLD, once, and then not again while it is warm — so the
// refresh is driven by the clock rather than by its own arrival.
func TestTodosFetchIsGatedOnStaleness(t *testing.T) {
	p := namePlane()
	p.exec = &apiv1.WorkerExecution{Id: "exec-1", Status: apiv1.ExecutionStatus_EXECUTION_STATUS_RUNNING}
	m := newModel(t, p)

	if m.todosRefreshCmd("exec-1") == nil {
		t.Error("a cold cache did not request a fetch — the todo list would never appear")
	}
	m.todos.put("exec-1", []*apiv1.TodoItem{{Content: "one"}})
	if m.todosRefreshCmd("exec-1") != nil {
		t.Error("a warm cache still requested a fetch — every detail landing would re-fetch, which is the loop")
	}
}

// --- report 2b: the pane keeps every part, whoever paints ---------------------------------------

// A session repaint does NOT drop the run's facts, and the facts' paint does NOT drop the session.
//
// The session used to install the whole body from four fields of its own (id / session events /
// status), so every live repaint replaced the run's record with that stub — "several of them still
// look like the old ones and they don't have all of the same details".
func TestSessionAndRecordComposeIntoOnePane(t *testing.T) {
	p := namePlane()
	p.exec = &apiv1.WorkerExecution{
		Id: "exec-1", Status: apiv1.ExecutionStatus_EXECUTION_STATUS_RUNNING,
		WorkerName: "DevOps Engineer", WorkflowName: "SDLC", TokenUsage: 4321, CostUsd: 0.12,
		Output: "did the thing",
	}
	m := newModel(t, p)
	m.Base.SelectSource(srcExecutions)
	m.Base.DeliverFetchForTest(srcExecutions, []screenkit.Item{{ID: "exec-1", Title: "exec-1", Meta: "running"}}, "")
	m.todos.put("exec-1", []*apiv1.TodoItem{{Content: "wire the loop detector"}})

	title, fields, body, err := m.detail(context.Background(), srcExecutions, "exec-1")
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	m.Base.DeliverDetailForTest(srcExecutions, "exec-1", title, body, fields)

	// A live session arrives.
	m.RenderSession([]chat.ChatItem{{Kind: chat.KindText, Text: "hello from the worker"}})

	_, afterFields, afterBody := m.Base.DetailForTest()
	after := fieldText(afterFields) + "\n" + afterBody
	for _, want := range []string{"DevOps Engineer", "SDLC", "4321", "wire the loop detector", "did the thing", "hello from the worker"} {
		if !strings.Contains(after, want) {
			t.Errorf("the pane lost %q after the session painted:\n%s", want, after)
		}
	}
}

// The todo list sits ABOVE the record and the transcript: it is context for reading them.
func TestTodoListLeadsTheComposedBody(t *testing.T) {
	p := namePlane()
	p.exec = &apiv1.WorkerExecution{Id: "exec-1", Status: apiv1.ExecutionStatus_EXECUTION_STATUS_RUNNING, Output: "did the thing"}
	m := newModel(t, p)
	m.Base.SelectSource(srcExecutions)
	m.Base.DeliverFetchForTest(srcExecutions, []screenkit.Item{{ID: "exec-1", Title: "exec-1"}}, "")
	m.todos.put("exec-1", []*apiv1.TodoItem{{Content: "wire the loop detector"}})

	_, _, body, err := m.detail(context.Background(), srcExecutions, "exec-1")
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if got := strings.Index(body, "wire the loop detector"); got < 0 {
		t.Fatalf("the todo list is not in the body:\n%s", body)
	} else if got > strings.Index(body, "did the thing") {
		t.Errorf("the todos come after the output — they are context for reading it:\n%s", body)
	}
}

// A session paint that arrives BEFORE the record must not blank the pane: the facts are on their way
// and they carry the fields.
func TestSessionBeforeRecordDoesNotBlankThePane(t *testing.T) {
	p := namePlane()
	p.exec = &apiv1.WorkerExecution{Id: "exec-1", WorkerName: "DevOps Engineer"}
	m := newModel(t, p)
	m.Base.SelectSource(srcExecutions)
	m.Base.DeliverFetchForTest(srcExecutions, []screenkit.Item{{ID: "exec-1", Title: "exec-1"}}, "")

	title, fields, body, err := m.detail(context.Background(), srcExecutions, "exec-1")
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	m.Base.DeliverDetailForTest(srcExecutions, "exec-1", title, body, fields)
	wantTitle, _, _ := m.Base.DetailForTest()

	// A session for a DIFFERENT execution (or one whose record has not landed) must leave the pane
	// alone rather than replacing it with a stub.
	m.RenderSession(nil)
	gotTitle, gotFields, _ := m.Base.DetailForTest()
	if gotTitle != wantTitle {
		t.Errorf("a session paint changed the pane's title: %q → %q", wantTitle, gotTitle)
	}
	if len(gotFields) == 0 {
		t.Error("a session paint emptied the pane's fields")
	}
}

// --- helpers ------------------------------------------------------------------------------------

// runAllCmds drains a command tree (including tea.Batch) through the screen, collecting the
// follow-ups a test needs to reach the end state an operator would see.
func runAllCmds(t *testing.T, m *Model, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		return
	}
	msg := cmd()
	if msg == nil {
		return
	}
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			runAllCmds(t, m, c)
		}
		return
	}
	_, next := m.Update(msg)
	runAllCmds(t, m, next)
}

// fieldText flattens a detail pane's fields for a substring assertion.
func fieldText(fields []screenkit.Field) string {
	var b strings.Builder
	for _, f := range fields {
		b.WriteString(f.Key + ": " + f.Value + "\n")
	}
	return b.String()
}
