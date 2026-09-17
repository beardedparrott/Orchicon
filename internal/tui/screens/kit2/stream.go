package kit2

import (
	"fmt"
	"strings"

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

// View renders the stream body (no border) — the caller wraps it in a
// Panel or emits it directly.
func (s *Stream) View() string {
	// Clamp first, so a caller that assigned Notice directly (bypassing SetNotice) still cannot draw a
	// window that starts past the last page.
	s.clamp()
	var b strings.Builder
	rows := s.bodyRows()
	vis := s.Visible()
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
