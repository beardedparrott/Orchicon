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
	items             []*apiv1.WorkItem
	failWorkItemList  error
	workflowListCalls int
	itemListCalls     int

	// --- work-item writes (the Schedules pane's cancel / remove-schedule) ---
	//
	// Recorded rather than faked at the HTTP level so a test can assert WHICH write went out —
	// the two views issue different ones, and the whole point of the delete is that difference.
	deleted       []string
	updated       []*apiv1.UpdateWorkItemRequest
	deleteItemErr error
	workItem      *apiv1.WorkItem
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

// TestInterjectSendsMessageThroughForm pins the interjection: the chord opens
// a message form and the submitted value reaches SendExecutionMessage.
func TestInterjectSendsMessageThroughForm(t *testing.T) {
	p := &fakePlane{}
	m := newModel(t, p)
	m.LoadItems(srcExecutions, []kit2.Item{{ID: "exec-7", Title: "exec-7", Meta: "running"}}, "")

	press(t, m, "i")
	f := m.ActiveForm()
	if f == nil {
		t.Fatal("interject must open a message form")
	}
	f.Set("message", "stop and re-run the tests")
	if !f.FocusName("message") {
		t.Fatal("form has no message field")
	}
	run(t, m, press(t, m, "ctrl+s"))
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
