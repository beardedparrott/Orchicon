package work

import (
	"strings"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// The Work Items Tree is a REAL tree now: treeRows carries depth / parent /
// has-children so the pane can indent a row, draw a +/- toggle on parents, and
// collapse a subtree. Before this the indent was baked into the title string
// and the table's tree machinery was unreachable.
func TestTreeRowsCarryTreeMetadata(t *testing.T) {
	epic := &apiv1.WorkItem{Id: "e", Title: "Epic", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC}
	feat := &apiv1.WorkItem{Id: "f", Title: "Feature", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_FEATURE, ParentId: "e"}
	task := &apiv1.WorkItem{Id: "t", Title: "Task", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK, ParentId: "f"}
	solo := &apiv1.WorkItem{Id: "s", Title: "Solo", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK}

	rows := treeRows([]*apiv1.WorkItem{epic, feat, task, solo}, sortSequence)
	type meta struct {
		depth  int
		parent string
		kids   bool
		title  string
	}
	got := map[string]meta{}
	for _, r := range rows {
		got[r.ID] = meta{r.Depth, r.Parent, r.HasChildren, r.Title}
	}
	if len(got) != 4 {
		t.Fatalf("treeRows returned %d rows, want 4", len(got))
	}

	// The epic is a root with children.
	if m := got["e"]; m.depth != 0 || m.parent != "" || !m.kids {
		t.Fatalf("epic metadata = %+v", m)
	}
	// The feature is a nested parent: depth 1 under the epic, with children.
	if m := got["f"]; m.depth != 1 || m.parent != "e" || !m.kids {
		t.Fatalf("feature metadata = %+v", m)
	}
	// The task is a leaf at depth 2.
	if m := got["t"]; m.depth != 2 || m.parent != "f" || m.kids {
		t.Fatalf("task metadata = %+v", m)
	}
	// An unparented item is a root leaf.
	if m := got["s"]; m.depth != 0 || m.parent != "" || m.kids {
		t.Fatalf("solo metadata = %+v", m)
	}

	// The indent is the PANE's job now (from Depth): the title must not
	// pre-pad itself or the tree would be indented twice.
	for _, r := range rows {
		if strings.HasPrefix(r.Title, " ") {
			t.Fatalf("tree title %q must not pre-indent (the pane draws the depth)", r.Title)
		}
	}
}

// The overall collapse/expand toggle on the Work screen ('O'): it flips every
// tree node at once, and it reaches the active source's table.
func TestCollapseExpandAllOnWorkItems(t *testing.T) {
	p := newPlane()
	p.seedProject("proj-1", "Orchicon")
	p.addItem(&apiv1.WorkItem{Id: "e", Title: "Epic", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC, Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING, ProjectId: "proj-1"})
	p.addItem(&apiv1.WorkItem{Id: "f", Title: "Feature", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_FEATURE, Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING, ProjectId: "proj-1", ParentId: "e"})
	p.addItem(&apiv1.WorkItem{Id: "t", Title: "Task", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK, Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING, ProjectId: "proj-1", ParentId: "f"})

	m := newModel(t, p)
	m.SelectSource(srcWorkItems)
	load(t, m, srcWorkItems)

	tbl := m.Base.ActiveTable()
	if tbl == nil {
		t.Fatal("the work-items source must expose a table")
	}
	if n := tbl.ExpandableCount(); n != 2 {
		t.Fatalf("expandable nodes = %d, want 2 (epic + feature)", n)
	}
	if n := len(tbl.VisibleRows()); n != 3 {
		t.Fatalf("fresh tree visible = %d, want 3", n)
	}

	// O collapses everything: only the epic root survives.
	press(t, m, "O")
	if tbl.AllExpanded() {
		t.Fatal("O must collapse every node")
	}
	if n := len(tbl.VisibleRows()); n != 1 {
		t.Fatalf("collapsed visible rows = %d, want 1 (the epic)", n)
	}

	// O again expands everything back.
	press(t, m, "O")
	if !tbl.AllExpanded() {
		t.Fatal("a second O must expand every node again")
	}
	if n := len(tbl.VisibleRows()); n != 3 {
		t.Fatalf("expanded visible rows = %d, want 3", n)
	}
}
