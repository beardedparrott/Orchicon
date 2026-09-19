package tui

// project_service_stub_test.go — a ProjectService backed by a real Connect server, so a test can drive the
// WIRING (does a load actually reach the service?) rather than only the pure functions around it.
//
// The operator's report was a wiring fault: the project list was never FETCHED, so the workspace picker offered
// nothing but "All projects". Nothing in this file's siblings could see it — they all test rules, and a rule
// cannot notice a request that was never made. This is the fixture that can.

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
	"github.com/beardedparrott/orchicon/internal/tui/config"
)

// stubProjectList answers ListProjects with a fixed set of names, and counts the calls so a test can assert a
// fetch happened — or that one was deliberately skipped.
type stubProjectList struct {
	apiv1connect.UnimplementedProjectServiceHandler

	names  []string
	direcs []string

	// failFirst makes the FIRST ListProjects fail, so a test can drive the transient-failure path — a load
	// that errors must not leave the picker permanently empty.
	failFirst bool

	mu    sync.Mutex
	calls int
}

func (s *stubProjectList) ListProjects(_ context.Context, _ *connect.Request[apiv1.ListProjectsRequest]) (*connect.Response[apiv1.ListProjectsResponse], error) {
	s.mu.Lock()
	s.calls++
	shouldFail := s.failFirst && s.calls == 1
	s.mu.Unlock()
	if shouldFail {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("transient"))
	}
	out := make([]*apiv1.Project, 0, len(s.names))
	for i, n := range s.names {
		dir := ""
		if i < len(s.direcs) {
			dir = s.direcs[i]
		}
		out = append(out, &apiv1.Project{Id: "prj-" + n, Name: n, ProjectDir: dir})
	}
	return connect.NewResponse(&apiv1.ListProjectsResponse{Projects: out}), nil
}

// callCount is the number of ListProjects calls made.
func (s *stubProjectList) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// appWithProjectService builds a shell whose ProjectService is the stub, on the Ask tab.
func appWithProjectService(t *testing.T, stub apiv1connect.ProjectServiceHandler) *App {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(apiv1connect.NewProjectServiceHandler(stub))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cl := client.NewWithHTTPClient(client.Options{BaseURL: srv.URL}, srv.Client())
	m := NewApp(cl, &config.Profile{Name: "default", URL: srv.URL}, "v0")
	m.width, m.height = 120, 40
	m.RegisterScreen(TabAsk, &stubScreen{id: "ask"})
	m.RegisterScreen(TabWork, &stubScreen{id: "work"})
	m.SwitchTo(TabAsk)
	return m
}

// flattenCmds expands a command's result into its member commands, mirroring how bubbletea's runtime delivers
// a BatchMsg: each member is run and its message fed back. A test that runs only the outer command would
// deliver a BatchMsg the shell never sees in production.
func flattenCmds(msg tea.Msg) []tea.Cmd {
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		if msg == nil {
			return nil
		}
		m := msg
		return []tea.Cmd{func() tea.Msg { return m }}
	}
	out := make([]tea.Cmd, 0, len(batch))
	for _, c := range batch {
		if c == nil {
			continue
		}
		out = append(out, c)
	}
	return out
}
