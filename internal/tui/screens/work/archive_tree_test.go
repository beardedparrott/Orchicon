package work

// archive_tree_test.go — the Archive view is a REAL tree, and the pane says which
// mode it is in.
//
// Two operator reports, and they are the same defect seen from two sides:
//
//	"The TUI archive view is still not a tree view."
//	"When in archive or normal mode in the TUI, it should say so at the top near
//	 the search box to indicate what mode you are in."
//
// The archive view rendered a FLAT list (each archived item a root row) while the
// Tree view next to it rendered the true Epic→Feature→Task→Subtask DAG — so the
// hierarchy vanished exactly when the operator was inspecting finished work. A
// mode label matters for the same reason: the two views show DIFFERENT SETS of
// items, so not knowing which one is on is not a cosmetic problem.

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// archivedRow is a row in the ARCHIVE partition: archived_at set (which the
// plane's filter is keyed on), and the status it will restore to.
func archivedRow(id, parent, from string, kind apiv1.WorkItemKind) *apiv1.WorkItem {
	return &apiv1.WorkItem{
		Id: id, Title: id, Kind: kind, ParentId: parent,
		ProjectId: "proj-1", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_ARCHIVED,
		ArchivedFromStatus: from,
	}
}

// archiveRowsByID indexes the pane's REAL rows by id (kit2.Row), so an assertion
// can name a row's depth/parent/meta rather than counting visible lines.
func archiveRowsByID(m *Model) map[string]kit2.Row {
	out := map[string]kit2.Row{}
	for _, r := range m.Base.ActiveTable().Rows {
		out[r.ID] = r
	}
	return out
}

// TestArchiveViewIsATree: the archived hierarchy is nested, with depth and parent
// metadata — the properties that make the pane a tree rather than a list.
func TestArchiveViewIsATree(t *testing.T) {
	p := newPlane()
	p.seedProject("proj-1", "Orchicon")
	// A fully-archived subtree: epic → feature → task, all archived.
	p.addArchive(archivedRow("ae", "", "succeeded", apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC))
	p.addArchive(archivedRow("af", "ae", "succeeded", apiv1.WorkItemKind_WORK_ITEM_KIND_FEATURE))
	p.addArchive(archivedRow("at", "af", "failed", apiv1.WorkItemKind_WORK_ITEM_KIND_TASK))

	m := newModel(t, p)
	m.SelectSource(srcWorkItems)
	load(t, m, srcWorkItems)
	press(t, m, "v")
	load(t, m, srcWorkItems)
	if m.ViewMode() != viewArchive {
		t.Fatalf("fixture: expected the archive view, got %q", m.ViewMode())
	}

	tbl := m.Base.ActiveTable()
	if n := len(tbl.Rows); n != 3 {
		t.Fatalf("archive rows = %d, want 3:\n%s", n, ansi.Strip(m.View()))
	}
	// The rows must come back ROOT FIRST, depth-ascending — that is what the table's
	// tree machinery walks. A flat list would render all three at depth 0.
	want := []struct {
		id     string
		depth  int
		parent string
		kids   bool
	}{
		{"ae", 0, "", true},
		{"af", 1, "ae", true},
		{"at", 2, "af", false},
	}
	for i, w := range want {
		r := tbl.Rows[i]
		if r.ID != w.id || r.Depth != w.depth || r.Parent != w.parent || r.Expand != w.kids {
			t.Fatalf("row %d = {%s depth=%d parent=%q kids=%v}, want {%s depth=%d parent=%q kids=%v}",
				i, r.ID, r.Depth, r.Parent, r.Expand, w.id, w.depth, w.parent, w.kids)
		}
	}
	// The indent is the PANE's job (from Depth): a title that pre-pads itself would
	// be indented twice.
	for _, r := range tbl.Rows {
		if strings.HasPrefix(r.Cells[0], " ") {
			t.Fatalf("archive title %q must not pre-indent", r.Cells[0])
		}
	}
	// And the rendered frame shows the nested structure via the +/- markers.
	v := ansi.Strip(m.View())
	if !strings.Contains(v, "[epic] ae") || !strings.Contains(v, "[task] at") {
		t.Fatalf("the archive frame must draw every level:\n%s", v)
	}
}

