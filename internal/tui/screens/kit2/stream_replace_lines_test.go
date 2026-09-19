package kit2

// stream_replace_lines_test.go — CONTENT THAT GROWS IN PLACE MUST NOT MOVE THE VIEW.
//
// A streaming reply changes its last line on every update instead of appending a new one, so the
// transcript renderer cannot treat it as a prefix extension and reaches for a replace. SetLines
// re-pins to the bottom unconditionally — correct for a reload, wrong here: with the durable poll
// updating once a second, an operator who scrolled up to re-read something would be dragged back down
// before finishing the sentence.
//
// The two behaviours are asserted together because the fix must not cost the one that matters most: an
// operator FOLLOWING a reply still has to be carried along with it.

import (
	"strings"
	"testing"
)

func streamOf(lines []string, w, h int) *Stream {
	s := NewStream("t", w, h)
	s.SetLines(lines)
	return s
}

// A VIEW THAT WAS AT THE BOTTOM STAYS AT THE BOTTOM, following the growing reply.
func TestReplaceLinesFollowsTheTailWhenAtTheBottom(t *testing.T) {
	s := streamOf([]string{"a", "b", "c", "d", "e"}, 20, 3)
	if !s.AtBottom() {
		t.Fatal("fixture: a freshly set stream should be at the bottom")
	}
	if len(s.Lines) != 5 {
		t.Fatalf("fixture: %d lines, want 5", len(s.Lines))
	}
	// The reply grows: the last line gets longer, which is a replace rather than an append.
	s.ReplaceLines([]string{"a", "b", "c", "d", "e and then some more"})

	vis := s.Visible()
	if len(vis) != 3 {
		t.Fatalf("visible rows = %d, want 3", len(vis))
	}
	if !strings.Contains(vis[len(vis)-1], "e and then some more") {
		t.Errorf("the newest line is not on screen after a replace: %v", vis)
	}
	if !s.AtBottom() {
		t.Error("the view is no longer at the bottom, so the next update would not be followed either")
	}
}

// A VIEW THAT WAS SCROLLED UP KEEPS ITS PLACE — the whole point of the method.
func TestReplaceLinesKeepsThePlaceOfAScrolledView(t *testing.T) {
	lines := make([]string, 0, 30)
	for i := 0; i < 30; i++ {
		lines = append(lines, "line "+string(rune('a'+i%26)))
	}
	s := streamOf(lines, 20, 5)

	// The operator scrolls up to read something.
	s.Wheel(-10)
	if s.AtBottom() {
		t.Fatal("fixture: the view did not scroll")
	}
	before := s.Offset
	beforeTop := s.Visible()[0]

	// The reply at the bottom grows in place, several times (as the poll does).
	for i := 0; i < 3; i++ {
		next := append([]string{}, lines...)
		next[len(next)-1] = "line z grown " + strings.Repeat(".", i+1)
		s.ReplaceLines(next)
	}

	if s.Offset != before {
		t.Errorf("offset moved %d -> %d across replaces — the operator is dragged back to the tail while "+
			"reading earlier content", before, s.Offset)
	}
	if got := s.Visible()[0]; got != beforeTop {
		t.Errorf("the top visible line changed from %q to %q — the view scrolled under the operator", beforeTop, got)
	}
}

// AND A SHRINKING REPLACE CLAMPS INSTEAD OF BLANKING.
//
// Content can get SHORTER between updates (a collapsed reasoning block, a re-grouped phase). An offset
// left past the end renders an empty window, so the clamp is not decoration.
func TestReplaceLinesClampsWhenTheContentShrinks(t *testing.T) {
	lines := make([]string, 30)
	for i := range lines {
		lines[i] = "line"
	}
	s := streamOf(lines, 20, 5)
	// Part way into the content — NOT to the very top, or the clamp would be trivially satisfied by an
	// offset that was already zero.
	s.Wheel(-10)
	if s.Offset == 0 {
		t.Fatal("fixture: the view did not scroll")
	}

	s.ReplaceLines([]string{"only", "two"})
	if s.Visible() == nil {
		t.Fatal("the window is empty after the content shrank — the offset was left past the end")
	}
	if s.Offset > s.maxOffset() {
		t.Errorf("offset %d is past the maximum %d", s.Offset, s.maxOffset())
	}
}

// A RESIZE MUST NOT STOP THE FOLLOW — THE OPERATOR'S "I had to scroll down once".
//
// AtBottom() is POSITIONAL, so it answers false for reasons that have nothing to do with intent: this
// stream is re-sized on every wake from the pane's body height, which moves with the dock's own row count.
// A pane that gains a row leaves a FOLLOWING view one row short of the bottom, and every auto-follow rule
// that re-derived its answer from that position then stopped following — permanently, because SetNotice
// only re-pinned when already at the bottom. The operator scrolled once to recover, which is the tell:
// scrolling was the only thing that could restore the pin.
//
// Following is now an INTENT, remembered across content and window changes.
func TestAResizeDoesNotStopTheFollow(t *testing.T) {
	lines := make([]string, 40)
	for i := range lines {
		lines[i] = "line"
	}
	s := streamOf(lines, 30, 10)
	if !s.Following() {
		t.Fatal("fixture: a fresh stream should be following")
	}

	// The window GROWS a row (the dock shrank, the notice went away, a field row changed) — ordinary
	// during a live turn.
	s.SetSize(30, 11)
	if !s.Following() {
		t.Error("growing the window stopped the follow — the view is a row short of a bottom that moved " +
			"under it, which is not a decision the operator made")
	}
	// And SHRINKS, the other direction.
	s.SetSize(30, 9)
	if !s.Following() {
		t.Error("shrinking the window stopped the follow")
	}
	// A replace after a resize still follows, which is what the operator was missing.
	s.ReplaceLines(append(append([]string{}, lines...), "the newest line"))
	if !s.AtBottom() {
		t.Error("the newest line is off screen after a resize plus a replace — this is the \"I had to " +
			"manually scroll down to see that streaming was happening\"")
	}
}

// AND A DELIBERATE SCROLL STILL STOPS IT — the fix must not make the view impossible to hold still
// while reading earlier content.
func TestADeliberateScrollStillStopsTheFollow(t *testing.T) {
	lines := make([]string, 40)
	for i := range lines {
		lines[i] = "line"
	}
	s := streamOf(lines, 30, 10)

	s.Wheel(-5) // the operator scrolls up
	if s.Following() {
		t.Fatal("scrolling up did not stop the follow — new content would drag the operator back down")
	}
	// Content arriving does not move it.
	s.ReplaceLines(append(append([]string{}, lines...), "more"))
	if s.AtBottom() {
		t.Error("a replace re-pinned a view the operator had deliberately scrolled away from")
	}
	// And scrolling back to the bottom resumes the follow, so the way back is not a special gesture.
	s.Wheel(1000)
	if !s.Following() {
		t.Error("scrolling to the bottom did not resume the follow")
	}
}
