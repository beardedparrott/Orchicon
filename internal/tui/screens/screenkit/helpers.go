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

// paneStride is the rendered width of one pane + the gap (3 columns of
// " │ " rendering + padding).
func (b *Base) paneStride() int { return b.paneW + 5 }

// mousePane returns the pane index under terminal column x (>= len
// (sources) = detail pane).
func (b *Base) mousePane(x int) int {
	if b.paneW <= 0 {
		return 0
	}
	p := x / b.paneStride()
	if p > len(b.sources) {
		p = len(b.sources) // detail
	}
	return p
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
