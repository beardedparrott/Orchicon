// NOTE (deliberately NOT fixed here): this screen does not implement FormOpen(), which
// every other screen does. The shell's Tab hard chord treats a missing interface as "no
// form open" and moves the ring, so on Work Tab walks out of an inline editor instead of
// advancing its fields. That is a real gap, but it is NOT the bug this file is about, and
// fixing it means choosing between two conflicting operator asks that are both recorded:
//
//   - internal/tui/router.go's Tab chord: "when in an edit form, tab should move through
//     the fields of the form just like up/down keys", with the note that the earlier rule
//     "let Tab ESCAPE an inline editor"; internal/tui/screens/execution/worker_crud_test.go
//     asserts the same ("FormOpen must report an open INLINE editor — otherwise Tab
//     escapes it").
//   - internal/tui/rails_layout_test.go's TestTabYieldsOnlyToAWindowedModalForm: "An
//     INLINE details-pane editor must NOT take Tab ... it leaves for the bar", quoting
//     "when inside an edit form, I feel this should be the one place where tab should
//     overwrite the tabbing through menus option".
//
// Both are operator preferences from different times. Adding the method here silently
// picks one, so it is left for an explicit decision rather than smuggled in beside a
// menu fix. The absence is also why the rails_layout test passes today — it is satisfied
// by the missing method, not by design.
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

// The screen deliberately does NOT implement FormOpen() — see the note at the top of this
// file. If that changes, this test is the place to assert the chosen semantics.
func TestWorkScreenLacksFormOpenByDesign(t *testing.T) {
	m := realWorkApp(t)
	if _, ok := m.screens[TabWork].(interface{ FormOpen() bool }); ok {
		t.Fatal("the Work screen now implements FormOpen() — that silently chooses one of the " +
			"two conflicting Tab-in-a-form asks recorded in rails_layout_test.go and router.go; " +
			"make the choice deliberate and update the stale test")
	}
}
