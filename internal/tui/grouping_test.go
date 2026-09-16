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
