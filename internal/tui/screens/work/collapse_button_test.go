package work

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"google.golang.org/protobuf/types/known/timestamppb"

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

// archiveTreePlane is treePlane's archive counterpart: an archived EPIC with an
// archived child, so the archive view has a real parent/child edge to draw. The
// archived epic's own parent ("root") stays ACTIVE, which is what exercises the
// ghost-anchor chain.
func archiveTreePlane(t *testing.T) *Model {
	t.Helper()
	p := newPlane()
	p.seedProject("proj-1", "Orchicon")
	// An ACTIVE root epic — the ancestor an archived item hangs under.
	p.addItem(&apiv1.WorkItem{Id: "root", Title: "Active Root", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC, Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING, ProjectId: "proj-1"})
	// An archived feature under the active root, with an archived child of its own.
	p.addArchive(&apiv1.WorkItem{Id: "af", Title: "Archived Feature", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_FEATURE, Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_ARCHIVED, ArchivedFromStatus: "succeeded", ProjectId: "proj-1", ParentId: "root"})
	p.addArchive(&apiv1.WorkItem{Id: "at", Title: "Archived Task", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK, Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_ARCHIVED, ArchivedFromStatus: "succeeded", ProjectId: "proj-1", ParentId: "af"})
	m := newModel(t, p)
	m.SelectSource(srcWorkItems)
	load(t, m, srcWorkItems)
	press(t, m, "v") // tree -> archive
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
	// THE TREE OPENS COLLAPSED, so the control's first label is "expand all" — and
	// that is the state the operator asked for, not a regression in the button.
	if n := len(tbl.VisibleRows()); n != 1 {
		t.Fatalf("fresh tree visible = %d, want 1 (a collapsed tree shows roots only)", n)
	}
	if v := ansi.Strip(m.View()); !strings.Contains(v, "expand all") {
		t.Fatalf("a collapsed tree must offer to expand:\n%s", v)
	}

	x, y := findText(t, m.View(), "expand all")
	clickAt(t, m, x, y)
	if !tbl.AllExpanded() {
		t.Fatal("clicking the button must expand every node")
	}
	if n := len(tbl.VisibleRows()); n != 2 {
		t.Fatalf("expanded visible rows = %d, want 2", n)
	}
	// The label now states the opposite action, and clicking it collapses again.
	if v := ansi.Strip(m.View()); !strings.Contains(v, "collapse all") {
		t.Fatalf("the button must offer to collapse once expanded:\n%s", v)
	}
	x, y = findText(t, m.View(), "collapse all")
	clickAt(t, m, x, y)
	if tbl.AllExpanded() {
		t.Fatal("clicking the button again must collapse every node")
	}
	if n := len(tbl.VisibleRows()); n != 1 {
		t.Fatalf("collapsed visible rows = %d, want 1 (the epic)", n)
	}
}

// THE ARCHIVE VIEW CARRIES THE CONTROL TOO — it is a tree now, which is the
// change that makes the button meaningful there. The operator's report was that
// the archive view "is still not a tree view"; this is the same fix seen from the
// control's side.
func TestCollapseAllButtonWorksInTheArchiveView(t *testing.T) {
	m := archiveTreePlane(t)
	if m.ViewMode() != viewArchive {
		t.Fatalf("fixture: expected the archive view, got %q", m.ViewMode())
	}
	tbl := m.Base.ActiveTable()
	// The archive opens EXPANDED (its roots are ghost anchors — see archiveRows), so the
	// whole hierarchy is already on screen here.
	// Ghost anchor ("Active Root") + the archived feature + the archived task.
	if n := len(tbl.VisibleRows()); n != 3 {
		t.Fatalf("archive visible rows = %d, want 3 (root anchor, feature, task):\n%s",
			n, ansi.Strip(m.View()))
	}

	x, y := findText(t, m.View(), "collapse all")
	clickAt(t, m, x, y)
	if tbl.AllExpanded() {
		t.Fatal("the archive view's control must collapse its nodes")
	}
	// Collapsing every parent leaves only the TOP-LEVEL rows. Here that is the ghost
	// anchor alone: the archived feature sits under it, and the archived task under
	// the feature — so one visible row is the correct, fully-collapsed archive.
	if n := len(tbl.VisibleRows()); n != 1 {
		t.Fatalf("collapsed archive visible rows = %d, want 1 (the root anchor)", n)
	}
	// The anchor is still drawn — collapsing its subtree must not drop the row that
	// keeps the hierarchy connected.
	if v := ansi.Strip(m.View()); !strings.Contains(v, "Active Root") {
		t.Fatalf("the ghost anchor must survive a collapse:\n%s", v)
	}
	x, y = findText(t, m.View(), "expand all")
	clickAt(t, m, x, y)
	if !tbl.AllExpanded() {
		t.Fatal("clicking again must expand every archive node")
	}
	if n := len(tbl.VisibleRows()); n != 3 {
		t.Fatalf("expanded archive visible rows = %d, want 3", n)
	}
}

// A pane with NO parent draws no control: on a flat list it would do nothing, and
// a dead control is worse than none. (This replaces a test that asserted the
// control was absent in the ARCHIVE view, on the premise that the archive was
// flat — the premise item 3 removes.)
func TestCollapseAllButtonAbsentOnAFlatList(t *testing.T) {
	p := newPlane()
	p.seedProject("proj-1", "Orchicon")
	// Two unparented epics: a legitimate page with no hierarchy at all.
	p.addItem(&apiv1.WorkItem{Id: "a", Title: "Alpha", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC, Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING, ProjectId: "proj-1"})
	p.addItem(&apiv1.WorkItem{Id: "b", Title: "Beta", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC, Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING, ProjectId: "proj-1"})
	m := newModel(t, p)
	m.SelectSource(srcWorkItems)
	load(t, m, srcWorkItems)

	if v := ansi.Strip(m.View()); strings.Contains(v, "collapse all") {
		t.Fatalf("a flat list must not draw a tree control:\n%s", v)
	}
	// …and the sort control is still there, so the row is not simply missing.
	if v := ansi.Strip(m.View()); !strings.Contains(v, "sort:") {
		t.Fatalf("the top row must still carry the sort control:\n%s", v)
	}
}

// addArchive seeds an ARCHIVED work item — one with archived_at set, which is what
// the plane's archive partition is actually keyed on (the fake's ListWorkItems
// filters on `ArchivedAt != nil != include_archived`). Without the timestamp the
// item would land in the ACTIVE view and the archive fixture would be testing the
// wrong set entirely.
func (p *fakePlane) addArchive(w *apiv1.WorkItem) *apiv1.WorkItem {
	w.ArchivedAt = timestamppb.Now()
	return p.addItem(w)
}
