package tui

// slash_after_text_test.go — A SLASH MID-SENTENCE IS PUNCTUATION, NOT A COMMAND.
//
// The operator: "When typing in the composer, if you type a / in your prompt after there is already
// text in the screen, we should assume the user is NOT trying to run a slash command and should NOT
// pop up the slash command reference box."
//
// The palette is a MODAL over a live transcript, so opening it unasked over someone's sentence is
// the steals-your-keystrokes class of annoyance — and a slash is ordinary in a path, a date or
// "and/or". The rule is the composer's state, not the character: an EMPTY composer opening with "/"
// is someone starting a command; anything else is prose.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// THE PALETTE OPENS ON A SLASH IN AN EMPTY COMPOSER, which is the case the reference box is for.
func TestSlashOpensThePaletteOnAnEmptyComposer(t *testing.T) {
	m := newTestApp()
	m.dock.Focus()
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	app := nm.(*App)
	if !app.palette.PaletteOpen() {
		t.Fatalf("a slash in an empty composer did not open the palette — there would be no way to "+
			"discover or run a command. dock=%q", app.dock.Value())
	}
	// The "/" is still TYPED, so the operator's "/pro" stays visible in the bar while the palette
	// filters on it (the behaviour that fix was for).
	if got := app.dock.Value(); !strings.HasPrefix(got, "/") {
		t.Errorf("dock = %q, want the typed slash to remain in the buffer", got)
	}
}

// AND IT DOES NOT OPEN WHEN THE COMPOSER ALREADY HAS TEXT — the operator's rule, and the case they
// hit. The slash must also land in the buffer as a literal character, not be swallowed.
func TestSlashDoesNotOpenThePaletteAfterText(t *testing.T) {
	m := newTestApp()
	m.dock.Focus()
	m.dock.SetValue("see the docs")
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	app := nm.(*App)

	if app.palette.PaletteOpen() {
		t.Errorf("typing \"/\" after existing text opened the slash reference box. A slash mid-sentence "+
			"is punctuation — a path, a date, \"and/or\" — and a modal over the operator's own sentence "+
			"takes the keystrokes they meant to type. dock=%q", app.dock.Value())
	}
	if got := app.dock.Value(); got != "see the docs/" {
		t.Errorf("dock = %q, want the slash appended as text (%q)", got, "see the docs/")
	}
}

// A BUFFER OF ONLY WHITESPACE COUNTS AS EMPTY, matching the rule the rail's chords use — otherwise
// a stray space blocks every command until the operator notices and clears it.
func TestSlashOpensThePaletteOnAWhitespaceOnlyComposer(t *testing.T) {
	m := newTestApp()
	m.dock.Focus()
	m.dock.SetValue("   ")
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	if app := nm.(*App); !app.palette.PaletteOpen() {
		t.Errorf("a slash after only whitespace did not open the palette; dock=%q", app.dock.Value())
	}
}
