package kit2

import (
	"context"
	"testing"
)

// Regression for the operator's "if I click on one conversation it might select
// one four or five rows down": the data-row offset ignored the shell chrome and
// the panel border, so every click selected a row several positions from the
// cursor. It must count chrome rows + the panel's top border (+ a column header
// when the table has columns).
func TestTableTopRowCountsChromeAndPanelBorder(t *testing.T) {
	b := &Base{}
	b.AddSource("a", "A", func(ctx context.Context, pageToken string) ([]Item, string, error) { return nil, "", nil })
	b.AddSource("b", "B", func(ctx context.Context, pageToken string) ([]Item, string, error) { return nil, "", nil })
	b.SetSize(100, 30)

	// A table with columns carries a header row; one without does not.
	b.sources[0].table.Columns = nil
	b.active = 0
	noHeader := b.tableTopRow()
	b.sources[0].table.Columns = []Column{{Title: "Name"}, {Title: "Status"}}
	withHeader := b.tableTopRow()

	if withHeader != noHeader+1 {
		t.Fatalf("a column header must add exactly one row: %d vs %d", noHeader, withHeader)
	}
	if noHeader != shellChromeRows+1 {
		t.Fatalf("tableTopRow = %d, want shell chrome (%d) + the panel border = %d", noHeader, shellChromeRows, shellChromeRows+1)
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
