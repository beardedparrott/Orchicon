package work

// keyclaim_test.go — the screen must not claim the keyboard by accident.

import (
	"testing"

	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// selectAProject focuses the Projects source and gives it one row — the state in which the
// stray assignment ran (projectActions needs an ActiveItem to get past its guard).
func selectAProject(t *testing.T, m *Model) {
	t.Helper()
	if !m.Base.SelectSource(srcProjects) {
		t.Fatal("fixture: could not select the Projects source")
	}
	if !m.Base.LoadItems(srcProjects, []kit2.Item{{ID: "p1", Title: "Orchicon", Meta: "active"}}, "") {
		t.Fatal("fixture: could not load a project row")
	}
	if _, ok := m.Base.ActiveItem(); !ok {
		t.Fatal("fixture: no active item after loading a row")
	}
}

// THE REGRESSION. Building the Projects action set must not latch a key claim.
func TestProjectActionsDoNotLatchAKeyClaim(t *testing.T) {
	m := newModel(t, &fakePlane{})
	selectAProject(t, m)

	// Building the actions is exactly what actionsForSelection does on every render of
	// the hints / key bindings.
	acts := m.projectActions()
	if len(acts) == 0 {
		t.Fatal("fixture: projectActions returned nothing, so the guard never ran")
	}
	if m.formLoading {
		t.Fatal("projectActions set formLoading — that latches ClaimsKeys forever and " +
			"swallows the submenu's arrows/Enter and the shell's shift+tab")
	}
	if m.ClaimsKeys() {
		t.Fatal("the screen claims every key just because a project is selected")
	}
	// And nothing else in the selection path claims either.
	_ = m.actionsForSelection()
	if m.ClaimsKeys() {
		t.Fatal("building the selection's actions made the screen claim the keyboard")
	}
}

// The claim must still be exactly what it says: while a form/modal is up the screen claims, a
// resting pane does not.
func TestClaimsKeysTracksRealModalState(t *testing.T) {
	m := newModel(t, &fakePlane{})
	selectAProject(t, m)
	if m.ClaimsKeys() {
		t.Fatal("resting pane claims keys")
	}
	m.Open = kit2.Confirm("x", "y", "z")
	if !m.ClaimsKeys() {
		t.Fatal("an open confirmation dialog must claim keys")
	}
	m.Open = nil
	if m.ClaimsKeys() {
		t.Fatal("closing the dialog must release the claim")
	}
}

// A source the operator merely lands on must never claim keys — checked for all three
// sources, since the bug was source-specific and would have been caught by this.
func TestNoSourceClaimsKeysJustByBeingSelected(t *testing.T) {
	for _, src := range []string{srcProjects, srcWorkItems, srcImages} {
		m := newModel(t, &fakePlane{})
		if !m.Base.SelectSource(src) {
			t.Fatalf("fixture: could not select %q", src)
		}
		m.Base.LoadItems(src, []kit2.Item{{ID: "x1", Title: "Row", Meta: "active"}}, "")
		_ = m.actionsForSelection()
		if m.ClaimsKeys() {
			t.Errorf("source %q claims every key once a row is selected", src)
		}
	}
}
