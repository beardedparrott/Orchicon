package kit2

import (
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
)

// treeFixture is a 3-level tree plus a root leaf: epic → feature → task, and a
// second root. Two expandable nodes (the epic and the feature).
func treeFixture() []screenkit.Item {
	return []screenkit.Item{
		{ID: "e", Title: "[epic] E", HasChildren: true},
		{ID: "f", Title: "[feature] F", Depth: 1, Parent: "e", HasChildren: true},
		{ID: "t", Title: "[task] T", Depth: 2, Parent: "f"},
		{ID: "s", Title: "[task] Solo", Depth: 0},
	}
}

// The tree pane's contract: an item's tree metadata reaches the row, a parent
// draws a +/- toggle, a fresh tree is fully expanded, collapsing hides the
// whole subtree, and the cursor steps over hidden rows rather than landing on
// them.
func TestTableTreeMetadataAndCollapse(t *testing.T) {
	tbl := NewTable("Work Items", Column{Title: ""})
	tbl.Width, tbl.Height = 80, 20
	tbl.SetItems(treeFixture(), "")

	// Metadata reached the rows.
	if r := tbl.Rows[0]; !r.Expand || r.Depth != 0 || r.Parent != "" {
		t.Fatalf("root row = %+v", r)
	}
	if r := tbl.Rows[1]; r.Depth != 1 || r.Parent != "e" || !r.Expand {
		t.Fatalf("nested parent row = %+v", r)
	}
	if r := tbl.Rows[2]; r.Depth != 2 || r.Parent != "f" || r.Expand {
		t.Fatalf("leaf row = %+v", r)
	}

	// A fresh tree is fully expanded and draws '-' on parents.
	if n := len(tbl.VisibleRows()); n != 4 {
		t.Fatalf("fresh tree visible = %d, want 4", n)
	}
	v := tbl.View()
	if !strings.Contains(v, "- [epic] E") {
		t.Fatalf("an expanded parent must draw a '-' toggle:\n%s", v)
	}
	if !strings.Contains(v, "[task] T") {
		t.Fatalf("the open tree must render the nested task:\n%s", v)
	}

	// Collapse the root: the whole subtree hides and the toggle flips to '+'.
	tbl.setCursorToID("e")
	if !tbl.Toggle() {
		t.Fatal("toggle on a parent must be consumed")
	}
	if n := len(tbl.VisibleRows()); n != 2 {
		t.Fatalf("collapsed root visible = %d, want 2 (epic + solo)", n)
	}
	v = tbl.View()
	if !strings.Contains(v, "+ [epic] E") {
		t.Fatalf("a collapsed parent must draw a '+' toggle:\n%s", v)
	}
	if strings.Contains(v, "[feature] F") || strings.Contains(v, "[task] T") {
		t.Fatalf("a collapsed subtree must not render its descendants:\n%s", v)
	}

	// A leaf has no toggle.
	tbl.setCursorToID("s")
	if tbl.Toggle() {
		t.Fatal("toggle on a leaf must be a no-op")
	}

	// Cursor movement steps over hidden rows: from the epic, one step lands on
	// the OTHER ROOT (the feature is hidden), never inside the collapsed node.
	tbl.setCursorToID("e")
	tbl.Move(1)
	if got := tbl.SelectedID(); got != "s" {
		t.Fatalf("Move must step over the collapsed subtree: got %q, want %q", got, "s")
	}
}

