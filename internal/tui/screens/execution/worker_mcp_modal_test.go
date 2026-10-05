package execution

// worker_mcp_modal_test.go — THE WORKERS PANE OPENS THE MCP + SKILLS MODAL.
//
// The operator: "It doesn't pop up a modal like /mcp or /scope does." The modal belongs to the shell (it
// layers over every screen); what is asserted here is the wiring: `m` on the Workers pane asks for it for
// the SELECTED worker.

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// fakeWorkerHost records what the pane asked the shell for.
type fakeWorkerHost struct {
	workerID, workerName string
	calls                int
}

func (f *fakeWorkerHost) OpenWorkerMCPModal(workerID, name string) tea.Cmd {
	f.workerID, f.workerName, f.calls = workerID, name, f.calls+1
	return nil
}

// workersPane focuses the Workers pane with one worker on it, and wires a host.
func workersPane(t *testing.T) (*Model, *fakeWorkerHost) {
	t.Helper()
	m := newModel(t, execPlane())
	host := &fakeWorkerHost{}
	m.Base.SetShell(host)
	if !m.Base.SelectSource(srcWorkers) {
		t.Fatal("fixture: cannot focus the Workers pane")
	}
	if !m.Base.LoadItems(srcWorkers, []kit2.Item{
		{ID: "wk1", Title: "Sweeper", Meta: "published"},
	}, "") {
		t.Fatal("fixture: cannot load the worker row")
	}
	return m, host
}

// `m` OPENS THE MODAL FOR THE SELECTED WORKER.
func TestMOnWorkersOpensTheMCPModal(t *testing.T) {
	m, host := workersPane(t)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("m")})
	if host.calls != 1 {
		t.Fatalf("`m` did not ask for the modal (calls=%d, notice=%q)", host.calls, m.Notice())
	}
	if host.workerID != "wk1" {
		t.Errorf("the modal was opened for %q, want the selected worker", host.workerID)
	}
	if host.workerName != "Sweeper" {
		t.Errorf("the modal was opened without the worker's name (%q) — its title would not say WHICH "+
			"version it is managing", host.workerName)
	}
}

// `m` CLAIMS NOTHING ON ANOTHER PANE. It is gated on the Workers source, so the same key on Workflows (or
// anywhere else) still belongs to that pane.
func TestMOnAnotherPaneIsNotTheModal(t *testing.T) {
	m := newModel(t, execPlane())
	host := &fakeWorkerHost{}
	m.Base.SetShell(host)
	if !m.Base.SelectSource(srcWorkflows) {
		t.Fatal("fixture: focus")
	}
	m.Base.LoadItems(srcWorkflows, []kit2.Item{{ID: "wf1", Title: "SDLC", Meta: "published"}}, "")
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("m")})
	if host.calls != 0 {
		t.Error("`m` on the Workflows pane asked for the worker MCP modal — the key must stay that pane's")
	}
}

// A WORKER WITH NO VERSION IS TOLD SO rather than opening a modal whose save would go nowhere. The check
// lives on the shell (which does the fetch), so this asserts the pane still ASKS — the refusal is the
// shell's, and the pane must not pre-empt it with a guess.
func TestMOnWorkersAsksEvenBeforeAVersionIsKnown(t *testing.T) {
	m, host := workersPane(t)
	m.Base.SelectItem(srcWorkers, "wk1")
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("m")})
	if host.calls != 1 {
		t.Fatal("the pane did not ask for the modal — the version check belongs to the shell that fetches")
	}
	_ = apiv1.Worker{}
}
