// NOTE — the Tab-in-a-form conflict is now RESOLVED, deliberately.
//
// This file used to carry a note explaining that the Work screen did not implement
// FormOpen(), so Tab walked OUT of an inline editor instead of advancing its fields, and
// that fixing it meant choosing between two conflicting operator asks recorded in the tree
// (router.go's Tab chord + worker_crud_test.go versus rails_layout_test.go's
// TestTabYieldsOnlyToAWindowedModalForm).
//
// The operator settled it: "ONCE IN EDIT/NEW MODE using down/up OR tab/shift+tab should
// move through the edit items as opposed to the top menu bar on every screen. Once you
// ctrl+s to save or hit Esc to get out of the editing mode, tab/shift+tab now affects the
// top tab menu again." Work now implements FormOpen(), and the stale rails_layout
// assertion was rewritten to the resolved rule (both halves: Tab goes to the form while it
// is open, and to the bar once it closes).
package tui

// work_menu_probe_test.go — the Work tab's MENU and the shell's keys must own the
// keyboard. (The screen-side regression lives in
// internal/tui/screens/work/keyclaim_test.go, where the action builder is reachable.)
//
// Operator report: "I still can't go into reverse tab (shift+tab) once I hit the
// Work/Projects screen. I also can't hit enter on any submenu under Work nor can I hit
// the up/down arrow keys on the Work menu."
//
// Root cause, one line: projectActions() — called to build the footer hints and key
// bindings — set m.formLoading = true as a side effect and nothing ever cleared it.
// ClaimsKeys() includes formLoading, and dispatch ran the claims gate BEFORE its menu
// handling, so on the Projects source the shell handed EVERY key to the screen: the
// submenu's arrows and Enter never reached menuHandleKey, and shift+tab (a global route,
// later still) was swallowed outright.
//
// These tests pin the ORDERING, against a screen that claims unconditionally, so the
// arrangement that turned one stray assignment into a dead tab bar cannot come back.

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/screens/work"
)

// claimingScreen claims every key, like a screen with a latched form/filter state.
type claimingScreen struct {
	stubScreen
}

func (s *claimingScreen) ClaimsKeys() bool { return true }

