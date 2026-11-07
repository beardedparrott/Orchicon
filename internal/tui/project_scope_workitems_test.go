package tui

// project_scope_workitems_test.go — THE SHELL→SCREEN→QUERY PATH, END TO END.
//
// This is the test that was missing, and its absence is why a broken feature shipped looking finished: the
// earlier tests proved that `work.Model.SetActiveProject` stores a value and that `fetchWorkItems` puts the
// stored value in the request. Neither of those touches the question the operator actually asked — does the
// SHELL tell the screen which project is active, at the moment the scope is set, and does the resulting page
// carry it?
//
// MEASURED CONSEQUENCE OF THE GAP: the operator ran the TUI with the scope showing "WorkerTesting" (7 work
// items) and saw 15 epics spanning every project in the tenant. The screen was there, the filter field was
// there, and nothing connected them.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"connectrpc.com/connect"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/config"
	"github.com/beardedparrott/orchicon/internal/tui/screens/work"
	"github.com/beardedparrott/orchicon/internal/tui/subs"
)

// projectFilterPlane records the project_id of every ListWorkItems call, and answers both the work-item and
// project services so the shell can be built against it.
type projectFilterPlane struct {
	apiv1connect.UnimplementedWorkItemServiceHandler
	apiv1connect.UnimplementedProjectServiceHandler

	mu      sync.Mutex
	project []string
}

func (p *projectFilterPlane) ListWorkItems(_ context.Context, req *connect.Request[apiv1.ListWorkItemsRequest]) (*connect.Response[apiv1.ListWorkItemsResponse], error) {
	p.mu.Lock()
	p.project = append(p.project, req.Msg.GetProjectId())
	p.mu.Unlock()
	return connect.NewResponse(&apiv1.ListWorkItemsResponse{}), nil
}

func (p *projectFilterPlane) ListProjects(_ context.Context, _ *connect.Request[apiv1.ListProjectsRequest]) (*connect.Response[apiv1.ListProjectsResponse], error) {
	return connect.NewResponse(&apiv1.ListProjectsResponse{Projects: []*apiv1.Project{
		{Id: "prj-orch", Name: "Orchicon", ProjectDir: "/home/me/projects/Orchicon"},
		{Id: "prj-test", Name: "WorkerTesting", ProjectDir: "/home/me/projects/WorkerTesting"},
	}}), nil
}

func (p *projectFilterPlane) sawProject() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.project...)
}

// THE SCOPE REACHES THE WORK SCREEN AND THE QUERY, through nothing but the shell's own machinery: the project
// list lands, the launch directory resolves to WorkerTesting, and the console shows WorkerTesting's items.
//
// THIS TEST FAILS WITHOUT THE WIRING. Measured by reverting pushProjectScope to a no-op: the plane is asked for
// project_id "" — every project — while the status line says "scope: WorkerTesting".
func TestTheShellPushesTheScopeIntoTheWorkItemsQuery(t *testing.T) {
	plane := &projectFilterPlane{}
	mux := http.NewServeMux()
	mux.Handle(apiv1connect.NewWorkItemServiceHandler(plane))
	mux.Handle(apiv1connect.NewProjectServiceHandler(plane))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cl := client.NewWithHTTPClient(client.Options{BaseURL: srv.URL}, srv.Client())
	m := NewApp(cl, &config.Profile{Name: "default", URL: srv.URL}, "v0",
		WithLaunchDir("/home/me/projects/WorkerTesting"))
	m.width, m.height = 140, 40
	// A REAL work screen, not a stub: the whole question is what the screen does with the scope.
	m.RegisterScreen(TabWork, work.New(cl, subs.NewRegistry(), ""))
	m.SwitchTo(TabWork)

	// The project list arrives, exactly as the rail's own load delivers it.
	m.onRailProjects(railProjectsMsg{Projects: []railProject{
		{ID: "prj-orch", Name: "Orchicon", Status: "active", Dir: "/home/me/projects/Orchicon"},
		{ID: "prj-test", Name: "WorkerTesting", Status: "active", Dir: "/home/me/projects/WorkerTesting"},
	}})
	if m.projectScope != "prj-test" {
		t.Fatalf("fixture: scope = %q, want prj-test (the launch directory's project)", m.projectScope)
	}

	// Anything the shell staged now runs, as it would on the next Update.
	if staged := m.drainStaged(); staged != nil {
		runCtx(t, m, staged, runCtxCmdBudget)
	}
	// The screen's own first load.
	runCtx(t, m, m.screens[TabWork].Init(), runCtxCmdBudget)

	saw := plane.sawProject()
	if len(saw) == 0 {
		t.Fatal("the work screen never asked for work items at all")
	}
	for i, got := range saw {
		if got != "prj-test" {
			t.Fatalf("ListWorkItems call %d carried project_id %q, want prj-test — the pane is showing every "+
				"project's items while the status line says the scope is WorkerTesting; all calls: %v", i, got, saw)
		}
	}
}

