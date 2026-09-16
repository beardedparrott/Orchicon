package tui

// grouping_test.go — a grouping the operator created must actually SHOW UP.
//
// THE REPORT: "I created a conversation category and assigned a conversation to it, but it is not
// showing up in the UI."
//
// It was literally true, and the reason is worth restating because it is the whole of the bug:
// ListCategories answers with the categories AND the assignments in one response, and the shell
// collected only the first and DISCARDED the second — and on top of that, nothing anywhere rendered an
// assignment on a row. So the grouping existed on the server, arrived in the response, was thrown away,
// and had nowhere to appear. Two halves, one symptom.
//
// These tests drive the real path end to end: the stub answers ListCategories with an assignment, the
// loader stores it, and the RENDERED rail row shows it.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// TestAssignmentReachesTheShellCache: the first half — the assignment must survive the load.
func TestAssignmentReachesTheShellCache(t *testing.T) {
	cat := &apiv1.Category{
		Id: "cat-1", Name: "Research",
		TargetType: apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_CONVERSATION,
	}
	m, stub := categoryApp(t, cat)
	stub.assigned = map[string]string{"conv-07": "cat-1"}
	m = loadCats(t, m)

	id, name := m.CategoryOf(apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_CONVERSATION, "conv-07")
	if id != "cat-1" || name != "Research" {
		t.Fatalf("the assignment must be stored and resolvable, got (%q, %q)", id, name)
	}
	// AND IT IS SCOPED TO THE TARGET TYPE: the same id under a different type must not resolve, which is
	// what stops a conversation's grouping appearing on a worker that happens to share its id.
	if id, _ := m.CategoryOf(apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_WORKER, "conv-07"); id != "" {
		t.Fatalf("an assignment must not leak across target types, got %q", id)
	}
	if id, _ := m.CategoryOf(apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_CONVERSATION, "conv-99"); id != "" {
		t.Fatalf("an unassigned entity must resolve to nothing, got %q", id)
	}
}

// TestRailRowShowsItsGrouping: the second half — and the one the operator was actually looking at.
func TestRailRowShowsItsGrouping(t *testing.T) {
	cat := &apiv1.Category{
		Id: "cat-1", Name: "Research",
		TargetType: apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_CONVERSATION,
	}
	m, stub := categoryApp(t, cat)
	stub.assigned = map[string]string{"conv-01": "cat-1"}
	m = loadCats(t, m)
	m.attachRail(3)

	rail := m.rightRailView()
	if !strings.Contains(rail, "Research") {
		t.Fatalf("the assigned conversation's grouping must be VISIBLE in the rail:\n%s", rail)
	}
	// The grouping rides on the ROW IT BELONGS TO, not on every row: conv-02 and conv-03 are in no
	// grouping, so the name must appear exactly once.
	if n := strings.Count(rail, "Research"); n != 1 {
		t.Fatalf("only the assigned row may carry the grouping, got %d occurrences:\n%s", n, rail)
	}
	// And the row keeps its message count, which is the right-hand anchor the rail has always had.
	if !strings.Contains(rail, "2 msgs") {
		t.Fatalf("the row must keep its meta:\n%s", rail)
	}
}

// TestGroupingSurvivesAFreshLoad: the assignment map is REBUILT on every load rather than merged, so an
// unassign must clear the row rather than leave it showing a grouping it is no longer in.
func TestGroupingIsClearedWhenUnassigned(t *testing.T) {
	cat := &apiv1.Category{
		Id: "cat-1", Name: "Research",
		TargetType: apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_CONVERSATION,
	}
	m, stub := categoryApp(t, cat)
	stub.assigned = map[string]string{"conv-01": "cat-1"}
	m = loadCats(t, m)
	m.attachRail(3)
	if !strings.Contains(m.rightRailView(), "Research") {
		t.Fatal("precondition: the grouping must be visible")
	}

	// The assignment is removed server-side (an unassign) and the shell reloads.
	stub.assigned = nil
	m = loadCats(t, m)

	if strings.Contains(m.rightRailView(), "Research") {
		t.Fatalf("an unassigned conversation must stop showing the grouping:\n%s", m.rightRailView())
	}
}

// TestGroupedPaneNestsRowsUnderTheirCategory was removed rather than kept as a type assertion: the
// arrangement it wanted to pin is the SHARED helper's job and is tested directly in
// screenkit/group_test.go, while the WIRING (does the pane actually call it, with the right target
// type, and refuse writes aimed at a category row?) lives in the execution package — which is where the
// pane is. A test here could only have asserted that a method exists.

// --- managing a grouping from the pane that shows it -------------------------------------------------

