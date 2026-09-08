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

// TestMouseRailTogglePins clicking the Ask rail header collapses it.
func TestMouseRailToggle(t *testing.T) {
	app := NewApp(nil, &config.Profile{URL: "http://x", Token: "t"}, "v0.2.51")
	app.RegisterScreen(TabAsk, &tabBarScreenStub{body: "b"})
	app.dispatch(tea.WindowSizeMsg{Width: 120, Height: 40})
	app.SwitchTo(TabAsk)
	if !app.railVisible() {
		t.Fatal("rail should be open by default")
	}
	// Click the rail header (absolute row 2, right columns).
	nm, _ := app.dispatch(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 120 - ConversationsRailWidth + 5, Y: 2})
	if nm.railVisible() {
		t.Fatal("click on rail header must collapse the rail")
	}
}

// TestMouseTabClickPins the tab bar mouse switch.
func TestMouseTabClick(t *testing.T) {
	app := NewApp(nil, &config.Profile{URL: "http://x", Token: "t"}, "v0.2.51")
	app.RegisterScreen(TabAsk, &tabBarScreenStub{body: "a"})
	app.RegisterScreen(TabWork, &tabBarScreenStub{body: "w"})
	app.dispatch(tea.WindowSizeMsg{Width: 120, Height: 40})
	x := strings.Index(app.tabBarView(), "2·Work")
	nm, _ := app.dispatch(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: x, Y: 0})
	if nm.ActiveTab() != TabWork {
		t.Fatalf("tab click: active = %s, want work", nm.ActiveTab())
	}
}

// TestCtrlRTogglesRailPins the documented ctrl+r rail toggle.
func TestCtrlRTogglesRail(t *testing.T) {
	app := NewApp(nil, &config.Profile{URL: "http://x", Token: "t"}, "v0.2.51")
	app.RegisterScreen(TabAsk, &tabBarScreenStub{body: "b"})
	app.dispatch(tea.WindowSizeMsg{Width: 120, Height: 40})
	app.SwitchTo(TabAsk)
	if !app.railVisible() {
		t.Fatal("rail should be open by default")
	}
	app.dispatch(tea.KeyMsg{Type: tea.KeyCtrlR})
	if app.railVisible() {
		t.Fatal("ctrl+r must collapse the rail")
	}
	app.dispatch(tea.KeyMsg{Type: tea.KeyCtrlR})
	if !app.railVisible() {
		t.Fatal("ctrl+r must re-open the rail")
	}
}
