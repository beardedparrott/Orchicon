package kit2

// placeholder_render_test.go — AN EMPTY FIELD SHOWS ITS HINT, ON THE FIELD THE OPERATOR IS ON.

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
func TestFillingAFieldDoesNotChangeTheFormsHeight(t *testing.T) {
	specs := []FieldSpec{
		{Name: "name", Label: "Name", Kind: KText, Initial: "Orchicon"},
		{Name: "ctx", Label: "Context files (abs paths inside the project dir)", Kind: KTextArea,
			Placeholder: "one path per line · a directory is read in full"},
		{Name: "tail", Label: "Project dir", Kind: KText, Placeholder: "/home/me/projects/thing"},
	}
	for _, width := range []int{60, 78, 100} {
		// PER CURSOR POSITION, not across positions.
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
