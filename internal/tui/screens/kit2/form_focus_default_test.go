package kit2

// form_focus_default_test.go — A FORM THAT ISN'T FOCUSED DOESN'T LOOK EDITABLE.
//
// `Form.Focused` is render-only: it decides whether View draws the `▸` cursor
// marker on the cursor's field and whether an empty field shows its placeholder.
// It does NOT gate key handling — every field accepts typing either way.
//
// That combination is the trap. A host that built a form and forgot the flag got a
// fully functional, fully keyboard-driven form that RENDERED as an inert list of
// "Label: value" lines. Nothing was broken; it just looked read-only. The operator
// reported exactly that from the launch prompt: "didn't actually give me the option
// to edit any of the fields, just simply ctrl+s to save defaults."
//
// It defaulted to false and was set by hand at each of the four call sites, three of
// which remembered. So NewForm defaults it to true now — there is no such thing as
// a form constructed for nobody — and these tests pin both halves: the default, and
// that the default is what makes the form LOOK editable.

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// A NEW FORM IS FOCUSED. The class-level guard: this is the assertion whose absence
// let four call sites each decide for themselves.
func TestNewFormIsFocusedByDefault(t *testing.T) {
	f := NewForm("t", FieldSpec{Name: "a", Label: "A", Kind: KText})
	if !f.Focused {
		t.Error("NewForm built an UNFOCUSED form. Focused is render-only, so such a form still accepts " +
			"typing while drawing no cursor marker and no placeholder — it looks read-only, and an operator " +
			"reasonably concludes they cannot edit it. There is no reason to construct an unfocused form; " +
			"a host that wants one can set the field.")
	}
}

// AND THAT IS WHAT MAKES IT LOOK EDITABLE — asserted on the RENDERED output, because
// the flag's whole meaning is visual and a test that only read the bool would pass
// while View ignored it.
func TestAFocusedFormDrawsItsCursorMarker(t *testing.T) {
	specs := []FieldSpec{
		{Name: "name", Label: "Name", Kind: KText, Required: true, Initial: "thing"},
		{Name: "slug", Label: "Slug", Kind: KText, Initial: "thing"},
	}

	focused := NewForm("t", specs...)
	focused.Width = 60
	got := ansi.Strip(focused.View())
	if !strings.Contains(got, FormCursorMark) {
		t.Errorf("a focused form does not render %q on its cursor's field, so nothing on screen shows which "+
			"field the keyboard is on:\n%s", FormCursorMark, got)
	}
	// The cursor marks the FIRST field, since NewForm puts the cursor at the top.
	firstLine := strings.SplitN(got, "\n", 3)
	marked := false
	for _, l := range firstLine {
		if strings.Contains(l, FormCursorMark) && strings.Contains(l, "Name") {
			marked = true
		}
	}
	if !marked {
		t.Errorf("the cursor marker is not on the cursor's field (the first one):\n%s", got)
	}

	// The contrast: blurred, the same form draws no marker — which is exactly the
	// appearance the operator read as read-only. Asserting this keeps the two states
	// distinguishable, so the test above cannot pass for an incidental reason.
	blurred := NewForm("t", specs...)
	blurred.Width = 60
	blurred.Focused = false
	if strings.Contains(ansi.Strip(blurred.View()), FormCursorMark) {
		t.Error("a BLURRED form draws a cursor marker, so the marker no longer distinguishes the two states")
	}
}

// A FOCUSED EDITABLE FIELD SHOWS A CARET — the actual signal that it is editable.
//
// Note what is DELIBERATELY absent: the placeholder. Form.View switches on
// `focused && f.editable(kind)` to valueWithCaret BEFORE it can reach display(),
// which is the branch that would render a placeholder — so an empty, focused,
// editable field shows a caret and no hint about the expected format. That is a
// decision rather than an oversight (the comment there explains that the
// append-only editor it replaced "read as 'I can't edit anything'"), so this pins
// the caret rather than demanding a placeholder the code chose not to draw.
func TestAFocusedEditableFieldShowsACaret(t *testing.T) {
	f := NewForm("t", FieldSpec{Name: "dir", Label: "Directory", Kind: KText, Placeholder: "/home/me/projects/x"})
	f.Width = 70
	got := ansi.Strip(f.View())
	if !strings.ContainsAny(got, "▏") {
		t.Errorf("a focused editable field draws no caret, so there is nothing on the row that says text goes "+
			"here — the operator reads the whole form as read-only:\n%s", got)
	}
	// And the value it holds is still what is displayed, so making the caret visible
	// did not cost the operator sight of their own input.
	f.Set("dir", "/tmp/x")
	if got := ansi.Strip(f.View()); !strings.Contains(got, "/tmp/x") {
		t.Errorf("the field's value is not rendered alongside the caret:\n%s", got)
	}
}
