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
	// scrollHandleGlyph marks the cell the SHELL's rail-resize handle occupies (the pane's last cell).
	//
	// IT IS DRAWN, AND DASHED, and both matter. Drawn, because an invisible handle is what made the
	// operator report that resizing was broken: the handle has its own column now (the bar moved one cell
	// in from the edge, so the two affordances stopped competing), and with nothing rendered there the only
	// visible thing in that region was the BAR — which is what they had been grabbing. Dashed, so it cannot
	// be confused with the bar's solid track or its block thumb.
	scrollHandleGlyph = "┊"
)

// scrollbarThumb is the thumb's row range within the track, for a viewport of `viewH` rows showing `total`
// lines scrolled to `scroll`.
//
// ONE DEFINITION, THREE READERS: the cell that draws it, the hit-test that decides whether a grab landed on
// the thumb, and the drag that needs the thumb's travel range to know how far a row of pointer movement
// should move the content. They used to be derived separately (the cell alone had the arithmetic), which is
// exactly how a bar can be drawn in one place and grabbed in another.
func scrollbarThumb(scroll, viewH, total int) (start, length int) {
	if viewH < 1 || total <= viewH {
		return 0, 0
	}
	length = viewH * viewH / total
	if length < 1 {
		length = 1
	}
	if length > viewH {
		length = viewH
	}
	track := viewH - length
	denom := total - viewH
	if denom > 0 && track > 0 {
		start = scroll * track / denom
	}
	return start, length
}

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
	thumbStart, thumbLen := scrollbarThumb(scroll, viewH, total)
	if i >= thumbStart && i < thumbStart+thumbLen {
		return theme.DiffScrollThumb.Render(scrollThumbGlyph)
	}
	return theme.DiffScrollTrack.Render(scrollTrackGlyph)
}

// withScrollbar pins each body line to the body content width, appends the scrollbar cell, and pads the
// cell the shell's rail-resize handle occupies, so every produced line is exactly m.bodyWidth()+2 cells
// (which the panel border then carries to m.Width — the shell's budget).
//
// THE TRAILING CELL IS THE RESIZE HANDLE'S, and emitting it here is what keeps the bar off the resize edge:
// the scrollbar is the cell BEFORE it, so a press on the bar reaches the pane and a press on the edge
// resizes the rail. It is RENDERED (a faint dashed rule) rather than left blank, because the blank version
// read as "there is no handle here" — the operator went looking for the resize grip and found only the bar.
func (m *Model) withScrollbar(lines []string, total int) string {
	if len(lines) == 0 {
		return ""
	}
	w := m.bodyWidth()
	viewH := m.viewHeight()
	out := make([]string, 0, len(lines))
	for i, l := range lines {
		// [content][bar][handle]: the last cell is the SHELL's resize handle, drawn here (the pane owns the
		// rail's cells) so the affordance the shell's hit-test uses is visible where it can be grabbed.
		out = append(out,
			fitToWidth(l, w)+
				scrollbarCell(i, m.scroll, viewH, total)+
				theme.DiffResizeHandle.Render(scrollHandleGlyph))
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
