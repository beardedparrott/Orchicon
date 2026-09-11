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

// maxOffset is the largest valid top-line index.
func (s *Stream) maxOffset() int {
	m := len(s.Lines) - s.innerH()
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
	end := s.Offset + s.innerH()
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
	var b strings.Builder
	vis := s.Visible()
	for i := 0; i < s.innerH(); i++ {
		line := ""
		if i < len(vis) {
			line = vis[i]
		}
		b.WriteString(Pad(line, s.Width))
		b.WriteString("\n")
	}
	if s.Notice != "" {
		b.WriteString(theme.HintText.Render(s.Notice))
	}
	return strings.TrimSuffix(b.String(), "\n")
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
