package execution

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
	"github.com/beardedparrott/orchicon/internal/tui/subs"
)

// fakePlane records the execution/run write RPCs the screen issues.
type fakePlane struct {
	apiv1connect.UnimplementedExecutionServiceHandler
	apiv1connect.UnimplementedWorkflowServiceHandler
	apiv1connect.UnimplementedWorkItemServiceHandler

	// --- worker list (the Workers pane's grouping) ---
	//
	// The list response carries the WORKER groupings alongside the page, exactly as the real service
	// enriches it. The pane groups from THESE, not from the shell's cache (which is loaded once at
	// startup and could be minutes stale) — so a test needs to be able to put groupings in the response.
	workerItems       []*apiv1.WorkerListItem
	workerCategories  []*apiv1.Category
	workerAssignments []*apiv1.CategoryAssignment

	mu       sync.Mutex
	cancel   []*apiv1.CancelExecutionRequest
	messages []*apiv1.SendExecutionMessageRequest
	retries  []string
	forced   []string

	// --- name resolution (names.go) ---
	//
	// A WorkflowRun carries ids and no names, so the runs views resolve them from these two
	// lists. They are fake state rather than real fixtures because the point is the RESOLUTION,
	// not the data.
	workflows         []*apiv1.Workflow
	runs              []*apiv1.WorkflowRun
	run               *apiv1.WorkflowRun
	stepRuns          []*apiv1.WorkflowStepRun
	items             []*apiv1.WorkItem
	failWorkItemList  error
	workflowListCalls int
	itemListCalls     int

	// runListReqs records every ListWorkflowRuns request, so a test can assert the SORT the
	// Schedules finished lens asked for. The fake serves whatever `runs` it was given, so the
	// ORDERING itself is the server's (internal/db/workflow.go, keyset on (started_at, id)); what
	// THIS client controls — and therefore what is worth asserting here — is that it asks for the
	// same sort the GUI asks for, so the two clients cannot render history in different orders.
	runListReqs []*apiv1.ListWorkflowRunsRequest

	// --- work-item writes (the Schedules pane's cancel / remove-schedule) ---
	//
	// Recorded rather than faked at the HTTP level so a test can assert WHICH write went out —
	// the two views issue different ones, and the whole point of the delete is that difference.
	deleted       []string
	updated       []*apiv1.UpdateWorkItemRequest
	deleteItemErr error
	workItem      *apiv1.WorkItem

	// --- executions (the Executions detail) ---
	exec      *apiv1.WorkerExecution
	todos     []*apiv1.TodoItem
	failTodos bool
	followUps []*apiv1.ContinueExecutionSessionRequest
}

func (p *fakePlane) GetExecution(_ context.Context, req *connect.Request[apiv1.GetExecutionRequest]) (*connect.Response[apiv1.GetExecutionResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.exec == nil || p.exec.GetId() != req.Msg.GetId() {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("execution not found"))
	}
	return connect.NewResponse(&apiv1.GetExecutionResponse{Execution: p.exec}), nil
}

