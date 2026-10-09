package work

import (
	"slices"
	"strings"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
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

// collapsedTreePlane is the fixture that exposed the report: an EPIC whose child
// matches, an epic that does not, and an unrelated root. The Tree view opens
// COLLAPSED, so this is exactly what the operator was looking at.
func collapsedTreePlane(t *testing.T) *Model {
	t.Helper()
	p := newPlane()
	p.seedProject("proj-1", "Orchicon")
	p.addItem(&apiv1.WorkItem{Id: "e", Title: "Epic alpha", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC, Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING, ProjectId: "proj-1"})
	p.addItem(&apiv1.WorkItem{Id: "k", Title: "kid alpha", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK, Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING, ProjectId: "proj-1", ParentId: "e"})
	p.addItem(&apiv1.WorkItem{Id: "b", Title: "Epic beta", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC, Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING, ProjectId: "proj-1"})
	p.addItem(&apiv1.WorkItem{Id: "g", Title: "grandkid gamma", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_SUBTASK, Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING, ProjectId: "proj-1", ParentId: "b"})
	m := newModel(t, p)
	m.SelectSource(srcWorkItems)
	load(t, m, srcWorkItems)
	return m
}

// idsOf is the visible row ids in draw order.
func idsOf(tbl *kit2.Table) []string {
	out := make([]string, 0)
	for _, r := range tbl.VisibleRows() {
		out = append(out, r.ID)
	}
	return out
}

// THE REPORT: "Searching work items in the TUI does NOT search items that are
// collapsed. That is a broken design. The search should most definitely search
// through collapsed items as well."
//
// The Tree view opens collapsed, so a collapsed epic's children were unreachable
// by search: no query could reveal them, and there was no key that could select
// what the list refused to draw.
func TestFilterReachesMatchesInsideCollapsedNodes(t *testing.T) {
	m := collapsedTreePlane(t)
	tbl := m.Base.ActiveTable()

	// Before: collapsed, so each epic's subtree is hidden.
	if got := idsOf(tbl); strings.Join(got, ",") != "e,b" {
		t.Fatalf("collapsed tree shows %v, want [e b]", got)
	}

	// Query a title that exists ONLY inside the collapsed epics.
	tbl.SetFilter("gamma")

	// The match two levels down is on screen despite BOTH its ancestors being
	// collapsed, and so is the chain that places it (the ancestor rule: a filtered
	// result keeps its hierarchy rather than surfacing as an indented orphan).
	got := idsOf(tbl)
	if strings.Join(got, ",") != "b,g" {
		t.Fatalf("filtered collapsed tree shows %v, want [b g] (the match and its collapsed ancestor)", got)
	}

	// Clearing the query restores the collapse — the query SUSPENDED it, it did not
	// rewrite the operator's tree.
	tbl.SetFilter("")
	if got := idsOf(tbl); strings.Join(got, ",") != "e,b" {
		t.Fatalf("after clearing the query the tree shows %v, want [e b] back", got)
	}
}

// The keyboard must be able to REACH what the query reveals — a match the list
// draws but the cursor steps over is not searchable either.
func TestFilterMakesCollapsedMatchesReachableByKeyboard(t *testing.T) {
	m := collapsedTreePlane(t)

	// Type the query as the operator does ('/'), then leave the box with enter
	// (StopFilter KEEPS the query), which is what hands the arrows back to the list.
	press(t, m, "/")
	for _, ch := range "alpha" {
		press(t, m, string(ch))
	}
	press(t, m, "enter")
	if m.Base.Filtering() {
		t.Fatal("enter must leave the search box")
	}

	tbl := m.Base.ActiveTable()
	if got := strings.Join(idsOf(tbl), ","); got != "e,k" {
		t.Fatalf("visible after searching alpha = %q, want \"e,k\"", got)
	}
	// The arrow keys step onto the collapsed epic's child, which is the row the
	// operator could not select before this fix.
	tbl.ResetCursor()
	tbl.Move(1)
	if got := tbl.SelectedID(); got != "e" {
		t.Fatalf("first step = %q, want the epic", got)
	}
	tbl.Move(1)
	if got := tbl.SelectedID(); got != "k" {
		t.Fatalf("the cursor cannot reach the match inside the collapsed epic: landed on %q, want \"k\"", got)
	}
}

// The count the search box prints must agree with what the list draws. Before
// this fix MatchCount counted matches the renderer refused to show ("3/40" with
// two rows on screen), which is how the defect was visible without being
// explicable.
func TestFilterCountAgreesWithTheList(t *testing.T) {
	m := collapsedTreePlane(t)
	tbl := m.Base.ActiveTable()
	tbl.SetFilter("alpha")

	matches, total := tbl.MatchCount()
	if matches != 2 {
		t.Fatalf("match count = %d, want 2 (Epic alpha, kid alpha)", matches)
	}
	if total != 4 {
		t.Fatalf("total = %d, want 4 rows", total)
	}
	// Every match is ON SCREEN: the count is a promise about the list.
	vis := idsOf(tbl)
	for _, id := range []string{"e", "k"} {
		if !slices.Contains(vis, id) {
			t.Fatalf("the box says %d matches but %q is not drawn (visible: %v)", matches, id, vis)
		}
	}
}

// NON-REGRESSION: with NO query the collapse is untouched. Collapsing must still
// hide a subtree — the fix suspends collapse for a QUERY, not permanently.
func TestCollapseStillHidesDescendantsWithoutAQuery(t *testing.T) {
	m := collapsedTreePlane(t)
	tbl := m.Base.ActiveTable()
	expandAll(t, m)
	if got := len(tbl.VisibleRows()); got != 4 {
		t.Fatalf("expanded rows = %d, want 4", got)
	}
	if !m.SelectItem(srcWorkItems, "b") {
		t.Fatal("could not select the epic")
	}
	if !tbl.Toggle() {
		t.Fatal("the epic must be toggleable")
	}
	if got := strings.Join(idsOf(tbl), ","); got != "e,k,b" {
		t.Fatalf("collapsing beta shows %q, want \"e,k,b\" (gamma hidden)", got)
	}
}

// A whitespace-only query is NOT a filter: it narrows nothing (matchesFilter
// trims), so it must not suspend the collapse either.
func TestWhitespaceQueryDoesNotSuspendCollapse(t *testing.T) {
	m := collapsedTreePlane(t)
	tbl := m.Base.ActiveTable()
	tbl.SetFilter("   ")
	if got := strings.Join(idsOf(tbl), ","); got != "e,b" {
		t.Fatalf("a blank query shows %q, want the collapsed tree [e b] untouched", got)
	}
}

// The sort control orders SIBLINGS for display without touching the stored
// sequence (the operator's "maybe we should have some actual sort controls at
// the top near the search box?").
func TestSortModesOrderSiblings(t *testing.T) {
	mk := func(id, title, status string, prio int32) *apiv1.WorkItem {
		return &apiv1.WorkItem{
			Id: id, Title: title, Priority: prio,
			Kind:   apiv1.WorkItemKind_WORK_ITEM_KIND_TASK,
			Status: statusFromName(status),
		}
	}
	items := func() []*apiv1.WorkItem {
		return []*apiv1.WorkItem{
			mk("c", "charlie", "running", 1),
			mk("a", "alpha", "pending", 9),
			mk("b", "bravo", "blocked", 5),
		}
	}

	// by title
	titles := func(mode sortMode) []string {
		it := items()
		sortSiblings(it, mode)
		out := make([]string, 0, len(it))
		for _, w := range it {
			out = append(out, w.GetTitle())
		}
		return out
	}

	if got := strings.Join(titles(sortTitle), ","); got != "alpha,bravo,charlie" {
		t.Fatalf("sort by title = %s", got)
	}
	if got := strings.Join(titles(sortPriority), ","); got != "alpha,bravo,charlie" {
		t.Fatalf("sort by priority (highest first: 9,5,1) = %s", got)
	}
	// Status sorts by the rendered pill, then title. The expectation is derived
	// from the pills themselves so it cannot drift from the vocabulary.
	{
		it := items()
		sortSiblings(it, sortStatus)
		var got []string
		for _, w := range it {
			got = append(got, statusPill(w.GetStatus())+":"+w.GetTitle())
		}
		for i := 1; i < len(it); i++ {
			pa, pb := statusPill(it[i-1].GetStatus()), statusPill(it[i].GetStatus())
			if pa > pb || (pa == pb && it[i-1].GetTitle() > it[i].GetTitle()) {
				t.Fatalf("status sort is not ordered by pill then title: %v", got)
			}
		}
	}
}

// The control cycles through the modes and comes back to the stored sequence.
func TestSortControlCycles(t *testing.T) {
	m := treePlane(t)
	if got := m.SortMode(); got != sortSequence {
		t.Fatalf("default sort = %q, want sequence", got)
	}
	seen := map[sortMode]bool{m.SortMode(): true}
	for i := 0; i < 4; i++ {
		press(t, m, "O") // unrelated control must not change the sort
		m.cycleSort()
		seen[m.SortMode()] = true
	}
	if len(seen) != 4 {
		t.Fatalf("the control must reach all four modes, saw %d", len(seen))
	}
	// After four advances it is back to the sequence.
	if got := m.SortMode(); got != sortSequence {
		t.Fatalf("four advances must return to sequence, got %q", got)
	}
	// The control is rendered on the pane's top row with its current mode.
	if v := m.View(); !strings.Contains(v, "sort: ") {
		t.Fatalf("the sort control must render:\n%s", v)
	}
}
