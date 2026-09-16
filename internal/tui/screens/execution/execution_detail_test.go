package execution

// execution_detail_test.go — the Executions detail: the run's facts, the worker's todo list, and
// the follow-up box.
//
// The operator: "Executions should definitely mimic the GUI as much as possible in the detail pane
// and also have the nudge/follow up box including the full recorded or live stream of the worker
// and context, token, cost, tool, todo list etc. information just like in the GUI (within reason
// of the limitation of a text interface of course)."
//
// The transcript and the nudge already existed (chat.MergeSessionItems feeds the body, and the `i`
// interject form calls SendExecutionMessage exactly as the GUI's composer does). These tests cover
// what was added: the run's own facts, the TODOS, and the FOLLOW-UP.

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/timestamppb"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// execPlane is a plane carrying one execution with everything the detail can show.
func execPlane() *fakePlane {
	return &fakePlane{
		exec: &apiv1.WorkerExecution{
			Id: "exec-1", WorkerId: "w_1", WorkerName: "Senior Engineer",
			WorkflowName: "SDLC (Human)", TaskId: "wi-1", Iteration: 1,
			Status:      apiv1.ExecutionStatus_EXECUTION_STATUS_SUCCEEDED,
			HealthState: apiv1.HealthState_HEALTH_STATE_HEALTHY,
			TokenUsage:  12345, CostUsd: 0.4321,
			WorktreeBranch: "orch/wi-1", WorktreeStatus: "ready",
			PrUrl: "https://example.test/pr/7", PrState: "open",
			StartedAt: timestamppb.Now(), EndedAt: timestamppb.Now(),
			Output: "did the thing",
		},
	}
}

// The detail carries the GUI's context strip: who ran it, on what, where, for how much.
func TestExecutionDetailShowsTheRunsFacts(t *testing.T) {
	m := newModel(t, execPlane())
	m.Base.SelectSource(srcExecutions)
	m.Base.LoadItems(srcExecutions, []kit2.Item{{ID: "exec-1", Title: "exec-1", Meta: "succeeded"}}, "")

	_, fields, body, err := m.detail(context.Background(), srcExecutions, "exec-1")
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	got := map[string]string{}
	for _, f := range fields {
		got[f.Key] = f.Value
	}
	for _, want := range []string{"worker", "workflow", "work item", "tokens", "cost", "branch", "pr", "iteration"} {
		if got[want] == "" {
			t.Errorf("the detail has no %q field — the GUI's context strip is missing it:\n%+v", want, got)
		}
	}
	// The WORKER NAME is what an operator recognises; the id stays for traceability.
	if !strings.Contains(got["worker"], "Senior Engineer") || !strings.Contains(got["worker"], "w_1") {
		t.Errorf("worker = %q, want the name with the id kept", got["worker"])
	}
	if !strings.Contains(got["workflow"], "SDLC (Human)") {
		t.Errorf("workflow = %q, want the name", got["workflow"])
	}
	if !strings.Contains(got["branch"], "ready") {
		t.Errorf("branch = %q, want its provisioning state alongside", got["branch"])
	}
	if !strings.Contains(got["pr"], "open") {
		t.Errorf("pr = %q, want its state alongside", got["pr"])
	}
	// The output is in the BODY (it is prose, not a signpost).
	if !strings.Contains(body, "did the thing") {
		t.Errorf("the detail body does not carry the output:\n%s", body)
	}
}

// An EMPTY fact is OMITTED rather than rendered blank or as a zero the operator has to interpret.
func TestExecutionDetailOmitsEmptyFacts(t *testing.T) {
	p := execPlane()
	p.exec.PrUrl = ""
	p.exec.PrState = ""
	p.exec.WorktreeBranch = ""
	p.exec.CostUsd = 0
	p.exec.ErrorMessage = ""
	m := newModel(t, p)
	m.Base.SelectSource(srcExecutions)

	_, fields, _, err := m.detail(context.Background(), srcExecutions, "exec-1")
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	got := map[string]string{}
	for _, f := range fields {
		got[f.Key] = f.Value
	}
	for _, unwanted := range []string{"pr", "branch", "cost", "error"} {
		if _, ok := got[unwanted]; ok {
			t.Errorf("field %q is present with an empty value — it must be omitted, not rendered: %q",
				unwanted, got[unwanted])
		}
	}
	// The facts that ARE set stay.
	if got["tokens"] == "" {
		t.Error("tokens went missing")
	}
}

