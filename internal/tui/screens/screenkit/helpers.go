package screenkit

import (
	"strings"

	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// PaneGap separates horizontally joined panes (a colored pipe keeps the
// columns visually split without full box borders at every size).
func PaneGap() string { return theme.HintText.Render(" │ ") }

// Hint renders dim helper text.
func Hint(s string) string { return theme.HintText.Render(s) }

// paneStride is the rendered width of the list pane + the gap (3 columns of " │ " rendering).
func (b *Base) paneStride() int { return b.paneW + 3 }

// mousePane returns 0 (the active list pane) or len(sources) (detail).
func (b *Base) mousePane(x int) int {
	if b.paneW <= 0 {
		return 0
	}
	if x < b.paneW+3 {
		return 0
	}
	return len(b.sources) // detail
}

// stripSourceAt maps a strip-row click x to a source index.
func (b *Base) stripSourceAt(x int) int {
	if b.stripH == 0 || len(b.sources) == 0 || x < 0 {
		return -1
	}
	col := 0
	for i := range b.sources {
		w := len([]rune(" " + b.sources[i].title + " "))
		if x >= col && x < col+w {
			return i
		}
		col += w
		if i < len(b.sources)-1 {
			col += 3 // " │ " separator
		}
	}
	return -1
}

// ClickDetail reports whether the click landed in the detail pane.
func (b *Base) ClickDetail(x int) bool { return b.mousePane(x) >= len(b.sources) }

// Loading reports whether any source pane is showing its spinner.
func (b *Base) Loading() bool {
	for _, s := range b.sources {
		if s.list.Loading {
			return true
		}
	}
	return false
}

// EmptyLines returns n blank lines (layout filler).
func EmptyLines(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("\n", n)
}
