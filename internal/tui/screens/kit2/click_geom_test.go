package kit2

import (
	"context"
	"testing"
)

// Regression for the operator's "I have to click above the item to select it".
//
// The click row must count EVERY row the pane draws above its first data row:
// the shell chrome, the panel's top border, and whatever the embedded table
// adds. The embedded table hides its own title (the panel border shows it, so
// rendering both duplicated the title AND shifted the hit-test by one) and does
// not draw an empty column header.
func TestTableTopRowMatchesRenderedRows(t *testing.T) {
	b := &Base{}
	b.AddSource("a", "A", func(ctx context.Context, pageToken string) ([]Item, string, error) { return nil, "", nil })
	b.AddSource("b", "B", func(ctx context.Context, pageToken string) ([]Item, string, error) { return nil, "", nil })
	b.SetSize(100, 30)
	b.active = 0
	tbl := b.sources[0].table

	// A pane embedded in a Panel: the panel owns the title.
	tbl.HideTitle = true
	if got := b.tableTopRow(); got != shellChromeRows+1 {
		t.Fatalf("tableTopRow = %d, want chrome(%d) + panel border = %d", got, shellChromeRows, shellChromeRows+1)
	}

	// A REAL column header adds exactly one row.
	tbl.Columns = []Column{{Title: "Name"}, {Title: "Status"}}
	if got := b.tableTopRow(); got != shellChromeRows+2 {
		t.Fatalf("with a header tableTopRow = %d, want %d", got, shellChromeRows+2)
	}

	// An EMPTY header must not add a row (the default single-column display).
	tbl.Columns = []Column{{Title: ""}}
	if got := b.tableTopRow(); got != shellChromeRows+1 {
		t.Fatalf("an empty header must not add a row: tableTopRow = %d, want %d", got, shellChromeRows+1)
	}

	// A standalone table (no panel) renders its title, and that row counts.
	tbl.HideTitle = false
	if got := b.tableTopRow(); got != shellChromeRows+2 {
		t.Fatalf("a standalone title row must count: tableTopRow = %d, want %d", got, shellChromeRows+2)
	}
}

// The X hit-test must describe the TWO-pane layout View() renders, not the old
// all-panes grid: the left pane is the focused source, everything past the gap
// is the detail.
func TestMouseRegionMatchesTwoPaneLayout(t *testing.T) {
	b := &Base{}
	for _, n := range []string{"a", "b", "c"} {
		b.AddSource(n, n, func(ctx context.Context, pageToken string) ([]Item, string, error) { return nil, "", nil })
	}
	b.SetSize(120, 30)
	b.active = 1

	ws := SplitWidths(120, 2, 1)
	if p, detail := b.mouseRegion(2); detail || p != 1 {
		t.Fatalf("left of the pane split must be the FOCUSED source (got p=%d detail=%v)", p, detail)
	}
	if _, detail := b.mouseRegion(ws[0] + 5); !detail {
		t.Fatal("right of the pane split must be the detail pane")
	}
}