// --- todos ------------------------------------------------------------------

// The todo list renders with the status marks the worker itself uses, and a done count.
func TestExecutionTodosRenderWithMarks(t *testing.T) {
	todos := []*apiv1.TodoItem{
		{Content: "read the code", Status: apiv1.TodoStatus_TODO_STATUS_COMPLETED},
		{Content: "write the fix", Status: apiv1.TodoStatus_TODO_STATUS_IN_PROGRESS, Priority: apiv1.TodoPriority_TODO_PRIORITY_HIGH},
		{Content: "run the tests", Status: apiv1.TodoStatus_TODO_STATUS_PENDING},
		{Content: "abandoned idea", Status: apiv1.TodoStatus_TODO_STATUS_CANCELLED},
	}
	out := renderTodos(todos, 80)
	for _, want := range []string{
		"[x] read the code",
		"[~] write the fix",
		"[ ] run the tests",
		"[-] abandoned idea",
		"todo (1/4 done)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the todo list does not render %q:\n%s", want, out)
		}
	}
	// Priority is flagged only when it is HIGH, so the flag keeps meaning something.
	if got := strings.Count(out, "(!)"); got != 1 {
		t.Errorf("the high-priority mark appears %d time(s), want exactly 1:\n%s", got, out)
	}
	// And the todos appear in the DETAIL BODY, above the output (context for reading it).
	m := newModel(t, execPlane())
	m.todos.put("exec-1", todos)
	m.Base.SelectSource(srcExecutions)
	_, _, body, err := m.detail(context.Background(), srcExecutions, "exec-1")
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if !strings.Contains(body, "write the fix") {
		t.Errorf("the todo list is not in the detail body:\n%s", body)
	}
	if strings.Index(body, "write the fix") > strings.Index(body, "did the thing") {
		t.Errorf("the todos come after the output — they are context for reading it:\n%s", body)
	}
}

// No todos means NO section: an empty list is not a header with nothing under it.
func TestExecutionTodosAbsentMeansNoSection(t *testing.T) {
	m := newModel(t, execPlane())
	m.Base.SelectSource(srcExecutions)
	_, _, body, err := m.detail(context.Background(), srcExecutions, "exec-1")
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if strings.Contains(body, "todo (") {
		t.Errorf("a todo header rendered with no todos:\n%s", body)
	}
}

// The todos cache round-trips, and its ZERO VALUE is usable (the screen is constructed without it).
func TestTodosCacheZeroValueIsUsable(t *testing.T) {
	var c todosCache
	if got := c.get("nope"); got != nil {
		t.Errorf("a fresh cache returned %v, want nothing", got)
	}
	todos := []*apiv1.TodoItem{{Content: "one"}}
	c.put("exec-1", todos)
	if got := c.get("exec-1"); len(got) != 1 {
		t.Errorf("cache round trip lost the list: %v", got)
	}
	if got := c.get("exec-2"); got != nil {
		t.Errorf("the cache leaked another execution's list: %v", got)
	}
}

// A FAILED todo fetch must not turn opening an execution into an error: the list is context.
func TestFailedTodoFetchLeavesTheDetailWorking(t *testing.T) {
	p := execPlane()
	p.failTodos = true
	m := newModel(t, p)
	m.Base.SelectSource(srcExecutions)

	if _, _, _, err := m.detail(context.Background(), srcExecutions, "exec-1"); err != nil {
		t.Fatalf("a failed TODO fetch broke the detail: %v", err)
	}
	// And the failed load's message is swallowed rather than replacing the list.
	m.Update(execTodosMsg{execID: "exec-1", err: context.DeadlineExceeded})
	if m.todos.get("exec-1") != nil {
		t.Error("a failed todo fetch installed an empty list over nothing")
	}
}

// --- the follow-up box ------------------------------------------------------

