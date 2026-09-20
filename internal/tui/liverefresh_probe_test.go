package tui

// liverefresh_probe_test.go — DOES THE ROLLING REFRESH WINDOW ACTUALLY REFRESH THE EXECUTION PANE?
//
// The operator, mid-live-test: "Executions are NOT updating live. I have to move off the execution and
// back onto it to see the newest updates" and "The execution list is also NOT updating live. You have
// to leave the screen and go back to it to see the live updates."
//
// refresh.go was written FOR this exact complaint (its own header quotes the earlier, identical one:
// "when you send a message, nothing happens unless you move away from the execution and then come
// back"). It arms ONE chain at Init and re-arms itself; the hook it calls is kit2.Base.RefreshView,
// which reloads the ACTIVE source's list and the open detail.
//
// So whether the chain EXISTS is not the question — whether one turn of it causes the Execution screen
// to RE-READ is. That is what is measured here, at the App layer, because that is the only layer at
// which "the tick reaches the screen" is true or false. The screen side (a live poke reaching the open
// execution, and the transcript it records) is pinned in the execution package's live_update_test.go;
// this file covers the HOP BETWEEN THEM, which is the one that had never been driven end to end.
//
// WHY THIS PROBE USED TO HANG FOR TEN MINUTES AND FAIL THE WHOLE PACKAGE (the reason it is written this
// way now). Its first version landed the initial load by way of `scr.Init()` and ran the whole command
// TREE it returned. Init is `tea.Batch(m.Load(), m.reg.WaitStatus("execution-events"),
// m.reg.WaitStatus("workflow-events"), m.reg.WaitEventPoke("execution-events"))` — and a WaitStatus is a
// command that RECEIVES from a subscription channel, so it does not return until the plane emits an
// event that never comes in a test. Executing the tree therefore blocked inside subs.Registry.WaitStatus
// (subs.go:421), the probing goroutine never finished, and `go test ./internal/tui` died on the 10
// minute alarm. MEASURED, not guessed: the panic dump named exactly that frame.
//
// Two consequences worth keeping in mind, because they are general:
//
//	A test may only run the commands whose CONTRACT is "return promptly". Init's batch mixes a fetch
//	(returns) with stream ARMS (block BY DESIGN until an event arrives), so a test that runs the whole
//	batch is testing the stream's patience, not the screen's refresh.
//
//	The same trap applies to the tick itself: its re-arm is a tea.Tick, which blocks until it fires.
//	One turn of the window is therefore driven through handleRefreshTick with a SHORTENED period, so the
//	timer returns immediately instead of costing the probe the production five seconds.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/config"
	"github.com/beardedparrott/orchicon/internal/tui/screens/execution"
)

// stubExecList counts the reads, so "did the pane re-read?" is a NUMBER rather than an impression.
//
// Both hops are counted, because both are part of the operator's report and they are re-read by two
// different commands: ListExecutions backs the LIST ("the execution list is also NOT updating live") and
// GetExecution backs the open DETAIL ("I have to move off the execution and back onto it to see the
// newest updates" — the transcript lives in the detail pane).
type stubExecList struct {
	apiv1connect.UnimplementedExecutionServiceHandler
	listCalls int
	getCalls  int
	status    apiv1.ExecutionStatus
}

func (s *stubExecList) ListExecutions(_ context.Context, _ *connect.Request[apiv1.ListExecutionsRequest]) (*connect.Response[apiv1.ListExecutionsResponse], error) {
	s.listCalls++
	return connect.NewResponse(&apiv1.ListExecutionsResponse{
		Executions: []*apiv1.WorkerExecution{{Id: "exec-1", Status: s.status}},
	}), nil
}

func (s *stubExecList) GetExecution(_ context.Context, req *connect.Request[apiv1.GetExecutionRequest]) (*connect.Response[apiv1.GetExecutionResponse], error) {
	s.getCalls++
	return connect.NewResponse(&apiv1.GetExecutionResponse{
		Execution: &apiv1.WorkerExecution{Id: req.Msg.GetId(), Status: s.status},
	}), nil
}

// newExecListApp builds the shell with an Execution screen registered and focused on the Executions
// pane, and the screen's FIRST load already landed — so every count below measures a REFRESH and not
// the initial fetch.
func newExecListApp(t *testing.T) (*App, *execution.Model, *stubExecList) {
	t.Helper()
	stub := &stubExecList{status: apiv1.ExecutionStatus_EXECUTION_STATUS_RUNNING}
	mux := http.NewServeMux()
	mux.Handle(apiv1connect.NewExecutionServiceHandler(stub))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	cl := client.NewWithHTTPClient(client.Options{BaseURL: srv.URL}, srv.Client())
	m := NewApp(cl, &config.Profile{Name: "default", URL: srv.URL}, "v0")
	m.width, m.height = 120, 40
	// The shell's rolling window only acts on the CURRENT generation's tick, so the probe must be
	// built after whatever Init did to it — reading it here is the honest way to say "the live chain".
	m.refreshGen++

	scr := execution.New(m.clients, m.reg, "")
	m.RegisterScreen(TabExecution, scr)
	scr.SetSize(m.contentWidth(), m.screenRows())
	m.active = TabExecution
	if !scr.SelectSource("executions") {
		t.Fatal("fixture: the Executions source could not be selected")
	}
	return m, scr, stub
}

