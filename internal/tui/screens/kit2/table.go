package kit2

import (
	"fmt"
	"sort"
	"strings"

	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// Column is one table column.
type Column struct {
	Title string
	Width int // 0 = flex
	// Right aligns the cell text.
	Right bool
}

// Row is one selectable table row (optionally a tree node).
type Row struct {
	ID     string
	Cells  []string
	Meta   string
	Depth  int
	Parent string
	Expand bool // has children
	Open   bool // children visible
	// Actions are per-row inline actions (rendered as key hints).
	Actions []Action
}

// Table is a selectable, sortable, tree-capable table. It is the list pane
// replacement: it owns the cursor, the scroll window, click hit-testing,
// sorting, and tree expansion.
type Table struct {
	Columns []Column
	Title   string
	Rows    []Row
	Cursor  int
	Offset  int
	Width   int
	Height  int
	Focused bool
	Err     string
	Empty   string
	// NextPageToken / Loading mirror the old list pane's pagination state.
	NextPageToken string
	Loading       bool
	// SortCol is the sorted column index (-1 = insertion order).
	SortCol  int
	SortDesc bool

	// HideTitle suppresses the table's own title row. Panels already embed the
	// source title in their border, so an embedded table must not repeat it —
	// the duplicate row also shifted every click hit-test by one.
	HideTitle bool
}

// NewTable builds an empty table with columns.
func NewTable(title string, cols ...Column) *Table {
	return &Table{Title: title, Columns: cols, SortCol: -1}
}

// SetItems seeds the table from list items (single-column display). Tree
// metadata on an item (Depth/Parent/HasChildren) is carried onto the row, so
// a fresh tree renders FULLY EXPANDED and collapsing is always an explicit
// operator action.
func (t *Table) SetItems(items []screenkit.Item, next string) {
	rows := make([]Row, 0, len(items))
	for _, it := range items {
		rows = append(rows, Row{
			ID:     it.ID,
			Cells:  []string{it.Title},
			Meta:   it.Meta,
			Depth:  it.Depth,
			Parent: it.Parent,
			Expand: it.HasChildren,
			Open:   true,
		})
	}
	t.Rows = rows
	t.NextPageToken = next
	t.Cursor, t.Offset = 0, 0
}

// SetRows replaces the rows.
func (t *Table) SetRows(rows []Row) {
	t.Rows = rows
	t.Cursor, t.Offset = 0, 0
}

// AppendRows adds a page of rows without resetting the cursor.
func (t *Table) AppendRows(rows []Row, next string) {
	t.Rows = append(t.Rows, rows...)
	t.NextPageToken = next
}

// Selected returns the row under the cursor (nil when empty).
func (t *Table) Selected() *Row {
	if t.Cursor >= 0 && t.Cursor < len(t.Rows) {
		return &t.Rows[t.Cursor]
	}
	return nil
}

// SelectedID returns the cursor row's id ("" when empty).
func (t *Table) SelectedID() string {
	if r := t.Selected(); r != nil {
		return r.ID
	}
	return ""
}

// visibleRows is the body row budget (title + cursor line overhead).
func (t *Table) visibleRows() int {
	h := t.Height - 3
	if h < 1 {
		h = 1
	}
	return h
}

// Move shifts the cursor by delta over the VISIBLE rows, keeping the window in
// view. Stepping in visible space is what makes a tree usable: a movement can
// never land on a row hidden inside a collapsed node. The cursor indexes Rows
// while visibility is decided by the tree's open/collapsed state, so the two
// are mapped through VisibleRows rather than assumed to agree.
func (t *Table) Move(delta int) {
	vis := t.VisibleRows()
	if len(vis) == 0 {
		return
	}
	pos := t.cursorVis()
	if pos < 0 {
		// The cursor sat inside a subtree that is now collapsed — re-seat it
		// on the first visible row rather than drifting to a hidden one.
		pos = 0
	}
	pos += delta
	if pos < 0 {
		pos = 0
	}
	if pos >= len(vis) {
		pos = len(vis) - 1
	}
	t.setCursorToID(vis[pos].ID)
	t.clampOffset()
}

// cursorVis is the cursor row's position within VisibleRows (-1 when the
// cursor row is hidden inside a collapsed ancestor).
func (t *Table) cursorVis() int { return t.visIndexOf(t.SelectedID()) }

// visIndexOf is the position of the row with id within VisibleRows (-1 when
// hidden or absent).
func (t *Table) visIndexOf(id string) int {
	for i, r := range t.VisibleRows() {
		if r.ID == id {
			return i
		}
	}
	return -1
}

// setCursorToID points the cursor at the row with id (no-op when absent).
func (t *Table) setCursorToID(id string) {
	for i := range t.Rows {
		if t.Rows[i].ID == id {
			t.Cursor = i
			return
		}
	}
}

// clampOffset keeps the cursor inside the window. Offset is a position in
// VISIBLE rows (that is what View renders), so the cursor is converted to its
// visible position first — comparing a Rows index against a visible offset is
// what let the highlight drift once a node was collapsed.
func (t *Table) clampOffset() {
	h := t.visibleRows()
	pos := t.cursorVis()
	if pos < 0 {
		pos = 0
	}
	if pos < t.Offset {
		t.Offset = pos
	}
	if pos >= t.Offset+h {
		t.Offset = pos - h + 1
	}
	if t.Offset < 0 {
		t.Offset = 0
	}
}

// Wheel scrolls the window (in visible rows, matching View).
func (t *Table) Wheel(delta int) {
	m := len(t.VisibleRows()) - t.visibleRows()
	if m < 0 {
		m = 0
	}
	t.Offset += delta
	if t.Offset > m {
		t.Offset = m
	}
	if t.Offset < 0 {
		t.Offset = 0
	}
}

// Click selects the row at body-relative row index (0 = the first VISIBLE
// body line, which is what View draws), mapping through the tree's visible set
// so a click can never land on a row hidden inside a collapsed node.
func (t *Table) Click(row int) bool {
	vis := t.VisibleRows()
	idx := t.Offset + row
	if row < 0 || idx < 0 || idx >= len(vis) {
		return false
	}
	t.setCursorToID(vis[idx].ID)
	return true
}

// SortBy sorts the flattened rows by a column. Toggling the same column
// flips the direction; tree children stay under their parent.
func (t *Table) SortBy(col int) {
	if col < 0 || col >= len(t.Columns) {
		return
	}
	if t.SortCol == col {
		t.SortDesc = !t.SortDesc
	} else {
		t.SortCol, t.SortDesc = col, false
	}
	less := func(a, b Row) bool {
		av, bv := cell(a, col), cell(b, col)
		if t.SortDesc {
			return av > bv
		}
		return av < bv
	}
	sort.SliceStable(t.Rows, func(i, j int) bool { return less(t.Rows[i], t.Rows[j]) })
	t.Cursor = 0
	t.Offset = 0
}

func cell(r Row, col int) string {
	if col < len(r.Cells) {
		return r.Cells[col]
	}
	return ""
}

// Toggle expands/collapses the selected tree node. Children are hidden by
// marking them invisible (Parent set + !Open on the ancestor). The window is
// re-clamped because collapsing can shrink the visible set under the cursor.
func (t *Table) Toggle() bool {
	r := t.Selected()
	if r == nil || !r.Expand {
		return false
	}
	r.Open = !r.Open
	t.clampOffset()
	return true
}

// ExpandAll opens (or closes) every expandable node in one gesture — the
// operator's "overall collapse and expand". It reports how many nodes CHANGED,
// so a caller can tell a no-op on a flat list from a real toggle.
func (t *Table) ExpandAll(open bool) int {
	n := 0
	for i := range t.Rows {
		if t.Rows[i].Expand && t.Rows[i].Open != open {
			t.Rows[i].Open = open
			n++
		}
	}
	t.clampOffset()
	return n
}

// AllExpanded reports whether every expandable node is currently open. A flat
// list (no parents) counts as expanded, so the first toggle closes.
func (t *Table) AllExpanded() bool {
	for _, r := range t.Rows {
		if r.Expand && !r.Open {
			return false
		}
	}
	return true
}

// ExpandableCount is the number of rows that have children.
func (t *Table) ExpandableCount() int {
	n := 0
	for _, r := range t.Rows {
		if r.Expand {
			n++
		}
	}
	return n
}

// VisibleRows returns the rows currently shown (tree-aware): a row is hidden
// when ANY ancestor on its parent chain is collapsed — not merely when its
// direct parent is. The direct-parent-only check left a collapsed node's
// GRANDCHILDREN on screen, because their own parent was still marked open.
func (t *Table) VisibleRows() []Row {
	open := map[string]bool{}
	parent := make(map[string]string, len(t.Rows))
	for _, r := range t.Rows {
		if r.Expand {
			open[r.ID] = r.Open
		}
		parent[r.ID] = r.Parent
	}
	out := make([]Row, 0, len(t.Rows))
	for _, r := range t.Rows {
		if hiddenUnderCollapsedAncestor(r, parent, open) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// hiddenUnderCollapsedAncestor walks a row's parent chain and reports whether
// any ancestor is collapsed. seen bounds the walk so a malformed (cyclic)
// parent chain can never hang the render.
func hiddenUnderCollapsedAncestor(r Row, parent map[string]string, open map[string]bool) bool {
	seen := 0
	for p := r.Parent; p != ""; {
		if !open[p] {
			return true
		}
		p = parent[p]
		seen++
		if seen > len(parent) {
			return false
		}
	}
	return false
}

// View renders the table body (title + header + rows + position).
// TitleRows reports whether the table renders a leading title row.
func (t *Table) TitleRows() int {
	if t.HideTitle {
		return 0
	}
	return 1
}

// HeaderRows reports whether the table renders a column-header row. An empty
// header (all column titles blank — the common single-column display) is NOT
// rendered: it would be a blank row that shifts the hit-test for nothing.
func (t *Table) HeaderRows() int {
	for _, c := range t.Columns {
		if strings.TrimSpace(c.Title) != "" {
			return 1
		}
	}
	return 0
}

func (t *Table) View() string {
	var b strings.Builder
	if t.TitleRows() > 0 {
		title := t.Title
		if t.NextPageToken != "" {
			title += theme.HintText.Render("  (more: f)")
		}
		if t.Focused {
			b.WriteString(theme.ListTitle.Render(title))
		} else {
			b.WriteString(theme.ListMeta.Render(title))
		}
		b.WriteString("\n")
	}

	if t.HeaderRows() > 0 {
		hb := make([]string, 0, len(t.Columns))
		for i, c := range t.Columns {
			mark := ""
			if t.SortCol == i {
				if t.SortDesc {
					mark = "↓"
				} else {
					mark = "↑"
				}
			}
			hb = append(hb, theme.DetailKey.Render(c.Title+mark))
		}
		b.WriteString(strings.Join(hb, "  "))
		b.WriteString("\n")
	}

	if t.Err != "" {
		b.WriteString(theme.ErrorText.Render("⚠ "+t.Err) + "\n")
		return b.String()
	}
	if len(t.Rows) == 0 {
		msg := t.Empty
		if msg == "" {
			msg = "nothing here"
		}
		b.WriteString(theme.HintText.Render(msg) + "\n")
		return b.String()
	}

	vis := t.VisibleRows()
	h := t.visibleRows()
	end := t.Offset + h
	if end > len(vis) {
		end = len(vis)
	}
	if t.Offset > len(vis) {
		t.Offset = 0
	}
	// Identify the selected row by ID: the cursor is a Rows index while the
	// rows drawn here are the VISIBLE ones, and the two stop agreeing as soon
	// as a node is collapsed. A table whose rows carry no IDs falls back to the
	// positional comparison.
	cursorID := t.SelectedID()
	for i := t.Offset; i < end; i++ {
		r := vis[i]
		indent := strings.Repeat("  ", r.Depth)
		// A parent carries a +/- toggle (the operator's "+ sign next to all
		// parents"): "+" collapsed, "-" expanded. A leaf keeps a blank gutter
		// so every row's cells line up.
		marker := "  "
		if r.Expand {
			if r.Open {
				marker = "- "
			} else {
				marker = "+ "
			}
		}
		line := indent + marker + strings.Join(r.Cells, "  ")
		if r.Meta != "" {
			line += "  " + r.Meta
		}
		if (cursorID != "" && r.ID == cursorID) || (cursorID == "" && i == t.Cursor) {
			b.WriteString(theme.ListItemSelected.Render(Pad(line, t.Width)))
		} else {
			b.WriteString(theme.ListItem.Render(Pad(line, t.Width)))
		}
		b.WriteString("\n")
	}
	b.WriteString(theme.HintText.Render(fmt.Sprintf("%d-%d/%d", t.Offset+1, end, len(vis))))
	return b.String()
}