// `f` opens the follow-up box on the Executions pane, and submitting it calls
// ContinueExecutionSession with the operator's text.
func TestFollowUpBoxCallsContinueExecutionSession(t *testing.T) {
	p := execPlane()
	m := newModel(t, p)
	m.Base.SelectSource(srcExecutions)
	m.Base.LoadItems(srcExecutions, []kit2.Item{{ID: "exec-1", Title: "exec-1", Meta: "succeeded"}}, "")

	if _, handled := m.handleActionKey(keyFollowUp); !handled {
		t.Fatal("f was not handled on the Executions pane")
	}
	if m.form == nil {
		t.Fatal("f did not open the follow-up box")
	}
	m.form.Set("message", "why did you skip the migration?")
	cmd, err := m.form.OnSubmit(m.form.Values, nil)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	runWrite(t, cmd)
	if len(p.followUps) != 1 {
		t.Fatalf("ContinueExecutionSession calls = %d, want 1", len(p.followUps))
	}
	if got := p.followUps[0].GetMessage(); got != "why did you skip the migration?" {
		t.Errorf("message = %q, want the operator's question", got)
	}
	if got := p.followUps[0].GetExecutionId(); got != "exec-1" {
		t.Errorf("execution = %q, want the selected one", got)
	}
	// The REPLY is surfaced — the whole point of asking.
	if !strings.Contains(m.notice, "the model's answer") {
		t.Errorf("notice = %q, want the reply", m.notice)
	}
}

// `f` is scoped to the Executions pane: on another pane it belongs to whatever that pane binds.
func TestFollowUpKeyIsScopedToTheExecutionsPane(t *testing.T) {
	m := newModel(t, execPlane())
	m.Base.SelectSource(srcRuns)
	if _, handled := m.handleActionKey(keyFollowUp); !handled {
		t.Fatal("f must still be HANDLED (so it is not silently dropped), but as a refusal")
	}
	if m.form != nil {
		t.Error("f opened the follow-up box on the Runs pane")
	}
	if !strings.Contains(m.notice, "Executions") {
		t.Errorf("notice = %q — the refusal must name where the box applies", m.notice)
	}
}

// A follow-up with no execution selected refuses rather than opening a box that cannot submit.
func TestFollowUpRefusesWithNoSelection(t *testing.T) {
	m := newModel(t, execPlane())
	m.Base.SelectSource(srcExecutions)
	// No rows loaded: there is no active item.
	if _, handled := m.handleActionKey(keyFollowUp); !handled {
		t.Fatal("f must be handled")
	}
	if m.form != nil {
		t.Error("f opened a form with nothing selected")
	}
	if !strings.Contains(m.notice, "select an execution") {
		t.Errorf("notice = %q, want it to ask for a selection", m.notice)
	}
}

// The NUDGE and the FOLLOW-UP are separate acts with separate keys: `i` requires a LIVE execution,
// `f` does not. Asserted together, because the distinction is the reason for two chords.
func TestNudgeAndFollowUpHaveDifferentPreconditions(t *testing.T) {
	p := execPlane()
	// The execution is SUCCEEDED — not live.
	p.exec.Status = apiv1.ExecutionStatus_EXECUTION_STATUS_SUCCEEDED
	m := newModel(t, p)
	m.Base.SelectSource(srcExecutions)
	m.Base.LoadItems(srcExecutions, []kit2.Item{{ID: "exec-1", Title: "exec-1", Meta: "succeeded"}}, "")

	// The nudge refuses: nothing live to steer.
	m.handleActionKey(keyInterject)
	if m.form != nil {
		t.Error("i opened the interject box for a FINISHED execution — a nudge needs a live session")
	}
	nudgeNotice := m.notice

	// The follow-up opens: there is still a session to ask about.
	m.notice = ""
	if _, handled := m.handleActionKey(keyFollowUp); !handled || m.form == nil {
		t.Errorf("f did not open the follow-up box for a finished execution (notice=%q)", m.notice)
	}
	if nudgeNotice == m.notice {
		t.Error("the two chords produced the same outcome — they are meant to differ")
	}
}
