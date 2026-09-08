package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestDiffPaneMouseTabSwitchPersistsThroughNav: a mouse click on the pane's
// "tree" tab switches the tab, and that choice survives navigating to another
// screen and back (the acceptance criterion: sidebar open state persists
// while navigating between screens). This exercises the terminal-global
// row/col offsets the shell forwards for pane clicks (pane tab bar sits at
// terminal row 2, below the shell tab bar + its bottom border).
func TestDiffPaneMouseTabSwitchPersistsThroughNav(t *testing.T) {
	m := newTestApp()
	ex := &diffStubOwner{detailID: "exec-1"}
	m.RegisterScreen(TabExecution, ex)
	m.setFocus(focusContent)
	m.width, m.height = 120, 40
	m.SwitchTo(TabExecution)
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'D'}})
	m = nm.(*App)
	m.width, m.height = 120, 40
	m.reflowForDiff()

	// Click the "tree" tab at the pane's tab-bar row (terminal row 2) and
	// terminal column 10 (the "tree" label text, right of the left border).
	m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 10, Y: 2})
	m.syncDiffPaneState()
	if m.diffTab != "tree" {
		t.Fatalf("mouse click did not switch pane to tree (shell diffTab=%s)", m.diffTab)
	}

	// Navigate away and back; the pane must still be on the tree tab.
	m.SwitchTo(TabWork)
	m.SwitchTo(TabExecution)
	if m.diffPane.Tab != "tree" {
		t.Fatalf("pane tab %s after navigation; want tree (state did not persist)", m.diffPane.Tab)
	}
}
