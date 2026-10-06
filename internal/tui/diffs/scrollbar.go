package diffs

import (
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// The scrollbar is a RESERVED 1-cell column at the body's right edge, on all
// three tabs. It is real cells — the body's usable content width is
// m.Width-2 (the panel's left border plus this column) — so the pane's outer
// geometry is unchanged and the bar can never overlap the diff text.
//
// The thumb and the track use DIFFERENT glyphs (a solid block over a thin
// rule), not merely different colours: a colour-only bar would be invisible on
// the ascii profile and indistinguishable to any stripped-text assertion, and
// an operator on a monochrome terminal still has to see where they are.
const (
	scrollThumbGlyph = "█"
	scrollTrackGlyph = "│"
)

// scrollbarCell returns the single-cell bar for viewport row `i` (0-based,
// within a viewport of `viewH` rows showing `total` content lines scrolled to
// `scroll`). When the content fits (total <= viewH) it returns a blank space —
// the column still occupies its cell so the geometry is stable, but no bar is
// drawn.
//
// The thumb is proportional AND tracks the full track: its length is
// viewH*viewH/total and its start maps the scroll range [0, total-viewH] onto
// [0, viewH-thumbLen], so at scroll 0 the thumb sits at the top and at
// scroll = total-viewH it ends exactly at the bottom. (A naive
// scroll*viewH/total start truncates to the same row for every small scroll —
// the thumb would visibly not move on a short list, which is the affordance
// failing at exactly the sizes this pane is used at.)
func scrollbarCell(i, scroll, viewH, total int) string {
	if viewH < 1 || total <= viewH {
		return " "
	}
	thumbLen := viewH * viewH / total
	if thumbLen < 1 {
		thumbLen = 1
	}
	if thumbLen > viewH {
		thumbLen = viewH
	}
	track := viewH - thumbLen
	denom := total - viewH
	thumbStart := 0
	if denom > 0 && track > 0 {
		thumbStart = scroll * track / denom
	}
	if i >= thumbStart && i < thumbStart+thumbLen {
		return theme.DiffScrollThumb.Render(scrollThumbGlyph)
	}
	return theme.DiffScrollTrack.Render(scrollTrackGlyph)
}

// withScrollbar pins each body line to the body content width, appends the scrollbar cell, and pads the
// cell the shell's rail-resize handle occupies, so every produced line is exactly m.bodyWidth()+2 cells
// (which the panel border then carries to m.Width — the shell's budget).
//
// THE TRAILING CELL IS THE DIVIDER'S, and emitting it here is what keeps the bar off the resize edge: the
// scrollbar is the cell BEFORE it, so a press on the bar reaches the pane and a press on the edge resizes
// the rail.
func (m *Model) withScrollbar(lines []string, total int) string {
	if len(lines) == 0 {
		return ""
	}
	w := m.bodyWidth()
	viewH := m.viewHeight()
	out := make([]string, 0, len(lines))
	for i, l := range lines {
		out = append(out, fitToWidth(l, w)+scrollbarCell(i, m.scroll, viewH, total)+" ")
	}
	return strings.Join(out, "\n")
}

// fitToWidth pins a rendered line to exactly `w` printable columns: overlong
// lines are clipped ANSI-aware (never mid-rune) and short lines are padded.
func fitToWidth(s string, w int) string {
	if w < 1 {
		w = 1
	}
	cur := ansi.StringWidth(s)
	if cur > w {
		s = ansi.Truncate(s, w, "")
		cur = w
	}
	if cur < w {
		s += strings.Repeat(" ", w-cur)
	}
	return s
}
