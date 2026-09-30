package kit2

// form_align_test.go — a form's fields render in ONE column, and a reference field
// shows how to open its control from any cursor position.
//
// The operator, looking at the worker edit form and hunting for the model:
//
//	"Here is the edit worker form and no model is showing to edit it"
//	"Oh I know why I didn't notice it, it's not inline with the other fields"
//
// Two independent causes, both in the form's own renderer:

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// modelRow extracts the single rendered line that carries the model field, so an
// assertion about that row cannot be satisfied by the footer or a neighbour. An
// earlier version of these tests asserted against the WHOLE form and passed on
// unfixed code because "enter" also appears in the footer's "enter: next".
func modelRow(t *testing.T, view string) string {
	t.Helper()
	var found string
	for _, l := range strings.Split(ansi.Strip(view), "\n") {
		if strings.Contains(l, "Model:") {
			if found != "" {
				t.Fatalf("more than one Model row — the form re-flowed:\n%s", view)
			}
			found = strings.TrimSpace(l)
		}
	}
	if found == "" {
		t.Fatalf("no Model row rendered:\n%s", view)
	}
	return found
}

// TEST 1: A WRAPPED ROW KEEPS ITS OWN INDENT.
//
// This is the cause of "it's not inline with the other fields". wrapFormLine
// returned a fitting row VERBATIM (prefix intact) but rebuilt a wrapping row out of
// `strings.Fields`, which discards leading whitespace — so the wrapped row landed at
// column 0 while its neighbours sat at column 2. Measured on the worker edit form:
// `Purpose:` rendered at 0 and `Plane role:` at 2, in the same list.
//
// The two cases are asserted together on purpose: the bug was not "the indent is
// wrong", it was "the same form indents differently depending on whether a value
// happened to fit", and a test that checks one case cannot see that.
func TestWrapFormLinePreservesTheRowsOwnIndent(t *testing.T) {
	const width = 40
	// Long enough to wrap, short enough to fit: same field, two values.
	long := "  Purpose: " + strings.Repeat("word ", 20)
	short := "  Purpose: short"

	got := wrapFormLine(long, width)
	if len(got) < 2 {
		t.Fatalf("fixture: the long row must wrap, got %d line(s)", len(got))
	}
	// EVERY line of the wrapped row starts where the row started.
	for i, l := range got {
		if !strings.HasPrefix(l, "  ") {
			t.Fatalf("wrapped line %d lost the row's indent: %q\nall lines: %q", i, l, got)
		}
	}
	// …and the SAME row unwrapped keeps it too, so the two agree.
	if one := wrapFormLine(short, width); len(one) != 1 || one[0] != short {
		t.Fatalf("a fitting row must pass through untouched: %q", one)
	}
}

// The continuation is still indented past the label, not merely to the row indent —
// a wrapped VALUE must read as belonging to its field.
func TestWrapFormLineIndentsContinuationsPastTheLabel(t *testing.T) {
	got := wrapFormLine("  Model: "+strings.Repeat("ref/segment ", 12), 40)
	if len(got) < 2 {
		t.Fatalf("fixture: expected a wrap, got %q", got)
	}
	// "  Model: " is 9 cells, so continuations align under the value.
	if want := strings.Repeat(" ", 9); !strings.HasPrefix(got[1], want) {
		t.Fatalf("continuation = %q, want it indented %d cells to the value column", got[1], len(want))
	}
}

// A row whose label is too far right for the ": " heuristic still never loses its
// own indent — it just falls back to a plain indent past it.
func TestWrapFormLineFallsBackWithoutLosingTheIndent(t *testing.T) {
	// No ": " at all: the continuation heuristic has nothing to align to.
	got := wrapFormLine("  "+strings.Repeat("plain ", 20), 30)
	if len(got) < 2 {
		t.Fatalf("fixture: expected a wrap, got %q", got)
	}
	if !strings.HasPrefix(got[0], "  ") || !strings.HasPrefix(got[1], "  ") {
		t.Fatalf("every line must keep the row's indent, got %q", got)
	}
}

