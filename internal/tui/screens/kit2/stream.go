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
	// follow is the AUTO-FOLLOW INTENT: true while the view is pinned to the newest line, false once the
	// operator has deliberately scrolled away from it.
	//
	// IT IS SEPARATE FROM Offset BECAUSE AtBottom() IS POSITIONAL, and a position changes underneath the
	// operator without them doing anything: this stream is re-sized on every wake from the pane's body
	// height, which moves with the dock's own row count. A pane that gains a row — or content that gains
	// one — leaves a FOLLOWING view one row short of the bottom, so AtBottom() answers false and the view
	// silently stops following. That is the operator's report: "I also had to manually scroll down to see
	// that streaming was happening. After I did that once it was fine" — the scroll re-pinned it and
	// nothing else could, because every auto-follow rule in this file re-derived the intent from a
	// position the resize had just invalidated.
	//
	// Following is an INTENT, so it is remembered rather than re-derived.
	follow bool
	// rowLine maps each PAINTED body row to the LOGICAL line index it shows, built by View.
	//
	// It exists so a CLICK can be resolved to content. A row is not a line: wrapOverflow expands an
	// over-wide line into several rows and drops the oldest when they exceed the budget, so
	// `row + Offset` is wrong whenever wrapping or tail-trimming happened. The map is recorded during the
	// render that produced the rows the operator is looking at, which is the only moment the two are
	// guaranteed to agree.
	//
	// nil until the first View, and -1 for a row with nothing behind it (padding past the content).
	rowLine []int
}

// NewStream builds a sized stream.
func NewStream(title string, w, h int) *Stream {
	// follow starts TRUE: a stream the operator has not touched shows its newest content, which is the
	// whole point of a live transcript.
	return &Stream{Title: title, Width: w, Height: h, follow: true}
}

// SetSize resizes the stream viewport, KEEPING A FOLLOWING VIEW PINNED.
//
// This is where the intent must be RE-APPLIED rather than re-derived: the height change moves the
// bottom, so a view that was following would otherwise end up a row short of it and stop.
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
//
// A plain field assignment is not enough: the notice takes a row from the body, which moves maxOffset,
// and an offset left where it was then hides the NEWEST line — the same class of bug the notice itself
// just fixed. Measured: with 20 lines in a 5-row stream, assigning .Notice directly left the window at
// [15,19), dropped line 19, and made AtBottom() false so the next chunk would not have been followed
// either.
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

// Following reports whether the view is pinned to the newest line — the INTENT, not the current
// position. See the follow field for why the two differ.
func (s *Stream) Following() bool { return s.follow }

// ScrollToBottom pins the viewport to the newest line AND starts following it.
func (s *Stream) ScrollToBottom() { s.Offset = s.maxOffset(); s.follow = true }

// Wheel scrolls by delta lines (positive = down).
//
// A wheel gesture is DELIBERATE, so this is the one place that changes the follow intent: scrolling up
// away from the newest line stops the auto-follow, and coming back to the bottom resumes it.
func (s *Stream) Wheel(delta int) {
	s.Offset += delta
	s.clampOffset()
	s.follow = s.Offset >= s.maxOffset()
}

// clampOffset keeps the offset within the content WITHOUT touching the follow intent — because the
// reason for a clamp is almost always that the CONTENT or the WINDOW changed size, not that the operator
// moved. Clearing follow there is the bug this field exists to prevent.
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
		//
		// IT KEEPS THE TAIL. Wrapping turns one line into several, so an expanded window holds MORE rows than
		// will fit and something must be dropped — and what is dropped must be the OLDEST, because the
		// transcript follows the newest line. Dropping the newest would make the safety net worse than the
		// truncation it replaced: the operator would lose sight of the live reply.
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
		// PAINT FROM ROW 0. Only the TRIM above drops rows, and it drops from the HEAD; a short transcript
		// therefore starts at the top exactly as the non-wrapped path does. An earlier version of this branch
		// computed a `start` offset and bottom-aligned short content — a rendering change nobody asked for,
		// caught by the geometry probe when its markers moved a dozen rows down the pane.
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
// index it shows, or -1 when the row holds nothing (padding past the content, or before the first
// render).
//
// This is the click geometry. It is answered from the LAST render rather than recomputed, because a
// row is not a line — see the rowLine field — and only the render that painted the operator's screen
// knows which line each row came from.
func (s *Stream) LineAtRow(row int) int {
	if row < 0 || row >= len(s.rowLine) {
		return -1
	}
	return s.rowLine[row]
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
