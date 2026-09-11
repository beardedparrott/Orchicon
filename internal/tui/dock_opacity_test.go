package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// gapCount counts cells that render on the TERMINAL's background: a run of
// printable/space cells after an SGR reset that is not immediately re-opened
// by another escape.
func gapCount(s string) int {
	gaps := 0
	rest := s
	for {
		i := strings.Index(rest, "\x1b[0m")
		if i < 0 {
			return gaps
		}
		rest = rest[i+len("\x1b[0m"):]
		for j := 0; j < len(rest); j++ {
			c := rest[j]
			if c == 0x1b {
				break
			}
			if c == '\n' || c == '\r' {
				continue
			}
			gaps++
		}
	}
}

// The composer must be fully opaque in BOTH states — with the placeholder
// (nothing typed) and with typed text — because the operator reported a hole
// peering into the terminal in both. The placeholder row and the input row
// carry styled spans whose resets would otherwise clear the box background.
func TestComposerIsOpaqueEmptyAndTyping(t *testing.T) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(termenv.Ascii) })

	for _, size := range [][2]int{{80, 24}, {120, 40}} {
		w, h := size[0], size[1]

		m := phase3App(w, h)
		m.setFocus(focusComposer)
		m.dock.Width = m.contentWidth()

		if g := gapCount(m.dock.View()); g > 0 {
			t.Errorf("%dx%d: EMPTY composer leaves %d unpainted cells:\n%q", w, h, g, m.dock.View())
		}

		for _, r := range "hello" {
			m.dispatch(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		}
		if g := gapCount(m.dock.View()); g > 0 {
			t.Errorf("%dx%d: TYPING leaves %d unpainted cells:\n%q", w, h, g, m.dock.View())
		}

		// And the whole frame must be opaque too (the shell's own pass).
		if g := gapCount(m.View()); g > 0 {
			t.Errorf("%dx%d: shell frame leaves %d unpainted cells", w, h, g)
		}
	}
}
