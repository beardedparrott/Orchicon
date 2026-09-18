package tui

// modal_test.go — the shell's modal COMPOSITION: a modal must be a solid, uniform rectangle, and
// splicing one over the frame must not disturb a single cell outside it.
//
// THE OPERATOR'S REPORT: "Weird visual bug when renaming a conversation. Probably should just be its
// own modal with a solid background. This also happens when applying to a category."
//
// TWO DEFECTS, both measured before they were fixed:
//
//  1. overlayRow took the BOX's maximum line width and used it as the width of EVERY row. A kit2
//     form's View is not a rectangle by design (title and hint rows are written at their natural
//     width, field rows are padded), so a SHORT row was spliced as if it were a wide one and `keep`
//     landed past the end of the glyphs — deleting the base cells in between and returning a row
//     shorter than the frame. fillView then padded at the FAR RIGHT, so everything after the modal
//     shifted sideways. Measured: a 5-cell row in a 25-cell box turned a 40-cell row into a 25-cell
//     one, and 18 base cells survived inside the box's rectangle.
//
//  2. The modals spliced that raw ragged form directly, so the base showed through inside the modal.
//     They now go through modalPanel, which wraps the body in a kit2.Panel (a rectangle by
//     construction: every interior row padded and painted with the opaque screen background).

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// splitLines splits a rendered frame into its rows (the shell's own frames never contain a trailing
// newline, so this is a plain split).
func splitLines(s string) []string { return strings.Split(s, "\n") }

// panelWidth is a panel's ON-SCREEN width. It uses lipgloss.Width, which reports the WIDEST LINE of a
// multi-line string, and NOT ansi.StringWidth, which SUMS every line of one: using the summing form
// here reported 432 for a 72-cell panel and sent the rectangle arithmetic off the map.
func panelWidth(panel string) int { return lipgloss.Width(panel) }

// changedColumns returns the indices at which two equally-wide strings differ.
func changedColumns(a, b string) map[int]bool {
	ra, rb := []rune(ansi.Strip(a)), []rune(ansi.Strip(b))
	out := map[int]bool{}
	n := len(ra)
	if len(rb) > n {
		n = len(rb)
	}
	for i := 0; i < n; i++ {
		var x, y rune = ' ', ' '
		if i < len(ra) {
			x = ra[i]
		}
		if i < len(rb) {
			y = rb[i]
		}
		if x != y {
			out[i] = true
		}
	}
	return out
}

// TestOverlayRowPreservesEveryCell pins the splice primitive, which is the root cause of the
// sideways shift: a ragged overlay must consume exactly its OWN cells and keep every cell after it.
func TestOverlayRowPreservesEveryCell(t *testing.T) {
	row := strings.Repeat("X", 40)

	// A SHORT line inside a box whose maximum width is much larger — the exact shape a kit2 form
	// produces (its title row is 19 cells while its field row is 69).
	got := overlayRow(row, "short", 10)

	if w := ansi.StringWidth(got); w != 40 {
		t.Fatalf("the row must keep its width: got %d, want 40 (the old code returned 25 here, which "+
			"shifted every later cell of the frame)", w)
	}
	want := strings.Repeat("X", 10) + "short" + strings.Repeat("X", 25)
	if ansi.Strip(got) != want {
		t.Fatalf("the splice lost or moved base cells:\n got %q\nwant %q", ansi.Strip(got), want)
	}
}

// TestOverlayRowAtTheEdges: left past the end, and an overlay that fills the row.
func TestOverlayRowAtTheEdges(t *testing.T) {
	// Overlay starting beyond the row: the gap is filled and nothing is dropped.
	got := overlayRow("abc", "Z", 6)
	if ansi.Strip(got) != "abc   Z" {
		t.Fatalf("a splice past the end must pad the gap, got %q", ansi.Strip(got))
	}
	// Overlay exactly covering the row: no suffix, no loss.
	got = overlayRow("abc", "ZZZ", 0)
	if ansi.Strip(got) != "ZZZ" {
		t.Fatalf("a full-width overlay must replace the row, got %q", ansi.Strip(got))
	}
}

