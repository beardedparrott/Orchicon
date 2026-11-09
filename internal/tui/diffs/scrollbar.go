package diffs

import (
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// The scrollbar is a RESERVED 1-cell column at the body's right edge, on all three tabs.
const (
	scrollThumbGlyph = "█"
	scrollTrackGlyph = "│"
	// scrollHandleGlyph marks the cell the SHELL's rail-resize handle occupies (the pane's last cell).
	scrollHandleGlyph = "┊"
)

// scrollbarThumb is the thumb's row range within the track, for a viewport of `viewH` rows showing
// `total` lines scrolled to `scroll`.
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

// scrollbarCell returns the single-cell bar for viewport row `i` (0-based, within a viewport of
// `viewH` rows showing `total` content lines scrolled to `scroll`).
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

// withScrollbar pins each body line to the body content width, appends the scrollbar cell, and pads
// the cell the shell's rail-resize handle occupies.
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