// TestArchiveViewAnchorsAnActiveAncestor: an archived item whose parent is still
// ACTIVE has no parent row in the archived page, so without an anchor it would
// float to the top level — the flat list by another route. The active ancestor is
// rendered as a marked, NON-restorable row instead.
func TestArchiveViewAnchorsAnActiveAncestor(t *testing.T) {
	p := newPlane()
	p.seedProject("proj-1", "Orchicon")
	// An ACTIVE grandparent epic, and an archived feature → task beneath it.
	p.addItem(&apiv1.WorkItem{Id: "live", Title: "Live Epic", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC,
		ProjectId: "proj-1", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING})
	p.addArchive(archivedRow("af", "live", "succeeded", apiv1.WorkItemKind_WORK_ITEM_KIND_FEATURE))
	p.addArchive(archivedRow("at", "af", "succeeded", apiv1.WorkItemKind_WORK_ITEM_KIND_TASK))

	m := newModel(t, p)
	m.SelectSource(srcWorkItems)
	load(t, m, srcWorkItems)
	press(t, m, "v")
	load(t, m, srcWorkItems)

	rows := archiveRowsByID(m)
	anchor, ok := rows["live"]
	if !ok {
		t.Fatalf("the ACTIVE ancestor must be rendered as an anchor, rows = %v", archiveRowIDs(m))
	}
	if anchor.Depth != 0 || !anchor.Expand {
		t.Fatalf("anchor = depth %d expand %v, want a root with a toggle", anchor.Depth, anchor.Expand)
	}
	if !strings.Contains(anchor.Meta, "not archived") {
		t.Fatalf("the anchor must SAY it is not archived, meta = %q", anchor.Meta)
	}
	// The archived pair keeps its real nesting under the anchor.
	if rows["af"].Parent != "live" || rows["af"].Depth != 1 {
		t.Fatalf("archived feature must sit under the anchor: %+v", rows["af"])
	}
	if rows["at"].Parent != "af" || rows["at"].Depth != 2 {
		t.Fatalf("archived task must nest under the feature: %+v", rows["at"])
	}
	// A NON-anchor row still says what it restores to.
	if !strings.Contains(rows["af"].Meta, "restores to succeeded") {
		t.Fatalf("an archived row must name its restore status, meta = %q", rows["af"].Meta)
	}
}

// TestArchiveAnchorIsNotRestorable: offering "restore" on an ACTIVE row would send
// the plane a request it refuses, and the operator would read the failure as an
// archived item that would not come back.
func TestArchiveAnchorIsNotRestorable(t *testing.T) {
	p := newPlane()
	p.seedProject("proj-1", "Orchicon")
	p.addItem(&apiv1.WorkItem{Id: "live", Title: "Live Epic", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC,
		ProjectId: "proj-1", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING})
	p.addArchive(archivedRow("af", "live", "succeeded", apiv1.WorkItemKind_WORK_ITEM_KIND_FEATURE))

	m := newModel(t, p)
	m.SelectSource(srcWorkItems)
	load(t, m, srcWorkItems)
	press(t, m, "v")
	load(t, m, srcWorkItems)

	// The archived row offers restore…
	if !m.SelectItem(srcWorkItems, "af") {
		t.Fatal("could not select the archived row")
	}
	if _, ok := m.actionByKey("R"); !ok {
		t.Fatalf("an archived row must offer restore: %v", actionLabels(m))
	}
	// …and the anchor does not.
	if !m.SelectItem(srcWorkItems, "live") {
		t.Fatal("could not select the anchor")
	}
	if a, ok := m.actionByKey("R"); ok {
		t.Fatalf("an ACTIVE anchor must not offer restore, got %+v", a)
	}
	if labels := archiveActionLabels(m); len(labels) != 0 {
		t.Fatalf("an anchor has no archive-view actions at all, got %v", labels)
	}
}

