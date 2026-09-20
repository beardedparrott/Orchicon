package dock

// composer_click_test.go — CLICKING IN THE COMPOSER MOVES THE CARET.
//
// The operator: "I also just realized you can't use your mouse in the composer to change the position of your
// cursor."
//
// The gesture cannot come from the terminal: orch enables tea.WithMouseCellMotion(), so every press is handed
// to the program instead of placing the caret natively. The app therefore has to place it, and the placement
// is arithmetic over the box's geometry — which is exactly the kind of arithmetic that drifts silently. So the
// first test here derives the text's origin FROM THE RENDERED BOX rather than from the constants, and would
// fail if the border or the padding changed.

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// dockWith builds a focused dock of a known width holding one line of text.
//
// It returns the ADDRESS: ClickAt has a pointer receiver (it moves the caret), and New() hands back a value.
func dockWith(text string) *Model {
	m := New()
	m.Width = 80
	m.Focus()
	m.SetValue(text)
	return &m
}

// THE GEOMETRY IS THE RENDERED ONE. The column the caret is placed from is found by locating the text in the
// box's own paint, so a change to the border or the padding fails here rather than silently shifting every
// click by a cell.
func TestTheComposerTextOriginMatchesThePaint(t *testing.T) {
	m := dockWith("hello world")
	row, col := m.TextOrigin()

	lines := strings.Split(m.View(), "\n")
	if row >= len(lines) {
		t.Fatalf("TextOrigin row %d is outside the rendered box (%d rows)", row, len(lines))
	}
	// The column of the text is where the rendered row actually starts saying "hello".
	painted := ansi.Strip(lines[row])
	at := strings.Index(painted, "hello")
	if at < 0 {
		t.Fatalf("row %d does not contain the text at all: %q", row, painted)
	}
	if got := ansi.StringWidth(painted[:at]); got != col {
		t.Errorf("TextOrigin says the text starts at column %d, but the box paints it at %d — every click "+
			"would land %d cells off", col, got, col-got)
	}
}

// AND A CLICK ON A WORD PUTS THE CARET ON THAT WORD.
func TestClickingInTheComposerMovesTheCaret(t *testing.T) {
	m := dockWith("hello world")
	row, col := m.TextOrigin()
	// Click the "w": "hello " is six cells.
	if !m.ClickAt(col+6, row) {
		t.Fatal("a click on the text was not accepted")
	}
	if got := m.ta.LineInfo().ColumnOffset; got != 6 {
		t.Errorf("the caret landed at column %d, want 6 (the start of \"world\")", got)
	}
}

// THE CARET'S LINE, for a multi-line draft — the case the operator hit with a longer message.
func TestClickingASecondLineMovesTheCaretThere(t *testing.T) {
	m := dockWith("first\nsecond\nthird")
	row, col := m.TextOrigin()
	if !m.ClickAt(col+3, row+2) {
		t.Fatal("a click on the third line was not accepted")
	}
	if got := m.ta.Line(); got != 2 {
		t.Errorf("the caret is on line %d, want 2", got)
	}
	if got := m.ta.LineInfo().ColumnOffset; got != 3 {
		t.Errorf("the caret is at column %d, want 3", got)
	}
}

// BELOW THE TEXT IS THE END OF THE TEXT — what a text box does, rather than a refusal. The input box is
// MinInputRows tall even for a one-word draft, so there IS empty space below the text to click.
func TestClickingBelowTheTextPutsTheCaretAtTheEnd(t *testing.T) {
	m := dockWith("short")
	row, col := m.TextOrigin()
	// The last input row, which is empty here — one row further is the hint row, and refused.
	if !m.ClickAt(col+2, row+MinInputRows-1) {
		t.Fatal("a click on the empty input row below the text was not accepted")
	}
	if got := m.ta.Line(); got != 0 {
		t.Errorf("the caret moved to line %d, want 0 (the only line)", got)
	}
	if got := m.ta.LineInfo().ColumnOffset; got != 5 {
		t.Errorf("the caret is at column %d, want 5 (the end of \"short\")", got)
	}
}

// A CLICK ON THE PROMPT IS A CLICK AT THE LINE'S START, not a refusal.
func TestClickingThePromptPutsTheCaretAtTheStart(t *testing.T) {
	m := dockWith("hello world")
	row, _ := m.TextOrigin()
	if !m.ClickAt(0, row) {
		t.Fatal("a click on the prompt was refused")
	}
	if got := m.ta.LineInfo().ColumnOffset; got != 0 {
		t.Errorf("the caret is at column %d, want 0", got)
	}
}

// A CLICK OUTSIDE THE INPUT ROWS IS NOT OURS — the chip row, the hint row and the box's border all report
// false, so the shell can go on to whatever those rows mean.
func TestAClickOutsideTheInputRowsIsRefused(t *testing.T) {
	m := dockWith("hello")
	row, col := m.TextOrigin()
	if m.ClickAt(col, row-1) {
		t.Error("a click on the box's top border was accepted as a caret placement")
	}
	if m.ClickAt(col, row+m.InputRows()+1) {
		t.Error("a click below the input rows was accepted as a caret placement")
	}
}

// A SOFT-WRAPPED DRAFT IS DELIBERATELY REFUSED rather than guessed at: the caret would land somewhere
// plausible and wrong, and the operator cannot tell it was a guess.
func TestAClickOnAWrappedLineIsRefusedRatherThanGuessed(t *testing.T) {
	m := dockWith(strings.Repeat("word ", 60)) // far wider than the box
	row, col := m.TextOrigin()
	if m.ClickAt(col+4, row+1) {
		t.Error("a click on a soft-wrapped line was resolved by guessing at the wrap grid")
	}
}
