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

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// folderSeed is one folder and the conversations in it, for seedFolders.
type folderSeed struct {
	name    string
	convIDs []string
}

// seedFolders installs a category set in ONE call.
//
// ONE CALL, deliberately: applyCategorySet REPLACES the categories and assignments for the target type, so two
// seedConvCategory calls would silently drop the first folder and the test would pass for the wrong reason. The
// seed is a SLICE rather than a map so the folder order is deterministic — the rail renders in the server's
// order, and a map iteration would make this test flake.
func seedFolders(t *testing.T, m *App, seeds ...folderSeed) {
	t.Helper()
	cats := make([]*apiv1.Category, 0, len(seeds))
	var assigns []*apiv1.CategoryAssignment
	for _, s := range seeds {
		id := "cat-" + s.name
		cats = append(cats, &apiv1.Category{
			Id: id, Name: s.name,
			TargetType: apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_CONVERSATION,
		})
		for _, cid := range s.convIDs {
			assigns = append(assigns, &apiv1.CategoryAssignment{
				EntityId:   cid,
				CategoryId: id,
				TargetType: apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_CONVERSATION,
			})
		}
	}
	m.applyCategorySet(apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_CONVERSATION, cats, assigns)
}

// railFolders is the folder titles the rail would draw, in order.
func railFolders(m *App) []string {
	var out []string
	for _, r := range m.railRows() {
		if r.folder {
			out = append(out, r.title)
		}
	}
	return out
}

// hasFolder reports whether the rail draws a folder of this name.
func hasFolder(m *App, title string) bool {
	for _, name := range railFolders(m) {
		if strings.Contains(name, title) {
			return true
		}
	}
	return false
}

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

// A FOLDER WITH NOTHING IN THIS WORKSPACE IS NOT PART OF IT.
//
// The operator: "In the GUI, folders are visible no matter what project you are on. This is wrong. You should
// only see the categories/folders of the currently selected project." The rail had the same fault in the other
// direction: it drew a folder for EVERY category the tenant has as soon as the scope held any conversation, so
// scoping to a project produced folders whose conversations all live in a different one.
func TestFoldersAreScopedToTheWorkspace(t *testing.T) {
	// scopePlane: c-1 and c-2 are Alpha (p-1), c-3 is Beta (p-2), c-4 is unassigned. Each folder holds
	// conversations from ONE project, which is what makes "is this folder part of this workspace?" a real
	// question.
	m := scopePlane(t)
	seedFolders(t, m,
		folderSeed{name: "Alpha work", convIDs: []string{"c-1", "c-2"}},
		folderSeed{name: "Beta work", convIDs: []string{"c-3"}},
	)

	// SCOPED TO ALPHA: Alpha's folder, and NOT Beta's — whose chats are in another project.
	m.setProjectScope("p-1")
	if !hasFolder(m, "Alpha work") {
		t.Errorf("the Alpha workspace does not show its own folder: %v", railFolders(m))
	}
	if hasFolder(m, "Beta work") {
		t.Errorf("the Alpha workspace shows a folder holding nothing in it: %v — this is the operator's "+
			"\"folders are visible no matter what project you are on\"", railFolders(m))
	}

	// AND THE MIRROR, so the rule cannot pass by showing nothing at all.
	m.setProjectScope("p-2")
	if !hasFolder(m, "Beta work") {
		t.Errorf("the Beta workspace does not show its own folder: %v", railFolders(m))
	}
	if hasFolder(m, "Alpha work") {
		t.Errorf("the Beta workspace shows Alpha's folder: %v", railFolders(m))
	}

	// ALL PROJECTS KEEPS EVERY FOLDER. It filters nothing, so it is the scope where a newly created folder —
	// which holds nothing yet, because creating one assigns no conversations — must be visible to drag into.
	m.setProjectScope(projectScopeAll)
	if !hasFolder(m, "Alpha work") || !hasFolder(m, "Beta work") {
		t.Errorf("All projects is missing a folder: %v — it must show every one", railFolders(m))
	}
}

// AND AN EMPTY FOLDER KEEPS THE EXEMPTION IN ALL PROJECTS. This is the half that would make a newly created
// folder VANISH if the rule were applied everywhere, which is why the exemption is not a special case but the
// reason the rule is safe to apply at all.
func TestANewEmptyFolderStaysVisibleInAllProjects(t *testing.T) {
	m := scopePlane(t)
	seedFolders(t, m, folderSeed{name: "Just made", convIDs: nil})

	m.setProjectScope(projectScopeAll)
	if !hasFolder(m, "Just made") {
		t.Errorf("a folder with nothing in it is hidden in All projects: %v — a folder you just created "+
			"would disappear before you could drag anything into it", railFolders(m))
	}

	// And inside a workspace it is correctly absent until it holds something there.
	m.setProjectScope("p-1")
	if hasFolder(m, "Just made") {
		t.Errorf("an empty folder is drawn inside a workspace: %v", railFolders(m))
	}
}
