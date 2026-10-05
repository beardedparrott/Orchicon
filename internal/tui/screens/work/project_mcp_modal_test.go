package work

// project_mcp_modal_test.go — THE PROJECTS PANE OPENS THE MCP + SKILLS MODAL.
//
// The operator: "It doesn't pop up a modal like /mcp or /scope does." The modal itself is tested in the
// tui package (it is the shell's); what is asserted HERE is the wiring an operator actually presses: `m`
// on the Projects pane asks the shell to open it, for the SELECTED project.

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// fakeMCPHost records what the pane asked the shell for.
type fakeMCPHost struct {
	projectID, projectName string
	workerID, workerName   string
}

func (f *fakeMCPHost) OpenProjectMCPModal(projectID, name string) tea.Cmd {
	f.projectID, f.projectName = projectID, name
	return nil
}

// projectsPane focuses the Projects pane with one project on it, and wires a host.
func projectsPane(t *testing.T) (*Model, *fakeMCPHost) {
	t.Helper()
	m := newModel(t, &fakePlane{})
	host := &fakeMCPHost{}
	m.Base.SetShell(host)
	if !m.Base.SelectSource(srcProjects) {
		t.Fatal("fixture: cannot focus the Projects pane")
	}
	if !m.Base.LoadItems(srcProjects, []kit2.Item{{ID: "p1", Title: "Orchicon"}}, "") {
		t.Fatal("fixture: cannot load the project row")
	}
	return m, host
}

// `m` OPENS THE MODAL FOR THE SELECTED PROJECT — not a create form, and not nothing.
func TestMOnProjectsOpensTheMCPModal(t *testing.T) {
	m, host := projectsPane(t)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("m")})
	if host.projectID != "p1" {
		t.Fatalf("`m` did not open the modal for the selected project (got %q, notice=%q)",
			host.projectID, m.Notice())
	}
	if host.projectName != "Orchicon" {
		t.Errorf("the modal was opened without the project's name (%q) — its title would not say WHICH "+
			"project it is managing", host.projectName)
	}
}

// THERE IS NO SECOND CHORD FOR THE CATALOG. The modal offers both add paths as keys inside itself
// (`a` add, `c` catalog), so a shortcut that jumped straight to one of them was a key to a key — and it
// made the project surface differ from the conversation one, which has only ever had the single opener.
//
// The operator: "Instead of 'm' and 'M' for add mcp server or add from catalog, can't we put both of
// those operations into 'm' and get rid of the additional control?"
func TestThereIsNoSeparateCatalogChord(t *testing.T) {
	m, host := projectsPane(t)
	// `M` must now be INERT on this pane rather than opening the modal on a verb the operator can reach
	// from the modal itself. (Nothing else on the Projects pane binds it.)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("M")})
	if host.projectID != "" {
		t.Errorf("`M` still opens the modal (for %q) — the second control was meant to be gone", host.projectID)
	}
	// And `m` is the one opener.
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("m")})
	if host.projectID != "p1" {
		t.Errorf("`m` did not open the modal (got %q)", host.projectID)
	}
}

// WITH NO HOST, `m` SAYS SO RATHER THAN DOING NOTHING. A key that appears to do nothing reads as broken.
func TestMWithoutAHostSaysSo(t *testing.T) {
	m := newModel(t, &fakePlane{})
	if !m.Base.SelectSource(srcProjects) {
		t.Fatal("fixture: focus")
	}
	m.Base.LoadItems(srcProjects, []kit2.Item{{ID: "p1", Title: "Orchicon"}}, "")
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("m")})
	if m.Notice() == "" {
		t.Error("`m` was silently inert with no modal host — it must name the reason")
	}
}
