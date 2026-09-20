package kit2

// form_edit_lock_test.go — seeing, moving and scrolling a multi-line value.
//
// The operator, after the field grew to fit the pane: "even though the text box is
// larger, it is still cut off and you can't see your cursor to edit. We need the
// ability to see and move the cursor and scroll vertically with the arrow and the
// mouse wheel. My take is we should not make 'enter' drop the next line since we
// already have the down/up doing that, but instead, we should make enter edit that
// field and then allow the scroll mechanism since the field would be locked until
// you hit Esc to break out of it."
//
// Two things were wrong and they are separable:
//
//  1. THE WINDOW WAS ANCHORED AT THE START OF THE VALUE. The caret is spliced in at
//     its position, but the visible slice was rows[0:budget] — so with the caret at
//     the end of a 39-line value it was NOT RENDERED AT ALL (measured: "caret rune
//     found at rendered row -1"). That is the whole of "you can't see your cursor".
//  2. UP/DOWN MEANT "NEXT FIELD" EVERYWHERE, so there was no way to move through a
//     value taller than its window.
//
// The fix keeps the caret as the single source of scroll position (so the cursor
// cannot leave the view) and adds the operator's lock: Enter goes INTO a multi-line
// field, where up/down and the wheel move the caret, and Esc comes back out.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// tallForm builds a form whose one multi-line field holds `n` numbered lines, with
// the caret left at the END — which is where typing continues from, and where the
// caret used to be invisible.
func tallForm(t *testing.T, n int) *Form {
	t.Helper()
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString("line" + string(rune('a'+i%26)) + "-padding-padding\n")
	}
	f := NewForm("Edit worker", FieldSpec{Name: "role", Label: "Role", Kind: KTextArea})
	f.Set("role", b.String())
	f.Focused, f.Width, f.Height = true, 90, 25
	f.FocusName("role")
	return f
}

const caretRune = "\u258f"

// caretRow is the rendered row carrying the caret, or -1 when it is not on screen.
func caretRow(f *Form) int {
	for i, r := range strings.Split(ansi.Strip(f.View()), "\n") {
		if strings.Contains(r, caretRune) {
			return i
		}
	}
	return -1
}

// THE BUG. The caret must be on screen even when it is at the far end of a value
// much taller than the field's window.
func TestFocusedMultiLineFieldKeepsTheCaretVisible(t *testing.T) {
	f := tallForm(t, 40)
	if got := caretRow(f); got < 0 {
		t.Fatalf("the caret is NOT rendered with a height-25 pane and the caret at the end of a "+
			"40-line value — the window is still anchored at the start of the value, so the operator "+
			"cannot see where they are typing:\n%s", ansi.Strip(f.View()))
	}

	// And it stays visible wherever the caret is put.
	lines := strings.Count(f.Values["role"], "\n")
	for _, frac := range []float64{0, 0.25, 0.5, 0.95, 1} {
		line := int(frac * float64(lines))
		// Put the caret at the start of that line.
		idx := 0
		for i := 0; i < line; i++ {
			next := strings.Index(f.Values["role"][idx:], "\n")
			if next < 0 {
				break
			}
			idx += next + 1
		}
		f.setCaret("role", idx)
		if got := caretRow(f); got < 0 {
			t.Errorf("caret at line %d of %d is NOT on screen:\n%s", line, lines, ansi.Strip(f.View()))
		}
	}
}

// ENTER GOES INTO A MULTI-LINE FIELD; ESC COMES BACK OUT.
//
// "we should not make 'enter' drop the next line ... instead, we should make enter
// edit that field ... locked until you hit Esc to break out of it."
func TestEditLockEnterAndEsc(t *testing.T) {
	f := tallForm(t, 40)

	if f.EditingField() != "" {
		t.Fatal("fixture: no field should be locked to begin with")
	}
	f.HandleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if f.EditingField() != "role" {
		t.Fatalf("Enter did not lock the multi-line field (editing = %q)", f.EditingField())
	}

	// ESC LEAVES THE FIELD, NOT THE FORM: handled by the form (true), and the edit
	// is not abandoned.
	if _, handled := f.HandleKey(tea.KeyMsg{Type: tea.KeyEsc}); !handled {
		t.Error("Esc with a locked field must be consumed by the form to release the field")
	}
	if f.EditingField() != "" {
		t.Error("Esc did not release the lock")
	}
	if f.Submitted {
		t.Error("Esc released the field but also submitted/abandoned the form")
	}
	// A SECOND Esc is the form's own cancel, which the HOST handles — the form
	// reports it as not-handled so the caller closes the editor.
	if _, handled := f.HandleKey(tea.KeyMsg{Type: tea.KeyEsc}); handled {
		t.Error("the second Esc must fall through to the host, which closes the editor")
	}
}

