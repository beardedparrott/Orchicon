package kit2

import (
	"fmt"
	"strings"
	"testing"
)

// A NOTICE FITS INSIDE THE ROW BUDGET.
//
// The operator: "I still don't see the 'Orchicon is thinking...' being printed when the model is
// responding." The notice was appended AFTER innerH rows of transcript, so the host's height budget
// clipped it away whenever the transcript filled the pane — it showed only while the transcript was
// SHORTER than the pane, which is exactly when it does not matter. The same bug hid "reconnecting…".
//
// The notice is installed through SetNotice, which is the API EVERY caller uses (app.go's chat repaint:
// the dock's own notice writes are a different widget). A direct field assignment is deliberately NOT
// exercised here: the widget has no memory of whether the operator was following the tail, so it cannot
// re-pin on its own — that is the whole reason the setter exists, and its View() only guarantees a direct
// assignment cannot draw past the last page.
func TestStreamNoticeFitsInsideTheRowBudget(t *testing.T) {
	s := NewStream("transcript", 40, 5)
	for i := 0; i < 20; i++ {
		s.Append(fmt.Sprintf("line %02d", i))
	}
	s.SetNotice("Orchicon is thinking…")

	out := s.View()
	if !strings.Contains(out, "Orchicon is thinking") {
		t.Fatalf("the notice was clipped away with a full transcript:\n%s", out)
	}
	// The widget must not outgrow the rows it was given.
	if got := len(strings.Split(out, "\n")); got > 5 {
		t.Errorf("the stream drew %d rows for a 5-row pane:\n%s", got, out)
	}
	// AND THE NEWEST LINE STAYS VISIBLE: the notice takes its row from the body, so the tail is
	// re-pinned rather than pushed out.
	if !strings.Contains(out, "line 19") {
		t.Errorf("the newest line was pushed out of view by the notice:\n%s", out)
	}
	// At the bottom it is still "at the bottom" with the notice up, so the next append follows.
	if !s.AtBottom() {
		t.Error("with a notice showing, the stream is no longer considered at the bottom — the next " +
			"chunk would not be followed")
	}
}

// WITHOUT A NOTICE THE FULL HEIGHT IS THE TRANSCRIPT'S, so the notice really is taking a row rather
// than the widget reserving one permanently.
func TestStreamWithoutANoticeUsesEveryRow(t *testing.T) {
	s := NewStream("transcript", 40, 5)
	for i := 0; i < 20; i++ {
		s.Append(fmt.Sprintf("line %02d", i))
	}
	out := strings.TrimRight(s.View(), "\n")
	if got := len(strings.Split(out, "\n")); got != 5 {
		t.Errorf("with no notice the stream drew %d rows, want 5", got)
	}
	if !strings.Contains(out, "line 19") {
		t.Error("the newest line must be visible with no notice")
	}
}
