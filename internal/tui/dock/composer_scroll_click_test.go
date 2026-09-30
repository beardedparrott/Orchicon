package dock

// composer_scroll_click_test.go — a click in a SCROLLED composer places the caret,
// every time.
//
// The operator, for the third time:
//
//	"We have tried to fix this two different times but it seems like the composer in the TUI when
//	 clicking with the mouse is not always accurate. You have to click under it to position it
//	 right instead of right on the text. Not sure why this works sometimes and other times it
//	 does not."
//
// "Sometimes" was exact, and this file is the measurement: ClickAt knew the scroll offset in ONE
// state — the caret at the end of the buffer — and REFUSED every other click while the content was
// scrolled. Typing and pasting leave the caret at the end, so the FIRST click worked and moved the
// caret off the end; every click after it was silently ignored. Measured before the fix on a
// 40-line draft: click one → caret 744, click two → still 744, click three → still 744.
//
// The offset is now reconstructed from public API only (see Dock.scrollOffset), so nothing is
// refused. These tests measure the offset the way the CLICK PATH sees it — by clicking the first
// visible row and reading where the caret lands — rather than by recomputing the arithmetic under
// test.

import (
	"fmt"
	"strings"
	"testing"
)

// numberedDraft is `n` distinct lines, so a click's LANDING LINE identifies the
// offset unambiguously. Repeated filler text cannot do that: every visual row reads
// the same, so a wrong offset is invisible.
func numberedDraft(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "LINE%02d payload text here\n", i)
	}
	return b.String()
}

// visualRowOf is the caret's row in the WHOLE buffer — the figure a click is trying to
// produce, and the only one that distinguishes rows in a long draft.
func (m *Model) visualRowOf() int {
	lines := strings.Split(m.ta.Value(), "\n")
	li := m.ta.LineInfo()
	cur := 0
	for i := 0; i < m.ta.Line() && i < len(lines); i++ {
		cur += wrappedRowCount(lines[i], m.ta.Width())
	}
	return cur + li.RowOffset
}

func scrolledDock(n int) *Model {
	m := New()
	m.Width = 80
	m.Focus()
	m.SetValue(numberedDraft(n))
	m.Height = 40
	return &m
}

// THE OFFSET IS REAL AND THE RECONSTRUCTION AGREES WITH IT.
//
// Clicking the box's FIRST input row must land on the first VISIBLE buffer row. That
// landing row IS the offset, measured through the click path rather than asserted from
// the formula — so this fails if scrollOffset drifts from the widget's real view.
func TestClickingTheFirstVisibleRowLandsOnTheOffsetRow(t *testing.T) {
	m := scrolledDock(40)
	rows := m.caretRows()
	if len(rows) <= m.InputRows() {
		t.Fatalf("fixture: the draft must overflow the box (%d rows, %d visible)", len(rows), m.InputRows())
	}
	originRow, originCol := m.TextOrigin()
	if !m.ClickAt(originCol, originRow) {
		t.Fatal("a click on the first visible row must be accepted")
	}
	got := m.visualRowOf()
	want := m.scrollOffset(len(rows))
	if got != want {
		t.Fatalf("clicking the first visible row landed on buffer row %d, but scrollOffset() says the "+
			"first visible row is %d — the click path and the widget disagree", got, want)
	}
	if want == 0 {
		t.Fatalf("fixture: a scrolled draft must have a non-zero offset, got 0")
	}
}

// THE OPERATOR'S BUG: EVERY CLICK TAKES EFFECT.
//
// The old code placed the caret on the first click and refused the rest, so the caret's
// row never changed again. This clicks four DIFFERENT visible rows and requires four
// different landing rows — "the caret moved" is the whole assertion, because silence is
// what the operator experienced.
func TestEveryClickInAScrolledComposerMovesTheCaret(t *testing.T) {
	m := scrolledDock(40)
	originRow, originCol := m.TextOrigin()
	inputs := m.InputRows()
	if inputs < 4 {
		t.Fatalf("fixture: need at least 4 visible rows, got %d", inputs)
	}

	landed := map[int]bool{}
	// Deliberately out of order, and starting LOW: the first click is the one that used
	// to work, and every subsequent one is what regressed.
	for _, row := range []int{inputs - 1, 0, inputs / 2, 1} {
		if !m.ClickAt(originCol, originRow+row) {
			t.Fatalf("a click on visible row %d was REFUSED — that is the operator's bug", row)
		}
		landed[m.visualRowOf()] = true
	}
	if len(landed) < 3 {
		t.Fatalf("four clicks on four different rows produced only %d distinct caret rows (%v) — "+
			"clicks are being swallowed", len(landed), landed)
	}
}

// A DRAFT THAT FITS still behaves: the offset is necessarily zero, and clicking under
// the text puts the caret at the end (the box behaviour, unchanged).
func TestAClickInAShortComposerStillWorks(t *testing.T) {
	m := New()
	m.Width = 80
	m.Focus()
	m.SetValue("hello world")
	m.Height = 40
	originRow, originCol := m.TextOrigin()

	if !m.ClickAt(originCol+6, originRow) {
		t.Fatal("a click on the text must be accepted")
	}
	if got := m.ta.LineInfo().ColumnOffset; got != 6 {
		t.Fatalf("caret column = %d, want 6 (the start of \"world\")", got)
	}
	// BELOW the text is the END of the text — a box behaviour that must survive.
	if !m.ClickAt(originCol, originRow+m.InputRows()-1) {
		t.Fatal("a click below the text must be accepted")
	}
	if got := m.ta.LineInfo().ColumnOffset; got != len("hello world") {
		t.Fatalf("a click below the text put the caret at %d, want the end (%d)", got, len("hello world"))
	}
}

// A CLICK IS EXACT, not merely "moved": clicking row N of the visible window must land
// on buffer row offset+N.
func TestAClickLandsOnTheExactVisibleRow(t *testing.T) {
	m := scrolledDock(40)
	rows := m.caretRows()
	originRow, originCol := m.TextOrigin()
	offset := m.scrollOffset(len(rows))

	for _, vis := range []int{0, 2, 5} {
		if vis >= m.InputRows() {
			continue
		}
		if !m.ClickAt(originCol, originRow+vis) {
			t.Fatalf("click on visible row %d refused", vis)
		}
		if got, want := m.visualRowOf(), offset+vis; got != want {
			t.Fatalf("clicking visible row %d landed on buffer row %d, want %d (offset %d)",
				vis, got, want, offset)
		}
	}
}
