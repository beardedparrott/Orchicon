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

// A SOFT-WRAPPED DRAFT IS RESOLVED EXACTLY, which it used to be REFUSED.
//
// The old behaviour was deliberately cautious: the wrap grid is unexported and a caret placed by guessing at
// it would land somewhere plausible and wrong. The operator then hit the cost of that caution — "I pasted in a
// large amount of text and then tried clicking somewhere and it did not move the cursor to that position" —
// because ANY wrapped line disabled the gesture for the whole box.
//
// The map is now obtained from the widget's own cursor (caretRows) rather than computed, so it is exact
// rather than guessed and the caution is no longer needed.
func TestAClickOnAWrappedLinePlacesTheCaretExactly(t *testing.T) {
	// One logical line, wide enough to wrap several times in an 80-cell box.
	m := dockWith(strings.Repeat("word ", 60))
	row, col := m.TextOrigin()

	rows := m.caretRows()
	if len(rows) < 3 {
		t.Fatalf("fixture: the draft occupies %d visual rows, so this test is not exercising wrapping", len(rows))
	}
	if rows[0].line != 0 || rows[1].line != 0 {
		t.Fatalf("fixture: the wrapped rows belong to lines %d and %d, so the text is not one wrapped line",
			rows[0].line, rows[1].line)
	}

	// Click 3 cells into the SECOND visual row, and assert the caret lands on the rune those cells cover —
	// read from the buffer, not assumed.
	target := rows[1]
	if !m.ClickAt(col+3, row+1) {
		t.Fatal("a click on a soft-wrapped line was refused")
	}
	if got := m.ta.Line(); got != 0 {
		t.Errorf("the caret is on line %d, want 0 (the wrapped text is ONE logical line)", got)
	}
	want := target.start + 3
	if got := m.ta.LineInfo().StartColumn + m.ta.LineInfo().ColumnOffset; got != want {
		t.Errorf("the caret is at buffer rune %d, want %d (3 cells into visual row %d, which starts at %d)",
			got, want, 1, target.start)
	}
}

// AND THE SAME FOR A ROW THAT IS NOT THE FIRST, so the placement is not accidentally satisfied by a
// zero-offset case.
func TestAClickOnALaterWrappedRowPlacesTheCaretThere(t *testing.T) {
	m := dockWith(strings.Repeat("abc ", 80) + "\nsecond line")
	row, col := m.TextOrigin()
	rows := m.caretRows()
	if len(rows) < 4 {
		t.Fatalf("fixture: only %d visual rows", len(rows))
	}
	// The last row before the second logical line belongs to line 0; click 2 cells into it.
	last := rows[0]
	for _, r := range rows {
		if r.line == 0 {
			last = r
		}
	}
	idx := 0
	for i, r := range rows {
		if r == last {
			idx = i
		}
	}
	if !m.ClickAt(col+2, row+idx) {
		t.Fatal("a click on a later wrapped row was refused")
	}
	if got := m.ta.Line(); got != 0 {
		t.Errorf("the caret is on line %d, want 0", got)
	}
	if got := m.ta.LineInfo().StartColumn + m.ta.LineInfo().ColumnOffset; got != last.start+2 {
		t.Errorf("the caret is at buffer rune %d, want %d", got, last.start+2)
	}
}

// A CLICK ON A SCROLLED AREA IS REFUSED WHEN THE OFFSET CANNOT BE KNOWN, and PLACED when it can.
//
// The widget's scroll offset is unexported and history-dependent, so it cannot be read from outside — but in
// the state the operator is actually in after a paste (the caret at the end of the buffer) the widget pins
// that caret to the BOTTOM row, which fixes the offset at `total - height` and makes the first visible row
// exactly row `total - height`. This asserts that resolved case works, and that the same click with the caret
// moved away from the end is refused rather than guessed at.
func TestAClickInAScrolledComposerIsResolvedWhenTheCaretIsAtTheEnd(t *testing.T) {
	m := dockWith(strings.Repeat("line of text\n", 200))
	row, col := m.TextOrigin()
	rows := m.caretRows()
	if len(rows) <= m.InputRows() {
		t.Fatalf("fixture: %d rows do not overflow the %d input rows, so nothing is scrolled",
			len(rows), m.InputRows())
	}
	// A paste leaves the caret at the end, which is the state this resolves.
	if m.ta.Line() != m.lineCount()-1 {
		t.Fatalf("fixture: the caret is on line %d, not at the end of the buffer", m.ta.Line())
	}
	offset := len(rows) - m.InputRows()
	wantRow := rows[offset+1]
	wantCol := wantRow.start + 2

	if !m.ClickAt(col+2, row+1) {
		t.Fatal("a click in a scrolled composer with the caret at the end was refused")
	}
	// THE LINE IS ASSERTED, not only the column. This fixture's lines are short and all start at rune 0, so a
	// column-only assertion is satisfied by a caret placed on line 1 just as well as on the correct line —
	// which is precisely the bug being fixed (the old code treated visual row 1 as logical line 1). The first
	// version of this test checked only the column and passed with the old rules restored.
	if got := m.ta.Line(); got != wantRow.line {
		t.Errorf("the caret is on line %d, want %d — the first visible row of a scrolled composer is row "+
			"%d, not row %d", got, wantRow.line, offset, 1)
	}
	if got := m.ta.LineInfo().StartColumn + m.ta.LineInfo().ColumnOffset; got != wantCol {
		t.Errorf("the caret is at buffer rune %d, want %d", got, wantCol)
	}

	// Now move the caret away from the end: the offset is no longer knowable, so the click must be refused
	// rather than land on a guess.
	m2 := dockWith(strings.Repeat("line of text\n", 200))
	row2, col2 := m2.TextOrigin()
	for i := 0; i < 5; i++ {
		m2.ta.CursorUp()
	}
	if m2.ClickAt(col2+2, row2+1) {
		t.Error("a click in a scrolled composer with an unobservable scroll offset was resolved by guessing")
	}
}

// THE WALK LEAVES THE CARET WHERE IT FOUND IT. caretRows mutates the cursor to read the layout, and a click
// that turns out not to be placeable must not have moved anything.
func TestAnUnplaceableClickLeavesTheCaretAlone(t *testing.T) {
	m := dockWith(strings.Repeat("line of text\n", 200))
	line, li := m.ta.Line(), m.ta.LineInfo()
	beforeCol := li.StartColumn + li.ColumnOffset

	for i := 0; i < 5; i++ {
		m.ta.CursorUp()
	}
	line, li = m.ta.Line(), m.ta.LineInfo()
	beforeCol = li.StartColumn + li.ColumnOffset

	row, _ := m.TextOrigin()
	if m.ClickAt(0, row+1) {
		t.Fatal("fixture: this click was expected to be refused")
	}
	afterLi := m.ta.LineInfo()
	if m.ta.Line() != line || afterLi.StartColumn+afterLi.ColumnOffset != beforeCol {
		t.Errorf("a refused click moved the caret from (line %d, col %d) to (line %d, col %d)",
			line, beforeCol, m.ta.Line(), afterLi.StartColumn+afterLi.ColumnOffset)
	}
}

// lineCount is the number of LOGICAL lines in the buffer (for fixtures).
func (m *Model) lineCount() int { return m.ta.LineCount() }