// OUTSIDE the lock, up/down still move between FIELDS — the form's navigation must
// not change just because a multi-line field exists.
func TestUpDownStillMoveFieldsWhenNotLocked(t *testing.T) {
	f := NewForm("Edit",
		FieldSpec{Name: "a", Label: "A", Kind: KTextArea},
		FieldSpec{Name: "b", Label: "B", Kind: KText},
		FieldSpec{Name: "c", Label: "C", Kind: KText},
	)
	f.Focused = true
	f.FocusName("a")

	f.HandleKey(tea.KeyMsg{Type: tea.KeyDown})
	if got := f.current().Name; got != "b" {
		t.Fatalf("down moved to field %q, want b — field navigation changed", got)
	}
	f.HandleKey(tea.KeyMsg{Type: tea.KeyUp})
	if got := f.current().Name; got != "a" {
		t.Fatalf("up moved to field %q, want a", got)
	}
}

// INSIDE the lock, up/down move the CARET through the value, and the view follows —
// that is "move the cursor and scroll vertically with the arrow".
func TestEditLockArrowsMoveTheCaretThroughTheValue(t *testing.T) {
	f := tallForm(t, 40)
	f.HandleKey(tea.KeyMsg{Type: tea.KeyEnter})

	end := f.caret("role")
	f.HandleKey(tea.KeyMsg{Type: tea.KeyUp})
	if f.caret("role") >= end {
		t.Fatalf("up did not move the caret back through the value (caret %d -> %d)", end, f.caret("role"))
	}
	if r := caretRow(f); r < 0 {
		t.Error("the caret left the window after moving up")
	}

	// Walk to the very top: the window must scroll all the way to the first line.
	for i := 0; i < 60; i++ {
		f.HandleKey(tea.KeyMsg{Type: tea.KeyUp})
	}
	if f.caret("role") != 0 {
		t.Fatalf("walking up 60 times left the caret at %d, want 0 (the first line)", f.caret("role"))
	}
	if r := caretRow(f); r < 0 {
		t.Error("the caret is not on screen at the top of the value")
	}
	view := ansi.Strip(f.View())
	if !strings.Contains(view, "linea-padding") {
		t.Errorf("the window did not scroll back to the first line:\n%s", view)
	}

	// And back down to the end.
	for i := 0; i < 80; i++ {
		f.HandleKey(tea.KeyMsg{Type: tea.KeyDown})
	}
	if got := f.caret("role"); got != len([]rune(f.Values["role"])) {
		t.Errorf("walking down left the caret at %d, want the end (%d)", got, len([]rune(f.Values["role"])))
	}
	if r := caretRow(f); r < 0 {
		t.Error("the caret is not on screen at the end of the value")
	}
}

// UP/DOWN STEP BY LOGICAL LINE, NOT BY WRAPPED ROW.
//
// A long line that soft-wraps into three rows is still ONE line to the operator;
// stepping by wrapped row would drop the caret at an arbitrary column mid-sentence.
func TestEditLockStepsByLogicalLine(t *testing.T) {
	f := NewForm("Edit", FieldSpec{Name: "d", Label: "D", Kind: KTextArea})
	// Line 1 is short; line 2 is long enough to wrap in a 20-cell field.
	f.Set("d", "abc\n"+strings.Repeat("w", 60)+"\nxyz\n")
	f.Focused, f.Width, f.Height = true, 20, 25
	f.FocusName("d")
	f.HandleKey(tea.KeyMsg{Type: tea.KeyEnter})

	f.setCaret("d", 1) // column 1 of the SHORT first line
	f.HandleKey(tea.KeyMsg{Type: tea.KeyDown})

	// On the long line, column 1 — not somewhere inside the wrapped remainder.
	val := []rune(f.Values["d"])
	got := f.caret("d")
	if got != 5 { // "abc\n" is 4 runes, so rune 5 is column 1 of line 2
		t.Fatalf("caret after down = %d, want 5 (column 1 of the second LOGICAL line); value=%q", got, string(val))
	}
	// The caret renders on the SECOND logical line, not inside the first wrap.
	if r := caretRow(f); r < 0 {
		t.Error("the caret is not on screen")
	}
}

// ENTER INSIDE THE LOCK INSERTS A NEWLINE.
//
// It is the only way to type one: Enter is the key that locks the field, so without
// this a text area could only ever hold a single line unless the text was pasted.
func TestEditLockEnterInsertsANewline(t *testing.T) {
	f := tallForm(t, 3)
	f.HandleKey(tea.KeyMsg{Type: tea.KeyEnter})

	before := strings.Count(f.Values["role"], "\n")
	f.HandleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if got := strings.Count(f.Values["role"], "\n"); got != before+1 {
		t.Fatalf("newline count %d -> %d, want +1", before, got)
	}
	// The same for the composer's convention, so either key works.
	f.HandleKey(tea.KeyMsg{Type: tea.KeyEnter, Alt: true})
	if got := strings.Count(f.Values["role"], "\n"); got != before+2 {
		t.Fatalf("alt+enter newline count = %d, want +2", got)
	}
}

