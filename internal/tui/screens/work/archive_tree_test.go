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
	// THE ARCHIVE OPENS EXPANDED, unlike the Tree — and that is deliberate. Its roots
	// are usually GHOST ANCHORS (active parents), so collapsing them would hide the
	// archived items behind a row that says "not archived": the operator would open the
	// archive and see nothing to restore. That is the bug this asserts against.
	v := ansi.Strip(m.View())
	if !strings.Contains(v, "[epic] ae") || !strings.Contains(v, "[task] at") {
		t.Fatalf("the archive frame must draw every level on open:\n%s", v)
	}
	if n := len(tbl.VisibleRows()); n != 3 {
		t.Fatalf("a fresh archive shows its whole hierarchy: %d visible, want 3", n)
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

// TestTreeOpensCollapsed — item 1: the work-items tree (BOTH views) opens with its
// parents collapsed, so a four-level hierarchy is readable on arrival.
//
// The operator: "I think the default for work items (both normal and archive) view
// should be 'collapsed all'." The TUI started fully expanded while the GUI's tree has
// always defaulted to collapsed (work-items-tree.tsx "default collapsed"), so the two
// clients showed the same data in different shapes on open.
//
// THE SCOPE MATTERS AS MUCH AS THE DEFAULT: kit2.Table has a SECOND tree — the category
// folder grouping on the workers/workflows panes — and a folder that opened collapsed
// would hide the rows the operator came for. That is why this is per-source
// (Table.CollapsedByDefault) rather than a change to the table's default.
func TestTreeOpensCollapsed(t *testing.T) {
	p := newPlane()
	p.seedProject("proj-1", "Orchicon")
	p.addItem(&apiv1.WorkItem{Id: "e", Title: "Epic", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC,
		ProjectId: "proj-1", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING})
	p.addItem(&apiv1.WorkItem{Id: "f", Title: "Feature", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_FEATURE,
		ProjectId: "proj-1", ParentId: "e", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING})
	p.addItem(&apiv1.WorkItem{Id: "t", Title: "Task", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK,
		ProjectId: "proj-1", ParentId: "f", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING})
	// An ARCHIVED pair too, so the archive view has a hierarchy of its own to open
	// collapsed (an empty archive would pass the count for the wrong reason).
	p.addArchive(archivedRow("ae", "", "succeeded", apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC))
	p.addArchive(archivedRow("af", "ae", "succeeded", apiv1.WorkItemKind_WORK_ITEM_KIND_FEATURE))

	m := newModel(t, p)
	m.SelectSource(srcWorkItems)
	load(t, m, srcWorkItems)

	tbl := m.Base.ActiveTable()
	if !tbl.CollapsedByDefault {
		t.Fatal("the work-items table must be marked collapsed-by-default")
	}
	if n := len(tbl.VisibleRows()); n != 1 {
		t.Fatalf("the TREE view must open with its root only: %d visible rows, want 1", n)
	}
	// …and the archive view, which is a tree too, gets the SAME default rather than a
	// second rule. It has its own archived hierarchy (ae → af) to prove it.
	press(t, m, "v")
	load(t, m, srcWorkItems)
	if am := m.ViewMode(); am != viewArchive {
		t.Fatalf("fixture: expected the archive view, got %q", am)
	}
	// THE ARCHIVE IS THE EXCEPTION, and it must be: it opens EXPANDED so its archived
	// items are on screen. See TestArchiveViewIsATree.
	atbl := m.Base.ActiveTable()
	if n := len(atbl.VisibleRows()); n != 2 {
		t.Fatalf("the ARCHIVE view must open EXPANDED: %d visible rows, want 2 (ae + af)", n)
	}
	rows := archiveRowsByID(m)
	if rows["af"].Depth != 1 || rows["af"].Parent != "ae" {
		t.Fatalf("the archived child must stay nested under its parent: %+v", rows["af"])
	}

	// And the operator's own choice still SURVIVES a reload — the rolling refresh
	// re-reads every few seconds, and a collapse that undid itself on the next tick
	// would be unusable.
	m.toggleAllTreeNodes() // collapse
	load(t, m, srcWorkItems)
	if n := len(m.Base.ActiveTable().VisibleRows()); n != 1 {
		t.Fatalf("an explicit collapse must STAY collapsed across a reload: %d visible, want 1", n)
	}
}

// The category-folder tree is NOT collapsed by this: its members are the point of the
// group. Asserted on the table default rather than on a live pane, because it is the
// DEFAULT that this change touched (and briefly broke).
func TestFolderGroupingStillOpensExpanded(t *testing.T) {
	var tbl kit2.Table
	tbl.SetItems([]kit2.Item{
		{ID: "group:cat-1", Title: "Engineering", HasChildren: true},
		{ID: "w1", Title: "writer", Depth: 1, Parent: "group:cat-1"},
	}, "")
	if !tbl.AllExpanded() {
		t.Fatal("a category folder must still open EXPANDED — its members are the point of the group")
	}
	if n := len(tbl.VisibleRows()); n != 2 {
		t.Fatalf("folder + member = %d visible rows, want 2", n)
	}
}

// TestArchiveRestoreIsReachableAndNamed — the operator: "It doesn't look like there is a
// way to restore a work item from the archive in the TUI but you can in the GUI."
//
// The chord (`R`) existed and worked; it was UNDISCOVERABLE, and reaching it was blocked,
// for three separate reasons that together made the feature invisible:
//
//  1. The archive view opened COLLAPSED, and its root is usually a GHOST ANCHOR — an
//     active parent rendered only to keep the hierarchy connected. So the first screen of
//     the archive showed a single row reading "active ancestor — not archived", with the
//     archived items folded underneath and nothing to say they were there.
//  2. The ghost anchor is not restorable (correctly — the plane would refuse), so the
//     focused row offered NO actions at all. The operator's first impression was a view
//     where nothing could be restored.
//  3. The composer's hint line advertised `a: archive`, which is a NO-OP in this view
//     (itemActions offers restore INSTEAD of archive), and never mentioned `R`. The one
//     chord that works was the one chord not written down.
//
// This asserts the whole path: the archived rows are on screen on arrival, the row offers
// the action, the hint names it, and confirming it calls RestoreWorkItem.
func TestArchiveRestoreIsReachableAndNamed(t *testing.T) {
	p := newPlane()
	p.seedProject("proj-1", "Orchicon")
	// The shape that broke it: an ACTIVE parent with an ARCHIVED child, so the archive's
	// root is a ghost anchor.
	p.addItem(&apiv1.WorkItem{Id: "live", Title: "Live Epic", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC,
		ProjectId: "proj-1", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING})
	p.addArchive(archivedRow("af", "live", "succeeded", apiv1.WorkItemKind_WORK_ITEM_KIND_FEATURE))

	m := newModel(t, p)
	m.SelectSource(srcWorkItems)
	load(t, m, srcWorkItems)
	press(t, m, "v")
	load(t, m, srcWorkItems)

	// 1. THE ARCHIVED ITEM IS ON SCREEN, on arrival, without expanding anything.
	if v := ansi.Strip(m.View()); !strings.Contains(v, "[feature] af") {
		t.Fatalf("the archived item must be visible when the archive view opens — otherwise "+
			"there is nothing the operator can see to restore:\n%s", v)
	}

	// 2. THE HINT NAMES THE CHORD (and does not advertise the no-op `a`).
	hint := ansi.Strip(m.HintLine())
	if !strings.Contains(hint, "R: restore") {
		t.Fatalf("the composer must name the restore chord, got %q", hint)
	}
	if strings.Contains(hint, "a: archive") {
		t.Fatalf("the archive view must not advertise `a: archive`, which does nothing here: %q", hint)
	}

	// 3. THE ROW OFFERS IT, and confirming calls the RPC.
	if !m.SelectItem(srcWorkItems, "af") {
		t.Fatal("could not focus the archived row")
	}
	if _, ok := m.actionByKey("R"); !ok {
		t.Fatalf("the archived row must offer `R`, got %v", archiveActionLabels(m))
	}
	press(t, m, "R")
	if !m.DialogOpen() {
		t.Fatal("`R` must ask for confirmation before restoring")
	}
	run(t, m, press(t, m, "enter"))
	if len(p.restored) != 1 || p.restored[0] != "af" {
		t.Fatalf("RestoreWorkItem calls = %v, want [af]", p.restored)
	}
}

// The TREE view keeps the collapsed default — the archive's exception must not have
// quietly removed it, which is the mistake this change corrected one layer down.
func TestTheTreeStillOpensCollapsedAfterTheArchiveException(t *testing.T) {
	p := newPlane()
	p.seedProject("proj-1", "Orchicon")
	p.addItem(&apiv1.WorkItem{Id: "e", Title: "Epic", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC,
		ProjectId: "proj-1", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING})
	p.addItem(&apiv1.WorkItem{Id: "t", Title: "Task", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK,
		ProjectId: "proj-1", ParentId: "e", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING})
	p.addArchive(archivedRow("af", "", "succeeded", apiv1.WorkItemKind_WORK_ITEM_KIND_FEATURE))

	m := newModel(t, p)
	m.SelectSource(srcWorkItems)
	load(t, m, srcWorkItems)
	if n := len(m.Base.ActiveTable().VisibleRows()); n != 1 {
		t.Fatalf("the Tree must still open collapsed: %d visible, want 1", n)
	}

	// …and the default follows the view in BOTH directions, not just into the archive.
	press(t, m, "v") // -> archive (expanded)
	load(t, m, srcWorkItems)
	if n := len(m.Base.ActiveTable().VisibleRows()); n != 1 {
		t.Fatalf("the archive's own root is archived, so it is one visible row: %d, want 1", n)
	}
	press(t, m, "v") // -> back to tree (collapsed)
	load(t, m, srcWorkItems)
	if n := len(m.Base.ActiveTable().VisibleRows()); n != 1 {
		t.Fatalf("returning to the Tree must restore its collapsed default: %d visible, want 1", n)
	}
	// And expanding there still works.
	expandAll(t, m)
	if n := len(m.Base.ActiveTable().VisibleRows()); n != 2 {
		t.Fatalf("expanding the tree must reveal the child: %d visible, want 2", n)
	}
}

// TestBulkRestoreFromTheArchive — the operator: "I noticed there is no option to restore
// on bulk items."
//
// RIGHT, and the reason was structural: bulkItemActions offered ARCHIVE and DELETE and
// knew nothing about the view. In the archive view both are wrong — `a: archive` is a
// no-op on an item that is already archived, and `x: delete` CANCELS it, which is a
// different outcome from the restore the operator is in that view to perform. So a marked
// selection had no restore path at all, while the GUI has had "Restore selected" from the
// start.
func TestBulkRestoreFromTheArchive(t *testing.T) {
	p := newPlane()
	p.seedProject("proj-1", "Orchicon")
	p.addItem(&apiv1.WorkItem{Id: "live", Title: "Live Epic", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC,
		ProjectId: "proj-1", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING})
	// A two-level archived subtree, so the ordering rule is exercised rather than assumed.
	p.addArchive(archivedRow("af", "live", "succeeded", apiv1.WorkItemKind_WORK_ITEM_KIND_FEATURE))
	p.addArchive(archivedRow("at", "af", "succeeded", apiv1.WorkItemKind_WORK_ITEM_KIND_TASK))

	m := newModel(t, p)
	m.SelectSource(srcWorkItems)
	load(t, m, srcWorkItems)
	press(t, m, "v")
	load(t, m, srcWorkItems)

	// Mark the two ARCHIVED rows (the anchor is not selectable for this).
	m.SelectItem(srcWorkItems, "af")
	press(t, m, " ")
	press(t, m, "down")
	press(t, m, " ")
	if n := m.Base.MarkCount(); n != 2 {
		t.Fatalf("fixture: expected 2 marked rows, got %d", n)
	}

	// THE OFFER: restore, on `R`, and NOT the tree's no-op chords.
	var labels []string
	for _, a := range m.actionsForSelection() {
		labels = append(labels, a.Key)
	}
	if _, ok := m.actionByKey("R"); !ok {
		t.Fatalf("a marked archive selection must offer restore, got keys %v", labels)
	}
	if _, ok := m.actionByKey("a"); ok {
		t.Fatalf("the archive view must not offer `a: archive` for a selection — it is a no-op "+
			"on already-archived items, got keys %v", labels)
	}
	if _, ok := m.actionByKey(keyBulkSet); ok {
		t.Fatalf("the archive view must not offer `%s` for a selection, got keys %v", keyBulkSet, labels)
	}

	// THE HINT names it (and not the tree's chords).
	hint := ansi.Strip(m.HintLine())
	if !strings.Contains(hint, "R: restore") {
		t.Fatalf("the marked hint must name restore, got %q", hint)
	}
	if strings.Contains(hint, "set workflow & image") {
		t.Fatalf("the marked hint must not advertise a chord this view does not offer: %q", hint)
	}

	// THE WRITE, and its ORDER: children before parents.
	press(t, m, "R")
	if !m.DialogOpen() {
		t.Fatal("`R` on a marked selection must ask for confirmation first")
	}
	run(t, m, press(t, m, "enter"))
	if len(p.restored) != 2 {
		t.Fatalf("RestoreWorkItem calls = %v, want both marked ids", p.restored)
	}
	if p.restored[0] != "at" || p.restored[1] != "af" {
		t.Fatalf("restore order = %v, want [at af] — DEEPEST FIRST. A parent restored before "+
			"its still-archived children leaves the hierarchy briefly inconsistent, which is "+
			"the same rule the GUI's bottomUpOrder applies.", p.restored)
	}
}

// The TREE keeps its own bulk vocabulary — the archive's set must not have replaced it.
func TestTheTreeKeepsItsBulkActions(t *testing.T) {
	p := newPlane()
	p.seedProject("proj-1", "Orchicon")
	p.addItem(&apiv1.WorkItem{Id: "e", Title: "Epic", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC,
		ProjectId: "proj-1", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING})
	p.addItem(&apiv1.WorkItem{Id: "t", Title: "Task", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK,
		ProjectId: "proj-1", ParentId: "e", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING})
	p.addItem(&apiv1.WorkItem{Id: "u", Title: "Other", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK,
		ProjectId: "proj-1", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING})

	m := newModel(t, p)
	m.SelectSource(srcWorkItems)
	load(t, m, srcWorkItems)

	if !m.SelectItem(srcWorkItems, "t") {
		t.Fatal("could not focus the task")
	}
	press(t, m, " ")
	if !m.SelectItem(srcWorkItems, "u") {
		t.Fatal("could not focus the other task")
	}
	press(t, m, " ")

	if _, ok := m.actionByKey("a"); !ok {
		t.Fatal("the TREE must still offer bulk archive")
	}
	if _, ok := m.actionByKey("R"); ok {
		t.Fatal("the TREE must not offer bulk restore — that is the archive view's operation")
	}
}