// realWorkApp wires the REAL Work screen, the way app.go does.
func realWorkApp(t *testing.T) *App {
	t.Helper()
	m := newTestApp()
	m.RegisterScreen(TabWork, work.New(nil, m.reg, ""))
	m.dispatch(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.SwitchTo(TabWork)
	return m
}

// An open submenu must outrank a screen's key claim: the menu is drawn on top and is
// what the operator just opened and is looking at.
func TestOpenSubmenuOutranksAScreenKeyClaim(t *testing.T) {
	m := newTestApp()
	cs := &claimingScreen{stubScreen: stubScreen{id: "work"}}
	m.RegisterScreen(TabWork, cs)
	m.dispatch(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.SwitchTo(TabWork)

	if !cs.ClaimsKeys() {
		t.Fatal("fixture: the screen under test must claim keys")
	}
	m.setFocus(focusTabs)
	m.openTabMenu(TabWork)
	if m.TabMenu() == nil {
		t.Fatal("fixture: the menu did not open")
	}
	if len(m.TabMenu().Entries) < 2 {
		t.Fatal("fixture: need >= 2 entries to test navigation")
	}

	before := m.TabMenu().Sel
	m.dispatch(tea.KeyMsg{Type: tea.KeyDown})
	if m.TabMenu() == nil {
		t.Fatal("down-arrow closed the submenu")
	}
	if m.TabMenu().Sel == before {
		t.Fatal("down-arrow did not move the OPEN submenu: the screen-claims gate is running " +
			"before the menu, which makes the menu inert whenever a screen claims keys")
	}
	m.dispatch(tea.KeyMsg{Type: tea.KeyEnter})
	if m.TabMenu() != nil {
		t.Fatal("Enter did not select the OPEN submenu entry: the screen-claims gate outranks the menu")
	}
	// The claim must still hold afterwards — the menu stole only its own keys.
	if !cs.ClaimsKeys() {
		t.Fatal("fixture: the screen stopped claiming, so this proves nothing")
	}
}

// The menu must NOT swallow keys it does not own: with the menu open over a claiming
// screen, a plain letter still belongs to the screen.
func TestOpenSubmenuLeavesTypingToTheScreen(t *testing.T) {
	m := newTestApp()
	cs := &claimingScreen{stubScreen: stubScreen{id: "work"}}
	m.RegisterScreen(TabWork, cs)
	m.dispatch(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.SwitchTo(TabWork)
	m.setFocus(focusTabs)
	m.openTabMenu(TabWork)

	m.dispatch(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("z")})
	if cs.lastKey != "z" {
		t.Fatalf("a plain letter did not reach the claiming screen (lastKey=%q) — "+
			"the menu must only take the keys it owns", cs.lastKey)
	}
}

// Shift+tab closes an open submenu (menuHandleKey's established behaviour), and it must
// do so even while the screen claims keys — it sits in the same handler as the arrows.
func TestOpenSubmenuShiftTabOutranksAClaim(t *testing.T) {
	m := newTestApp()
	m.RegisterScreen(TabWork, &claimingScreen{stubScreen: stubScreen{id: "work"}})
	m.dispatch(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.SwitchTo(TabWork)
	m.setFocus(focusTabs)
	m.openTabMenu(TabWork)
	if m.TabMenu() == nil {
		t.Fatal("fixture: the menu did not open")
	}
	m.dispatch(tea.KeyMsg{Type: tea.KeyShiftTab})
	if m.TabMenu() != nil {
		t.Fatal("shift+tab did not reach the open submenu while a screen claimed keys")
	}
}

// With the real Work screen, the submenu drives from a resting pane.
func TestWorkSubmenuOwnsArrowsAndEnter(t *testing.T) {
	m := realWorkApp(t)
	m.setFocus(focusTabs)
	m.openTabMenu(TabWork)
	if m.TabMenu() == nil {
		t.Fatal("fixture: the Work menu did not open")
	}
	if len(m.TabMenu().Entries) < 2 {
		t.Fatal("fixture: Work menu needs >= 2 entries")
	}
	before := m.TabMenu().Sel
	m.dispatch(tea.KeyMsg{Type: tea.KeyDown})
	if m.TabMenu() == nil {
		t.Fatal("the dropdown CLOSED on down-arrow — the key never reached it")
	}
	if m.TabMenu().Sel == before {
		t.Fatal("down-arrow did not move the Work dropdown selection")
	}
	m.dispatch(tea.KeyMsg{Type: tea.KeyUp})
	if m.TabMenu().Sel != before {
		t.Fatal("up-arrow did not restore the Work dropdown selection")
	}
	m.dispatch(tea.KeyMsg{Type: tea.KeyEnter})
	if m.TabMenu() != nil {
		t.Fatal("Enter did not select the highlighted Work submenu entry")
	}
}

// Shift+tab reaches the shell's reverse ring from the Work tab: it is a global route,
// which sits behind the claims gate.
func TestWorkTabDoesNotSwallowShiftTab(t *testing.T) {
	m := realWorkApp(t)
	m.setFocus(focusTabs)
	start := m.ActiveTab()
	m.dispatch(tea.KeyMsg{Type: tea.KeyShiftTab})
	if got := m.ActiveTab(); got == start {
		t.Fatalf("shift+tab did nothing from the Work tab (still %q)", got)
	}
}

// The Work screen must report an open form, so the shell's Tab chord routes Tab to the
// FORM's fields rather than walking the bar. This is the resolved Tab-in-a-form rule; the
// complementary assertion (that Tab returns to the bar once the form closes) lives in
// rails_layout_test.go, which drives it through the real dispatch.
func TestWorkScreenReportsOpenForms(t *testing.T) {
	m := realWorkApp(t)
	ws, ok := m.screens[TabWork].(interface{ FormOpen() bool })
	if !ok {
		t.Fatal("the Work screen does not implement FormOpen() — the shell's Tab chord cannot " +
			"tell an open editor from a resting pane, so Tab escapes the form")
	}
	if ws.FormOpen() {
		t.Fatal("a resting pane must not report an open form")
	}
}