// TestModalPanelIsASolidUniformRectangle: the operator's "just be its own modal with a solid
// background". Every line of a panel is the same width and the interior is painted opaquely, so
// nothing behind it can show through.
func TestModalPanelIsASolidUniformRectangle(t *testing.T) {
	m := railApp(t, 2)

	// A body with deliberately ragged lines, exactly like Form.View().
	body := strings.Join([]string{
		"Rename conversation",
		"Title *: " + strings.Repeat("a long value ", 8),
		"",
		"ctrl+s: save",
	}, "\n")

	want := m.modalWidth()
	panel := m.modalPanel(body, want)
	lines := splitLines(panel)
	if len(lines) == 0 {
		t.Fatal("the panel rendered nothing")
	}
	for i, l := range lines {
		if got := ansi.StringWidth(l); got != want {
			t.Fatalf("panel line %d is %d cells, want %d — a panel must be a rectangle:\n%q",
				i, got, want, ansi.Strip(l))
		}
	}
	// TOP AND BOTTOM BORDERS: it reads as a modal, not as loose text.
	top, bottom := ansi.Strip(lines[0]), ansi.Strip(lines[len(lines)-1])
	if !strings.HasPrefix(top, "┌") || !strings.HasSuffix(top, "┐") {
		t.Fatalf("the panel must have a top border, got %q", top)
	}
	if !strings.HasPrefix(bottom, "└") || !strings.HasSuffix(bottom, "┘") {
		t.Fatalf("the panel must have a bottom border, got %q", bottom)
	}
	// The panel is sized to its CONTENT, not to the viewport: a viewport-tall panel would paint a
	// wall of background over the screen behind it.
	if len(lines) != 4+2 {
		t.Fatalf("the panel must be its content plus two border rows, got %d lines", len(lines))
	}
}

// TestRenameModalCoversExactlyItsRectangle is the decisive assertion for the reported bug.
//
// It renders the frame with the modal open and again with it closed, and requires that the only cells
// that changed are the ones inside the modal's own rectangle. That catches BOTH defects at once: base
// content surviving inside (an unchanged cell where the modal should be) and cells shifting outside it
// (a change where the modal is not).
func TestRenameModalCoversExactlyItsRectangle(t *testing.T) {
	m := railApp(t, 3)
	m.width, m.height = 120, 36
	m.conversations[0].Title = "I would like to create a simple work item that doesn't inact any change and keeps going"
	m.convSel = 0

	without := splitLines(m.viewFrame())
	m.openRenameConversation(m.conversations[0].ID, m.conversations[0].Title)
	// OPENING THE MODAL LEGITIMATELY CHANGES ONE ROW OUTSIDE ITS RECTANGLE: the composer's affordance
	// row is refreshed to describe the FORM's keys (ctrl+s / esc) while a form holds the keyboard. That
	// is deliberate and tested elsewhere, so this test isolates the OVERLAY's contribution by holding
	// the hint fixed: the form is detached for the 'before' frame and re-attached for the 'after' one.
	// What remains must be the modal, and nothing but the modal.
	form := m.renameConv
	m.renameConv = nil
	without = splitLines(m.viewFrame())
	m.renameConv = form
	with := splitLines(m.viewFrame())

	if len(with) != len(without) {
		t.Fatalf("the modal changed the frame height: %d vs %d", len(with), len(without))
	}
	panel := m.modalPanel(m.renameConv.View(), m.modalWidth())
	pw := panelWidth(panel)
	ph := len(splitLines(panel))
	top := (m.height - ph) / 2
	left := (m.width - pw) / 2

	for i := range with {
		if got := ansi.StringWidth(with[i]); got != m.width {
			t.Fatalf("row %d is %d cells after compositing, want %d", i, got, m.width)
		}
		if i < top || i >= top+ph {
			// NOT a modal row: it must be byte-identical to the un-modal frame — a change here is the
			// sideways shift the operator saw.
			if ansi.Strip(with[i]) != ansi.Strip(without[i]) {
				t.Fatalf("row %d is OUTSIDE the modal and changed — the composition shifted content:\n modal off %q\n modal on  %q",
					i, ansi.Strip(without[i]), ansi.Strip(with[i]))
			}
			continue
		}
		// A MODAL row: every changed cell must be inside the modal's own columns.
		for c := range changedColumns(without[i], with[i]) {
			if c < left || c >= left+pw {
				t.Fatalf("row %d column %d changed but is OUTSIDE the modal [%d,%d):\n modal off %q\n modal on  %q",
					i, c, left, left+pw, ansi.Strip(without[i]), ansi.Strip(with[i]))
			}
		}
	}
}