func (p *fakePlane) GetExecutionTodos(_ context.Context, req *connect.Request[apiv1.GetExecutionTodosRequest]) (*connect.Response[apiv1.GetExecutionTodosResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failTodos {
		return nil, connect.NewError(connect.CodeInternal, errors.New("todos unavailable"))
	}
	return connect.NewResponse(&apiv1.GetExecutionTodosResponse{Todos: p.todos}), nil
}

func (p *fakePlane) ContinueExecutionSession(_ context.Context, req *connect.Request[apiv1.ContinueExecutionSessionRequest]) (*connect.Response[apiv1.ContinueExecutionSessionResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.followUps = append(p.followUps, req.Msg)
	return connect.NewResponse(&apiv1.ContinueExecutionSessionResponse{Reply: "the model's answer"}), nil
}

func (p *fakePlane) GetWorkItem(_ context.Context, req *connect.Request[apiv1.GetWorkItemRequest]) (*connect.Response[apiv1.GetWorkItemResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.workItem != nil && p.workItem.GetId() == req.Msg.GetId() {
		return connect.NewResponse(&apiv1.GetWorkItemResponse{WorkItem: p.workItem}), nil
	}
	for _, it := range p.items {
		if it.GetId() == req.Msg.GetId() {
			return connect.NewResponse(&apiv1.GetWorkItemResponse{WorkItem: it}), nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, errors.New("work item not found"))
}

func (p *fakePlane) DeleteWorkItem(_ context.Context, req *connect.Request[apiv1.DeleteWorkItemRequest]) (*connect.Response[apiv1.DeleteWorkItemResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.deleted = append(p.deleted, req.Msg.GetId())
	if p.deleteItemErr != nil {
		return nil, p.deleteItemErr
	}
	return connect.NewResponse(&apiv1.DeleteWorkItemResponse{}), nil
}

func (p *fakePlane) UpdateWorkItem(_ context.Context, req *connect.Request[apiv1.UpdateWorkItemRequest]) (*connect.Response[apiv1.UpdateWorkItemResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.updated = append(p.updated, req.Msg)
	return connect.NewResponse(&apiv1.UpdateWorkItemResponse{WorkItem: &apiv1.WorkItem{Id: req.Msg.GetId()}}), nil
}

func (p *fakePlane) ListWorkflows(context.Context, *connect.Request[apiv1.ListWorkflowsRequest]) (*connect.Response[apiv1.ListWorkflowsResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.workflowListCalls++
	return connect.NewResponse(&apiv1.ListWorkflowsResponse{Workflows: p.workflows}), nil
}

func (p *fakePlane) ListWorkflowRuns(_ context.Context, req *connect.Request[apiv1.ListWorkflowRunsRequest]) (*connect.Response[apiv1.ListWorkflowRunsResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.runListReqs = append(p.runListReqs, req.Msg)
	return connect.NewResponse(&apiv1.ListWorkflowRunsResponse{Runs: p.runs}), nil
}

func (p *fakePlane) GetWorkflowRun(_ context.Context, req *connect.Request[apiv1.GetWorkflowRunRequest]) (*connect.Response[apiv1.GetWorkflowRunResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.run
	if r == nil {
		for _, cand := range p.runs {
			if cand.GetId() == req.Msg.GetId() {
				r = cand
				break
			}
		}
	}
	if r == nil {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("run not found"))
	}
	return connect.NewResponse(&apiv1.GetWorkflowRunResponse{Run: r}), nil
}

// GetWorkflowStepRuns serves the step runs the run flow renders.
func (p *fakePlane) GetWorkflowStepRuns(context.Context, *connect.Request[apiv1.GetWorkflowStepRunsRequest]) (*connect.Response[apiv1.GetWorkflowStepRunsResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return connect.NewResponse(&apiv1.GetWorkflowStepRunsResponse{StepRuns: p.stepRuns}), nil
}

func (p *fakePlane) ListWorkItems(_ context.Context, req *connect.Request[apiv1.ListWorkItemsRequest]) (*connect.Response[apiv1.ListWorkItemsResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.itemListCalls++
	if p.failWorkItemList != nil {
		return nil, p.failWorkItemList
	}
	// HONOUR THE STATUS FILTER, as the plane does. A fake that ignores it would let a broken
	// fetch pass: the upcoming view's whole behaviour is that it asks for SCHEDULED, so the
	// fixture must be able to tell the difference.
	if req.Msg.Status == nil {
		return connect.NewResponse(&apiv1.ListWorkItemsResponse{WorkItems: p.items}), nil
	}
	want := req.Msg.GetStatus()
	out := make([]*apiv1.WorkItem, 0, len(p.items))
	for _, it := range p.items {
		if it.GetStatus() == want {
			out = append(out, it)
		}
	}
	return connect.NewResponse(&apiv1.ListWorkItemsResponse{WorkItems: out}), nil
}

func (p *fakePlane) CancelExecution(_ context.Context, req *connect.Request[apiv1.CancelExecutionRequest]) (*connect.Response[apiv1.CancelExecutionResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cancel = append(p.cancel, req.Msg)
	return connect.NewResponse(&apiv1.CancelExecutionResponse{}), nil
}

func (p *fakePlane) SendExecutionMessage(_ context.Context, req *connect.Request[apiv1.SendExecutionMessageRequest]) (*connect.Response[apiv1.SendExecutionMessageResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.messages = append(p.messages, req.Msg)
	return connect.NewResponse(&apiv1.SendExecutionMessageResponse{}), nil
}

func (p *fakePlane) RetryFailedWorkflowRun(_ context.Context, req *connect.Request[apiv1.RetryFailedWorkflowRunRequest]) (*connect.Response[apiv1.RetryFailedWorkflowRunResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.retries = append(p.retries, req.Msg.GetRunId())
	return connect.NewResponse(&apiv1.RetryFailedWorkflowRunResponse{}), nil
}

func (p *fakePlane) ForceProgressWorkflowRun(_ context.Context, req *connect.Request[apiv1.ForceProgressWorkflowRunRequest]) (*connect.Response[apiv1.ForceProgressWorkflowRunResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.forced = append(p.forced, req.Msg.GetRunId())
	return connect.NewResponse(&apiv1.ForceProgressWorkflowRunResponse{}), nil
}

func newModel(t *testing.T, p *fakePlane) *Model {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(apiv1connect.NewExecutionServiceHandler(p))
	mux.Handle(apiv1connect.NewWorkflowServiceHandler(p))
	// The runs views resolve workflow/work-item NAMES from these two lists (names.go), so the
	// fixture has to serve them.
	mux.Handle(apiv1connect.NewWorkItemServiceHandler(p))
	// The WORKER list, so the Workers pane's own fetch (and its grouping) can be driven.
	mux.Handle(apiv1connect.NewWorkerServiceHandler(&fakeWorkers{plane: p}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	m := New(client.New(client.Options{BaseURL: srv.URL}), subs.NewRegistry(), "")
	m.SetSize(200, 40)
	return m
}

func kmsg(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "ctrl+x":
		return tea.KeyMsg{Type: tea.KeyCtrlX}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func press(t *testing.T, m *Model, key string) tea.Cmd {
	t.Helper()
	_, cmd := m.Update(kmsg(key))
	return cmd
}

// run executes a cmd and feeds its message back into the screen.
func run(t *testing.T, m *Model, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a command, got nil")
	}
	msg := cmd()
	if msg == nil {
		t.Fatal("command produced no message")
	}
	m.Update(msg)
}

// TestCancelExecutionConfirmsAndWrites pins the cancel path: the chord opens
// a CONFIRM dialog (not an immediate write), and only confirming issues
// CancelExecution with the recorded reason — reconciling the list.
func TestCancelExecutionConfirmsAndWrites(t *testing.T) {
	p := &fakePlane{}
	m := newModel(t, p)
	m.LoadItems(srcExecutions, []kit2.Item{{ID: "exec-1", Title: "exec-1", Meta: "running"}}, "")

	if cmd := press(t, m, "c"); cmd != nil {
		t.Fatal("cancel must open a confirm dialog, not write immediately")
	}
	if !m.DialogOpen() {
		t.Fatal("expected the confirm dialog to be open")
	}
	if len(p.cancel) != 0 {
		t.Fatalf("no write may happen before confirming, got %d", len(p.cancel))
	}

	run(t, m, press(t, m, "enter"))
	if len(p.cancel) != 1 {
		t.Fatalf("CancelExecution calls = %d, want 1", len(p.cancel))
	}
	if p.cancel[0].GetId() != "exec-1" || p.cancel[0].GetReason() != cancelReason {
		t.Fatalf("unexpected cancel request: %+v", p.cancel[0])
	}
}

// TestCancelUnavailableOnFinishedExecution pins the state gate: a finished
// execution exposes no cancel action and the chord explains why.
func TestCancelUnavailableOnFinishedExecution(t *testing.T) {
	p := &fakePlane{}
	m := newModel(t, p)
	m.LoadItems(srcExecutions, []kit2.Item{{ID: "exec-2", Title: "exec-2", Meta: "succeeded"}}, "")

	if cmd := press(t, m, "c"); cmd != nil {
		t.Fatal("no write expected for a finished execution")
	}
	if m.DialogOpen() {
		t.Fatal("no confirm dialog may open for a finished execution")
	}
	if m.Notice() == "" {
		t.Fatal("the refusal must be surfaced, not a silent no-op")
	}
	if len(p.cancel) != 0 {
		t.Fatal("no CancelExecution may be issued")
	}
}

// TestRetryFailedRunConfirmsAndWrites pins RetryFailedWorkflowRun.
func TestRetryFailedRunConfirmsAndWrites(t *testing.T) {
	p := &fakePlane{}
	m := newModel(t, p)
	m.SelectSource(srcRuns)
	m.LoadItems(srcRuns, []kit2.Item{{ID: "run-9", Title: "run-9", Meta: "failed"}}, "")

	press(t, m, "t")
	if !m.DialogOpen() {
		t.Fatal("retry must be Confirm-gated")
	}
	run(t, m, press(t, m, "enter"))
	if len(p.retries) != 1 || p.retries[0] != "run-9" {
		t.Fatalf("RetryFailedWorkflowRun calls = %v, want [run-9]", p.retries)
	}
}

// TestForceProgressWedgedRunConfirmsAndWrites pins ForceProgressWorkflowRun
// and that it is only offered on a wedged (running) run.
func TestForceProgressWedgedRunConfirmsAndWrites(t *testing.T) {
	p := &fakePlane{}
	m := newModel(t, p)
	m.SelectSource(srcRuns)
	m.LoadItems(srcRuns, []kit2.Item{{ID: "run-3", Title: "run-3", Meta: "running"}}, "")

	if cmd := press(t, m, "t"); cmd != nil || m.DialogOpen() {
		t.Fatal("retry is not offered on a running run")
	}
	if m.Notice() == "" {
		t.Fatal("the retry refusal on a running run must be explained")
	}

	press(t, m, "p")
	if !m.DialogOpen() {
		t.Fatal("force-progress must be Confirm-gated")
	}
	run(t, m, press(t, m, "enter"))
	if len(p.forced) != 1 || p.forced[0] != "run-3" {
		t.Fatalf("ForceProgressWorkflowRun calls = %v, want [run-3]", p.forced)
	}
}

// TestInterjectGoesThroughTheInlineComposer pins the interjection: the message box at the bottom of
// the execution posts to SendExecutionMessage.
//
// This REPLACES the `i` modal test. The operator retired that interface: "You said in executions 'f'
// was a follow up yet the interface says 'f' is more pages ... typed in a question and hit ctrl+s to
// save it and nothing happened. This doesn't seem to be working and frankly is very weird interface
// wise. Not very intuitive. I would rather a chat box be at the bottom of the execution ... and then
// type in your response and hit enter to send it. (Interjects should work the same way on live
// executions)". So the nudge is now the SAME box as the follow-up, decided by the execution's state
// rather than by which key was pressed — and sending is `enter`, not ctrl+s.
func TestInterjectGoesThroughTheInlineComposer(t *testing.T) {
	p := &fakePlane{}
	m := newModel(t, p)
	m.Base.SelectSource(srcExecutions)
	m.LoadItems(srcExecutions, []kit2.Item{{ID: "exec-7", Title: "exec-7", Meta: "running"}}, "")
	m.Base.SetFocusForTest("detail")

	// The retired chord must not open a modal; it lands in the box and says where the box is.
	press(t, m, "i")
	if m.form != nil {
		t.Fatal("i still opens a modal — the box is inline now")
	}
	if !m.blocks.cursor.atComposer {
		t.Fatal("i did not land the operator in the message box")
	}

	// Type and send: enter, on a LIVE execution, is SendExecutionMessage.
	m.composer.value = "stop and re-run the tests"
	m.composer.cursor = len([]rune(m.composer.value))
	run(t, m, press(t, m, "enter"))

	if len(p.messages) != 1 {
		t.Fatalf("SendExecutionMessage calls = %d, want 1", len(p.messages))
	}
	if p.messages[0].GetExecutionId() != "exec-7" || p.messages[0].GetMessage() != "stop and re-run the tests" {
		t.Fatalf("unexpected interjection: %+v", p.messages[0])
	}
}

// The operator: "Workflows should be under Execution not Automation."
//
// The Execution tab owns the Execution domain — executions, workflow runs,
// workflows and workers — so the Workflows source (and its detail) lives here,
// and it must NOT remain an Automation source.
func TestWorkflowsSourceIsOnTheExecutionTab(t *testing.T) {
	m := newModel(t, &fakePlane{})
	var found bool
	for _, s := range m.Base.SourcesForTest() {
		if s.Name == "workflows" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the Execution tab must carry a Workflows source: %v", sourceNames(m))
	}
	// Its RPC wiring is real: selecting it loads through ListWorkflows.
	if !m.Base.SelectSource("workflows") {
		t.Fatal("the workflows source must be selectable")
	}
}

func sourceNames(m *Model) []string {
	var out []string
	for _, s := range m.Base.SourcesForTest() {
		out = append(out, s.Name)
	}
	return out
}

// ListExecutions serves the fixture's single execution, so the list path is exercised end to end
// (titles, meta, and the fetch's own name resolution) rather than only its helpers.
func (p *fakePlane) ListExecutions(_ context.Context, req *connect.Request[apiv1.ListExecutionsRequest]) (*connect.Response[apiv1.ListExecutionsResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	resp := &apiv1.ListExecutionsResponse{}
	if p.exec != nil {
		resp.Executions = []*apiv1.WorkerExecution{p.exec}
	}
	return connect.NewResponse(resp), nil
}
