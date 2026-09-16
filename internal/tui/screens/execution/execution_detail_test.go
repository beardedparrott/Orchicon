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

// --- the message box (the retired `f` / `i` modals) -------------------------

// The composer posts ContinueExecutionSession for a FINISHED execution, with the operator's text.
//
// This replaces the `f` modal: the operator's report was that the chord opened a box, they typed a
// question, pressed ctrl+s, and NOTHING happened — and that the interface was "very weird" and "not
// very intuitive". The box is now a position in the transcript (walk down past the last block) and
// sending is `enter`, which is what they asked for: "type in your response and hit enter to send
// it".
func TestComposerSendsAFollowUpOnAFinishedExecution(t *testing.T) {
	p := execPlane()
	p.exec.Status = apiv1.ExecutionStatus_EXECUTION_STATUS_SUCCEEDED
	m := newModel(t, p)
	m.Base.SelectSource(srcExecutions)
	m.Base.LoadItems(srcExecutions, []kit2.Item{{ID: "exec-1", Title: "exec-1", Meta: "succeeded"}}, "")

	m.blocks.cursor.atComposer = true
	m.composer.value = "why did you skip the migration?"
	m.composer.cursor = len([]rune(m.composer.value))

	cmd := m.sendComposer()
	if cmd == nil {
		t.Fatal("sending produced no command")
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
	// The draft is CLEARED, so the same question cannot be posted twice by a second enter.
	if m.composer.value != "" {
		t.Errorf("the composer kept the draft (%q) — a second enter would post it again", m.composer.value)
	}
}

// The composer messages a LIVE execution instead — the SAME box, the same key, decided by state.
//
// This is the operator's "(Interjects should work the same way on live executions)": one control
// for both acts, which is also the GUI's model (a single textarea whose placeholder changes).
func TestComposerMessagesALiveExecution(t *testing.T) {
	p := execPlane()
	p.exec.Status = apiv1.ExecutionStatus_EXECUTION_STATUS_RUNNING
	m := newModel(t, p)
	m.Base.SelectSource(srcExecutions)
	m.Base.LoadItems(srcExecutions, []kit2.Item{{ID: "exec-1", Title: "exec-1", Meta: "running"}}, "")

	m.blocks.cursor.atComposer = true
	m.composer.value = "check the migration path first"
	m.composer.cursor = len([]rune(m.composer.value))

	runWrite(t, m.sendComposer())
	if len(p.messages) != 1 {
		t.Fatalf("SendExecutionMessage calls = %d, want 1", len(p.messages))
	}
	if got := p.messages[0].GetMessage(); got != "check the migration path first" {
		t.Errorf("message = %q, want the operator's text", got)
	}
}

// The placeholder names the act, so the operator knows what enter will DO before pressing it —
// "nudge the live session" vs "ask a follow-up". The GUI's own rule.
func TestComposerPlaceholderNamesTheAct(t *testing.T) {
	if got := composerPlaceholder("running"); !strings.Contains(got, "mid-run") {
		t.Errorf("live placeholder = %q, want it to describe messaging the worker", got)
	}
	if got := composerPlaceholder("succeeded"); !strings.Contains(got, "follow-up") {
		t.Errorf("finished placeholder = %q, want it to describe a follow-up", got)
	}
}

// An empty draft sends nothing — enter on an empty box must not create a mutation.
func TestComposerRefusesAnEmptyDraft(t *testing.T) {
	p := execPlane()
	m := newModel(t, p)
	m.Base.SelectSource(srcExecutions)
	m.Base.LoadItems(srcExecutions, []kit2.Item{{ID: "exec-1", Title: "exec-1", Meta: "succeeded"}}, "")
	m.blocks.cursor.atComposer = true
	if cmd := m.sendComposer(); cmd != nil {
		t.Error("an empty draft produced a command")
	}
	if len(p.followUps) != 0 {
		t.Error("an empty draft posted a follow-up")
	}
}

// The composer TYPES: printable runes insert at the caret, editing keys edit, and enter is
// deliberately NOT consumed by the text editor (it means SEND).
func TestComposerTypingAndEditing(t *testing.T) {
	m := newModel(t, execPlane())
	m.Base.SelectSource(srcExecutions)
	m.Base.LoadItems(srcExecutions, []kit2.Item{{ID: "exec-1", Title: "exec-1", Meta: "succeeded"}}, "")
	// The transcript's keys are scoped to the DETAIL focus, like the runs flow: with the LIST
	// focused the same keys move the list.
	m.Base.SetFocusForTest("detail")
	m.blocks.cursor.atComposer = true

	// A character routes through the composer's key handler.
	if _, handled := m.handleActionKey("a"); !handled {
		t.Fatal("a printable rune was not handled by the composer")
	}
	if m.composer.value != "a" {
		t.Fatalf("value = %q, want a", m.composer.value)
	}
	// backspace
	m.handleActionKey("backspace")
	if m.composer.value != "" {
		t.Errorf("after backspace value = %q, want empty", m.composer.value)
	}
	// ctrl+u clears
	m.composer.value = "long draft"
	m.composer.cursor = len([]rune(m.composer.value))
	m.handleActionKey("ctrl+u")
	if m.composer.value != "" {
		t.Errorf("ctrl+u left %q", m.composer.value)
	}
	// up LEAVES the box (back to the blocks) without destroying the draft.
	m.composer.value = "keep me"
	if _, handled := m.handleActionKey("up"); !handled {
		t.Error("up was not handled")
	}
	if m.blocks.cursor.atComposer {
		t.Error("up did not leave the composer")
	}
	if m.composer.value != "keep me" {
		t.Errorf("leaving the box destroyed the draft (%q)", m.composer.value)
	}
}

// The retired chords EXPLAIN instead of going silent, and land the operator IN the box.
func TestRetiredChordsExplainAndFocusTheComposer(t *testing.T) {
	m := newModel(t, execPlane())
	m.Base.SelectSource(srcExecutions)
	m.Base.LoadItems(srcExecutions, []kit2.Item{{ID: "exec-1", Title: "exec-1", Meta: "succeeded"}}, "")

	for _, k := range []string{keyFollowUpLegacy, keyInterjectLegacy} {
		m.blocks.cursor.atComposer = false
		m.notice = ""
		if _, handled := m.handleActionKey(k); !handled {
			t.Fatalf("%q must still be HANDLED, so it is not silently dropped", k)
		}
		if m.form != nil {
			t.Errorf("%q opened a modal — the modals are gone", k)
		}
		if !m.blocks.cursor.atComposer {
			t.Errorf("%q did not land the operator in the message box", k)
		}
		if !strings.Contains(m.notice, "INLINE") {
			t.Errorf("%q notice = %q, want it to say where the box went", k, m.notice)
		}
	}
}

// `f` is no longer claimed as a WRITE chord, which matters because the BASE also binds it (the
// pager, advertised as "more pages: press f"). The stub must not swallow the pager on another pane.
func TestRetiredKeysAreScopedToTheExecutionsPane(t *testing.T) {
	m := newModel(t, execPlane())
	m.Base.SelectSource(srcRuns)
	if _, handled := m.actionByKey(keyFollowUpLegacy); handled {
		t.Error("f is still bound as an execution action")
	}
	if _, handled := m.actionByKey(keyInterjectLegacy); handled {
		t.Error("i is still bound as an execution action")
	}
}
