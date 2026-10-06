package control

// themes_project_bind_test.go — BINDING IS A DELIBERATE ACTION, NEVER A SIDE EFFECT OF THE CURSOR
// PREVIEW (criterion 2).
//
// Driven through the screen's REAL actionsForSelection and the cursor's real preview, against a fake
// shell that stands in for App's project-binding surface (projecttheme.go) — so a regression that wires
// "bind" to the same hook as "preview" shows up here, not just as a passing unit test on the App side.

import (
	"context"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// fakeProjectShell stands in for the App: it answers CurrentProjectTheme from its own map, and
// Bind/UnbindProjectTheme mutate it — exactly the split projecttheme.go makes, so a test against this
// fake proves the SAME thing a test against the real App would.
type fakeProjectShell struct {
	notices []string
	bound   map[string]string // project id -> palette
	project string            // the scope's active project id ("" = no project)
}

func (s *fakeProjectShell) DockError(string)    {}
func (s *fakeProjectShell) DockNotice(m string) { s.notices = append(s.notices, m) }

func (s *fakeProjectShell) SetTheme(name string) bool { return theme.Use(name) }

func (s *fakeProjectShell) BindProjectTheme(name string) (string, bool) {
	if s.project == "" {
		return "", false
	}
	if s.bound == nil {
		s.bound = map[string]string{}
	}
	s.bound[s.project] = name
	return s.project, true
}

func (s *fakeProjectShell) UnbindProjectTheme() (string, bool) {
	if s.project == "" {
		return "", false
	}
	if _, ok := s.bound[s.project]; !ok {
		return s.project, false
	}
	delete(s.bound, s.project)
	return s.project, true
}

func (s *fakeProjectShell) CurrentProjectTheme() (label, bound string, hasProject bool) {
	if s.project == "" {
		return "", "", false
	}
	return s.project, s.bound[s.project], true
}

// projectThemesModel loads the real Themes pane against a fake shell scoped to a real project, so bind
// and unbind are both offered.
func projectThemesModel(t *testing.T) (*Model, *fakeProjectShell) {
	t.Helper()
	t.Cleanup(func() { theme.Use(theme.DefaultName) })
	theme.Use("dark")

	m := New(nil, nil)
	sh := &fakeProjectShell{project: "p-1"}
	m.SetShell(sh)
	if !m.SelectSource("themes") {
		t.Fatal("fixture: themes is not a registered source")
	}
	items, _, err := m.fetchThemes(context.Background(), "")
	if err != nil {
		t.Fatalf("fetchThemes: %v", err)
	}
	if !m.LoadItems("themes", items, "") {
		t.Fatal("fixture: could not load the themes rows")
	}
	return m, sh
}

// MOVING THE CURSOR ACROSS PALETTES — the live preview — LEAVES THE PROJECT->PALETTE MAP UNCHANGED.
func TestBrowsingThemesNeverWritesAProjectBinding(t *testing.T) {
	m, sh := projectThemesModel(t)
	ids := themeRowIDs(t, m)

	for i := 0; i < len(ids)+2; i++ {
		m.Update(tea.KeyMsg{Type: tea.KeyDown})
	}
	if len(sh.bound) != 0 {
		t.Fatalf("browsing the themes list wrote a project binding: %v", sh.bound)
	}
}

// ONLY THE EXPLICIT BIND ACTION WRITES THE MAP, and it binds the HIGHLIGHTED palette — the one the
// cursor landed on, not whatever else might be active.
func TestExplicitBindActionWritesTheProjectBinding(t *testing.T) {
	m, sh := projectThemesModel(t)
	target := firstInactive(t, m)
	if !m.SelectItem("themes", target) {
		t.Fatalf("could not select %q", target)
	}

	acts := m.actionsForSelection()
	found := false
	for _, a := range acts {
		if a.Label == "bind" {
			found = true
			a.Apply()
		}
	}
	if !found {
		t.Fatalf("no bind action offered for %q on a real project scope (actions: %+v)", target, acts)
	}
	if sh.bound["p-1"] != target {
		t.Fatalf("bind did not record the binding: %v, want p-1 -> %q", sh.bound, target)
	}
}

// UNBIND REMOVES IT, and is offered only once the row IS the binding.
func TestUnbindActionRemovesTheProjectBinding(t *testing.T) {
	m, sh := projectThemesModel(t)
	target := firstInactive(t, m)
	sh.bound = map[string]string{"p-1": target}

	if !m.SelectItem("themes", target) {
		t.Fatalf("could not select %q", target)
	}
	acts := m.actionsForSelection()
	found := false
	for _, a := range acts {
		if a.Label == "unbind" {
			found = true
			a.Apply()
		}
		if a.Label == "bind" {
			t.Errorf("a row that IS the binding also offered \"bind\" — want only \"unbind\"")
		}
	}
	if !found {
		t.Fatalf("no unbind action offered for the bound row %q (actions: %+v)", target, acts)
	}
	if _, ok := sh.bound["p-1"]; ok {
		t.Errorf("unbind did not remove the binding: %v", sh.bound)
	}
}

// NO PROJECT SCOPE OFFERS NEITHER — there is nothing to bind to.
func TestNoProjectScopeOffersNoBindAction(t *testing.T) {
	m, _ := projectThemesModel(t)
	m.Shell().(*fakeProjectShell).project = ""
	target := firstInactive(t, m)
	if !m.SelectItem("themes", target) {
		t.Fatalf("could not select %q", target)
	}
	for _, a := range m.actionsForSelection() {
		if a.Label == "bind" || a.Label == "unbind" {
			t.Errorf("a scope with no project offered %q", a.Label)
		}
	}
}
