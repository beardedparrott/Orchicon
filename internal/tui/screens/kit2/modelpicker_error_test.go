package kit2

// modelpicker_error_test.go — AN ACTIONABLE ERROR MUST BE READABLE.
//
// The report: choosing the opencode adapter showed
//
//	⚠ internal: list opencode models: opencode listed no models — the command
//
// and cut off there. The remedy was in the part that did not fit, which made the message useless
// precisely when it mattered — and the box was wide enough for the text, so this was a layout
// decision (a single padded status row) rather than a shortage of space.
//
// These pin the property rather than the wording: EVERY WORD of the error must appear somewhere in
// the rendered box. A test that asserted a particular sentence would pass while the same truncation
// happened to a different message.

import (
	"strings"
	"testing"
)

// manyWords is a long, realistic error — the shape that was being cut: a sentence with a remedy.
const longError = "internal: list opencode models: opencode listed no models: the command succeeded " +
	"and printed NOTHING, which is how it reports an empty list. Check `opencode auth list` and run " +
	"`opencode models` by hand in the same shell"

// pickerWith builds a picker with the given error already set, at a screen size. View() takes no
// arguments — it reads the screen size the host sets — so the size goes in through SetScreen.
func pickerWith(t *testing.T, errText string, screenW, screenH int) *ModelPicker {
	t.Helper()
	mp := NewModelPicker("Ask model")
	mp.SetScreen(screenW, screenH)
	mp.err = errText
	return mp
}

// TestALongErrorIsFullyVisible is the regression pin for the report.
func TestALongErrorIsFullyVisible(t *testing.T) {
	// Two sizes, because the bug was width-dependent: it appeared at the operator's terminal width
	// and not necessarily at another.
	for _, size := range []struct{ w, h int }{{110, 24}, {80, 30}, {64, 40}} {
		view := pickerWith(t, longError, size.w, size.h).View()
		for _, word := range strings.Fields(longError) {
			if !strings.Contains(view, word) {
				t.Errorf("at %dx%d the error is missing %q — the message is being CUT, so the "+
					"operator cannot read the remedy. Full view:\n%s", size.w, size.h, word, view)
			}
		}
	}
}

// TestTheErrorGetsMoreThanOneRow states the mechanism, so a future layout change that reintroduces a
// fixed single status row fails here rather than in a screenshot.
func TestTheErrorGetsMoreThanOneRow(t *testing.T) {
	mp := pickerWith(t, longError, 0, 0)
	if got := len(mp.statusLines(80)); got < 2 {
		t.Errorf("statusLines returned %d row(s) for a %d-character error at width 80 — an actionable "+
			"error cannot fit on one row, and giving it one is what truncated it", got, len(longError))
	}
}

// TestAnErrorIsCappedSoThePickerStaysUsable is the other side: the cap must exist, or a pathological
// message eats the list. It must still SAY it was cut.
func TestAnErrorIsCappedSoThePickerStaysUsable(t *testing.T) {
	mp := pickerWith(t, strings.Repeat("verylongword ", 400), 0, 0)
	rows := mp.statusLines(60)
	if len(rows) > maxErrorRows {
		t.Errorf("statusLines returned %d rows, above the %d-row cap — a long error must not consume "+
			"the whole box", len(rows), maxErrorRows)
	}
	if !strings.Contains(strings.Join(rows, ""), "…") {
		t.Error("a capped error must say it was cut, rather than ending mid-sentence")
	}
}

// TestAShortStatusStaysOneRow keeps the change from costing the normal case: no error, one row, as
// before — the wrapping applies to errors, not to every status.
func TestAShortStatusStaysOneRow(t *testing.T) {
	mp := pickerWith(t, "", 0, 0)
	if got := len(mp.statusLines(80)); got != 1 {
		t.Errorf("a non-error status returned %d rows, want 1 — padding a short summary across rows "+
			"would leave the list shorter for no reason", got)
	}
}
