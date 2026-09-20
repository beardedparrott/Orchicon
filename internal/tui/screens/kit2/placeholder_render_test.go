package kit2

// placeholder_render_test.go — AN EMPTY FIELD SHOWS ITS HINT, ON THE FIELD THE OPERATOR IS ON.
//
// The operator, on the project form: "There isn't really any guidance here." The field labels and
// placeholders had been written — and every one of them was DEAD TEXT, on every form in the client.
//
// display() is the branch that renders a placeholder, and it required the field to be the CURSOR
// (`f.current().Name == s.Name`). But the cursor's editable field never reaches display() — it
// renders through valueWithCaret. The two conditions were mutually exclusive, so no placeholder
// could ever appear on a field being typed into, which is the only field whose hint matters.

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// THE CURSOR'S EMPTY EDITABLE FIELD SHOWS ITS PLACEHOLDER, as ghost text before the caret.
func TestTheCursorsEmptyFieldShowsItsPlaceholder(t *testing.T) {
	f := NewForm("t", FieldSpec{
		Name: "slug", Label: "Slug", Kind: KText,
		Placeholder: "one absolute path per line",
	})
	f.Width = 80
	got := ansi.Strip(f.View())
	if !strings.Contains(got, "one absolute path per line") {
		t.Errorf("the cursor's empty field does not show its placeholder, so the one field the operator "+
			"is filling in is the one field with no guidance:\n%s", got)
	}
	// It is marked as GUIDANCE rather than rendered bare: a placeholder that looks like a value is
	// indistinguishable from one, and "Slug: orchicon" cannot be told from a set slug.
	if !strings.Contains(got, "(one absolute path per line)") {
		t.Errorf("the placeholder is not marked as guidance (the parentheses idiom this form already "+
			"uses for \"nothing is set\", e.g. \"(none)\"):\n%s", got)
	}
}

// AND IT IS REPLACED BY THE VALUE the moment there is one — a hint over the top of real input would
// hide what the operator typed.
func TestThePlaceholderYieldsToTheValue(t *testing.T) {
	f := NewForm("t", FieldSpec{Name: "slug", Label: "Slug", Kind: KText, Placeholder: "suggested-shape"})
	f.Width = 80
	f.Set("slug", "my-real-slug")
	got := ansi.Strip(f.View())
	if strings.Contains(got, "suggested-shape") {
		t.Errorf("the placeholder is still drawn over a set value:\n%s", got)
	}
	if !strings.Contains(got, "my-real-slug") {
		t.Errorf("the value is not drawn:\n%s", got)
	}
}

// THE FORM DOES NOT CHANGE HEIGHT when a field is filled in.
//
// This is the property that broke, in the counter-intuitive direction: the hint was added to HELP,
// and it made the form one row taller only while the field was EMPTY — so typing the first character
// SHRANK the form and everything below it jumped up. Measured on the project edit form's
// context-files field (long label, long hint): 13 rows empty, 12 filled.
//
// Two separate causes had to go, so this covers both: the 8-cell floor that forced the hint wider
// than the space (the cursor's row), and display() returning the hint UNFITTED (every other row,
// which is where this field actually rendered once the cursor had moved on).
func TestFillingAFieldDoesNotChangeTheFormsHeight(t *testing.T) {
	specs := []FieldSpec{
		{Name: "name", Label: "Name", Kind: KText, Initial: "Orchicon"},
		{Name: "ctx", Label: "Context files (abs paths inside the project dir)", Kind: KTextArea,
			Placeholder: "one path per line · a directory is read in full"},
		{Name: "tail", Label: "Project dir", Kind: KText, Placeholder: "/home/me/projects/thing"},
	}
	for _, width := range []int{60, 78, 100} {
		// PER CURSOR POSITION, not across positions. A focused multi-line field legitimately draws
		// more rows than an unfocused one — its label, its value and its enter/expand affordances —
		// so comparing heights BETWEEN cursor positions would assert a different thing. What must
		// hold is that FILLING a field changes nothing.
		for _, cursor := range []string{"ctx", "name"} {
			rows := func(value string) int {
				f := NewForm("t", specs...)
				f.Width = width
				f.Set("ctx", value)
				f.FocusName(cursor)
				return len(strings.Split(f.View(), "\n"))
			}
			empty, filled := rows(""), rows("/x")
			if empty != filled {
				t.Errorf("width %d, cursor on %q: the form is %d rows with the hint showing and %d once "+
					"filled — filling a field must not resize the form, or everything below it jumps as the "+
					"operator types", width, cursor, empty, filled)
			}
		}
	}
}

// A HINT LONGER THAN THE COLUMN IS TRUNCATED, NOT WRAPPED — and every row still fits the form.
//
// Wrapping is the failure mode that matters: a hint that wraps adds a row, which is exactly the jump
// above. The stable height must not have been bought by letting the row overflow, either: the host
// would clip the tail with nothing to say so.
func TestALongPlaceholderFitsWithoutWrapping(t *testing.T) {
	specs := []FieldSpec{
		{Name: "ctx", Label: "Context files (abs paths inside the project dir)", Kind: KText,
			Placeholder: strings.Repeat("a long hint ", 20)},
	}
	for _, width := range []int{50, 70, 90} {
		for _, cursor := range []string{"ctx", ""} {
			f := NewForm("t", specs...)
			f.Width = width
			if cursor != "" {
				f.FocusName(cursor)
			} else {
				f.Focused = false
			}
			for i, l := range strings.Split(ansi.Strip(f.View()), "\n") {
				if w := len([]rune(l)); w > width {
					t.Errorf("width %d (cursor %q): row %d is %d cells wide: %q", width, cursor, i, w, l)
				}
			}
		}
	}
}

// AND THE CARET STAYS AT THE VALUE POSITION, so the field does not jitter as the operator tabs
// between hints of different lengths.
func TestTheCaretStaysAtTheValuePosition(t *testing.T) {
	col := func(ph string) int {
		f := NewForm("t", FieldSpec{Name: "a", Label: "A", Kind: KText, Placeholder: ph})
		f.Width = 80
		for _, l := range strings.Split(ansi.Strip(f.View()), "\n") {
			if i := strings.Index(l, "▏"); i >= 0 {
				return i
			}
		}
		return -1
	}
	short, long := col("x"), col("a considerably longer placeholder than that one")
	if short < 0 || long < 0 {
		t.Fatal("no caret found on the cursor's field")
	}
	if short != long {
		t.Errorf("the caret sits at column %d for a short hint and %d for a long one — it must stay at "+
			"the value position", short, long)
	}
}