// landFirstLoad runs the screen's LIST command — Load, NOT Init — and nothing else.
//
// The distinction is the whole repair (see the header): Load is one fetch per source and returns; Init
// also ARMS the streams, and a stream arm blocks until the plane emits, which in a test is for ever.
func landFirstLoad(t *testing.T, scr *execution.Model) {
	t.Helper()
	execCmdTree(t, scr.Load(), 0)
}

// execCmdTree runs a command tree, so the RPC a fetch command issues actually happens. The messages
// it produces are deliberately NOT dispatched: this probe asks whether the pane RE-READS, and the
// call count rises when the command runs, not when its result lands.
//
// CALLERS MUST PASS A TREE WHOSE COMMANDS RETURN. A tree containing a stream arm will hang here, which
// is exactly the failure this file's header documents.
func execCmdTree(t *testing.T, cmd tea.Cmd, depth int) {
	t.Helper()
	if cmd == nil || depth > 5 {
		return
	}
	msg := runCmdBounded(cmd, runCtxCmdBudget)
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			execCmdTree(t, c, depth+1)
		}
	}
}

// rollOneTick drives ONE turn of the rolling window exactly as the shell does, through the tick
// HANDLER, so the generation guard, the dispatch and the re-arm are all in the path rather than only
// the dispatch.
func rollOneTick(t *testing.T, m *App) {
	t.Helper()
	// The re-arm is a tea.Tick and BLOCKS until it fires. The production five seconds is not what is
	// under test here (the re-arm and the generation guard have their own tests in refresh_test.go),
	// so the period is shortened for the duration of the turn.
	prev := refreshPeriod
	refreshPeriod = time.Millisecond
	t.Cleanup(func() { refreshPeriod = prev })

	_, cmd := m.handleRefreshTick(refreshTickMsg{gen: m.refreshGen})
	if cmd == nil {
		t.Fatalf("a rolling tick produced NO command for the Execution screen, so the pane it is meant " +
			"to refresh is never re-read")
	}
	execCmdTree(t, cmd, 0)
}

// THE PROBE, HOP 4: one turn of the rolling window must cause the Execution screen's LIST to be
// re-read. Without it the pane can only change when the operator leaves and returns — the report.
func TestRollingTickRefetchesTheExecutionList(t *testing.T) {
	m, scr, stub := newExecListApp(t)

	landFirstLoad(t, scr)
	first := stub.listCalls
	if first == 0 {
		t.Fatalf("precondition: the screen's first load never called ListExecutions")
	}

	rollOneTick(t, m)

	if stub.listCalls <= first {
		t.Fatalf("a rolling tick did not re-read the execution list (calls %d -> %d); the pane can then "+
			"only update when the operator leaves and returns, which is the operator's report",
			first, stub.listCalls)
	}
}

// THE PROBE, HOP 4 AGAIN, for the DETAIL pane: a turn must also re-read the OPEN execution, because the
// transcript the operator is watching lives there. This is the "nothing happens unless you move away
// from the execution and then come back" half of the report, and it is a SEPARATE command from the list
// read — a refresh that reloaded only the list would leave the open transcript frozen.
func TestRollingTickRefetchesTheOpenExecutionDetail(t *testing.T) {
	m, scr, stub := newExecListApp(t)

	landFirstLoad(t, scr)
	// The detail pane owns the transcript for the execution it is showing; the id is what tells the
	// refresh which item to re-read.
	scr.SetDetailID("exec-1")
	if cmd := scr.RequestDetail("executions", "exec-1"); cmd != nil {
		execCmdTree(t, cmd, 0)
	}
	first := stub.getCalls
	if first == 0 {
		t.Fatalf("precondition: the detail pane's first read never called GetExecution")
	}

	rollOneTick(t, m)

	if stub.getCalls <= first {
		t.Fatalf("a rolling tick did not re-read the OPEN execution (GetExecution calls %d -> %d); the "+
			"transcript would then stay frozen until the operator leaves the screen and returns",
			first, stub.getCalls)
	}
}

// AND THE TICK IS DISPATCHED TO THE SCREEN THAT OWNS IT. A tick for a superseded generation must NOT
// re-read anything: refreshing the view the operator has already left is wasted work and a visible
// flicker, which is why the generation is carried on the message at all.
func TestAStaleTickDoesNotRefetchTheView(t *testing.T) {
	m, scr, stub := newExecListApp(t)
	landFirstLoad(t, scr)
	before := stub.listCalls

	prev := refreshPeriod
	refreshPeriod = time.Millisecond
	t.Cleanup(func() { refreshPeriod = prev })

	// A tick from a generation the shell has moved past.
	_, cmd := m.handleRefreshTick(refreshTickMsg{gen: m.refreshGen - 1})
	if cmd == nil {
		t.Fatal("a stale tick produced no command — the window must always re-arm, or one dropped tick " +
			"ends the refresh for the rest of the session")
	}
	execCmdTree(t, cmd, 0)

	if stub.listCalls != before {
		t.Errorf("a STALE tick re-read the list (%d -> %d); the generation guard is not in the path",
			before, stub.listCalls)
	}
}