// archiveActionLabels lists the actions offered for the currently focused row in
// the ARCHIVE view — the row-bound set, not the pane's top-row controls (those are
// ActiveTableActions).
func archiveActionLabels(m *Model) []string {
	var out []string
	for _, a := range m.itemActions() {
		out = append(out, a.Label)
	}
	return out
}

// archiveRowIDs lists the pane's rendered row ids in order (for a readable
// failure message).
func archiveRowIDs(m *Model) []string {
	out := make([]string, 0, len(m.Base.ActiveTable().Rows))
	for _, r := range m.Base.ActiveTable().Rows {
		out = append(out, r.ID)
	}
	return out
}

// TestThePaneSaysWhichModeItIsIn: the label beside the search box names the view
// AND what its rows are, and it follows the switch.
func TestThePaneSaysWhichModeItIsIn(t *testing.T) {
	m := treePlane(t)
	v := ansi.Strip(m.View())
	if !strings.Contains(v, "TREE") {
		t.Fatalf("the tree mode must be stated on the control row:\n%s", v)
	}
	if !strings.Contains(v, "active items") {
		t.Fatalf("the label must say what the rows ARE:\n%s", v)
	}

	press(t, m, "v")
	load(t, m, srcWorkItems)
	v = ansi.Strip(m.View())
	if !strings.Contains(v, "ARCHIVE") {
		t.Fatalf("the archive mode must be stated on the control row:\n%s", v)
	}
	if !strings.Contains(v, "archived items") {
		t.Fatalf("the archive label must say what its rows are:\n%s", v)
	}
	if strings.Contains(v, "TREE (active items)") {
		t.Fatalf("the label must follow the switch, not linger:\n%s", v)
	}
	// …and back.
	press(t, m, "v")
	load(t, m, srcWorkItems)
	if v := ansi.Strip(m.View()); !strings.Contains(v, "TREE (active items)") {
		t.Fatalf("switching back must restore the tree label:\n%s", v)
	}
}

// TestTheModeLabelNeverCrowdsOutTheControls: the caption is fitted, and the row
// NEVER draws a partially-clipped control — a clipped control leaves a hit-box the
// operator cannot read. The mode label is the thing that gives way.
func TestTheModeLabelNeverCrowdsOutTheControls(t *testing.T) {
	m := treePlane(t)
	for _, sz := range [][2]int{{200, 40}, {120, 40}, {100, 32}, {80, 24}} {
		m.SetSize(sz[0], sz[1])
		v := ansi.Strip(m.View())
		// A control is either drawn IN FULL or not at all — never clipped, because a
		// clipped control still holds a hit-box derived from where it was drawn.
		for _, a := range m.Base.ActiveTableActions() {
			label := "[ " + a.Label() + " ]"
			if i := strings.Index(v, label); i >= 0 {
				continue // drawn in full
			}
			// Absent is allowed (the row had no space), but a PREFIX of it must not
			// appear: that is the clipped case.
			prefix := label[:min(len(label), 8)]
			if strings.Contains(v, prefix) {
				t.Fatalf("at %dx%d the control %q was CLIPPED rather than dropped:\n%s",
					sz[0], sz[1], a.Label(), v)
			}
		}
		// The SORT control is the one the operator always needs (it is the pane's only
		// ordering control), so it must survive every width; the collapse control may be
		// dropped on a pane too narrow to draw it.
		if !strings.Contains(v, "sort:") {
			t.Fatalf("at %dx%d the sort control must always be drawn:\n%s", sz[0], sz[1], v)
		}
		// Nothing may be drawn PARTIALLY: a clipped control leaves a hit-box that
		// cannot be read, which is worse than an absent one.
		if strings.Contains(v, "[ sort:") && !strings.Contains(v, "[ sort: sequence ]") && !strings.Contains(v, "[ sort: title ]") && !strings.Contains(v, "[ sort: status ]") && !strings.Contains(v, "[ sort: priority ]") {
			t.Fatalf("at %dx%d a control was clipped mid-label:\n%s", sz[0], sz[1], v)
		}
	}
}
