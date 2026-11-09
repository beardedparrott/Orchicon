package kit2

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"

	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// Stream is a first-class scrolling transcript/log (Ask turns, execution
// sessions, activity feeds). Two invariants define it:
//
//  1. Appending NEVER yanks the operator's scroll offset: a new line only
//     moves the viewport when the operator was already pinned to the bottom.
//  2. Auto-follow happens ONLY when already at the bottom.
//
// That is the exact behaviour the old list panes lacked (they repainted
// from a fetch, so a background append both jumped the pane and lost the
// reader's place).
type Stream struct {
	Lines  []string
	Offset int // index of the top visible line
	Width  int
	Height int
	// Title is the panel title when the stream is rendered inside a Panel.
	Title string
	// Notice is a transient status line ("reconnecting…").
	Notice string
	// wrapOverflow makes View WRAP an over-wide line rather than truncating it. See WrapOverflow.
	wrapOverflow bool
	// follow is the AUTO-FOLLOW INTENT: true while the view is pinned to the newest line, false once
	// the operator has deliberately scrolled away from it.
	follow bool
	// rowLine maps each PAINTED body row to the LOGICAL line index it shows, built by View.
	rowLine []int
}

// NewStream builds a sized stream.
func NewStream(title string, w, h int) *Stream {
	// follow starts TRUE: a stream the operator has not touched shows its newest content, which is the
	// whole point of a live transcript.
	return &Stream{Title: title, Width: w, Height: h, follow: true}
}

// SetSize resizes the stream viewport, KEEPING A FOLLOWING VIEW PINNED.
func (s *Stream) SetSize(w, h int) {
	s.Width, s.Height = w, h
	if s.follow {
		s.ScrollToBottom()
		return
	}
	s.clampOffset()
}

// Append adds lines. The offset is preserved unless the stream is FOLLOWING (the auto-follow rule).
func (s *Stream) Append(lines ...string) {
	s.Lines = append(s.Lines, lines...)
	if s.follow {
		s.ScrollToBottom()
		return
	}
	s.clampOffset()
}

// SetLines replaces the transcript (e.g. a reload) — always re-pins to the
// bottom, since there is no prior offset to preserve.
func (s *Stream) SetLines(lines []string) {
	s.Lines = append([]string{}, lines...)
	s.ScrollToBottom()
}

// ReplaceLines swaps the content WITHOUT moving the operator's view, unless they were already at
// the bottom — in which case it follows the tail, as every other update here does.
func (s *Stream) ReplaceLines(lines []string) {
	s.Lines = append([]string{}, lines...)
	if s.follow {
		s.ScrollToBottom()
		return
	}
	s.clampOffset()
}

// innerH is the visible line count.
func (s *Stream) innerH() int {
	h := s.Height
	if h <= 0 {
		h = 1
	}
	return h
}

// SetNotice sets the transient status line, KEEPING THE VIEW PINNED if it was pinned.
func (s *Stream) SetNotice(n string) {
	s.Notice = n
	if s.follow {
		s.ScrollToBottom()
		return
	}
	s.clampOffset()
}

// bodyRows is the rows available to the TRANSCRIPT, which is innerH minus the notice row when a
// notice is showing.
func (s *Stream) bodyRows() int {
	rows := s.innerH()
	if s.Notice != "" {
		rows--
		if rows < 1 {
			rows = 1
		}
	}
	return rows
}

// maxOffset is the largest valid top-line index.
func (s *Stream) maxOffset() int {
	m := len(s.Lines) - s.bodyRows()
	if m < 0 {
		m = 0
	}
	return m
}

// AtBottom reports whether the viewport is pinned to the last line.
func (s *Stream) AtBottom() bool { return s.Offset >= s.maxOffset() }

// Following reports whether the view is pinned to the newest line — the INTENT, not the current
// position. See the follow field for why the two differ.
func (s *Stream) Following() bool { return s.follow }

// ScrollToBottom pins the viewport to the newest line AND starts following it.
func (s *Stream) ScrollToBottom() { s.Offset = s.maxOffset(); s.follow = true }

// ScrollToTop pins the viewport to the FIRST line and STOPS following.
func (s *Stream) ScrollToTop() { s.Offset = 0; s.follow = false }

// Wheel scrolls by delta lines (positive = down).
func (s *Stream) Wheel(delta int) {
	s.Offset += delta
	s.clampOffset()
	s.follow = s.Offset >= s.maxOffset()
}

// clampOffset keeps the offset within the content WITHOUT touching the follow intent — because the
// reason for a clamp is almost always that the CONTENT or the WINDOW changed size.
func (s *Stream) clampOffset() {
	if s.Offset > s.maxOffset() {
		s.Offset = s.maxOffset()
	}
	if s.Offset < 0 {
		s.Offset = 0
	}
}

// Visible returns the currently visible lines.
func (s *Stream) Visible() []string {
	end := s.Offset + s.bodyRows()
	if end > len(s.Lines) {
		end = len(s.Lines)
	}
	if s.Offset > len(s.Lines) {
		return nil
	}
	return s.Lines[s.Offset:end]
}

