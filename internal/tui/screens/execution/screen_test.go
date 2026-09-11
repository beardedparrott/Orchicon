package execution

import (
	"context"
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

	mu       sync.Mutex
	cancel   []*apiv1.CancelExecutionRequest
	messages []*apiv1.SendExecutionMessageRequest
	retries  []string
	forced   []string
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
	run(t, m, press(t, m, "enter"))
	if len(p.messages) != 1 {
		t.Fatalf("SendExecutionMessage calls = %d, want 1", len(p.messages))
	}
	if p.messages[0].GetExecutionId() != "exec-7" || p.messages[0].GetMessage() != "stop and re-run the tests" {
		t.Fatalf("unexpected interjection: %+v", p.messages[0])
	}
}
