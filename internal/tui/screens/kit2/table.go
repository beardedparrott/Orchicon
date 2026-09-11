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
}

// NewTable builds an empty table with columns.
func NewTable(title string, cols ...Column) *Table {
	return &Table{Title: title, Columns: cols, SortCol: -1}
}

// SetItems seeds the table from list items (single-column display).
func (t *Table) SetItems(items []screenkit.Item, next string) {
	rows := make([]Row, 0, len(items))
	for _, it := range items {
		rows = append(rows, Row{ID: it.ID, Cells: []string{it.Title}, Meta: it.Meta})
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

// Move shifts the cursor by delta (clamped), keeping the window in view.
func (t *Table) Move(delta int) {
	if len(t.Rows) == 0 {
		return
	}
	t.Cursor += delta
	if t.Cursor < 0 {
		t.Cursor = 0
	}
	if t.Cursor >= len(t.Rows) {
		t.Cursor = len(t.Rows) - 1
	}
	t.clampOffset()
}

func (t *Table) clampOffset() {
	h := t.visibleRows()
	if t.Cursor < t.Offset {
		t.Offset = t.Cursor
	}
	if t.Cursor >= t.Offset+h {
		t.Offset = t.Cursor - h + 1
	}
	if t.Offset < 0 {
		t.Offset = 0
	}
}

// Wheel scrolls the window.
func (t *Table) Wheel(delta int) {
	m := len(t.Rows) - t.visibleRows()
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

// Click selects the row at body-relative row index (0 = first body line).
func (t *Table) Click(row int) bool {
	idx := t.Offset + row
	if row >= 0 && idx >= 0 && idx < len(t.Rows) {
		t.Cursor = idx
		return true
	}
	return false
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
// marking them invisible (Parent set + !Open on the ancestor).
func (t *Table) Toggle() bool {
	r := t.Selected()
	if r == nil || !r.Expand {
		return false
	}
	r.Open = !r.Open
	return true
}

// VisibleRows returns the rows currently shown (tree-aware): a row whose
// ancestor chain contains a collapsed node is hidden.
func (t *Table) VisibleRows() []Row {
	open := map[string]bool{}
	for _, r := range t.Rows {
		if r.Expand {
			open[r.ID] = r.Open
		}
	}
	out := make([]Row, 0, len(t.Rows))
	for _, r := range t.Rows {
		if r.Parent != "" && !open[r.Parent] {
			continue
		}
		out = append(out, r)
	}
	return out
}

// View renders the table body (title + header + rows + position).
func (t *Table) View() string {
	var b strings.Builder
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

	if len(t.Columns) > 0 {
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
	for i := t.Offset; i < end; i++ {
		r := vis[i]
		indent := strings.Repeat("  ", r.Depth)
		marker := "  "
		if r.Expand {
			if r.Open {
				marker = "▾ "
			} else {
				marker = "▸ "
			}
		}
		line := indent + marker + strings.Join(r.Cells, "  ")
		if r.Meta != "" {
			line += "  " + r.Meta
		}
		if i == t.Cursor {
			b.WriteString(theme.ListItemSelected.Render(Pad(line, t.Width)))
		} else {
			b.WriteString(theme.ListItem.Render(Pad(line, t.Width)))
		}
		b.WriteString("\n")
	}
	b.WriteString(theme.HintText.Render(fmt.Sprintf("%d-%d/%d", t.Offset+1, end, len(vis))))
	return b.String()
}
