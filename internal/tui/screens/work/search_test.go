package work

import (
	"strings"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

func searchPlane(t *testing.T) *Model {
	t.Helper()
	p := newPlane()
	p.seedProject("proj-1", "Orchicon")
	for _, w := range []struct{ id, title string }{
		{"wi-1", "Sweeper retry"},
		{"wi-2", "Telemetry overhaul"},
		{"wi-3", "Sweeper metrics"},
	} {
		p.addItem(&apiv1.WorkItem{
			Id: w.id, Title: w.title, Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK,
			Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING, ProjectId: "proj-1",
		})
	}
	m := newModel(t, p)
	m.SelectSource(srcWorkItems)
	load(t, m, srcWorkItems)
	return m
}

// The operator: "There is still no search box to filter through the list of work
// items." '/' focuses the search row at the top of the list, typing narrows the
// visible rows live, and esc clears it.
func TestWorkItemsSearchFiltersTheList(t *testing.T) {
	m := searchPlane(t)
	if got := len(itemsOf(m, srcWorkItems)); got != 3 {
		t.Fatalf("seed rows = %d, want 3", got)
	}

	// '/' focuses the search box; it is a text input, so it owns the keys.
	press(t, m, "/")
	if !m.Base.Filtering() {
		t.Fatal("'/' must focus the search box")
	}
	if !m.ClaimsKeys() {
		t.Fatal("the search box must claim the keys while typing")
	}

	// Typing narrows the list.
	for _, ch := range "sweeper" {
		press(t, m, string(ch))
	}
	tbl := m.Base.ActiveTable()
	if tbl == nil {
		t.Fatal("no active table")
	}
	vis := tbl.VisibleRows()
	if len(vis) != 2 {
		t.Fatalf("filtered rows = %d, want 2 (the two Sweeper items)", len(vis))
	}
	for _, r := range vis {
		if !strings.Contains(strings.ToLower(r.Cells[0]), "sweeper") {
			t.Fatalf("row %q does not match the filter", r.Cells[0])
		}
	}
	// The count is reported so a narrowing filter is never silent.
	if v := m.View(); !strings.Contains(v, "2/3") {
		t.Errorf("the search row must report matches (2/3):\n%s", v)
	}

	// Narrow further: "sweeper r" matches only "Sweeper retry".
	for _, ch := range " r" {
		press(t, m, string(ch))
	}
	if tbl.Filter != "sweeper r" {
		t.Fatalf("query = %q, want %q", tbl.Filter, "sweeper r")
	}
	if got := len(tbl.VisibleRows()); got != 1 {
		t.Fatalf("query %q rows = %d, want 1", tbl.Filter, got)
	}

	// backspace WIDENS it again: "sweeper " matches both.
	press(t, m, "backspace")
	if tbl.Filter != "sweeper " {
		t.Fatalf("query = %q, want %q", tbl.Filter, "sweeper ")
	}
	if got := len(tbl.VisibleRows()); got != 2 {
		t.Fatalf("after backspace (%q) rows = %d, want 2", tbl.Filter, got)
	}

	// esc clears the query and leaves the box: the full list is back.
	press(t, m, "esc")
	if m.Base.Filtering() {
		t.Fatal("esc must leave the search box")
	}
	if tbl.Filter != "" {
		t.Fatalf("esc must clear the query, got %q", tbl.Filter)
	}
	if got := len(tbl.VisibleRows()); got != 3 {
		t.Fatalf("cleared filter rows = %d, want 3", got)
	}
}

// A filter must not corrupt the tree: collapsing still hides descendants, and
// the cursor still steps over hidden rows.
func TestSearchAndTreeInteract(t *testing.T) {
	p := newPlane()
	p.seedProject("proj-1", "Orchicon")
	p.addItem(&apiv1.WorkItem{Id: "e", Title: "Epic alpha", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC, Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING, ProjectId: "proj-1"})
	p.addItem(&apiv1.WorkItem{Id: "k", Title: "kid alpha", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK, Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING, ProjectId: "proj-1", ParentId: "e"})
	p.addItem(&apiv1.WorkItem{Id: "z", Title: "other beta", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK, Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING, ProjectId: "proj-1"})
	m := newModel(t, p)
	m.SelectSource(srcWorkItems)
	load(t, m, srcWorkItems)

	tbl := m.Base.ActiveTable()
	// Filter to "alpha": the epic and its kid survive, the unrelated root drops.
	tbl.SetFilter("alpha")
	if got := len(tbl.VisibleRows()); got != 2 {
		t.Fatalf("filtered rows = %d, want 2", got)
	}
	// Collapsing the epic still hides its child, filter or not.
	if !m.SelectItem(srcWorkItems, "e") {
		t.Fatal("could not select the epic")
	}
	if !tbl.Toggle() {
		t.Fatal("the epic must be toggleable")
	}
	if got := len(tbl.VisibleRows()); got != 1 {
		t.Fatalf("collapsed+filtered rows = %d, want 1", got)
	}
}