// AND SWITCHING THE WORKSPACE RE-FILTERS IMMEDIATELY, without waiting for an unrelated message. setProjectScope is
// called from key and mouse handlers that cannot return a command, so it stages one — and the staging has to be
// drained for the switch to take effect.
func TestSwitchingTheWorkspaceRefiltersTheWorkItems(t *testing.T) {
	plane := &projectFilterPlane{}
	mux := http.NewServeMux()
	mux.Handle(apiv1connect.NewWorkItemServiceHandler(plane))
	mux.Handle(apiv1connect.NewProjectServiceHandler(plane))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cl := client.NewWithHTTPClient(client.Options{BaseURL: srv.URL}, srv.Client())
	m := NewApp(cl, &config.Profile{Name: "default", URL: srv.URL}, "v0",
		WithLaunchDir("/home/me/projects/WorkerTesting"))
	m.width, m.height = 140, 40
	m.RegisterScreen(TabWork, work.New(cl, subs.NewRegistry(), ""))
	m.SwitchTo(TabWork)
	m.onRailProjects(railProjectsMsg{Projects: []railProject{
		{ID: "prj-orch", Name: "Orchicon", Status: "active", Dir: "/home/me/projects/Orchicon"},
		{ID: "prj-test", Name: "WorkerTesting", Status: "active", Dir: "/home/me/projects/WorkerTesting"},
	}})
	runCtx(t, m, m.drainStaged(), runCtxCmdBudget)

	// The operator switches to the other project, as /project does.
	m.setProjectScope("prj-orch")
	if staged := m.drainStaged(); staged != nil {
		runCtx(t, m, staged, runCtxCmdBudget)
	}

	saw := plane.sawProject()
	if len(saw) < 2 {
		t.Fatalf("switching the workspace made no new request (%d total); the pane still shows the old "+
			"project's items", len(saw))
	}
	if last := saw[len(saw)-1]; last != "prj-orch" {
		t.Fatalf("the last request carried project_id %q, want prj-orch — the switch did not re-filter", last)
	}
}

// THE SCOPE REACHES THE SCREEN IN THE SAME CYCLE IT IS SET — no Update in between.
//
// This test exists because the end-to-end one above does NOT isolate this property: it drives commands back through
// App.Update, which re-runs rebindScreens, so the filter would be applied by the NEXT message even without the push.
// That "eventually" path is not good enough on its own — rebindScreens runs at the TOP of dispatch, before
// onRailProjects applies the scope in the same cycle, so the screen keeps the previous project's rows (or none)
// until some unrelated message arrives. In a quiet session that is a visible flash of the wrong project's work
// items, which is exactly the symptom reported.
//
// So: run ONLY the command onRailProjects returns, and nothing else.
func TestTheScopeReachesTheScreenWithoutAnotherMessage(t *testing.T) {
	plane := &projectFilterPlane{}
	mux := http.NewServeMux()
	mux.Handle(apiv1connect.NewWorkItemServiceHandler(plane))
	mux.Handle(apiv1connect.NewProjectServiceHandler(plane))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cl := client.NewWithHTTPClient(client.Options{BaseURL: srv.URL}, srv.Client())
	m := NewApp(cl, &config.Profile{Name: "default", URL: srv.URL}, "v0",
		WithLaunchDir("/home/me/projects/WorkerTesting"))
	m.width, m.height = 140, 40
	m.RegisterScreen(TabWork, work.New(cl, subs.NewRegistry(), ""))
	m.SwitchTo(TabWork)

	pushed := m.onRailProjects(railProjectsMsg{Projects: []railProject{
		{ID: "prj-orch", Name: "Orchicon", Status: "active", Dir: "/home/me/projects/Orchicon"},
		{ID: "prj-test", Name: "WorkerTesting", Status: "active", Dir: "/home/me/projects/WorkerTesting"},
	}})
	if pushed == nil {
		t.Fatal("onRailProjects returned no command — the scope was applied and nobody was told, so the Work " +
			"screen keeps the previous project's rows until an unrelated message happens to arrive")
	}
	// Run the batch and its members directly — NO App.Update, which is the whole point.
	runCtxNoUpdate(t, m, pushed)

	saw := plane.sawProject()
	if len(saw) == 0 {
		t.Fatal("the push produced no work-items request")
	}
	if last := saw[len(saw)-1]; last != "prj-test" {
		t.Fatalf("the pushed request carried project_id %q, want prj-test", last)
	}
}

// runCtxNoUpdate runs a command and its batched members WITHOUT feeding anything back through App.Update.
//
// It is the isolation this file needs: the shell's Update chain re-runs rebindScreens, which pushes the scope
// anyway, so a test that goes through Update cannot tell whether the caller pushed it eagerly.
func runCtxNoUpdate(t *testing.T, m *App, cmd tea.Cmd) {
	t.Helper()
	queue := []tea.Cmd{cmd}
	for steps := 0; len(queue) > 0 && steps < 32; steps++ {
		c := queue[0]
		queue = queue[1:]
		if c == nil {
			continue
		}
		done := make(chan tea.Msg, 1)
		go func(fn tea.Cmd) { done <- fn() }(c)
		var msg tea.Msg
		select {
		case msg = <-done:
		case <-time.After(runCtxCmdBudget):
			continue
		}
		if batch, ok := msg.(tea.BatchMsg); ok {
			queue = append(queue, flattenCmds(batch)...)
		}
	}
	_ = m
}