// WrapOverflow makes View WRAP a line that is wider than the stream rather than truncating it.
func (s *Stream) WrapOverflow() { s.wrapOverflow = true }

// wrapLine splits an over-wide line into the rows it needs, on CELL boundaries and never inside an
// ANSI sequence.
func wrapLine(line string, width int) []string {
	if width <= 0 || lipgloss.Width(line) <= width {
		return []string{line}
	}
	var rows []string
	rest := line
	for len(rest) > 0 {
		if lipgloss.Width(rest) <= width {
			rows = append(rows, rest)
			break
		}
		idx := cellCutIndex(rest, width)
		if idx <= 0 {
			// No forward progress: a zero-width glyph or a malformed escape the measurer disagrees about.
			// Emit what is left and stop rather than spin forever.
			rows = append(rows, rest)
			break
		}
		rows = append(rows, rest[:idx])
		rest = rest[idx:]
	}
	return rows
}

// cellCutIndex returns the BYTE index at which to split s so the prefix is at most w display cells
// wide, visiting whole escape sequences and whole runes only.
func cellCutIndex(s string, w int) int {
	cells := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			i = skipEscape(s, i)
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if rw := lipgloss.Width(string(r)); rw > 0 {
			if cells+rw > w {
				return i
			}
			cells += rw
		}
		i += size
	}
	return len(s)
}

// skipEscape returns the index just past the escape sequence starting at i (\x1b[...final, or a bare
// two-byte escape), so a cut can never land inside one and leak styling into the following rows.
func skipEscape(s string, i int) int {
	j := i + 1
	if j < len(s) && s[j] == '[' {
		j++
		for j < len(s) && (s[j] < '@' || s[j] > '~') {
			j++
		}
		if j < len(s) {
			j++ // the final byte
		}
		return j
	}
	if j < len(s) {
		j++
	}
	return j
}

// View renders the stream body (no border) — the caller wraps it in a
// Panel or emits it directly.
func (s *Stream) View() string {
	// Clamp first, so a caller that assigned Notice directly (bypassing SetNotice) still cannot draw a
	// window that starts past the last page.
	s.clampOffset()
	var b strings.Builder
	rows := s.bodyRows()
	vis := s.Visible()
	// Row → logical line, rebuilt alongside the rows so the two cannot disagree.
	rowLine := make([]int, rows)
	for i := range rowLine {
		rowLine[i] = -1
	}
	if s.wrapOverflow {
		// Expand the window into the rows the lines actually need.
		type expRow struct {
			text string
			line int
		}
		expanded := make([]expRow, 0, len(vis))
		for i, l := range vis {
			for _, w := range wrapLine(l, s.Width) {
				expanded = append(expanded, expRow{text: w, line: s.Offset + i})
			}
		}
		if len(expanded) > rows {
			expanded = expanded[len(expanded)-rows:]
		}
		// PAINT FROM ROW 0.
		for i := 0; i < rows; i++ {
			line := ""
			if i < len(expanded) {
				line = expanded[i].text
				rowLine[i] = expanded[i].line
			}
			b.WriteString(Pad(line, s.Width))
			b.WriteString("\n")
		}
		s.rowLine = rowLine
		if s.Notice != "" {
			b.WriteString(Pad(theme.HintText.Render(s.Notice), s.Width))
		}
		return strings.TrimSuffix(b.String(), "\n")
	}
	for i := 0; i < rows; i++ {
		line := ""
		if i < len(vis) {
			line = vis[i]
			rowLine[i] = s.Offset + i
		}
		b.WriteString(Pad(line, s.Width))
		b.WriteString("\n")
	}
	s.rowLine = rowLine
	if s.Notice != "" {
		// INSIDE the row budget, not appended past it — see bodyRows.
		b.WriteString(Pad(theme.HintText.Render(s.Notice), s.Width))
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// LineAtRow resolves a PAINTED body row (0-based, from the top of the body) to the logical line
// index it shows, or -1 when the row holds nothing (padding past the content.
func (s *Stream) LineAtRow(row int) int {
	if row < 0 || row >= len(s.rowLine) {
		return -1
	}
	return s.rowLine[row]
}

// Overflowing reports whether the stream's content is taller than the rows it can show, i.e.
// whether scrolling is meaningful at all.
func (s *Stream) Overflowing() bool {
	return len(s.Lines) > s.bodyRows()
}

// ScrollLabel renders a "n-m/total" indicator (and a follow marker).
func (s *Stream) ScrollLabel() string {
	top := s.Offset + 1
	end := s.Offset + s.innerH()
	if end > len(s.Lines) {
		end = len(s.Lines)
	}
	if len(s.Lines) == 0 {
		top = 0
	}
	mark := "↑ scrollback"
	if s.AtBottom() {
		mark = "▼ following"
	}
	return fmt.Sprintf("%d-%d/%d · %s", top, end, len(s.Lines), mark)
}
