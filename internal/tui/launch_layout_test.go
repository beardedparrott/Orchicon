package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// The launch layout (opencode-style): with no session yet, Ask renders the
// brand + the composer centered in the viewport instead of a docked composer
// under an empty transcript. Once a session exists it reverts to the docked
// layout.
func TestLaunchLayoutCentersComposerUntilSessionStarts(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {120, 40}} {
		w, h := size[0], size[1]
		m := phase3App(w, h) // Ask, no session
		if !m.welcomeMode() {
			t.Fatalf("%dx%d: Ask with no session must be in the launch layout", w, h)
		}

		v := m.View()
		lines := strings.Split(v, "\n")
		if len(lines) != h {
			t.Fatalf("%dx%d: launch frame = %d rows, want %d", w, h, len(lines), h)
		}
		for i, l := range lines {
			if got := lipgloss.Width(l); got != w {
				t.Fatalf("%dx%d: launch row %d width %d, want %d", w, h, i, got, w)
			}
		}

		plain := lipglossStrip(v)
		if !strings.Contains(plain, "██") {
			t.Fatalf("%dx%d: launch layout is missing the block-letter brand", w, h)
		}
		if !strings.Contains(plain, "Ask Orchicon anything") {
			t.Fatalf("%dx%d: launch layout is missing the tagline", w, h)
		}
		// The conversations list must NOT be on the launch screen.
		if strings.Contains(plain, "Conversations") {
			t.Fatalf("%dx%d: the conversations rail must be hidden at launch", w, h)
		}
		if m.railVisible() {
			t.Fatalf("%dx%d: railVisible must be false in the launch layout", w, h)
		}
		// The composer must be OUT of the bottom dock block: the dock rows are
		// the last dock.Lines() rows above the footer.
		dockRows := m.dock.Lines()
		inDock := false
		for i := h - 1 - dockRows; i <= h-2 && i >= 0; i++ {
			if strings.Contains(lipglossStrip(lines[i]), "❯") {
				inDock = true
			}
		}
		if inDock {
			t.Fatalf("%dx%d: the launch composer must be centered, not in the dock block", w, h)
		}
		if !strings.Contains(plain, "❯") {
			t.Fatalf("%dx%d: launch layout is missing the composer prompt", w, h)
		}

		// Starting a session reverts to the docked transcript layout.
		m.chatConvID = "conv-1"
		if m.welcomeMode() {
			t.Fatalf("%dx%d: an open session must leave the launch layout", w, h)
		}
	}
}