// TEST 2: A REFERENCE FIELD NAMES ITS GESTURE FROM ANYWHERE.
//
// The model row read as inert text because the `enter: choose model` hint was drawn
// only while that field held the cursor, and the form's footer said "enter: next" —
// which is WRONG on a reference field, where enter opens a picker rather than
// advancing. The operator was on the Name field, so nothing on screen said the model
// row could be opened.
//
// The convention already existed: a multi-line field names its chords unconditionally
// (label row: "· enter: edit · ctrl+e: expand"). This asserts the reference family
// does the same.
func TestReferenceFieldsNameTheirGestureWithoutFocus(t *testing.T) {
	f := NewForm("Worker",
		FieldSpec{Name: "name", Label: "Name", Kind: KText, Initial: "writer"},
		FieldSpec{Name: "model_ref", Label: "Model", Kind: KModel,
			Initial: "claude/anthropic/claude-sonnet-5"},
		FieldSpec{Name: "scheduled", Label: "Scheduled start", Kind: KDateTime},
	)
	f.Width = 70
	f.Focused = true
	// The cursor is NOT on any reference field — the operator's own situation.
	f.FocusName("name")

	row := modelRow(t, f.View())
	// The model ROW states its gesture while the cursor is elsewhere. Asserted on the
	// row itself: the footer also says "enter", so a form-wide check would pass on
	// unfixed code (it did, in this file's first version).
	if !strings.Contains(row, "claude/anthropic/claude-sonnet-5") {
		t.Fatalf("the model row must show its value, got %q", row)
	}
	if !strings.Contains(row, "enter") {
		t.Fatalf("the model row must state how to open the picker while the cursor is "+
			"elsewhere, got %q", row)
	}
	// …and the datetime row states its own, or the family is inconsistent again.
	var schedRow string
	for _, l := range strings.Split(ansi.Strip(f.View()), "\n") {
		if strings.Contains(l, "Scheduled start:") {
			schedRow = strings.TrimSpace(l)
		}
	}
	if schedRow == "" || !strings.Contains(schedRow, "enter") {
		t.Fatalf("the datetime row must name its gesture too, got %q", schedRow)
	}
}

// An UNSET reference field names its gesture too — and it is the SAME affordance the
// set row uses, so the row does not change character as the operator fills it in.
//
// NO PLACEHOLDER IS SUPPLIED. The first version of this test passed the placeholder in
// itself, which made it tautological: it asserted that a string it had just written
// appeared on screen. The gesture must come from the form's own renderer.
func TestUnsetReferenceFieldsNameTheirGesture(t *testing.T) {
	f := NewForm("Worker",
		FieldSpec{Name: "name", Label: "Name", Kind: KText, Initial: "writer"},
		FieldSpec{Name: "model_ref", Label: "Model", Kind: KModel},
	)
	f.Width = 70
	f.Focused = true
	f.FocusName("name")

	row := modelRow(t, f.View())
	if !strings.Contains(row, "— none —") {
		t.Fatalf("an unset model row must show the affordance, got %q", row)
	}
	if !strings.Contains(row, "enter to choose a model") {
		t.Fatalf("an unset model row must name the gesture, got %q", row)
	}
}

// The footer must not claim enter merely advances when it can also OPEN something:
// on a reference field the field-specific gesture wins, and the footer is the line
// that can say so generically.
func TestTheFormFooterDoesNotLieAboutEnter(t *testing.T) {
	f := NewForm("Worker",
		FieldSpec{Name: "model_ref", Label: "Model", Kind: KModel, Initial: "claude/anthropic/x"},
	)
	f.Width = 70
	f.Focused = true
	f.FocusName("model_ref")
	v := ansi.Strip(f.View())
	if strings.Contains(v, "enter: next") && !strings.Contains(v, "open") {
		t.Fatalf("the footer says enter only advances, but on a reference field it opens a picker:\n%s", v)
	}
}

// A SET reference row must still fit on ONE line at the operator's pane width.
//
// This is the constraint that decided the affordance's wording, and it is worth
// pinning because a longer hint is the obvious "improvement" a later reader would
// make. A model ref is ~32 cells on its own, and a hint that pushes the row over the
// pane width wraps it — which drops every field BELOW it down a row, so one verbose
// label re-flows the whole form.
func TestASetModelRowFitsOnOneLine(t *testing.T) {
	f := NewForm("Edit worker",
		FieldSpec{Name: "name", Label: "Name", Kind: KText, Initial: "Quick Software Engineer"},
		FieldSpec{Name: "model_ref", Label: "Model", Kind: KModel,
			Initial: "claude/anthropic/claude-sonnet-5"},
	)
	f.Width = 58 // the detail pane's inner width at a 122-column terminal
	f.Focused = true
	f.FocusName("name")

	lines := f.View()
	var modelRows int
	for _, l := range strings.Split(ansi.Strip(lines), "\n") {
		if strings.Contains(l, "Model:") {
			modelRows++
		}
	}
	if modelRows != 1 {
		t.Fatalf("the Model row occupies %d lines, want 1 (a wrapped row re-flows the form):\n%s",
			modelRows, lines)
	}
	row := modelRow(t, lines)
	// It must fit AND still carry the gesture: a row with no hint at all fits
	// trivially, which is how the first version of this test passed on unfixed code.
	if !strings.Contains(row, "claude/anthropic/claude-sonnet-5") {
		t.Fatalf("the model row must show its whole ref, got %q", row)
	}
	if !strings.Contains(row, "enter") {
		t.Fatalf("the row must still name its gesture after fitting, got %q", row)
	}
}
