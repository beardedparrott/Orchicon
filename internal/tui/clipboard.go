// clipboard.go — SELECT AND COPY in the TUI.
//
// The operator: "Copying of text on every screen should work but it's not. It's not allowing me to
// highlight any text. I should be able to highlight any text and then a pop up should say 'copied
// to clipboard'."
//
// WHY IT DID NOT WORK: `orch` runs with tea.WithMouseCellMotion(), which turns on the terminal's
// button-event reporting — the terminal hands every press, DRAG and release to the program instead
// of running its own text selection. With no in-app selection to replace it, there was nothing to
// drag with: the mouse could click, and it could not select. Native selection survives only as the
// terminal's shift-drag bypass, which is a per-terminal convention the operator cannot be expected
// to know (and which some terminals do not honour at all).
//
// So the app does the selection itself, which is also what makes the gesture work uniformly on
// EVERY screen — the shell owns the frame, so it can select over chrome and panes alike:
//
//	press · drag · release  →  the dragged text goes to the clipboard, and a toast says so
//
// The selection addresses CELLS of the drawn frame, never the widgets that produced it. That is
// why it works everywhere and why nothing in a screen has to cooperate: the shell already has the
// exact text it painted, so what the operator highlights is what they read.
package tui

import (
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charmbracelet/x/ansi"

	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// clipToastDuration is how long "copied to clipboard" stays up. Long enough to be read without
// looking for it, short enough that it is gone when the operator goes back to work.
const clipToastDuration = 2 * time.Second

// clipToastText is the confirmation. The operator named it.
const clipToastText = "copied to clipboard"

// clipToastMsg clears a copy toast.
type clipToastMsg struct{ seq int }

// clipState is the frame the shell last painted plus the drag in progress over it.
type clipState struct {
	frame []string // the last rendered frame, one entry per ROW
	width int

	// drag is true from a left press until its release. ax/ay are the anchor (where the press
	// landed) and hx/hy the head (the latest drag position) — both in frame CELLS.
	drag   bool
	ax, ay int
	hx, hy int

	// region is the pane the CURRENT drag started in, and hasRegion says whether one was found.
	region    clipRegion
	hasRegion bool

	// toast is the transient confirmation line; toastSeq guards its timer.
	toast    string
	toastSeq int
}

// clipRegion is the pane a selection is confined to.
type clipRegion struct{ x0, y0, x1, y1 int }

// setRegion confines the next drag to a pane; clearRegion leaves it unbounded.
func (c *clipState) setRegion(r clipRegion) { c.region, c.hasRegion = r, true }
func (c *clipState) clearRegion()           { c.region, c.hasRegion = clipRegion{}, false }

// selectionRegionAt classifies a frame CELL to the PANE that owns it, so a drag can be confined to
// the pane its anchor fell in.
func (m *App) selectionRegionAt(x, y int) (clipRegion, bool) {
	w, h := m.width, m.height
	if w <= 0 || h <= 0 || x < 0 || x >= w || y < 0 || y >= h {
		return clipRegion{}, false
	}
	top := tabBarRows + 1 // row 0 the tab bar, row 1 the rule, row 2 the blank separator
	screenRows, panelRows, dockRows := m.screenRows(), m.panelRows(), m.dock.Lines()
	bottom := top + screenRows + panelRows + dockRows - 1
	// THE FOOTER IS NOT A PANE, and a body taller than the frame is truncated by fillView — so the last
	// row that can carry one is the row above the footer.
	if last := h - 2; bottom > last {
		bottom = last
	}
	if y < top || y > bottom {
		return clipRegion{}, false
	}
	// The rail and the diff pane are joined over the whole body, so they claim their columns before the
	// center column is considered.
	if m.railVisible() && x >= w-ConversationsRailWidth {
		return clipRegion{w - ConversationsRailWidth, top, w - 1, bottom}, true
	}
	left := 0
	if m.diffOpen && m.diffPane != nil {
		if x < m.diffPaneWidth() {
			return clipRegion{0, top, m.diffPaneWidth() - 1, bottom}, true
		}
		left = m.diffPaneWidth()
	}
	right := left + m.contentWidth() - 1
	// Three panes stacked in the center column: the screen, the strip, the dock.
	switch {
	case y < top+screenRows:
		return clipRegion{left, top, right, top + screenRows - 1}, true
	case y < top+screenRows+panelRows:
		return clipRegion{left, top + screenRows, right, top + screenRows + panelRows - 1}, true
	}
	return clipRegion{left, top + screenRows + panelRows, right, bottom}, true
}

func clampTo(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// setFrame records the frame the renderer just painted. The selection is expressed in ITS cells.
func (c *clipState) setFrame(frame string) {
	c.frame = strings.Split(frame, "\n")
	if len(c.frame) > 0 {
		c.width = ansi.StringWidth(c.frame[0])
	}
}

// dragging reports whether a selection is currently being made — the shell inverts it while it is.
func (c *clipState) dragging() bool {
	_, _, _, _, ok := c.bounds()
	return ok
}

// bounds normalizes the drag into an inclusive cell rectangle, top-left first.
func (c *clipState) bounds() (x0, y0, x1, y1 int, ok bool) {
	if !c.drag {
		return 0, 0, 0, 0, false
	}
	x0, y0, x1, y1 = c.ax, c.ay, c.hx, c.hy
	// Reading order: the anchor may be to the right of or BELOW the head (dragging backwards).
	if y0 > y1 || (y0 == y1 && x0 > x1) {
		x0, y0, x1, y1 = x1, y1, x0, y0
	}
	// CONFINED TO THE PANE THE DRAG STARTED IN — the operator's "even past the conversations window into
	// the conversation list". See clipRegion.
	if c.hasRegion {
		x0 = clampTo(x0, c.region.x0, c.region.x1)
		x1 = clampTo(x1, c.region.x0, c.region.x1)
		y0 = clampTo(y0, c.region.y0, c.region.y1)
		y1 = clampTo(y1, c.region.y0, c.region.y1)
	}
	if x0 == x1 && y0 == y1 {
		return 0, 0, 0, 0, false
	}
	return x0, y0, x1, y1, true
}

// rowExtent is the first and last cell of a row that carry CONTENT.
func (c *clipState) rowExtent(row int) (int, int) {
	lo, hi := 0, c.rowWidth(row)-1
	// THE REGION COMES FIRST, THEN THE BORDER STRIP — and the order is load-bearing, not stylistic.
	if c.hasRegion {
		lo = clampTo(lo, c.region.x0, c.region.x1)
		hi = clampTo(hi, c.region.x0, c.region.x1)
	}
	for lo < hi && c.isBorderCell(row, lo) {
		lo++
	}
	for hi > lo && c.isBorderCell(row, hi) {
		hi--
	}
	// AND THE HIGHLIGHT STOPS WHERE THE CONTENT STOPS.
	if last := c.lastNonBlank(row, lo, hi); last > lo {
		hi = last
	}
	return lo, hi
}

// lastNonBlank is the rightmost cell of a range that holds content, or -1 when the range is all blanks.
func (c *clipState) lastNonBlank(row, lo, hi int) int {
	if row < 0 || row >= len(c.frame) {
		return -1
	}
	for i := hi; i >= lo; i-- {
		if strings.TrimSpace(cellText(c.frame[row], i, i)) != "" {
			return i
		}
	}
	return -1
}

// isBorderCell reports whether the cell at (row, col) is a pane border character.
func (c *clipState) isBorderCell(row, col int) bool {
	if row < 0 || row >= len(c.frame) || col < 0 {
		return false
	}
	switch strings.TrimSpace(cellText(c.frame[row], col, col)) {
	case "│", "┃", "║":
		return true
	}
	return false
}

// span returns the cell range this row contributes to the selection, or ok=false when the row is
// outside it.
func (c *clipState) span(row, x0, y0, x1, y1 int) (int, int, bool) {
	if row < y0 || row > y1 {
		return 0, 0, false
	}
	lo, hi := c.rowExtent(row)
	var sx, ex int
	switch {
	case y0 == y1:
		sx, ex = clampTo(x0, lo, hi), clampTo(x1, lo, hi)
	case row == y0:
		sx, ex = clampTo(x0, lo, hi), hi
	case row == y1:
		sx, ex = lo, clampTo(x1, lo, hi)
	default:
		sx, ex = lo, hi
	}
	if sx > ex {
		return 0, 0, false
	}
	return sx, ex, true
}

// rowWidth is a row's cell width (the frame is padded, so rows are uniform; this is a guard).
func (c *clipState) rowWidth(row int) int {
	if row < 0 || row >= len(c.frame) {
		return 0
	}
	return ansi.StringWidth(c.frame[row])
}

// text returns the selected text: each row's span, trailing blanks trimmed (a selection that ends
// in a pane's padding must not carry that padding into the clipboard), joined with newlines.
func (c *clipState) text() string {
	x0, y0, x1, y1, ok := c.bounds()
	if !ok {
		return ""
	}
	var lines []string
	for row := y0; row <= y1; row++ {
		if row < 0 || row >= len(c.frame) {
			continue
		}
		sx, ex, in := c.span(row, x0, y0, x1, y1)
		if !in {
			continue
		}
		lines = append(lines, strings.TrimRight(cellText(c.frame[row], sx, ex), " "))
	}
	// Trailing blank rows are an artifact of a drag that ran past the text (the frame's padding),
	// not something the operator selected.
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

// highlight inverts the selected cells in the frame, so the drag is VISIBLE while it happens.
func (c *clipState) highlight(frame string) string {
	x0, y0, x1, y1, ok := c.bounds()
	if !ok {
		return frame
	}
	rows := strings.Split(frame, "\n")
	for row := y0; row <= y1 && row < len(rows); row++ {
		sx, ex, in := c.span(row, x0, y0, x1, y1)
		if !in {
			continue
		}
		rows[row] = highlightCells(rows[row], sx, ex)
	}
	return strings.Join(rows, "\n")
}

// decorate prepares the frame for display: the selection highlight while dragging, and the toast
// once something has been copied.
func (c *clipState) decorate(frame string) string {
	if c.dragging() {
		frame = c.highlight(frame)
	}
	if c.toast != "" {
		frame = toastBand(frame, c.toast)
	}
	return frame
}

// --- the cell walker ---------------------------------------------------------------------------

// cellText returns the text of cells [x0, x1] (inclusive) of a rendered line.
func cellText(line string, x0, x1 int) string {
	if x1 < x0 {
		return ""
	}
	var b strings.Builder
	col := 0
	state := byte(ansi.NormalState)
	for i := 0; i < len(line); {
		seq, width, n, newState := ansi.WcWidth.DecodeSequenceInString(line[i:], state, nil)
		state = newState
		i += n
		if n == 0 {
			break // defensive: a decoder that cannot advance would spin
		}
		if width == 0 {
			continue // an escape sequence, or a zero-width mark: no cell
		}
		// A wide rune straddling the selection's edge is INCLUDED: half a character cannot be
		// copied, and dropping it would silently lose text the operator dragged across.
		if col+width-1 >= x0 && col <= x1 {
			b.WriteString(seq)
		}
		col += width
		if col > x1 {
			break
		}
	}
	return b.String()
}

// highlightCells wraps cells [x0, x1] of a rendered line in reverse video, leaving the line's own
// styling intact around the span.
func highlightCells(line string, x0, x1 int) string {
	if x1 < x0 {
		return line
	}
	const on, off = "\x1b[7m", "\x1b[27m"
	var b strings.Builder
	col := 0
	state := byte(ansi.NormalState)
	opened := false
	closed := false
	for i := 0; i < len(line); {
		seq, width, n, newState := ansi.WcWidth.DecodeSequenceInString(line[i:], state, nil)
		state = newState
		i += n
		if n == 0 {
			break
		}
		if width == 0 {
			// Escape sequences pass through unchanged — but NOT the ones that reset attributes mid-span: a
			// style reset inside the highlight would turn the inverse video off for the rest of the selection.
			b.WriteString(seq)
			if opened && !closed && isFullReset(seq) {
				b.WriteString(on)
			}
			continue
		}
		if !opened && col+width-1 >= x0 {
			b.WriteString(on)
			opened = true
		}
		b.WriteString(seq)
		col += width
		if !closed && opened && col > x1 {
			b.WriteString(off)
			closed = true
		}
	}
	if opened && !closed {
		b.WriteString(off)
	}
	return b.String()
}

// isFullReset reports whether a sequence is a complete SGR reset (ESC[0m / ESC[m) — the only one
// that drops the reverse-video attribute along with everything else.
func isFullReset(seq string) bool {
	return seq == "\x1b[0m" || seq == "\x1b[m"
}

// toastBand shows the confirmation on the shell's BLANK SEPARATOR ROW.
func toastBand(frame, text string) string {
	rows := strings.Split(frame, "\n")
	if len(rows) == 0 {
		return frame
	}
	row := 2 // rows 0 (tab bar) and 1 (rule) precede it; see the layout in app.go
	if row >= len(rows) {
		row = len(rows) - 1
	}
	width := ansi.StringWidth(rows[row])
	rows[row] = toastRow("✓ "+text, width)
	return strings.Join(rows, "\n")
}

// toastRow builds the band as EXACTLY one row of `width` cells.
func toastRow(label string, width int) string {
	if width <= 0 {
		return ""
	}
	if ansi.StringWidth(label) > width {
		// Clipped with an ellipsis, so a narrow window shows that the message was cut rather than
		// showing a shorter one that reads as the whole thing.
		label = ansi.Truncate(label, width, "…")
	}
	pad := (width - ansi.StringWidth(label)) / 2
	right := width - pad - ansi.StringWidth(label)
	return theme.PanelBgStyle.Render(strings.Repeat(" ", pad) + label + strings.Repeat(" ", right))
}

// --- the shell-facing API ----------------------------------------------------------------------

// beginDrag anchors a selection at a cell.
func (c *clipState) beginDrag(x, y int) {
	c.drag = true
	c.ax, c.ay, c.hx, c.hy = x, y, x, y
}

// extend moves the head. consumed reports whether the motion belonged to the selection — once a
// drag is under way the motion is the operator selecting, not anything else's.
func (c *clipState) extend(x, y int) (consumed bool) {
	if !c.drag {
		return false
	}
	if x == c.hx && y == c.hy {
		return true
	}
	c.hx, c.hy = x, y
	return true
}

// finish ends the drag.
func (c *clipState) finish() (text string, copied bool) {
	defer func() { c.drag = false }()
	if !c.dragging() {
		return "", false
	}
	return c.text(), true
}

// copyCmd puts the selection on the clipboard and arms the toast.
func (c *clipState) copyCmd(text string) tea.Cmd {
	c.toast = clipToastText
	c.toastSeq++
	seq := c.toastSeq
	return tea.Batch(
		func() tea.Msg {
			// Written straight to the terminal: OSC 52 is a control sequence that paints
			// nothing, so it cannot disturb the frame the renderer is managing.
			fmt.Fprint(os.Stdout, ansi.SetSystemClipboard(text))
			return nil
		},
		tea.Tick(clipToastDuration, func(time.Time) tea.Msg { return clipToastMsg{seq: seq} }),
	)
}

// clearToast drops the toast, but only if its timer still belongs to the current copy.
func (c *clipState) clearToast(seq int) {
	if seq == c.toastSeq {
		c.toast = ""
	}
}

// handleMouse routes a mouse event through the selection.
func (c *clipState) handleMouse(m tea.MouseMsg) (consumed bool, copyText string) {
	switch m.Action {
	case tea.MouseActionPress:
		if m.Button == tea.MouseButtonLeft {
			c.beginDrag(m.X, m.Y)
		}
		return false, ""
	case tea.MouseActionMotion:
		// Only a left-button drag selects. Motion is delivered only while a button is held
		// (tea.WithMouseCellMotion = button-event reporting), so this IS the drag.
		if m.Button == tea.MouseButtonLeft || m.Button == tea.MouseButtonNone {
			return c.extend(m.X, m.Y), ""
		}
		return false, ""
	case tea.MouseActionRelease:
		text, copied := c.finish()
		if !copied {
			return false, ""
		}
		return true, text
	}
	return false, ""
}
