package tui

// rail_scope_empty_test.go — A SCOPE THAT HOLDS NOTHING IS NOT AN EMPTY RAIL.
//
// The rail filters the conversation list by the ACTIVE WORKSPACE, and the workspace now defaults to the project
// the operator launched from. On the operator's own instance those two facts collide: every conversation that
// predates the project column is UNASSIGNED, so scoping to a project filters the whole list out.
//
// Before this, that state fell through to the rail's default branch, which drew NO rows and then printed its
// row counter over an empty list — \"1-0/0\". The operator's 29 conversations become invisible and the pane
// reads as BROKEN rather than as FILTERED, which is the worse of the two failures: a filter you cannot see is
// one you cannot undo.

import (
	"strings"
	"testing"
)

// A WORKSPACE WITH NO CONVERSATIONS SAYS SO, names itself, and names the way out.
func TestAScopeThatHoldsNothingExplainsItself(t *testing.T) {
	// scopePlane's p-4 \"Delta\" is the project with NO conversations at all, which is precisely the state the
	// launch-directory default lands the operator in.
	m := scopePlane(t)
	m.setProjectScope("p-4")

	view := m.rightRailView()
	if strings.Contains(view, "1-0/0") {
		t.Errorf("a scope that filters everything out still printed the row counter \"1-0/0\" over an empty list — "+
			"the pane reads as broken instead of filtered: %q", view)
	}
	if !strings.Contains(view, "none in Delta") {
		t.Errorf("the rail does not say which workspace is empty: %q", view)
	}
	// IT NAMES THE SINGULAR. /projects is the NAVIGATION command — it takes the operator to the Work area's
	// Projects pane, which is not how you leave an empty workspace.
	if !strings.Contains(view, "/project to switch") {
		t.Errorf("the empty state does not name the way out — /project is how the operator switches workspace: %q", view)
	}
}

// AND IT DID NOT SWALLOW THE NORMAL PATH: a scope that holds conversations still draws them.
func TestANonEmptyScopeStillDrawsItsRows(t *testing.T) {
	m := scopePlane(t)
	m.setProjectScope("p-1") // Alpha, which has two conversations

	view := m.rightRailView()
	if strings.Contains(view, "none in ") {
		t.Errorf("a scope holding conversations rendered the empty-scope state: %q", view)
	}
	if strings.Contains(view, "1-0/0") {
		t.Errorf("a scope holding conversations rendered no rows: %q", view)
	}
}

// AND ALL PROJECTS IS UNTOUCHED, since it filters nothing and is the state everything predates.
func TestAllProjectsIsNeverTheEmptyScopeState(t *testing.T) {
	m := scopePlane(t)
	m.setProjectScope(projectScopeAll)
	if view := m.rightRailView(); strings.Contains(view, "none in ") {
		t.Errorf("All projects rendered the scoped-empty state: %q", view)
	}
}