// TestAssignModalIsAlsoASolidRectangle: "This also happens when applying to a category." The assign
// modal is a hosted form like the rename box, and it must be composited the same way — the report
// named both, so both are pinned.
func TestAssignModalIsAlsoASolidRectangle(t *testing.T) {
	m, _ := categoryApp(t, convCat("cat-1", "Research"))
	m = loadCats(t, m)
	m.width, m.height = 120, 36
	m.attachRail(3)

	without := splitLines(m.viewFrame())
	m.OpenAssignCategory("conv-01", "conversation 01", convCat("cat-1", "Research").GetTargetType())
	// Same isolation as the rename test: opening a form refreshes the composer's affordance row to
	// describe the FORM's keys, which is deliberate and changes a row outside the modal. The form is
	// detached for the 'before' frame so what remains is the overlay's own contribution.
	form := m.assignForm
	m.assignForm = nil
	without = splitLines(m.viewFrame())
	m.assignForm = form
	with := splitLines(m.viewFrame())

	panel := m.modalPanel(m.assignForm.View(), m.modalWidth())
	pw := panelWidth(panel)
	ph := len(splitLines(panel))
	top := (m.height - ph) / 2
	left := (m.width - pw) / 2

	if m.assignForm == nil {
		t.Fatal("the assign modal must be open")
	}
	for i, l := range splitLines(panel) {
		if got := ansi.StringWidth(l); got != pw {
			t.Fatalf("assign panel line %d is %d cells, want %d", i, got, pw)
		}
	}
	for i := range with {
		if ansi.StringWidth(with[i]) != m.width {
			t.Fatalf("row %d is %d cells, want %d", i, ansi.StringWidth(with[i]), m.width)
		}
		if i < top || i >= top+ph {
			if ansi.Strip(with[i]) != ansi.Strip(without[i]) {
				t.Fatalf("row %d is outside the assign modal and changed:\n off %q\n on  %q",
					i, ansi.Strip(without[i]), ansi.Strip(with[i]))
			}
			continue
		}
		for c := range changedColumns(without[i], with[i]) {
			if c < left || c >= left+pw {
				t.Fatalf("row %d column %d changed outside the assign modal [%d,%d)", i, c, left, left+pw)
			}
		}
	}
}

// TestModalLeavesNoBaseContentVisible is the operator's ACTUAL complaint, and it is a SEPARATE property
// from "nothing outside the rectangle moved": "Probably should just be its own modal with a solid
// background."
//
// A raw ragged form satisfies the rectangle test above trivially: its short rows leave the base showing
// INSIDE its own bounding box, which changes nothing outside the rectangle and so trips no assertion.
// This test fills the base with a marker glyph the modal never renders, so anything surviving inside
// the modal's rectangle is base content bleeding through — the exact shape of the operator's
// screenshot, where the screen's ASCII banner and the composer box appeared inside the rename box.
//
// It exercises the PAIR the modals use (modalPanel over overlayCentered against a marker base) rather
// than the whole shell frame, because the base is what has to be controlled for the assertion to mean
// anything: an earlier version of this test registered a marker-filled stub screen and silently
// measured an empty wall, since the Ask LAUNCH PAGE is drawn by the shell and not by that screen.
func TestModalLeavesNoBaseContentVisible(t *testing.T) {
	const marker = "≈"
	m := newTestApp()
	m.width, m.height = 80, 24

	rows := make([]string, m.height)
	for i := range rows {
		rows[i] = strings.Repeat(marker, m.width)
	}
	base := strings.Join(rows, "\n")

	// A deliberately ragged body, exactly like Form.View().
	body := strings.Join([]string{
		"Rename conversation",
		"Title *: " + strings.Repeat("value ", 20),
		"ctrl+s: save · esc: cancel",
	}, "\n")
	panel := m.modalPanel(body, m.modalWidth())
	pw := panelWidth(panel)
	ph := len(splitLines(panel))
	top := (m.height - ph) / 2
	left := (m.width - pw) / 2

	out := splitLines(m.overlayCentered(base, panel))
	for i := 0; i < ph; i++ {
		r := top + i
		if r < 0 || r >= len(out) {
			continue
		}
		plain := []rune(ansi.Strip(out[r]))
		for c := left; c < left+pw && c < len(plain); c++ {
			if string(plain[c]) == marker {
				t.Fatalf("base content is visible INSIDE the modal at row %d col %d — the modal is not solid:\n%q",
					r, c, ansi.Strip(out[r]))
			}
		}
	}
}

