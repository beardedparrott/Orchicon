package md

import (
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/yuin/goldmark/ast"
	east "github.com/yuin/goldmark/extension/ast"
)

// tableMinCol is the narrowest a column may be squeezed to before the table gives up on the grid.
const tableMinCol = 4

// table renders a GFM table as an aligned grid.
//
// TABLES ARE THE ONE CONSTRUCT THAT CANNOT SIMPLY REFLOW, which is why the operator called them out
// separately. A grid has a minimum viable width (one cell per column plus separators); below that,
// aligning columns stops being possible at all. So there are two shapes:
//
//  1. THE GRID, when the columns fit. Cell contents are WRAPPED (never truncated) into their column,
//     because a clipped cell is exactly the silent data loss this package exists to remove.
//  2. A VERTICAL RECORD, when even the minimum grid would not fit — each row becomes "column: value"
//     lines. It loses the grid but keeps every value, and it ALWAYS fits, which is the operator's
//     hard requirement ("as long as it only takes up the width of the pane it is currently in").
//
// Every function here takes the renderer (and therefore the source bytes) rather than reading a
// package-level copy: the TUI can render from more than one goroutine, so shared mutable parse state
// would be a data race waiting for a busy tenant.
func (r *renderer) table(n *east.Table, in indent) {
	cols, align := r.tableShape(n)
	if cols == 0 {
		return
	}
	avail := r.width - in.prefixWidth()
	if avail < 8 {
		avail = 8
	}
	rows := r.tableRows(n)

	// Widths come from the CONTENT, then get squeezed to fit.
	widths := make([]int, cols)
	for _, row := range rows {
		for j, cell := range row.cells {
			if j >= cols {
				break
			}
			if w := spansWidth(cell); w > widths[j] {
				widths[j] = w
			}
		}
	}
	// A header cell is a single token, so it cannot wrap below its own label's width.
	for j, h := range align.headers {
		if j < cols {
			if w := lipgloss.Width(h); w > widths[j] {
				widths[j] = w
			}
		}
	}
	for j := range widths {
		if widths[j] < tableMinCol {
			widths[j] = tableMinCol
		}
	}

	// gridWidth is what the grid needs; each boundary separator is " │ ", i.e. 3 columns.
	gridWidth := func(w []int) int {
		t := 0
		for _, c := range w {
			t += c
		}
		return t + 3*(len(w)-1)
	}
	for gridWidth(widths) > avail {
		// Squeeze the WIDEST column that can still give up a cell, so the loss is shared out rather
		// than taken entirely from the first column.
		worst, wi := 0, -1
		for j, w := range widths {
			if w > tableMinCol && w > worst {
				worst, wi = w, j
			}
		}
		if wi < 0 {
			break // nothing left to give: the grid cannot be made to fit
		}
		widths[wi]--
	}
	if gridWidth(widths) > avail {
		r.tableRecords(rows, cols, align.headers, in)
		return
	}

	// --- the grid ---------------------------------------------------------------------------------
	for _, row := range rows {
		// Wrap every cell into its column, then emit the row ONE VISUAL LINE AT A TIME, so the
		// columns stay aligned across a cell that needed wrapping.
		cellLines := make([][]string, cols)
		height := 1
		for j := 0; j < cols; j++ {
			var spans []span
			if j < len(row.cells) {
				spans = row.cells[j]
			}
			if row.header {
				spans = boldAll(spans)
			}
			var rendered []string
			for _, ln := range wrapSpans(spans, widths[j]) {
				rendered = append(rendered, spansRender(ln))
			}
			if len(rendered) == 0 {
				rendered = []string{""}
			}
			cellLines[j] = rendered
			if len(rendered) > height {
				height = len(rendered)
			}
		}
		for h := 0; h < height; h++ {
			parts := make([]string, cols)
			for j := 0; j < cols; j++ {
				txt := ""
				if h < len(cellLines[j]) {
					txt = cellLines[j][h]
				}
				parts[j] = padCell(txt, widths[j], align.at(j))
			}
			r.raw(in.first + strings.Join(parts, " \x1b[2m│\x1b[22m "))
		}
		if row.header {
			segs := make([]string, cols)
			for j := range segs {
				segs[j] = strings.Repeat("─", widths[j])
			}
			r.raw(in.rest + "\x1b[2m" + strings.Join(segs, "─┼─") + "\x1b[22m")
		}
	}
}

