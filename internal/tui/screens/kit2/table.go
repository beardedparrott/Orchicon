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

// Table is a selectable, sortable, tree-capable table.
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

	// HideTitle suppresses the table's own title row.
	HideTitle bool

	// Filter narrows the visible rows (case-insensitive substring over the row's cells, meta and id).
	Filter string

	// CollapsedByDefault is the OPEN STATE a row takes the FIRST TIME it appears. It applies ONLY to a
	// row with no surviving state (see SetItems).
	CollapsedByDefault bool

	// Marks is the MULTI-SELECTION: row IDs the operator has marked with space, for the bulk operations
	// every list needs a consistent way to reach.
	Marks map[string]bool
}

// IsMarked reports whether a row is part of the multi-selection.
func (t *Table) IsMarked(id string) bool { return id != "" && t.Marks[id] }

// MarkCount is how many rows are marked.
func (t *Table) MarkCount() int { return len(t.Marks) }

// ToggleMark flips a row's mark and reports the NEW state. An empty id is refused: a
// gutter/parent row or an item that never got an id cannot be a member of the selection.
func (t *Table) ToggleMark(id string) bool {
	if id == "" {
		return false
	}
	if t.Marks == nil {
		t.Marks = map[string]bool{}
	}
	if t.Marks[id] {
		delete(t.Marks, id)
		return false
	}
	t.Marks[id] = true
	return true
}

// ClearMarks empties the multi-selection. It reports whether there was anything to clear, so
// esc can fall through to its other meaning when the operator had nothing marked.
func (t *Table) ClearMarks() bool {
	if len(t.Marks) == 0 {
		return false
	}
	t.Marks = nil
	return true
}

// MarkedIDs returns the marked row IDs in DRAW ORDER (the visible order the operator sees), not map
// order — a bulk action that reports what it acted on must name them in the sequence on screen.
func (t *Table) MarkedIDs() []string {
	if len(t.Marks) == 0 {
		return nil
	}
	out := make([]string, 0, len(t.Marks))
	for _, r := range t.VisibleRows() {
		if t.Marks[r.ID] {
			out = append(out, r.ID)
		}
	}
	return out
}

// RowByID returns the row with the given id, and whether it is present.
func (t *Table) RowByID(id string) (Row, bool) {
	for i := range t.Rows {
		if t.Rows[i].ID == id {
			return t.Rows[i], true
		}
	}
	return Row{}, false
}

// PruneMarks drops marks whose rows are no longer present, and reports how many went.
func (t *Table) PruneMarks() int {
	if len(t.Marks) == 0 {
		return 0
	}
	present := make(map[string]bool, len(t.Rows))
	for _, r := range t.Rows {
		present[r.ID] = true
	}
	gone := 0
	for id := range t.Marks {
		if !present[id] {
			delete(t.Marks, id)
			gone++
		}
	}
	if len(t.Marks) == 0 {
		t.Marks = nil
	}
	return gone
}

// NewTable builds an empty table with columns.
func NewTable(title string, cols ...Column) *Table {
	return &Table{Title: title, Columns: cols, SortCol: -1}
}

// SetItems seeds the table from list items (single-column display).
func (t *Table) ApplyCollapseDefault() {
	for i := range t.Rows {
		t.Rows[i].Open = !t.CollapsedByDefault
	}
	t.clampOffset()
}

