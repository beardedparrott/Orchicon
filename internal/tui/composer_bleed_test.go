package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// Regression for the operator's "blue box riding off the pane": the composer's
// composed rows ended with an OPEN surface-background SGR (the repair
// re-asserted the box background before a line break, and at the block's end).
// The shell pads every row out to the terminal width, so that open background
// leaked onto the padding — a band of the box's colour extending right, past
// its border.
//
// The contract: a composed row must never END with an unterminated SGR, so
// whatever the caller appends inherits the caller's own background, not the
// block's.
func TestComposedRowsDoNotLeakTheirBackground(t *testing.T) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(termenv.Ascii) })

	m := phase3App(120, 40)
	d := m.dock
	d.Width = 76

	for i, line := range strings.Split(d.View(), "\n") {
		if strings.HasSuffix(line, "\x1b[0m") {
			continue // properly terminated
		}
		t.Fatalf("dock row %d does not end with a reset — its background leaks onto the caller's padding:\n%q", i, line)
	}

	// And the composed launch block must be clean too.
	for i, line := range m.centeredWelcomeView(120, 30) {
		if line == "" {
			continue
		}
		if !strings.HasSuffix(line, "\x1b[0m") && strings.Contains(line, "\x1b[") {
			t.Fatalf("launch row %d leaves an open SGR:\n%q", i, line)
		}
	}
}

// Regression for the operator's "when you start typing in a text box, it
// becomes black and screws up the theme color".
//
// bubbles renders the CURSOR LINE's trailing padding with computedEndOfBuffer()
// and nothing else, and its default EndOfBuffer carries a FOREGROUND only. With
// no background on those cells they fall through to the terminal's own
// background — a black band appearing the instant text exists. And because
// bubbles takes a separate placeholderView() path for an empty buffer, the
// empty box looked right while the typed one did not.
func TestComposerKeepsThemeBackgroundWhileTyping(t *testing.T) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(termenv.Ascii); theme.Use(theme.DefaultName) })

	for _, name := range []string{"dark", "gruvbox-dark", "violet", "slate-light"} {
		if !theme.Use(name) {
			t.Fatalf("theme %q not registered", name)
		}
		m := newTestApp()
		m.width, m.height = 100, 24
		m.dock.Width = 96
		m.dock.ApplyTheme()
		m.setFocus(focusComposer)
		for _, r := range "hello" {
			nm, _ := m.dispatch(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
			m = nm
		}

		out := m.dock.View()
		if !strings.Contains(out, "hello") {
			t.Fatalf("%s: typed text missing from the composer", name)
		}
		// No cell may render on the terminal's own background.
		gaps := 0
		rest := out
		for {
			i := strings.Index(rest, "\x1b[0m")
			if i < 0 {
				break
			}
			rest = rest[i+len("\x1b[0m"):]
			for j := 0; j < len(rest); j++ {
				if rest[j] == 0x1b {
					break
				}
				if rest[j] != '\n' && rest[j] != '\r' {
					gaps++
				}
			}
		}
		if gaps > 0 {
			t.Errorf("%s: typing leaves %d unpainted cells (the black band)", name, gaps)
		}
		// Every row must end terminated, so the fill cannot leak either.
		for i, line := range strings.Split(out, "\n") {
			if !strings.HasSuffix(line, "\x1b[0m") {
				t.Errorf("%s: composer row %d leaves an open SGR:\n%q", name, i, line)
			}
		}
	}
}

// The composer shows a plain cursor, not placeholder text (the operator: the
// "ask orchicon…" hint was redundant now that the affordance row carries the
// hints, and bubbles renders the placeholder through a different path).
func TestComposerHasNoPlaceholder(t *testing.T) {
	m := newTestApp()
	if got := m.dock.Placeholder(); got != "" {
		t.Fatalf("composer placeholder = %q, want empty (a plain cursor)", got)
	}
}
