package work

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

func treePlane(t *testing.T) *Model {
	t.Helper()
	p := newPlane()
	p.seedProject("proj-1", "Orchicon")
	p.addItem(&apiv1.WorkItem{Id: "e", Title: "Epic", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC, Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING, ProjectId: "proj-1"})
	p.addItem(&apiv1.WorkItem{Id: "k", Title: "Kid", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK, Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING, ProjectId: "proj-1", ParentId: "e"})
	m := newModel(t, p)
	m.SelectSource(srcWorkItems)
	load(t, m, srcWorkItems)
	return m
}

// findText locates a literal substring in the RENDERED frame, returning its
// absolute (x, y) — so a click test aims at what is actually on screen rather
// than at coordinates duplicated from the layout code.
func findText(t *testing.T, view, want string) (x, y int) {
	t.Helper()
	for i, line := range strings.Split(view, "\n") {
		plain := ansi.Strip(line)
		if j := strings.Index(plain, want); j >= 0 {
			return j, i
		}
	}
	t.Fatalf("rendered frame does not contain %q:\n%s", want, ansi.Strip(view))
	return 0, 0
}

// clickAt presses the left mouse button at an absolute TERMINAL cell. A screen
// renders from its own row 0, so the shell's chrome is added to the row a
// screen-level test measured.
func clickAt(t *testing.T, m *Model, x, y int) {
	t.Helper()
	m.Update(tea.MouseMsg{
		X: x, Y: y + kit2.ShellChromeRows(),
		Button: tea.MouseButtonLeft, Action: tea.MouseActionPress,
	})
}

// The operator: "There is still no collapse all button at the top of work
// items." The Tree pane carries a CLICKABLE control on its top row, and its
// label states what pressing it will do.
func TestCollapseAllButtonRendersAndWorks(t *testing.T) {
	m := treePlane(t)
	tbl := m.Base.ActiveTable()
	if n := len(tbl.VisibleRows()); n != 2 {
		t.Fatalf("fresh tree visible = %d, want 2", n)
	}

	// The button is drawn, and offers to COLLAPSE while everything is open.
	x, y := findText(t, m.View(), "collapse all")
	clickAt(t, m, x, y)

	if tbl.AllExpanded() {
		t.Fatal("clicking the button must collapse every node")
	}
	if n := len(tbl.VisibleRows()); n != 1 {
		t.Fatalf("collapsed visible rows = %d, want 1 (the epic)", n)
	}
	// The label now states the opposite action, and clicking it expands again.
	if v := ansi.Strip(m.View()); !strings.Contains(v, "expand all") {
		t.Fatalf("the button must offer to expand once collapsed:\n%s", v)
	}
	x, y = findText(t, m.View(), "expand all")
	clickAt(t, m, x, y)
	if !tbl.AllExpanded() {
		t.Fatal("clicking the button again must expand every node")
	}
	if n := len(tbl.VisibleRows()); n != 2 {
		t.Fatalf("expanded visible rows = %d, want 2", n)
	}
}

// The flat views have no tree, so the button is not drawn there (a control that
// does nothing is worse than no control).
func TestCollapseAllButtonAbsentInFlatViews(t *testing.T) {
	m := treePlane(t)
	press(t, m, "Z") // Archive — a flat list
	load(t, m, srcWorkItems)
	if v := ansi.Strip(m.View()); strings.Contains(v, "collapse all") {
		t.Fatalf("the archive view must not draw a tree control:\n%s", v)
	}
}
