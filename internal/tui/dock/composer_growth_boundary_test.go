package dock

// composer_growth_boundary_test.go — THE BOX MUST GROW WHEN THE TEXT WRAPS, NOT A SENTENCE LATER.
//
// The operator: "if you type a long message, it does extend the message box but not right away, only after
// you type a sentence about halfway does it stretch the box, leaving users to type blind for a short time."
//
// THE CAUSE, and it was documented in this file as a "cosmetic shortfall": visualRows() estimated the wrap
// as `ceil(cells/width)` while bubbles' textarea WORD-WRAPS and — at the boundary — uses `>=`, so a line of
// exactly the available width becomes TWO rows. The estimate therefore under-counted, the box lagged, and
// for a stretch of characters the caret sat on a wrapped row that was not drawn.
//
// WHY IT READ AS "ABOUT HALFWAY": the box has a resting height (MinInputRows = 3), so the first two rows of
// error are invisible — the operator only sees the lag once the real wrap passes the floor, which is a
// sentence in. That is why a short-string test passes on the OLD code and is worthless as a regression test.
//
// These tests therefore drive the boundary directly and would fail on the old estimator.

import (
	"strings"
	"testing"
)

// TestEstimatorMatchesTheWidgetAcrossTheWidthRange compares the dock's estimate with the textarea's own
// wrapped height for content spanning several rows — the range where the old estimate drifted.
func TestEstimatorMatchesTheWidgetAcrossTheWidthRange(t *testing.T) {
	m := New()
	m.Width = 80
	m.SetViewportRows(40)
	m.resizeTa()
	tw := m.taWidth()

	base := "aaa bbb ccc ddd eee fff ggg hhh iii jjj kkk lll mmm nnn ooo ppp qqq rrr sss ttt uuu vvv"
	var mismatches int
	for pad := 0; pad < tw*3; pad++ {
		v := base + " " + strings.Repeat("z", pad)
		m.ta.SetValue(v)
		m.resizeTa()
		est := m.visualRows()
		real := m.ta.LineInfo().Height
		if est != real {
			mismatches++
			if mismatches <= 5 {
				t.Errorf("cells=%d: estimate %d rows, widget wraps to %d — the box would lag the text",
					len(v), est, real)
			}
		}
	}
	if mismatches > 0 {
		t.Fatalf("%d/%d widths disagreed with the widget", mismatches, tw*3)
	}
}

// TestBoxNeverShowsFewerRowsThanTheWidgetWraps is the operator-visible property: if the box is shorter than
// the wrapped text, the caret can sit on a row that is not drawn — "typing blind".
func TestBoxNeverShowsFewerRowsThanTheWidgetWraps(t *testing.T) {
	m := New()
	m.Width = 80
	m.SetViewportRows(40)
	m.resizeTa()
	tw := m.taWidth()

	base := "the quick brown fox jumps over the lazy dog and keeps running through the field toward the barn"
	for pad := 0; pad < tw*3; pad++ {
		v := base + " " + strings.Repeat("q", pad)
		m.ta.SetValue(v)
		m.resizeTa()
		visible := m.InputRows()
		realRows := m.ta.LineInfo().Height
		if realRows > visible {
			t.Fatalf("cells=%d: the widget wraps to %d rows but the box shows %d — the operator types blind",
				len(v), realRows, visible)
		}
	}
}

// TestFullWidthLineCountsAsTwoRows pins the exact boundary the fix is about.
func TestFullWidthLineCountsAsTwoRows(t *testing.T) {
	m := New()
	m.Width = 80
	m.resizeTa()
	tw := m.taWidth()

	m.ta.SetValue(strings.Repeat("x", tw))
	m.resizeTa()
	if got := m.visualRows(); got < 2 {
		t.Fatalf("a line of exactly taWidth (%d) cells reports %d row(s); the textarea wraps it to 2 — "+
			"this is the off-by-one that made the box grow late", tw, got)
	}
	// One cell short must still be a single row, so the fix does not over-grow either.
	m.ta.SetValue(strings.Repeat("x", tw-1))
	m.resizeTa()
	if got := m.visualRows(); got != 1 {
		t.Fatalf("a line of taWidth-1 cells reports %d rows, want 1 — over-growing is the other failure", got)
	}
}
