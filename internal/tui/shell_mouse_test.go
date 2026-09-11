package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/config"
)

// TestMouseFooterChipPins the footer's "Mouse Enabled" chip (tea uses
// WithMouseCellMotion, but the shell reports its own state).
func TestMouseFooterChip(t *testing.T) {
	app := NewApp(nil, &config.Profile{URL: "http://x", Token: "t"}, "v0.2.51")
	app.RegisterScreen(TabAsk, &tabBarScreenStub{body: "b"})
	app.dispatch(tea.WindowSizeMsg{Width: 120, Height: 40})
	if !app.Footer().MouseEnabled {
		t.Fatal("footer must show MouseEnabled after window size")
	}
	v := app.Footer().View()
	if !strings.Contains(v, "Mouse Enabled") {
		t.Fatalf("footer missing Mouse Enabled chip: %q", v)
	}
}

// The conversations rail is always on for MVP1, so a click on its header must
// NOT hide it (the header is no longer a collapse affordance).
func TestMouseRailToggle(t *testing.T) {
	app := NewApp(nil, &config.Profile{URL: "http://x", Token: "t"}, "v0.2.51")
	app.RegisterScreen(TabAsk, &tabBarScreenStub{body: "b"})
	app.dispatch(tea.WindowSizeMsg{Width: 120, Height: 40})
	app.SwitchTo(TabAsk)
	if !app.railVisible() {
		t.Fatal("the conversations rail must be on for Ask (MVP1)")
	}
	nm, _ := app.dispatch(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 120 - ConversationsRailWidth + 5, Y: railTopRow})
	if !nm.railVisible() {
		t.Fatal("clicking the rail header must not hide the always-on rail")
	}
}

// TestMouseTabClickPins the tab bar mouse switch (Phase 2a: a click also
// OPENS the tab's dropdown submenu — a second click closes it again, so
// switch + close-menu is the two-step contract; tested via tabStartCol,
// the visible-column hit-test that survives centering).
func TestMouseTabClick(t *testing.T) {
	app := NewApp(nil, &config.Profile{URL: "http://x", Token: "t"}, "v0.2.51")
	app.RegisterScreen(TabAsk, &tabBarScreenStub{body: "a"})
	app.RegisterScreen(TabWork, &tabBarScreenStub{body: "w"})
	app.dispatch(tea.WindowSizeMsg{Width: 120, Height: 40})
	var work Tab
	for _, tb := range Tabs {
		if tb.ID == TabWork {
			work = tb
		}
	}
	x := app.tabStartCol(work)
	nm, _ := app.dispatch(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: x, Y: 0})
	if nm.ActiveTab() != TabWork {
		t.Fatalf("tab click: active = %s, want work", nm.ActiveTab())
	}
	if nm.MenuOpenID() != TabWork {
		t.Fatalf("tab click must open the tab's dropdown, menu = %q", nm.MenuOpenID())
	}
	// Clicking the same tab again closes its dropdown (menu remains closed,
	// active tab unchanged).
	nm, _ = nm.dispatch(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: x, Y: 0})
	if nm.ActiveTab() != TabWork || nm.MenuOpenID() != "" {
		t.Fatalf("second click must close the menu: active=%s menu=%q", nm.ActiveTab(), nm.MenuOpenID())
	}
}

// TestCtrlRTogglesRail is retired: the conversations rail is disabled, so
// ctrl+r is deliberately inert. See TestRailToggleIsInertWhileRailsAreDisabled
// in rails_layout_test.go for the pinned contract.