// ExpandAll / AllExpanded are the operator's overall collapse/expand toggle.
func TestTableExpandAll(t *testing.T) {
	tbl := NewTable("t", Column{Title: ""})
	tbl.Width, tbl.Height = 80, 20
	tbl.SetItems(treeFixture(), "")

	if !tbl.AllExpanded() {
		t.Fatal("a fresh tree must be fully expanded")
	}
	if n := tbl.ExpandableCount(); n != 2 {
		t.Fatalf("expandable nodes = %d, want 2", n)
	}
	// Collapsing all changes only the two parents.
	if n := tbl.ExpandAll(false); n != 2 {
		t.Fatalf("ExpandAll(false) changed %d rows, want 2", n)
	}
	if tbl.AllExpanded() {
		t.Fatal("AllExpanded must be false after collapsing all")
	}
	// Both roots remain; every descendant is hidden.
	if n := len(tbl.VisibleRows()); n != 2 {
		t.Fatalf("collapse-all visible = %d, want 2 roots", n)
	}
	// Expand-all restores the whole tree.
	if n := tbl.ExpandAll(true); n != 2 {
		t.Fatalf("ExpandAll(true) changed %d rows, want 2", n)
	}
	if n := len(tbl.VisibleRows()); n != 4 {
		t.Fatalf("expand-all visible = %d, want 4", n)
	}
	// A second collapse-all moves the same two nodes.
	if n := tbl.ExpandAll(false); n != 2 {
		t.Fatalf("second ExpandAll(false) changed %d rows, want 2", n)
	}
}

// A flat list reports itself fully expanded with nothing to toggle, so the
// expand/collapse-all gesture can no-op cleanly.
func TestTableExpandAllOnFlatList(t *testing.T) {
	tbl := NewTable("t", Column{Title: ""})
	tbl.SetItems([]screenkit.Item{{ID: "a", Title: "a"}, {ID: "b", Title: "b"}}, "")
	if !tbl.AllExpanded() {
		t.Fatal("a flat list must report fully expanded")
	}
	if n := tbl.ExpandableCount(); n != 0 {
		t.Fatalf("flat list expandable = %d, want 0", n)
	}
	if n := tbl.ExpandAll(false); n != 0 {
		t.Fatalf("ExpandAll on a flat list changed %d rows, want 0", n)
	}
}

// Click maps through the VISIBLE set: with a node collapsed, a click on a line
// that would have been a hidden descendant must miss rather than select it.
func TestTableClickRespectsCollapsedRows(t *testing.T) {
	tbl := NewTable("t", Column{Title: ""})
	tbl.Width, tbl.Height = 80, 20
	tbl.SetItems([]screenkit.Item{
		{ID: "p", Title: "parent", HasChildren: true},
		{ID: "c", Title: "child", Depth: 1, Parent: "p"},
	}, "")

	tbl.ExpandAll(false)
	if tbl.Click(1) {
		t.Fatal("clicking past the last VISIBLE row must miss")
	}
	if got := tbl.SelectedID(); got != "p" {
		t.Fatalf("cursor = %q, want p", got)
	}

	tbl.ExpandAll(true)
	if !tbl.Click(1) {
		t.Fatal("click on the now-visible child line must land")
	}
	if got := tbl.SelectedID(); got != "c" {
		t.Fatalf("cursor = %q, want c", got)
	}
}

// Marker hit-testing: a click on a parent's +/- toggles it, a click elsewhere
// on the row selects (ToggleAt returns false so the caller falls through), and
// a leaf's gutter is inert.
func TestTableToggleAtMarker(t *testing.T) {
	tbl := NewTable("t", Column{Title: ""})
	tbl.Width, tbl.Height = 80, 20
	tbl.SetItems(treeFixture(), "")

	// The epic is a depth-0 parent: its marker sits at columns 1..2.
	if !tbl.ToggleAt(0, 1) {
		t.Fatal("a click on the root parent's marker must toggle it")
	}
	if tbl.Rows[0].Open {
		t.Fatal("the root node must now be collapsed")
	}
	// After collapsing the epic only the epic and the solo root remain visible;
	// a click below them is out of range.
	if tbl.ToggleAt(5, 1) {
		t.Fatal("a click past the last visible row must not toggle anything")
	}
	// A click to the RIGHT of the marker selects rather than toggles: ToggleAt
	// reports false so the caller falls through to Click.
	tbl.ExpandAll(true)
	if tbl.ToggleAt(0, 30) {
		t.Fatal("a click away from the marker must not toggle")
	}
	// The gutter of a LEAF is inert.
	for i, r := range tbl.VisibleRows() {
		if r.ID == "s" && tbl.ToggleAt(i, 1) {
			t.Fatal("a leaf has no marker to toggle")
		}
	}
}
