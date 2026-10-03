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
	catalog                bool
	workerID, workerName   string
}

func (f *fakeMCPHost) OpenProjectMCPModal(projectID, name string) tea.Cmd {
	f.projectID, f.projectName = projectID, name
	return nil
}

func (f *fakeMCPHost) OpenProjectMCPCatalog() tea.Cmd {
	f.catalog = true
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

// `M` OPENS THE SAME MODAL STRAIGHT ON ITS CATALOG VERB, so the shortcut and the modal cannot drift into
// two different add flows.
func TestMShiftOnProjectsOpensTheModalCatalog(t *testing.T) {
	m, host := projectsPane(t)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("M")})
	if host.projectID != "p1" {
		t.Fatalf("the catalog shortcut did not open the modal (got %q)", host.projectID)
	}
	if !host.catalog {
		t.Error("the catalog shortcut opened the modal but not its catalog verb")
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
