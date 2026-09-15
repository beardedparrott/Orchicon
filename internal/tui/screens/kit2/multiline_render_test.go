package kit2

// multiline_render_test.go — how a multi-line field reads on a form.
//
// The operator: "in general across ALL edit screens (work items, workers, settings), etc.
// we need a better view on multiline boxes. Editing it in a single line going back and
// forth off the screen is NOT very intuitive. We need to expand it out within the
// details/edit pane just like it would look in the GUI to see the whole text and
// formatted properly."
//
// Two defects behind that. First, a FOCUSED multi-line field was edited on ONE
// horizontally-windowed line, so prose scrolled off screen and you only ever saw a slice;
// you had to know ctrl+e to read your own text. Second, and worse, an UNFOCUSED one was
// rendered raw — display() returns the value verbatim, newlines included, into a string
// the host then pads to the pane width, so one field occupied several visual rows and
// shifted every row below it.

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// renderForm renders the form at a fixed width. View() reads f.Width (falling back to
// 64), so the width is set on the form rather than passed.
func renderForm(f *Form, width int) []string {
	f.Width = width
	return strings.Split(f.View(), "\n")
}

// A focused multi-line field is WRAPPED by default: the whole text is on screen, and its
// own line breaks survive.
func TestFocusedMultilineFieldWrapsByDefault(t *testing.T) {
	f := NewForm("Edit worker",
		FieldSpec{Name: "role", Label: "Role", Kind: KTextArea},
		FieldSpec{Name: "after", Label: "After", Kind: KText},
	)
	f.Focused = true
	f.Set("role", "First line.\nSecond line.\nThird line.")
	f.Set("after", "sentinel")

	rows := renderForm(f, 60)
	joined := strings.Join(rows, "\n")

	for _, want := range []string{"First line.", "Second line.", "Third line."} {
		if !strings.Contains(joined, want) {
			t.Errorf("the focused multi-line field did not show %q — it is windowed to one row\n%s", want, joined)
		}
	}
	// All three logical lines are on SEPARATE rows, not flattened onto one.
	seen := 0
	for _, r := range rows {
		if strings.Contains(r, "First line.") || strings.Contains(r, "Second line.") || strings.Contains(r, "Third line.") {
			seen++
		}
	}
	if seen != 3 {
		t.Errorf("expected 3 rows carrying one line each, got %d", seen)
	}
	// A field BELOW it is still rendered — the wrapped block grew the form, it did not
	// swallow its neighbours.
	if !strings.Contains(joined, "sentinel") {
		t.Errorf("the field after the wrapped block went missing:\n%s", joined)
	}
	// And the field says how to see everything.
	if !strings.Contains(joined, "ctrl+e") {
		t.Errorf("the wrapped field does not name the chord for the full view:\n%s", joined)
	}
}

// The value's own indentation is preserved: pretty-printed JSON wrapped by
// strings.Fields (what the ctrl+e path used) would collapse it.
func TestWrappedBodyPreservesIndentation(t *testing.T) {
	f := NewForm("Edit step", FieldSpec{Name: "config", Label: "Config", Kind: KJSON})
	f.Set("config", "{\n  \"a\": 1,\n  \"b\": 2\n}")

	rows, hidden := f.wrappedBody("config", 60, 10)
	if hidden != 0 {
		t.Fatalf("hidden = %d, want 0 for a short value", hidden)
	}
	if len(rows) != 4 {
		t.Fatalf("rows = %d (%q), want the value's own 4 lines", len(rows), rows)
	}
	if !strings.HasPrefix(rows[1], "  \"a\": 1,") {
		t.Errorf("indentation was collapsed: %q", rows[1])
	}
}

// A single line LONGER than the row soft-wraps instead of running off it.
func TestWrappedBodySoftWrapsALongLine(t *testing.T) {
	f := NewForm("Edit", FieldSpec{Name: "d", Label: "D", Kind: KTextArea})
	long := strings.Repeat("word ", 40) // 200 cells
	f.Set("d", long)

	rows, _ := f.wrappedBody("d", 40, 0)
	if len(rows) < 4 {
		t.Fatalf("a 200-cell line in a 40-cell row produced %d rows", len(rows))
	}
	for i, r := range rows {
		if w := lipgloss.Width(r); w > 40 {
			t.Errorf("row %d is %d cells wide, over the 40-cell budget: %q", i, w, r)
		}
	}
}

