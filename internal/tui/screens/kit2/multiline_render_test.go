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
	"github.com/charmbracelet/x/ansi"
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

	rows, above, below := f.wrappedBody("config", 60, 10)
	if above != 0 || below != 0 {
		t.Fatalf("hidden = %d/%d, want 0/0 for a short value", above, below)
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

	rows, _, _ := f.wrappedBody("d", 40, 0)
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

	rows, _, _ := f.wrappedBody("d", 30, 0)
	for i, r := range rows {
		if w := lipgloss.Width(r); w > 30 {
			t.Errorf("row %d is %d cells wide, over the 30-cell budget: %q", i, w, r)
		}
	}
}

// Rows are BOUNDED and the withheld count is stated, so one long value cannot push the
// rest of the form out of reach.
// TestWrappedBodyBoundsRowsAndSaysSo: a value taller than the window is BOUNDED,
// and the rows withheld are reported rather than dropped silently. The window is
// now centered on the CARET, so the number withheld is split between the rows above
// and below it.
func TestWrappedBodyBoundsRowsAndSaysSo(t *testing.T) {
	f := NewForm("Edit", FieldSpec{Name: "d", Label: "D", Kind: KTextArea})
	f.Set("d", strings.Repeat("line\n", 40))

	rows, above, below := f.wrappedBody("d", 60, maxWrappedRows)
	if len(rows) != maxWrappedRows {
		t.Fatalf("rows = %d, want the %d-row bound", len(rows), maxWrappedRows)
	}
	if above+below <= 0 {
		t.Fatal("nothing reported as out of view but rows were withheld — the operator would lose text silently")
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

// A FOCUSED MULTI-LINE FIELD GROWS TO FIT THE PANE.
//
// The row budget used to be a flat 6 regardless of how much pane there was, so
// the operator's Dockerfile override — 39 lines in a 40-row terminal — showed 6
// of them and read as a box that had "scrunch[ed] down into a smaller text box".
// A fixed bound is most wrong exactly where the value is long, which is when the
// operator most needs to see it.
//
// The height comes from the HOST (kit2.Base sets Form.Height from the pane); a
// host that supplies none keeps the conservative bound, which is what modal
// forms rely on.
func TestFocusedMultiLineFieldGrowsToTheFormHeight(t *testing.T) {
	var dockerfile strings.Builder
	dockerfile.WriteString("FROM golang:1.24\n")
	for i := 0; i < 38; i++ {
		dockerfile.WriteString("RUN echo step\n")
	}

	// The field's VALUE BLOCK is the rows strictly between its label row and its
	// hint row. Counting rows that carry text would undercount by one now that the
	// window follows the caret: the caret's own row holds only the caret.
	valueBlockRows := func(f *Form) int {
		f.Focused = true
		f.FocusName("dockerfile_override")
		rows := renderForm(f, 176)
		label, hint := -1, -1
		for i, r := range rows {
			s := strings.TrimSpace(ansi.Strip(r))
			if label < 0 && strings.HasPrefix(s, "▸ Dockerfile override") {
				label = i
			}
			if label >= 0 && hint < 0 && strings.HasPrefix(s, "Dockerfile override:") {
				hint = i
			}
		}
		if label < 0 || hint < 0 {
			t.Fatalf("could not locate the field's label/hint rows:\n%s", strings.Join(rows, "\n"))
		}
		return hint - label - 1
	}

	tall := NewForm("Edit runtime image",
		FieldSpec{Name: "name", Label: "Name", Kind: KText, Initial: "img"},
		FieldSpec{Name: "dockerfile_override", Label: "Dockerfile override", Kind: KTextArea, Initial: dockerfile.String()},
		FieldSpec{Name: "tag", Label: "Tag", Kind: KText, Initial: "t"},
	)
	tall.Height = 37 // a 40-row terminal: 2 border rows + 1 hint row are not ours
	got := valueBlockRows(tall)
	if got <= maxWrappedRows {
		t.Fatalf("a focused 39-line field showed %d rows with a 37-row pane — it is still clamped to the "+
			"fixed %d-row bound, so the operator cannot read what they are editing", got, maxWrappedRows)
	}
	// It really is using the room: the pane's height minus the reserve.
	if want := tall.focusedWrapRows(); got != want {
		t.Errorf("value rows shown = %d, want %d (the pane's budget)", got, want)
	}

	// A host that supplies no height keeps the old, conservative bound.
	modal := NewForm("Edit runtime image",
		FieldSpec{Name: "dockerfile_override", Label: "Dockerfile override", Kind: KTextArea, Initial: dockerfile.String()},
	)
	if got := valueBlockRows(modal); got != maxWrappedRows {
		t.Errorf("with no host height, value rows = %d, want the conservative %d", got, maxWrappedRows)
	}
}

// THE VALUE GETS THE PANE'S WIDTH, NOT THE LABEL'S.
//
// The value used to start on the label's row and indent every continuation to the
// label's width — 23 cells for "dockerfile_override" — so a code block lost a
// fifth of the pane to a label it already had above it. The label now has its own
// row and the value is indented a fixed two cells.
//
// The value here is 85 cells: it fits in 100 with a 2-cell indent (1 row) and
// does NOT fit with the old 23-cell indent (2 rows), so the row count is the
// assertion that tells the two apart.
func TestFocusedMultiLineValueUsesThePaneWidth(t *testing.T) {
	long := strings.Repeat("x", 85)
	f := NewForm("Edit",
		FieldSpec{Name: "dockerfile_override", Label: "Dockerfile override", Kind: KTextArea, Initial: long},
	)
	f.Focused, f.Width, f.Height = true, 100, 30
	f.FocusName("dockerfile_override")

	rows := renderForm(f, 100)
	carrying := 0
	for _, r := range rows {
		if strings.Contains(ansi.Strip(r), "xxxx") {
			carrying++
		}
	}
	if carrying != 1 {
		t.Errorf("the 85-cell value occupied %d rows at width 100 — it wrapped, so it is still being "+
			"indented by the label's width instead of a fixed two cells", carrying)
	}
	// And the value starts two cells in, under the label's own row.
	for _, r := range rows {
		s := ansi.Strip(r)
		if strings.Contains(s, "xxxx") {
			if !strings.HasPrefix(s, "  x") {
				t.Errorf("value row = %q, want it indented two cells", s)
			}
			break
		}
	}
}

// THE HOST MUST TELL THE FORM HOW MUCH PANE IT HAS.
//
// This is the WIRING test, and it is the one that matters: the two above set
// Form.Height directly, so they stay green even if the host stops supplying it —
// which is exactly what happened when I removed the assignment in
// Base.detailPaneView to check the tests bit. Nothing failed. The rendered pane
// is what the operator actually sees, so the assertion is made on it.
func TestDetailEditHostScrollsATallFocusedFieldIntoView(t *testing.T) {
	var dockerfile strings.Builder
	dockerfile.WriteString("FROM golang:1.24\n")
	for i := 0; i < 38; i++ {
		dockerfile.WriteString("RUN echo step\n")
	}

	b := &Base{}
	b.HideSources = true
	b.SetSize(180, 40) // the operator's terminal

	f := NewForm("Edit runtime image",
		FieldSpec{Name: "name", Label: "Name", Kind: KText, Initial: "img"},
		FieldSpec{Name: "dockerfile_override", Label: "Dockerfile override", Kind: KTextArea, Initial: dockerfile.String()},
		FieldSpec{Name: "tag", Label: "Tag", Kind: KText, Initial: "t"},
	)
	f.Focused = true
	f.FocusName("dockerfile_override")
	b.BeginDetailEdit("Edit runtime image", f)

	// Render first: the host supplies the height while laying the pane out.
	view := b.View()

	if f.Height <= 0 {
		t.Fatal("the host did not tell the form its height, so a focused field cannot know how much " +
			"room it has and falls back to the fixed 6-row bound")
	}

	rows := 0
	for _, r := range strings.Split(view, "\n") {
		if strings.Contains(r, "FROM ") || strings.Contains(r, "RUN ") {
			rows++
		}
	}
	if rows <= maxWrappedRows {
		t.Fatalf("the rendered pane shows %d value rows of a 39-line Dockerfile on a 40-row terminal — "+
			"the form was not given the pane's height", rows)
	}
	// And the field's own line is on screen, so the operator can see where they are.
	if !strings.Contains(view, "Dockerfile override") {
		t.Error("the focused field's label is not on the rendered pane")
	}
}
