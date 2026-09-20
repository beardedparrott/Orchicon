package tui

// composer_select_fill_test.go — CTRL+A HAS TO LOOK LIKE A SELECTION.
//
// The operator, on the composer in the TUI:
//
//	"when I hit ctrl+a in the composer it DOES select all but it doesn't actually show the cursor highlight
//	 over all of the text, it just gives you a little message that the text is highlighted. This needs to be
//	 fixed."
//
// The sibling file (composer_select_all_test.go) pins the STATE — the flag, the notice, the clipboard — and it
// was GREEN on the broken build, because not one of its assertions looks at a rendered cell. That is how a
// missing highlight survived a suite that covers the feature: every test asked "did the chord register?" and
// none asked "can you see it?".
//
// These are RENDER assertions, deliberately. The bug was in the render path, so the render path is what they
// read.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// composerUnderColour puts the process into a COLOUR-CAPABLE profile and the default palette before a composer
// is built.
//
// BOTH HALVES ARE LOAD-BEARING. lipgloss STRIPS colour when the terminal profile carries none — which is the
// profile a test binary has by default, and which other tests in this package (composer_bleed_test,
// dock_opacity_test) deliberately leave behind — so without TrueColor every assertion below would pass
// vacuously against colourless output, including the ones that are supposed to FAIL. And theme is
// package-level state, so a test that switched palette earlier would change the fill these tests measure.
func composerUnderColour(t *testing.T) {
	t.Helper()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(termenv.Ascii) })
	theme.Use(theme.DefaultName)
	t.Cleanup(func() { theme.Use(theme.DefaultName) })
}

// selectionFillSeq is the exact escape prefix theme.ComposerSelect emits, MEASURED off a render rather than
// restated here — so a palette change cannot make these assertions pass by matching a sequence nothing paints.
func selectionFillSeq(t *testing.T) string {
	t.Helper()
	rendered := theme.ComposerSelect.Render("MARK")
	i := strings.Index(rendered, "MARK")
	if i <= 0 {
		t.Fatalf("theme.ComposerSelect painted nothing measurable (%q), so the composer has no selection fill to "+
			"show — this is the operator's report at the level it exists", rendered)
	}
	return rendered[:i]
}

// NOTHING IS FILLED UNTIL SOMETHING IS SELECTED — the fill is the selection's, not the composer's.
func TestTheComposerIsUnfilledBeforeSelection(t *testing.T) {
	composerUnderColour(t)
	m := dockWith(t, "a draft I might want to copy")
	if strings.Contains(m.View(), selectionFillSeq(t)) {
		t.Error("the composer is painted on the selection fill before anything is selected, so the fill no " +
			"longer means \"the whole buffer is selected\"")
	}
}

// THE OPERATOR'S TEXT IS INSIDE THE FILL. "Over all of the text" is the requirement, so the assertion is that
// the characters are INSIDE the fill — not merely that the sequence appears somewhere on the frame, which an
// empty row would satisfy.
func TestSelectAllFillsTheOperatorText(t *testing.T) {
	composerUnderColour(t)
	m := dockWith(t, "a draft I might want to copy")
	m.SelectAll()

	view := m.View()
	open := selectionFillSeq(t)
	if !strings.Contains(view, open) {
		t.Fatalf("ctrl+a marked the composer selected but not one row is painted on the selection fill: the only "+
			"evidence of the selection is the notice strip, which is exactly the operator's report. view=%q", view)
	}
	if !strings.Contains(view, open+"❯ a draft I might want to copy") {
		t.Errorf("the operator's text is not inside the selection fill — the highlight does not cover what it "+
			"claims to have selected: %q", view)
	}
}

// EVERY ROW OF A MULTI-LINE DRAFT IS FILLED. A selection that covered only the first row would look like a
// partial selection on a draft the operator broke across lines.
func TestAMultiLineDraftIsFilledOnEveryRow(t *testing.T) {
	composerUnderColour(t)
	m := dockWith(t, "first line\nsecond line")
	m.SelectAll()
	view := m.View()
	if n := strings.Count(view, selectionFillSeq(t)); n < 2 {
		t.Errorf("only %d row(s) carry the selection fill, want at least 2 for a two-line draft — \"all of the "+
			"text\" has to mean all of it: %q", n, view)
	}
}

// AND THE FILL GOES AWAY WITH THE SELECTION, or the composer would read as permanently selected and the next
// keystroke would look like it was about to wipe the draft.
func TestDroppingTheSelectionDropsTheFill(t *testing.T) {
	composerUnderColour(t)
	m := dockWith(t, "a draft I might want to copy")
	m.SelectAll()
	if !strings.Contains(m.View(), selectionFillSeq(t)) {
		t.Fatal("fixture: the fill was never drawn, so dropping it proves nothing")
	}
	m.ClearSelectAll()
	if strings.Contains(m.View(), selectionFillSeq(t)) {
		t.Error("the fill survived the selection being dropped")
	}
}

// AND A KEY THAT REPLACES THE SELECTION IS ITSELF THE END OF IT — the state cannot outlive the gesture.
func TestTypingOverTheSelectionDropsTheFill(t *testing.T) {
	composerUnderColour(t)
	m := dockWith(t, "a draft I might want to copy")
	m.SelectAll()
	pressDock(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	if strings.Contains(m.View(), selectionFillSeq(t)) {
		t.Error("the fill survived the key that replaced the selection")
	}
	if got := m.Value(); got != "n" {
		t.Errorf("the replacing key left %q, want the rune alone", got)
	}
}
