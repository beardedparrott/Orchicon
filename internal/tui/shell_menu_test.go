package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/config"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// TestTabDropdownOpensAndSelects is the Phase-2a dropdown acceptance gate:
// every tab opens a dropdown of its sub-screens (entries from the nav
// registry = the screen inventory), arrows + enter select, esc closes,
// and a different tab's chord re-points the menu.
func TestTabDropdownOpensAndSelects(t *testing.T) {
	m := newTestApp()
	// Open Work's dropdown (the same open path the mouse route + chord
	// re-press use).
	m.openTabMenu(TabWork)
	tm := m.TabMenu()
	if tm == nil || m.MenuOpenID() != TabWork {
		t.Fatalf("openTabMenu must open Work's dropdown (menu=%q)", m.MenuOpenID())
	}
	if len(tm.Entries) == 0 {
		t.Fatal("Work's dropdown must list its sub-screens from the nav registry")
	}
	// Entries must mirror the nav registry for the tab (no drift).
	want := m.navEntries(TabWork)
	if len(tm.Entries) != len(want) {
		t.Fatalf("dropdown entries = %d, want %d (nav registry)", len(tm.Entries), len(want))
	}
	for i, e := range want {
		if tm.Entries[i].Cmd != e.Cmd || tm.Entries[i].Label != e.Label {
			t.Fatalf("dropdown row %d = %q/%q, want %q/%q", i, tm.Entries[i].Cmd, tm.Entries[i].Label, e.Cmd, e.Label)
		}
	}
	// The rendered menu overlays the view: header (tab title ▾) + rows.
	m.width, m.height = 120, 40
	v := m.View()
	if !strings.Contains(v, "Work ▾") {
		t.Fatalf("dropdown header missing from view: %q", v[:200])
	}
	for _, e := range tm.Entries {
		if !strings.Contains(v, e.Label) {
			t.Errorf("dropdown row %q missing from view", e.Label)
		}
	}
	// esc closes.
	nm, _ := m.dispatch(tea.KeyMsg{Type: tea.KeyEsc})
	m = nm
	if m.MenuOpenID() != "" {
		t.Fatal("esc must close the dropdown")
	}
	// Up/down + enter select: down moves the selection, enter selects +
	// closes (the row's source is selected on the tab's screen).
	m.openTabMenu(TabWork)
	tm = m.TabMenu()
	if tm == nil || len(tm.Entries) == 0 {
		t.Fatal("menu must be open with entries")
	}
	nm2, _ := m.dispatch(tea.KeyMsg{Type: tea.KeyDown})
	m = nm2
	nm3, _ := m.dispatch(tea.KeyMsg{Type: tea.KeyEnter})
	m = nm3
	if m.MenuOpenID() != "" {
		t.Fatal("enter must close the menu after selecting")
	}
}

// TestWorkMenuSubScreens pins the mockup's example: Projects / Work Items
// come from the Work tab's Sources() (the dropdown's no-drift source), and
// the mockup's Runtime Images row exists as /runtime-images (Control
// source, docs/tui-parity.md inventory — the registry cannot drift).
func TestWorkMenuSubScreens(t *testing.T) {
	m := newTestApp()
	var labels []string
	for _, e := range m.navEntries(TabWork) {
		labels = append(labels, e.Label)
	}
	joined := strings.Join(labels, "|")
	for _, want := range []string{"Projects", "Work Items"} {
		if !strings.Contains(joined, want) {
			t.Errorf("Work menu missing %q (entries: %v)", want, labels)
		}
	}
	ri := m.slash.resolve("/runtime-images")
	if ri == nil {
		t.Fatal("mockup Work Menu row Runtime Images missing (/runtime-images unregistered)")
	}
}

// TestThemeCommandPins the /theme command: no arg lists the themes with an
// active marker, /theme light switches the palette (styles re-derive),
// /theme bogus errors, and the choice persists to the profile.
func TestThemeCommand(t *testing.T) {
	m := newDockTestApp()
	// list
	m.dispatchSlash("/theme")
	if !strings.Contains(m.dock.Notice, "dark") || !strings.Contains(m.dock.Notice, "light") {
		t.Fatalf("/theme must list themes, got %q", m.dock.Notice)
	}
	// switch
	handled, _ := m.dispatchSlash("/theme light")
	if !handled {
		t.Fatal("/theme light must dispatch")
	}
	if theme.Active().Name != "light" {
		t.Fatalf("active theme = %q, want light", theme.Active().Name)
	}
	if strings.Contains(theme.ScreenBg.Render("x"), "\x1b[48;5;232m") {
		t.Fatal("light theme must repaint the background (dark bg still active)")
	}
	if m.profile.Theme != "light" {
		t.Fatalf("profile.Theme = %q, want light (persisted)", m.profile.Theme)
	}
	// unknown
	m.dispatchSlash("/theme bogus")
	if !strings.Contains(m.dock.Err, "bogus") {
		t.Fatalf("unknown theme must error, got %q", m.dock.Err)
	}
	// back to dark
	m.dispatchSlash("/theme dark")
	if theme.Active().Name != "dark" {
		t.Fatal("/theme dark must switch back")
	}
}

// TestThemeRegistryAndConfig pins the theme registry: the TUI-owned palette set
// (the launch default + light + the gruvbox pair), selectable via config
// profile.Theme. Themes are TUI palettes validated for terminal contrast, not a
// copy of the GUI's CSS tokens.
//
// THE DEFAULT IS ASSERTED BY NAME, not by position: the palette registry is in REGISTRY order (the
// original family list), and the launch default is a separate choice — currently Ember, at the
// operator's request. Expecting the default to be names[0] would be asserting the registry's
// ordering, which is a different fact.
func TestThemeRegistryAndConfig(t *testing.T) {
	names := theme.Names()
	if len(names) < 2 {
		t.Fatalf("theme names = %v, want at least dark+light", names)
	}
	// The base palette is always registered and always listed, so switching off the default always
	// has somewhere to land.
	listed := map[string]bool{}
	for _, n := range names {
		listed[n] = true
	}
	if !listed["dark"] {
		t.Fatalf("theme names = %v, want the base dark palette listed", names)
	}
	for _, want := range []string{"dark", "light", theme.DefaultName} {
		if theme.Lookup(want) == nil {
			t.Errorf("theme %q must be registered", want)
		}
	}
	if theme.DefaultName != "ember" {
		t.Fatalf("default theme = %q, want ember (the operator's request)", theme.DefaultName)
	}
	if !theme.Use("light") {
		t.Fatal("theme.Use(light) must switch")
	}
	if theme.Use("bogus") {
		t.Fatal("theme.Use(bogus) must be rejected")
	}
	theme.Use("dark")
	// config: a profile with Theme=light must apply it at NewApp time.
	_ = NewApp(&client.Clients{}, &config.Profile{URL: "http://x", Token: "t", Theme: "light"}, "v0.2.51")
	if theme.Active().Name != "light" {
		t.Fatalf("config Theme=light must apply at NewApp, got %q", theme.Active().Name)
	}
	theme.Use("dark")
}