// tableAlign carries the column headers and their alignment.
type tableAlign struct {
	headers []string
	aligns  []east.Alignment
}

func (t tableAlign) at(j int) east.Alignment {
	if j < len(t.aligns) {
		return t.aligns[j]
	}
	return east.AlignNone
}

// tableRow is one row: its cells (as spans) and whether it is the header.
type tableRow struct {
	cells  [][]span
	header bool
}

// tableShape returns the column count, the headers and the alignments.
//
// The alignments and the headers are not always on the same node — GFM records alignments per row as
// well as on the header — so the widest source wins, and the column count is the MAXIMUM over the
// header, the alignments and every row (a row with more cells than the header still renders).
func (r *renderer) tableShape(n *east.Table) (int, tableAlign) {
	var t tableAlign
	cols := 0
	if hdr, ok := n.FirstChild().(*east.TableHeader); ok {
		if len(hdr.Alignments) > len(t.aligns) {
			t.aligns = hdr.Alignments
		}
		for cell := hdr.FirstChild(); cell != nil; cell = cell.NextSibling() {
			tc, is := cell.(*east.TableCell)
			if !is {
				continue
			}
			t.headers = append(t.headers, spansText(inline(tc, r.src, 0)))
		}
	}
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		row, ok := c.(*east.TableRow)
		if !ok {
			continue
		}
		if len(row.Alignments) > len(t.aligns) {
			t.aligns = row.Alignments
		}
		if cnt := row.ChildCount(); cnt > cols {
			cols = cnt
		}
	}
	if len(t.headers) > cols {
		cols = len(t.headers)
	}
	if len(t.aligns) > cols {
		cols = len(t.aligns)
	}
	return cols, t
}

// tableRows flattens the header and body rows into one list, header first.
func (r *renderer) tableRows(n *east.Table) []tableRow {
	var out []tableRow
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		switch row := c.(type) {
		case *east.TableHeader:
			out = append(out, tableRow{cells: r.tableCells(row), header: true})
		case *east.TableRow:
			out = append(out, tableRow{cells: r.tableCells(row)})
		}
	}
	return out
}

func (r *renderer) tableCells(row ast.Node) [][]span {
	var out [][]span
	for c := row.FirstChild(); c != nil; c = c.NextSibling() {
		tc, ok := c.(*east.TableCell)
		if !ok {
			continue
		}
		out = append(out, inline(tc, r.src, 0))
	}
	return out
}

// boldAll applies bold to every span (the header row).
func boldAll(spans []span) []span {
	out := make([]span, len(spans))
	for i, s := range spans {
		s.a |= aBold
		out[i] = s
	}
	return out
}

// padCell pads a RENDERED cell to width according to its alignment. It measures with lipgloss.Width,
// because the cell may already carry SGR codes.
func padCell(rendered string, width int, align east.Alignment) string {
	pad := width - lipgloss.Width(rendered)
	if pad < 0 {
		pad = 0
	}
	switch align {
	case east.AlignRight:
		return strings.Repeat(" ", pad) + rendered
	case east.AlignCenter:
		l := pad / 2
		return strings.Repeat(" ", l) + rendered + strings.Repeat(" ", pad-l)
	default:
		return rendered + strings.Repeat(" ", pad)
	}
}

// tableRecords is the fallback for a table too wide to align: each row becomes "column: value" lines,
// wrapped to the pane. The grid is lost; NOT ONE VALUE IS, and it cannot overflow.
func (r *renderer) tableRecords(rows []tableRow, cols int, headers []string, in indent) {
	first := true
	for _, row := range rows {
		if row.header {
			continue
		}
		if !first {
			r.blank()
		}
		first = false
		for j := 0; j < cols; j++ {
			label := "col" + strconv.Itoa(j+1)
			if j < len(headers) && headers[j] != "" {
				label = headers[j]
			}
			// The label is bold, then the value wraps under it — indented so the NEXT label is
			// visually distinct from a continuation of the value above it.
			prefix := in.first + "\x1b[1m" + label + "\x1b[22m: "
			body := in.rest + strings.Repeat(" ", lipgloss.Width(label)+2)
			var spans []span
			if j < len(row.cells) {
				spans = row.cells[j]
			}
			r.emit(indent{first: prefix, rest: body}, spans)
		}
	}
}