func (t *Table) SetItems(items []screenkit.Item, next string) {
	prev := t.SelectedID()
	wasOpen := make(map[string]bool, len(t.Rows))
	for _, r := range t.Rows {
		if r.ID != "" {
			wasOpen[r.ID] = r.Open
		}
	}
	rows := make([]Row, 0, len(items))
	for _, it := range items {
		open, seen := wasOpen[it.ID]
		if !seen {
			open = !t.CollapsedByDefault
		}
		rows = append(rows, Row{
			ID:     it.ID,
			Cells:  []string{it.Title},
			Meta:   it.Meta,
			Depth:  it.Depth,
			Parent: it.Parent,
			Expand: it.HasChildren,
			Open:   open,
		})
	}
	t.Rows = rows
	t.NextPageToken = next
	t.Cursor, t.Offset = 0, 0
	if prev != "" {
		t.setCursorToID(prev)
		t.clampOffset()
	}
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

// Move shifts the cursor by delta over the VISIBLE rows, keeping the window in view.
func (t *Table) Move(delta int) {
	idxs := t.visibleIndices()
	if len(idxs) == 0 {
		return
	}
	pos := -1
	for i, ri := range idxs {
		if ri == t.Cursor {
			pos = i
			break
		}
	}
	if pos < 0 {
		// The cursor sat inside a subtree that is now collapsed — re-seat it on
		// the first visible row rather than drifting to a hidden one.
		pos = 0
	} else {
		pos += delta
	}
	if pos < 0 {
		pos = 0
	}
	if pos >= len(idxs) {
		pos = len(idxs) - 1
	}
	t.Cursor = idxs[pos]
	t.clampOffset()
}

// visibleIndices returns the Rows indices of the visible rows, in order.
func (t *Table) visibleIndices() []int {
	open, parent := t.collapseState()
	matching, keep := t.filterSets()
	out := make([]int, 0, len(t.Rows))
	for i, r := range t.Rows {
		if t.filterActive() {
			// A FILTER SUSPENDS COLLAPSE.
			if matching[r.ID] || keep[r.ID] {
				out = append(out, i)
			}
			continue
		}
		if hiddenUnderCollapsedAncestor(r, parent, open) {
			continue
		}
		out = append(out, i)
	}
	return out
}

// collapseState snapshots the tree's collapse state: which parents are open, and
// each row's parent.
func (t *Table) collapseState() (open map[string]bool, parent map[string]string) {
	open = make(map[string]bool, len(t.Rows))
	parent = make(map[string]string, len(t.Rows))
	for _, r := range t.Rows {
		if r.Expand {
			open[r.ID] = r.Open
		}
		parent[r.ID] = r.Parent
	}
	return open, parent
}

// filterActive reports whether a narrowing query is in force.
func (t *Table) filterActive() bool { return strings.TrimSpace(t.Filter) != "" }

// filterSets returns the ids that pass the query and the ids that must remain VISIBLE for structure
// (the ancestors of matches).
func (t *Table) filterSets() (matching, keep map[string]bool) {
	if !t.filterActive() {
		return nil, nil
	}
	matching = make(map[string]bool, len(t.Rows))
	for _, r := range t.Rows {
		if t.matchesFilter(r) && r.ID != "" {
			matching[r.ID] = true
		}
	}
	_, parent := t.collapseState()
	keep = make(map[string]bool, len(matching))
	for id := range matching {
		seen := 0
		for p := parent[id]; p != ""; p = parent[p] {
			keep[p] = true
			// Bounded like hiddenUnderCollapsedAncestor: a malformed (cyclic) parent
			// chain must not hang a render.
			seen++
			if seen > len(parent) {
				break
			}
		}
	}
	return matching, keep
}

// VisibleRows returns the rows currently shown.
func (t *Table) VisibleRows() []Row {
	idxs := t.visibleIndices()
	out := make([]Row, 0, len(idxs))
	for _, i := range idxs {
		out = append(out, t.Rows[i])
	}
	return out
}

// cursorVis is the cursor row's position within VisibleRows (-1 when the cursor row is hidden
// inside a collapsed ancestor).
func (t *Table) cursorVis() int {
	for i, ri := range t.visibleIndices() {
		if ri == t.Cursor {
			return i
		}
	}
	return -1
}

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

// clampOffset keeps the cursor inside the window.
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

// Click selects the row at body-relative row index (0 = the first VISIBLE body line, which is what
// View draws), mapping through the tree's visible set so a click can never land on a row hidden.
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

// Toggle expands/collapses the selected tree node.
func (t *Table) Toggle() bool {
	r := t.Selected()
	if r == nil || !r.Expand {
		return false
	}
	r.Open = !r.Open
	t.clampOffset()
	return true
}

// ToggleAt toggles the tree node whose +/- marker sits at pane-relative column x on body row `row`
// (0 = the first visible body line).
func (t *Table) ToggleAt(row, x int) bool {
	vis := t.VisibleRows()
	idx := t.Offset + row
	if row < 0 || idx < 0 || idx >= len(vis) {
		return false
	}
	r := vis[idx]
	if !r.Expand {
		return false
	}
	markerX := 1 + 2*r.Depth
	if x < markerX || x > markerX+1 {
		return false
	}
	t.setCursorToID(r.ID)
	return t.Toggle()
}

// ExpandAll opens (or closes) every expandable node in one gesture — the operator's "overall
// collapse and expand".
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

// ResetCursor parks the cursor BEFORE the first row, so the next SetItems (a reload with no
// surviving selection) starts at the TOP rather than restoring the previous row.
func (t *Table) ResetCursor() {
	t.Cursor, t.Offset = -1, 0
}

// SetFilter narrows the visible rows and re-clamps the window (a filter can
// shrink the visible set under the cursor).
func (t *Table) SetFilter(q string) {
	t.Filter = q
	t.clampOffset()
}

// matchesFilter reports whether a row satisfies the current filter (a case-insensitive substring
// over its cells, meta and id).
func (t *Table) matchesFilter(r Row) bool {
	q := strings.ToLower(strings.TrimSpace(t.Filter))
	if q == "" {
		return true
	}
	hay := strings.ToLower(strings.Join(r.Cells, " ") + " " + r.Meta + " " + r.ID)
	return strings.Contains(hay, q)
}

// MatchCount reports how many rows pass the filter and how many exist — the
// "n/m" the filter row shows, so a narrowing filter is never silent.
func (t *Table) MatchCount() (int, int) {
	n := 0
	for _, r := range t.Rows {
		if t.matchesFilter(r) {
			n++
		}
	}
	return n, len(t.Rows)
}

// hiddenUnderCollapsedAncestor walks a row's parent chain and reports whether any ancestor is
// collapsed.
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

// HeaderRows reports whether the table renders a column-header row.
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
		// NO "more pages" marker: every source is fetched WHOLE (base.loadSource), so a page token is never
		// set and there is never anything left unfetched to advertise.
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
	// Identify the selected row by ID: the cursor is a Rows index while the rows drawn here are the
	// VISIBLE ones, and the two stop agreeing as soon as a node is collapsed.
	cursorID := t.SelectedID()
	// The MARK gutter appears once anything is marked, and only then: drawing an empty "[ ]" on every
	// row of every list would spend a gutter on an affordance most panes never use.
	showMarks := t.MarkCount() > 0
	for i := t.Offset; i < end; i++ {
		r := vis[i]
		indent := strings.Repeat("  ", r.Depth)
		// A parent carries a +/- toggle (the operator's "+ sign next to all parents"): "+" collapsed, "-"
		// expanded.
		marker := "  "
		if r.Expand {
			if r.Open {
				marker = "- "
			} else {
				marker = "+ "
			}
		}
		mark := ""
		if showMarks {
			if t.Marks[r.ID] && r.ID != "" {
				mark = "[x] "
			} else {
				mark = "[ ] "
			}
		}
		line := indent + mark + marker + strings.Join(r.Cells, "  ")
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