// TestRenameModalActuallyDrawsThePanel pins the WIRING, which the two tests above cannot: they test
// modalPanel and overlayCentered in isolation, so a modal that went back to splicing its raw form
// would still pass both. Here the REAL frame is compared against the panel row by row — if the modal
// stops going through the panel, the frame's rows stop matching it.
//
// Proven by disabling: reverting renameConvView to splice m.renameConv.View() directly fails this with
// the frame's row and the panel's row printed side by side.
func TestRenameModalActuallyDrawsThePanel(t *testing.T) {
	m := railApp(t, 2)
	m.width, m.height = 120, 36
	m.conversations[0].Title = "a title long enough that the form's field row is the widest line in it"
	m.openRenameConversation(m.conversations[0].ID, m.conversations[0].Title)

	// SET THE FORM'S WIDTH EXACTLY AS THE HOST DOES, before rendering the expectation.
	//
	// This is not incidental setup. renameConvView assigns the form the modal's INNER
	// width during the frame render, and the FOCUSED row is rendered PADDED TO THAT
	// WIDTH (theme.ListItemSelected.Render(Pad(l, width))) while an unfocused row is not
	// padded at all. So a panel computed at the form's pre-render width is a different
	// panel from the one the frame paints, and the comparison fails on the padded row.
	//
	// That went unnoticed while forms defaulted to UNFOCUSED and no row was padded. It
	// surfaced when NewForm began defaulting Focused to true — which is the change that
	// made this modal render a cursor marker at all, having never done so before.
	m.renameConv.Width = m.modalInnerWidth()
	panel := m.modalPanel(m.renameConv.View(), m.modalWidth())
	panelRows := splitLines(panel)
	pw := panelWidth(panel)
	left := (m.width - pw) / 2
	top := (m.height - len(panelRows)) / 2

	frame := splitLines(m.viewFrame())
	for i, want := range panelRows {
		r := top + i
		if r < 0 || r >= len(frame) {
			continue
		}
		got := []rune(ansi.Strip(frame[r]))
		w := []rune(ansi.Strip(want))
		for c := 0; c < pw; c++ {
			gc, wc := ' ', ' '
			if left+c < len(got) {
				gc = got[left+c]
			}
			if c < len(w) {
				wc = w[c]
			}
			if gc != wc {
				t.Fatalf("the rendered modal does not match the panel at row %d col %d: frame %q, panel %q",
					r, c, string(gc), string(wc))
			}
		}
	}
}

// TestModalFormIsGivenTheInnerWidth: a form laid out to the modal's OUTER width overflows the panel by
// exactly its two border cells, and the panel then truncates the right-hand columns of every field.
func TestModalFormIsGivenTheInnerWidth(t *testing.T) {
	m := railApp(t, 2)
	m.openRenameConversation("conv-01", "a title")
	m.renameConvView("base", m.width, m.height)
	if got, want := m.renameConv.Width, m.modalInnerWidth(); got != want {
		t.Fatalf("the form must be laid out to the panel's interior: got %d, want %d", got, want)
	}
	if m.modalInnerWidth() >= m.modalWidth() {
		t.Fatal("the inner width must be smaller than the outer width")
	}
}
