package screenkit

import "testing"

// footer_band_test.go — A FOOTER IS A BAND, NOT TEXT.
//
// These pin the geometry that turned a small text bug into a disorienting one. The footer is installed
// OUTSIDE the scrolling region and the body is sized from what is left (BodyHeightFor:
// `overhead += d.footerRows()`), so the footer's row count is a LAYOUT INPUT — and footerRows() answers
// 0 for "" and 2 for anything else:
//
//	footer ""            -> 0 rows -> the body gets 2 MORE rows
//	footer "anything"    -> 2 rows -> the body gets 2 FEWER rows
//
// So a caller that returns "" to mean "nothing to say right now" does not leave the pane unchanged: it
// RESIZES it by two rows, re-clamps the body's scroll offset against a new height, and the transcript
// visibly jumps. When the value that flips is produced on a race — a card being settled by another
// goroutine between one render's test and the next — that is not a one-off jump but an oscillation.
//
// The reported symptom that led here: "the screen is jumping up and down very fast and it's very
// disorienting." These assertions exist so the next caller cannot treat "" as harmless, and so a change
// that made footerRows agree between the two cases would be a DELIBERATE decision rather than an accident.

func TestAnEmptyFooterOccupiesNoRows(t *testing.T) {
	d := &Detail{}
	d.SetFooter("")
	if got := d.footerRows(); got != 0 {
		t.Fatalf("an empty footer must occupy no rows, got %d", got)
	}
}

func TestAnyFooterTextCostsTheBodyTwoRows(t *testing.T) {
	d := &Detail{}
	d.SetFooter("⏸ waiting for your approval · write main.go")
	if got := d.footerRows(); got != 2 {
		t.Fatalf("a one-line footer is a separator plus the band (2 rows), got %d", got)
	}
}

// And the consequence, stated as the invariant a caller depends on: the body shrinks by exactly the
// band when one appears. This is the assertion that fails if anyone ever "optimises" footerRows to
// return 0 for a short string.
func TestAFutureFooterStealsItsRowsFromTheBody(t *testing.T) {
	d := &Detail{Height: 40}
	withNothing := d.BodyHeightFor(0, true)

	d.SetFooter("⏸ waiting for your approval")
	withCard := d.BodyHeightFor(0, true)

	if diff := withNothing - withCard; diff != 2 {
		t.Fatalf("appearing footer must take exactly 2 body rows (0 for empty, 2 for a line), took %d "+
			"— a footer is a layout band, and a caller returning \"\" to mean \"say nothing\" is "+
			"RESIZING the pane, not clearing text", diff)
	}
}
