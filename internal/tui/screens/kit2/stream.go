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
}

// NewStream builds a sized stream.
func NewStream(title string, w, h int) *Stream {
	return &Stream{Title: title, Width: w, Height: h}
}

// SetSize resizes the stream viewport.
func (s *Stream) SetSize(w, h int) { s.Width, s.Height = w, h; s.clamp() }

// Append adds lines. The offset is preserved unless the stream was already
// pinned to the bottom (the auto-follow rule).
func (s *Stream) Append(lines ...string) {
	follow := s.AtBottom()
	s.Lines = append(s.Lines, lines...)
	if follow {
		s.ScrollToBottom()
	} else {
		s.clamp()
	}
}

// SetLines replaces the transcript (e.g. a reload) — always re-pins to the
// bottom, since there is no prior offset to preserve.
func (s *Stream) SetLines(lines []string) {
	s.Lines = append([]string{}, lines...)
	s.ScrollToBottom()
}

// ReplaceLines swaps the content WITHOUT moving the operator's view, unless they were already at the
// bottom — in which case it follows the tail, as every other update here does.
//
// WHY IT EXISTS: SetLines re-pins unconditionally, which is right for a reload (there is no continuity
// to preserve) and WRONG for content that GROWS IN PLACE. A streaming reply changes its last line on
// every update instead of appending a new one, so the renderer cannot treat it as a prefix extension —
// and with SetLines that made every update yank the view to the bottom. During a live turn the durable
// poll updates once a second, so an operator who scrolled up to re-read something would be dragged back
// down before they could finish the sentence.
//
// The offset is CLAMPED rather than preserved verbatim: content that grew SHORTER (a collapsed reasoning
// block, a re-grouped phase) can leave the old offset past the end, and an out-of-range offset renders an
// empty window — see Visible(), which returns nil for it.
func (s *Stream) ReplaceLines(lines []string) {
	wasBottom := s.AtBottom()
	s.Lines = append([]string{}, lines...)
	if wasBottom {
		s.ScrollToBottom()
		return
	}
	s.clamp()
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
//
// A plain field assignment is not enough: the notice takes a row from the body, which moves maxOffset,
// and an offset left where it was then hides the NEWEST line — the same class of bug the notice itself
// just fixed. Measured: with 20 lines in a 5-row stream, assigning .Notice directly left the window at
// [15,19), dropped line 19, and made AtBottom() false so the next chunk would not have been followed
// either.
func (s *Stream) SetNotice(n string) {
	wasBottom := s.AtBottom()
	s.Notice = n
	if wasBottom {
		s.ScrollToBottom()
	}
	s.clamp()
}

// bodyRows is the rows available to the TRANSCRIPT, which is innerH minus the notice row when a
// notice is showing.
//
// THE NOTICE TAKES ITS ROW FROM THE BODY, and that is the fix for the operator's "I still don't see
// the 'Orchicon is thinking...' being printed". The notice used to be appended AFTER innerH rows of
// transcript, so the host's height budget clipped it away whenever the transcript filled the pane —
// the notice was visible only while the transcript was SHORTER than the pane, which is exactly when
// it does not matter. The same bug hid "reconnecting…".
//
// Every offset calculation goes through this, so the tail stays pinned WITH the notice taking a row:
// AtBottom/ScrollToBottom/maxOffset would otherwise allow an offset that pushes the newest line out
// of the window.
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

// ScrollToBottom pins the viewport to the newest line.
func (s *Stream) ScrollToBottom() { s.Offset = s.maxOffset() }

// Wheel scrolls by delta lines (positive = down).
func (s *Stream) Wheel(delta int) { s.Offset += delta; s.clamp() }

func (s *Stream) clamp() {
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
//
// TRUNCATION LOSES TEXT, and loss is never acceptable for a transcript. Pad() shortens to Width, so any line
// that arrives too wide — a stale width after a reflow, a host that measured differently, a caller with its
// own arithmetic — silently drops its tail. The operator reads that as a sentence cut mid-word ("Let me find
// the existi") with no cue that anything is missing.
//
// WRAPPING IS THE SAFE DEGRADATION: the line is shown in full, on as many rows as it needs. A caller whose
// layout is already correct sees no difference; one that is wrong shows readable text instead of a lie.
func (s *Stream) WrapOverflow() { s.wrapOverflow = true }

// wrapLine splits an over-wide line into the rows it needs, on CELL boundaries and never inside an ANSI
// sequence.
//
// IT CUTS BY SCANNING, NOT BY RE-SLICING A TRUNCATING CALL. The obvious implementation —
// `cut := ansi.Truncate(rest, width, "")` then `rest = rest[len(cut):]` — is WRONG, and measurably so: the
// truncating call RE-OPENS the styling sequences that span the cut, so its output is not a prefix of the
// input and the slice skips into the middle of the text. Measured: 3 of 6 repetitions of a styled run
// survived the wrap. Scanning to the cut index and slicing the ORIGINAL string cannot have that problem,
// because the suffix is the untouched remainder.
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

// cellCutIndex returns the BYTE index at which to split s so the prefix is at most w display cells wide,
// visiting whole escape sequences and whole runes only.
//
// An escape sequence consumes no cells but must not be split, so it is skipped whole; a rune is measured in
// cells (a wide glyph counts two). Returns len(s) when the whole string already fits, and 0 only when the
// very first thing does not — which the caller treats as "give up, do not loop".
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
	s.clamp()
	var b strings.Builder
	rows := s.bodyRows()
	vis := s.Visible()
	if s.wrapOverflow {
		// Expand the window into the rows the lines actually need.
		//
		// IT KEEPS THE TAIL. Wrapping turns one line into several, so an expanded window holds MORE rows than
		// will fit and something must be dropped — and what is dropped must be the OLDEST, because the
		// transcript follows the newest line. Dropping the newest would make the safety net worse than the
		// truncation it replaced: the operator would lose sight of the live reply.
		expanded := make([]string, 0, len(vis))
		for _, l := range vis {
			expanded = append(expanded, wrapLine(l, s.Width)...)
		}
		if len(expanded) > rows {
			expanded = expanded[len(expanded)-rows:]
		}
		vis = expanded
	}
	for i := 0; i < rows; i++ {
		line := ""
		if i < len(vis) {
			line = vis[i]
		}
		b.WriteString(Pad(line, s.Width))
		b.WriteString("\n")
	}
	if s.Notice != "" {
		// INSIDE the row budget, not appended past it — see bodyRows.
		b.WriteString(Pad(theme.HintText.Render(s.Notice), s.Width))
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// Overflowing reports whether the stream's content is taller than the rows it
// can show, i.e. whether scrolling is meaningful at all. Callers use it to
// decide whether to surface the scroll position: with no overflow there is
// nothing hidden, and a scroll affordance would be noise.
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
