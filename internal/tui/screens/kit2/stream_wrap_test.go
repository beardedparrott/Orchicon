package kit2

// stream_wrap_test.go — AN OVER-WIDE LINE IS WRAPPED, NOT TRUNCATED.
//
// The operator, on a live transcript: the text was "scrunch[ed] ... into one block" with sentences cut
// mid-word. A cut sentence is the visible end of a silent loss: the stream's View pads every line to its
// width with ansi.Truncate, so a line that arrives WIDER than the stream loses its tail — no marker, no
// wrap, nothing to tell the operator that words are missing.
//
// A caller whose own layout is right never hits this. The point of wrapping is that a caller whose layout is
// WRONG (a stale width after a reflow, a host that measured differently) shows readable text instead of a
// lie — and the cost is only that the window holds a row or two fewer.

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// THE DEFAULT STILL TRUNCATES, so this is a decision the host makes rather than a silent change of behaviour
// for every other stream in the TUI (the build log, the step flow).
func TestStreamTruncatesByDefault(t *testing.T) {
	s := NewStream("t", 20, 3)
	s.Lines = []string{strings.Repeat("x", 60)}
	out := s.View()
	if strings.Count(out, "x") != 20 {
		t.Errorf("the default should truncate to the width; got %d x's", strings.Count(out, "x"))
	}
}

// WITH WrapOverflow, NO TEXT IS LOST. The long line becomes as many rows as it needs.
func TestStreamWrapsOverflowWhenAsked(t *testing.T) {
	s := NewStream("t", 20, 5)
	s.WrapOverflow()
	s.Lines = []string{strings.Repeat("x", 60)}

	out := s.View()
	if n := strings.Count(out, "x"); n != 60 {
		t.Fatalf("a wrapped stream kept %d of 60 characters — text was still lost:\n%s", n, out)
	}
	// Every painted row still fits the width, so wrapping cannot overflow the pane either.
	for _, ln := range strings.Split(out, "\n") {
		if w := lipgloss.Width(ln); w > 20 {
			t.Errorf("a wrapped row is %d cells wide, want <= 20: %q", w, ln)
		}
	}
}

// A WRAPPED LINE CONSUMES ROWS, so the window shows fewer LINES — the honest trade, and the reason this is a
// host opt-in rather than the default.
func TestWrappingCostsRowsNotCharacters(t *testing.T) {
	s := NewStream("t", 10, 3)
	s.WrapOverflow()
	s.Lines = []string{"abcdefghij", strings.Repeat("y", 25), "tail"}

	out := s.View()
	// 1 + 3 + 1 = 5 rows of content in a 3-row window: the window shows the newest, so "tail" is present.
	if !strings.Contains(out, "tail") {
		t.Errorf("the newest line fell out of a wrapped window:\n%s", out)
	}
	// And nothing was cut: every character of the long line is in the expanded window before the first 3
	// rows are drawn — asserted by requiring the full 25 y's somewhere in the wrap of that line.
	full := wrapLine(strings.Repeat("y", 25), 10)
	if strings.Join(full, "") != strings.Repeat("y", 25) {
		t.Errorf("wrapLine lost characters: %q", full)
	}
}

// wrapLine SPLITS IN DISPLAY CELLS, NOT RUNES, and it must not cut INSIDE an ANSI sequence — the transcript
// carries styled spans, and a sequence cut in half leaks its styling into every following row.
func TestWrapLineIsAnsiAware(t *testing.T) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	styled := "\x1b[1m" + strings.Repeat("bold words ", 6) + "\x1b[22m"

	rows := wrapLine(styled, 20)
	if len(rows) < 2 {
		t.Fatalf("an over-wide styled line did not wrap: %d rows", len(rows))
	}
	joined := strings.Join(rows, "")
	// Every character survives, and the bold markers survive too (they are re-opened per row).
	if got := strings.Count(joined, "bold words"); got != 6 {
		t.Errorf("wrapping lost text: %d of 6 repetitions survived", got)
	}
	for i, r := range rows {
		if w := lipgloss.Width(r); w > 20 {
			t.Errorf("row %d is %d cells, want <= 20", i, w)
		}
	}
	// A line that already fits is returned untouched — no gratuitous re-styling.
	short := "\x1b[31mred\x1b[39m"
	if got := wrapLine(short, 40); len(got) != 1 || got[0] != short {
		t.Errorf("a fitting line was altered: %q", got)
	}
}

// A DEGENERATE WIDTH CANNOT SPIN. A zero-width or misleading measurer must terminate, not loop: a hang here
// would freeze the whole UI.
func TestWrapLineTerminatesOnADegenerateWidth(t *testing.T) {
	done := make(chan []string, 1)
	go func() { done <- wrapLine(strings.Repeat("z", 100), 0) }()
	select {
	case rows := <-done:
		if len(rows) != 1 {
			t.Errorf("width 0 should return the line unwrapped, got %d rows", len(rows))
		}
	case <-timeAfterProbe():
		t.Fatal("wrapLine did not terminate on width 0")
	}
}

// timeAfterProbe bounds the termination check without importing time into the assertion above.
func timeAfterProbe() <-chan time.Time { return time.After(2 * time.Second) }