// MOVING TO ANOTHER FIELD RELEASES THE LOCK, so the arrows cannot stay hijacked by
// a field the operator has left.
func TestMovingOffALockedFieldReleasesIt(t *testing.T) {
	f := NewForm("Edit",
		FieldSpec{Name: "a", Label: "A", Kind: KTextArea},
		FieldSpec{Name: "b", Label: "B", Kind: KText},
	)
	f.Focused = true
	f.FocusName("a")
	f.HandleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if f.EditingField() != "a" {
		t.Fatal("fixture: field a should be locked")
	}

	f.HandleKey(tea.KeyMsg{Type: tea.KeyTab})
	if f.current().Name != "b" {
		t.Fatalf("tab moved to %q, want b", f.current().Name)
	}
	if f.EditingField() != "" {
		t.Error("the lock survived the cursor moving to another field")
	}
	// Field navigation works again.
	f.HandleKey(tea.KeyMsg{Type: tea.KeyUp})
	if f.current().Name != "a" {
		t.Errorf("after leaving the locked field, up did not walk fields (at %q)", f.current().Name)
	}
}

// THE WHEEL SCROLLS THE LOCKED FIELD — "scroll vertically with ... the mouse wheel".
//
// It moves the CARET, because the caret IS the scroll position: a separate offset
// could drift and leave the cursor off screen, which is the bug being fixed.
func TestWheelScrollsALockedField(t *testing.T) {
	f := tallForm(t, 40)

	// Not locked: the form does NOT consume the wheel, so the host can scroll its own
	// panes.
	if f.Wheel(3) {
		t.Error("the wheel was consumed with no field locked — that would break pane scrolling")
	}

	f.HandleKey(tea.KeyMsg{Type: tea.KeyEnter})
	before := f.caret("role")
	if !f.Wheel(-3) {
		t.Fatal("the wheel was not consumed while a field is locked")
	}
	if f.caret("role") >= before {
		t.Errorf("wheel-up did not move the caret back (caret %d -> %d)", before, f.caret("role"))
	}
	if r := caretRow(f); r < 0 {
		t.Error("the caret left the window after a wheel scroll")
	}

	// Walk up with the wheel until the window reaches the top of the value.
	for i := 0; i < 20; i++ {
		f.Wheel(-5)
	}
	if f.caret("role") != 0 {
		t.Errorf("wheel-up left the caret at %d, want 0", f.caret("role"))
	}
	if view := ansi.Strip(f.View()); !strings.Contains(view, "linea-padding") {
		t.Errorf("the wheel did not scroll the window to the first line:\n%s", view)
	}
}

// THE FOOTER NAMES ctrl+p — the operator's second report: "there is no hint in the
// detail pane saying ctrl+p shows markdown preview".
//
// The field's OWN hint sits below its value, so on a tall field it is pushed off the
// pane exactly when the value is long enough to want a preview. The footer is always
// visible, so the chord is named there too — but only when the focused field can
// actually preview, so it never advertises a chord that would do nothing.
func TestFooterNamesThePreviewChord(t *testing.T) {
	// A prose field with markdown in it: the chord is offered AND named.
	f := NewForm("Edit worker", FieldSpec{Name: "role", Label: "Role", Kind: KTextArea,
		Initial: "# Heading\n\nsome **bold** text\n"})
	f.Focused, f.Width, f.Height = true, 100, 30
	f.FocusName("role")
	if view := ansi.Strip(f.View()); !strings.Contains(view, "ctrl+p") {
		t.Errorf("the footer does not name ctrl+p for a previewable field:\n%s", view)
	}

	// A code field: no chord, and nothing advertising it.
	g := NewForm("Edit runtime image", FieldSpec{Name: "dockerfile_override", Label: "Dockerfile override",
		Kind: KTextArea, Initial: "# syntax=docker/dockerfile:1\nFROM alpine\n", NoPreview: true})
	g.Focused, g.Width, g.Height = true, 100, 30
	g.FocusName("dockerfile_override")
	if view := ansi.Strip(g.View()); strings.Contains(view, "ctrl+p") {
		t.Errorf("the footer advertises ctrl+p on a field that cannot preview:\n%s", view)
	}

	// Once locked, the footer describes the lock instead.
	f.HandleKey(tea.KeyMsg{Type: tea.KeyEnter})
	view := ansi.Strip(f.View())
	if !strings.Contains(view, "esc: leave field") {
		t.Errorf("the footer does not explain how to leave the locked field:\n%s", view)
	}
}
