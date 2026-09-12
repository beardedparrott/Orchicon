package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// Regression for the operator's "every domain is blank / nothing is here".
//
// SwitchTo stages the newly-activated screen's first load (it cannot return a
// tea.Cmd itself). The chord routes happened to flush that staged cmd, but the
// MOUSE tab-click path returned nil — so clicking a tab never ran the screen's
// load and the pane rendered its empty state forever. Update() now flushes
// whatever a handler staged, so every path is covered.
func TestMouseTabClickRunsScreenFirstLoad(t *testing.T) {
	m := newTestApp()
	work := &loadCountScreen{}
	m.RegisterScreen(TabWork, work)
	m.dispatch(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.SwitchTo(TabAsk)

	// Find the Work tab's rendered column and click it (row 0 = tab bar).
	var x int
	for _, tb := range Tabs {
		if tb.ID == TabWork {
			x = m.tabStartCol(tb)
		}
	}
	_, cmd := m.Update(tea.MouseMsg{
		Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: x, Y: 0,
	})
	if work.inits != 1 {
		t.Fatalf("clicking the Work tab must run its first load (inits=%d)", work.inits)
	}
	if cmd == nil {
		t.Fatal("clicking a tab must return the screen's load cmd")
	}
}

// The dropdown path (a tab chord while a menu is open) switched tabs and
// dropped the staged load the same way.
func TestMenuChordRunsScreenFirstLoad(t *testing.T) {
	m := newTestApp()
	exec := &loadCountScreen{}
	m.RegisterScreen(TabExecution, exec)
	m.dispatch(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.SwitchTo(TabAsk)
	m.openTabMenu(TabAsk)

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlE})
	if exec.inits != 1 {
		t.Fatalf("a chord from the open menu must run the screen's first load (inits=%d)", exec.inits)
	}
	if cmd == nil {
		t.Fatal("the menu chord path must return the load cmd")
	}
}

// Regression: clicking a pane must move focus to the CONTENT, so the screen's
// own keys (v for the work-item view, arrows for the list cursor, n/e/x for
// create/edit/delete) reach it instead of being typed into the chat composer —
// which is the launch focus and otherwise keeps every key for itself.
//
// Update has a VALUE receiver, so every assertion reads the RETURNED model.
func TestClickingAPaneFocusesContent(t *testing.T) {
	m := newTestApp()
	m.RegisterScreen(TabWork, &stubScreen{id: "work"})
	m.dispatch(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.SwitchTo(TabWork)
	if m.chatFocus != focusComposer {
		t.Fatal("precondition: the composer is the launch focus")
	}

	nm, _ := m.Update(tea.MouseMsg{
		Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 20, Y: 10,
	})
	m2 := nm.(*App)
	if m2.chatFocus != focusContent {
		t.Fatal("clicking a pane must move keyboard focus to the content")
	}
	if m2.footer.ComposerFocus {
		t.Fatal("the footer must reflect the content focus after a pane click")
	}

	// Clicking the composer moves focus back.
	nm, _ = m2.Update(tea.MouseMsg{
		Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
		X: 20, Y: m2.height - 2,
	})
	m3 := nm.(*App)
	if m3.chatFocus != focusComposer {
		t.Fatal("clicking the dock must return focus to the composer")
	}
}