// TestRenameCategoryIsPrefilledAndWrites is the GUI's folder rename, reached from a pane: the box opens
// with the CURRENT name and description (an empty box makes the operator retype a value they cannot
// see) and the write carries the grouping's own id.
func TestRenameCategoryIsPrefilledAndWrites(t *testing.T) {
	cat := &apiv1.Category{
		Id: "cat-1", Name: "Research", Description: "deep dives",
		TargetType: apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_CONVERSATION,
	}
	m, stub := categoryApp(t, cat)
	m = loadCats(t, m)

	m.OpenRenameCategory("cat-1")
	if m.catForm == nil {
		t.Fatal("OpenRenameCategory must open the rename form")
	}
	if got := m.catForm.Values[catAdminName]; got != "Research" {
		t.Fatalf("the form must be PREFILLED with the current name, got %q", got)
	}
	if got := m.catForm.Values[catAdminDesc]; got != "deep dives" {
		t.Fatalf("the form must be prefilled with the current description, got %q", got)
	}

	m.catForm.Set(catAdminName, "Investigations")
	cmd, err := m.catForm.Submit()
	if err != nil {
		t.Fatalf("a valid rename must submit: %v", err)
	}
	if cmd == nil {
		t.Fatal("a changed name must produce a write")
	}
	cmd()

	if stub.updatedID != "cat-1" {
		t.Fatalf("the update must target the grouping that was opened, got %q", stub.updatedID)
	}
	if stub.updatedName != "Investigations" {
		t.Fatalf("the update sent %q", stub.updatedName)
	}
}

// TestRenameCategoryRefusesABlankNameWithTheFormOpen: a save that closes and writes nothing is the
// silent-rejection class — the operator sees the box go away and the grouping is unchanged.
func TestRenameCategoryRefusesABlankNameWithTheFormOpen(t *testing.T) {
	cat := &apiv1.Category{
		Id: "cat-1", Name: "Research",
		TargetType: apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_CONVERSATION,
	}
	m, stub := categoryApp(t, cat)
	m = loadCats(t, m)
	m.OpenRenameCategory("cat-1")

	m.catForm.Set(catAdminName, "   ")
	cmd, err := m.catForm.Submit()
	if err == nil {
		t.Fatal("a blank name must be refused")
	}
	if cmd != nil {
		t.Fatal("nothing may be written when the name is blank")
	}
	if m.catForm.Submitted {
		t.Fatal("a refused rename must leave the form OPEN")
	}
	if stub.updatedID != "" {
		t.Fatalf("UpdateCategory ran for a blank name: %q", stub.updatedID)
	}
}

// TestRenameCategoryIsANoOpWhenUnchanged: the GUI's rule — a write on every save is audit noise.
func TestRenameCategoryIsANoOpWhenUnchanged(t *testing.T) {
	cat := &apiv1.Category{
		Id: "cat-1", Name: "Research", Description: "d",
		TargetType: apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_CONVERSATION,
	}
	m, stub := categoryApp(t, cat)
	m = loadCats(t, m)
	m.OpenRenameCategory("cat-1")
	cmd, err := m.catForm.Submit() // untouched
	if err != nil {
		t.Fatalf("an unchanged rename is valid: %v", err)
	}
	if cmd != nil {
		t.Fatal("an unchanged grouping must not be written")
	}
	if stub.updatedID != "" {
		t.Fatalf("an unchanged rename wrote: %q", stub.updatedID)
	}
}

// TestDeleteCategoryConfirmNamesWhatHappensToTheItems: the operator's real question is not whether the
// grouping goes but what happens to what was in it — so the confirm says so, and says how many.
func TestDeleteCategoryConfirmSaysWhereTheItemsGo(t *testing.T) {
	cat := &apiv1.Category{
		Id: "cat-1", Name: "Research",
		TargetType: apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_CONVERSATION,
	}
	m, stub := categoryApp(t, cat)
	stub.assigned = map[string]string{"conv-01": "cat-1", "conv-02": "cat-1"}
	m = loadCats(t, m)

	m.OpenDeleteCategory("cat-1")
	if m.bulkConfirm == nil {
		t.Fatal("OpenDeleteCategory must raise a confirm")
	}
	body := m.bulkConfirm.Body
	if !strings.Contains(body, "Uncategorized") {
		t.Fatalf("the confirm must say where the items go, got %q", body)
	}
	if !strings.Contains(body, "2") {
		t.Fatalf("the confirm must say HOW MANY items are affected, got %q", body)
	}

	// Dismissing writes nothing.
	m.bulkConfirmKey(tea.KeyMsg{Type: tea.KeyEsc})
	if stub.deletedID != "" {
		t.Fatalf("dismissing must not delete, got %q", stub.deletedID)
	}

	// Confirming deletes exactly that grouping. The dialog RETURNS the operation's command rather than
	// running it, so the test has to run it the way the shell does — merely receiving it would let a
	// delete that never fires pass.
	m.OpenDeleteCategory("cat-1")
	_, cmd := m.bulkConfirmKey(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("confirming must produce the delete command")
	}
	cmd()
	if stub.deletedID != "cat-1" {
		t.Fatalf("confirming must delete cat-1, got %q", stub.deletedID)
	}
}

// TestManagingAGroupingWithAStaleRowRefuses: the grouping can be deleted (here or in the GUI) between
// the fetch that drew the row and the keypress. Opening a form against an id that no longer exists
// would write to nothing and report nothing.
func TestManagingAStaleGroupingRefuses(t *testing.T) {
	m, stub := categoryApp(t)
	m = loadCats(t, m)
	m.dock.SetError("")

	m.OpenRenameCategory("cat_GONE")
	if m.catForm != nil {
		t.Fatal("a stale grouping must not open a rename form")
	}
	if m.dock.Err == "" {
		t.Fatal("a stale grouping must say so")
	}
	m.OpenDeleteCategory("cat_GONE")
	if m.bulkConfirm != nil {
		t.Fatal("a stale grouping must not raise a delete confirm")
	}
	if stub.deletedID != "" {
		t.Fatalf("nothing may be deleted for a stale row, got %q", stub.deletedID)
	}
}
