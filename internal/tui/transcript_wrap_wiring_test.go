package tui

// transcript_wrap_wiring_test.go — THE WIRING, WHICH NOTHING ELSE COVERED.
//
// kit2's stream_wrap_test.go proves the stream CAN wrap an over-wide line — but
// it calls WrapOverflow() itself, so it proves nothing about whether anything in
// production ever does. The transcript is the caller that matters, and its
// opt-in was a bare line inside a large paint method.
//
// That made the fix unguarded in the exact way it arrived: delete
// WrapOverflow() from the builder and every test in the repo still passes, while
// the operator's "sentences cut mid-word" bug comes back silently — no error, no
// failing test, just text quietly missing from the end of long lines.

import (
	"strings"
	"testing"
)

// THE TRANSCRIPT STREAM WRAPS, so an over-wide line is shown in full rather than
// losing its tail at the pane edge.
func TestTheTranscriptStreamWrapsRatherThanLosingText(t *testing.T) {
	m := newTestApp()
	const w = 20

	// The STREAM BUILDER the transcript actually uses — not kit2.NewStream, so
	// this asserts the wiring rather than the wrapping algorithm.
	str := m.newTranscriptStream("conv1", w, 40)

	// A line wider than the pane: what a stale width after a reflow, or a host
	// that measured its pane differently, produces in practice.
	const n = 60
	str.SetLines([]string{strings.Repeat("x", n)})
	out := str.View()

	if got := strings.Count(out, "x"); got != n {
		t.Fatalf("the transcript stream kept %d of %d characters, so the line was TRUNCATED at the pane edge. "+
			"A truncated transcript loses the end of every long line with nothing on screen to say words are "+
			"missing — the operator reads a sentence cut mid-word and has no cue that it happened. The stream "+
			"built for the transcript must wrap: newTranscriptStream → kit2.Stream.WrapOverflow().\n%s", got, n, out)
	}
}