// A token with no spaces (a minified JSON blob, a URL) is hard-split rather than emitted
// over-wide.
func TestWrappedBodyHardSplitsAnUnbreakableToken(t *testing.T) {
	f := NewForm("Edit", FieldSpec{Name: "d", Label: "D", Kind: KJSON})
	f.Set("d", strings.Repeat("x", 120))

	rows, _ := f.wrappedBody("d", 30, 0)
	for i, r := range rows {
		if w := lipgloss.Width(r); w > 30 {
			t.Errorf("row %d is %d cells wide, over the 30-cell budget: %q", i, w, r)
		}
	}
}

// Rows are BOUNDED and the withheld count is stated, so one long value cannot push the
// rest of the form out of reach.
func TestWrappedBodyBoundsRowsAndSaysSo(t *testing.T) {
	f := NewForm("Edit", FieldSpec{Name: "d", Label: "D", Kind: KTextArea})
	f.Set("d", strings.Repeat("line\n", 40))

	rows, hidden := f.wrappedBody("d", 60, maxWrappedRows)
	if len(rows) != maxWrappedRows {
		t.Fatalf("rows = %d, want the %d-row bound", len(rows), maxWrappedRows)
	}
	if hidden <= 0 {
		t.Fatal("hidden = 0 but rows were withheld — the operator would lose text silently")
	}
}

// THE LAYOUT BUG. An UNFOCUSED multi-line value must not put raw newlines into a row.
func TestUnfocusedMultilineFieldIsFlattened(t *testing.T) {
	f := NewForm("Edit worker",
		FieldSpec{Name: "role", Label: "Role", Kind: KTextArea},
		FieldSpec{Name: "after", Label: "After", Kind: KText},
	)
	f.Focused = false
	f.Set("role", "one\ntwo\nthree")
	f.Set("after", "sentinel")

	rows := renderForm(f, 60)
	// The Role row must be ONE row carrying the flattened value plus a line count.
	roleRows := 0
	for _, r := range rows {
		if strings.Contains(r, "Role") {
			roleRows++
			if !strings.Contains(r, "one") {
				t.Errorf("the Role row does not show its value: %q", r)
			}
			if !strings.Contains(r, "3 lines") {
				t.Errorf("the flattened row does not say how many lines are hidden: %q", r)
			}
		}
	}
	if roleRows != 1 {
		t.Errorf("Role occupied %d rows on an unfocused form, want exactly 1\n%s", roleRows, strings.Join(rows, "\n"))
	}
	if !strings.Contains(strings.Join(rows, "\n"), "sentinel") {
		t.Error("the field after the multi-line one went missing")
	}
}

// oneLine marks the breaks rather than dropping them, so the operator can see structure.
func TestOneLineMarksBreaks(t *testing.T) {
	got := oneLine("a\nb\nc")
	if strings.Contains(got, "\n") {
		t.Fatalf("oneLine returned a newline: %q", got)
	}
	if !strings.Contains(got, "a") || !strings.Contains(got, "b") || !strings.Contains(got, "c") {
		t.Errorf("oneLine lost text: %q", got)
	}
	if !strings.Contains(got, "\u21b5") {
		t.Errorf("oneLine did not mark the break: %q", got)
	}
	// A single-line value is returned untouched.
	if oneLine("plain") != "plain" {
		t.Errorf("oneLine altered a single-line value: %q", oneLine("plain"))
	}
	// Trailing blank lines carry no information in a summary.
	if got := oneLine("a\n\n\n"); got != "a" {
		t.Errorf("oneLine kept trailing blanks: %q", got)
	}
}

// lineCount drives the "N lines" marker.
func TestLineCount(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"", 0}, {"one", 1}, {"a\nb", 2}, {"a\r\nb", 2}, {"a\n", 2},
	} {
		if got := lineCount(tc.in); got != tc.want {
			t.Errorf("lineCount(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// A single-line field is untouched: the wrapped path is for multi-line kinds only.
func TestSingleLineFieldsStillWindowWithACaret(t *testing.T) {
	f := NewForm("Edit", FieldSpec{Name: "name", Label: "Name", Kind: KText})
	f.Focused = true
	f.Set("name", "a short name")
	joined := strings.Join(renderForm(f, 60), "\n")
	if !strings.Contains(joined, "a short name") {
		t.Errorf("a KText field did not render its value:\n%s", joined)
	}
	if strings.Contains(joined, "ctrl+e") {
		t.Errorf("a single-line field offered the multiline chord:\n%s", joined)
	}
}
