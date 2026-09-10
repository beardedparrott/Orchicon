package tui

// phase3_test.go — the Phase-3 acceptance render tests for the six fixes:
// full-screen takeover / overlay discipline, the connect modal, tab
// submenus (key + mouse), palette input discipline, and full-viewport
// screens. These are the STRING-level layout gates; the real-pty gate in
// orch_pty_phase3_test.go proves the live behavior.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/config"
)

func phase3App(w, h int) *App {
	m := NewApp(&client.Clients{}, &config.Profile{Name: "default", URL: "https://x.example.com"}, "v9.9.9")
	m.dispatch(tea.WindowSizeMsg{Width: w, Height: h})
	m.SwitchTo(TabAsk)
	return m
}

// TestPaletteTypingOwnedByComposer pins finding 4: the bottom composer owns
// ALL typing — "/pro" stays visible in the bar while the palette above
// filters live; up/down move the selection (consumed, never typed); esc
// closes the palette without losing the text.
func TestPaletteTypingOwnedByComposer(t *testing.T) {
	m := phase3App(120, 40)
	for _, r := range "/pro" {
		nm, _ := m.dispatch(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = nm
	}
	if !m.palette.PaletteOpen() {
		t.Fatal("/ must open the palette")
	}
	if got := m.dock.Value(); got != "/pro" {
		t.Fatalf("composer value = %q, want %q (the bar owns the typing)", got, "/pro")
	}
	if len(m.palette.filter) == 0 {
		t.Fatal("palette must filter live on the composer text")
	}
	for _, c := range m.palette.filter {
		if !strings.Contains(c.Name, "pro") {
			t.Fatalf("candidate %q does not match the typed \"pro\"", c.Name)
		}
	}
	// The typed text is visible in the composer line of the rendered frame.
	if v := m.View(); !strings.Contains(lipglossStrip(v), "❯ /pro") {
		t.Fatalf("composer line must show the typed /pro while the palette is open")
	}
	// Up/down move the palette selection and are never inserted in the bar.
	before := m.palette.sel
	m.paletteSelect(1)
	if m.palette.sel == before && len(m.palette.filter) > 1 {
		t.Fatal("down must move the palette selection")
	}
	nm, _ := m.dispatch(tea.KeyMsg{Type: tea.KeyDown})
	m = nm
	if got := m.dock.Value(); got != "/pro" {
		t.Fatalf("arrow keys must be consumed, never typed into the bar: %q", got)
	}
	// esc closes the palette; the typed text survives.
	nm, _ = m.dispatch(tea.KeyMsg{Type: tea.KeyEsc})
	m = nm
	if m.palette.PaletteOpen() {
		t.Fatal("esc must close the palette")
	}
	if got := m.dock.Value(); got != "/pro" {
		t.Fatalf("esc must not drop the composer text: %q", got)
	}
}

// TestSubmenuOpensByKeyAndSelects pins finding 3's key path: Enter on the
// active tab opens its dropdown, arrows move, Enter selects (navigating),
// Esc closes.
func TestSubmenuOpensByKeyAndSelects(t *testing.T) {
	m := phase3App(120, 40)
	if len(m.navEntries(m.active)) < 2 {
		t.Skip("active tab has no submenu entries to exercise")
	}
	nm, _ := m.dispatch(tea.KeyMsg{Type: tea.KeyEnter})
	m = nm
	if m.MenuOpenID() != m.active {
		t.Fatalf("enter must open the active tab's submenu (menuOpen=%q)", m.MenuOpenID())
	}
	tm := m.TabMenu()
	want := tm.Entries[1]
	nm, _ = m.dispatch(tea.KeyMsg{Type: tea.KeyDown})
	m = nm
	if m.TabMenu().Sel != 1 {
		t.Fatalf("down must move the dropdown selection (sel=%d)", m.TabMenu().Sel)
	}
	nm, _ = m.dispatch(tea.KeyMsg{Type: tea.KeyEnter})
	m = nm
	if m.MenuOpenID() != "" {
		t.Fatalf("enter must select + close the dropdown (menuOpen=%q)", m.MenuOpenID())
	}
	if s := m.screens[m.active]; s != nil {
		if ar, ok := s.(interface{ ActiveSourceName() string }); ok && ar.ActiveSourceName() != want.Source {
			t.Fatalf("selected entry must navigate: active source %q, want %q", ar.ActiveSourceName(), want.Source)
		}
	}
	// Space also opens it (second activation key), then esc closes.
	nm, _ = m.dispatch(tea.KeyMsg{Type: tea.KeySpace})
	m = nm
	if m.MenuOpenID() != m.active {
		t.Fatalf("space must open the active tab's submenu (menuOpen=%q)", m.MenuOpenID())
	}
	nm, _ = m.dispatch(tea.KeyMsg{Type: tea.KeyEsc})
	m = nm
	if m.MenuOpenID() != "" {
		t.Fatal("esc must close the dropdown")
	}
	// A non-empty composer keeps Enter as send (no surprise menu).
	m.dock.SetValue("hello")
	nm, _ = m.dispatch(tea.KeyMsg{Type: tea.KeyEnter})
	m = nm
	if m.MenuOpenID() != "" {
		t.Fatal("enter with a non-empty composer must send, not open the submenu")
	}
}

// TestSubmenuMouseEntriesAreHitTargets pins the mouse half of finding 3:
// clicking entry i (the geometry the panel is painted at) selects entry i.
// Regression: the old mapping was off by two rows (it treated the header
// as entry 0), so no entry was ever clickable.
func TestSubmenuMouseEntriesAreHitTargets(t *testing.T) {
	m := phase3App(120, 40)
	m.openTabMenu(m.active)
	tm := m.TabMenu()
	if tm == nil || len(tm.Entries) == 0 {
		t.Skip("no submenu entries on this tab")
	}
	top, left := m.menuGeometry()
	if m.menuEntryRow(tm, top+1) != -1 {
		t.Fatal("the header row must not be an entry hit target")
	}
	// Entry i is painted at top+2+i (border, header, then rows).
	nm, _ := m.dispatchMouse(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: left + 2, Y: top + 2})
	m = nm
	if m.MenuOpenID() != "" {
		t.Fatal("clicking an entry must activate + close the dropdown")
	}
}

// TestConnectOverlayIsModal pins finding 2: while the overlay is open every
// key reaches the connection form — the shell's global routes (tab chords,
// rail toggle, help, quit) are suspended; esc alone cancels.
func TestConnectOverlayIsModal(t *testing.T) {
	m := phase3App(120, 40)
	m.openConnectOverlay()
	if !m.ConnectOverlayOpen() {
		t.Fatal("overlay must be open")
	}
	active := m.active
	rail := m.convRailOpen
	for _, k := range []tea.KeyMsg{
		{Type: tea.KeyCtrlW}, {Type: tea.KeyCtrlR}, {Type: tea.KeyRunes, Runes: []rune{'?'}},
		{Type: tea.KeyRunes, Runes: []rune{'q'}}, {Type: tea.KeyRight},
	} {
		nm, _ := m.dispatch(k)
		m = nm
	}
	if !m.ConnectOverlayOpen() {
		t.Fatal("global keys must not close the overlay (only esc cancels)")
	}
	if m.active != active {
		t.Fatalf("tab chord leaked through the modal: active=%q", m.active)
	}
	if m.convRailOpen != rail {
		t.Fatal("ctrl+r leaked through the modal (rail toggled)")
	}
	if m.help.open {
		t.Fatal("? leaked through the modal (help opened)")
	}
	if m.quitting {
		t.Fatal("q leaked through the modal (quit)")
	}
	// esc alone cancels.
	nm, _ := m.dispatch(tea.KeyMsg{Type: tea.KeyEsc})
	m = nm
	if m.ConnectOverlayOpen() {
		t.Fatal("esc must cancel the overlay")
	}
}

// TestViewCoversViewportWithOverlays is the Phase-3 takeover gate: on every
// tab, in every overlay state, at 80x24 and 120x40, the view is EXACTLY
// height rows of EXACTLY width cells — no overlay may open a hole or change
// the row count.
func TestViewCoversViewportWithOverlays(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {120, 40}} {
		w, h := size[0], size[1]
		for _, tab := range Tabs {
			states := map[string]func(m *App){
				"base":    func(m *App) {},
				"palette": func(m *App) { m.dock.SetValue("/pro"); m.openPalette() },
				"submenu": func(m *App) { m.openTabMenu(m.active) },
				"help":    func(m *App) { m.help.open = true },
				"connect": func(m *App) { m.openConnectOverlay() },
			}
			for name, apply := range states {
				m := phase3App(w, h)
				m.SwitchTo(tab.ID)
				apply(m)
				v := m.View()
				lines := strings.Split(v, "\n")
				if len(lines) != h {
					t.Errorf("%s/%s %dx%d: %d rows, want exactly %d", tab.ID, name, w, h, len(lines), h)
					continue
				}
				for i, l := range lines {
					if got := lipgloss.Width(l); got != w {
						t.Errorf("%s/%s %dx%d row %d: width %d, want %d", tab.ID, name, w, h, i, got, w)
						break
					}
				}
				// Chrome stays painted first/last in every non-centered state.
				if name == "base" || name == "palette" || name == "submenu" {
					if !strings.Contains(lines[0], tab.Title) {
						t.Errorf("%s/%s: tab bar missing from row 0", tab.ID, name)
					}
					if !strings.Contains(lipglossStrip(lines[h-1]), "·") {
						t.Errorf("%s/%s: footer missing from the last row", tab.ID, name)
					}
				}
			}
		}
	}
}

// TestScreensFillContentRegion pins finding 5: every screen renders its
// panes across the FULL content region the shell budgeted (height minus tab
// bar/footer/dock), never a content-sized box at the top-left.
func TestScreensFillContentRegion(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {120, 40}} {
		w, h := size[0], size[1]
		for _, tab := range Tabs {
			m := phase3App(w, h)
			m.SwitchTo(tab.ID)
			s := m.screens[tab.ID]
			if s == nil {
				t.Fatalf("%s: screen not built", tab.ID)
			}
			lines := strings.Split(s.View(), "\n")
			if len(lines) != m.contentHeight() {
				t.Errorf("%s %dx%d: screen renders %d rows, want the full content region %d", tab.ID, w, h, len(lines), m.contentHeight())
			}
			for i, l := range lines {
				if got := lipgloss.Width(l); got != m.contentWidth() {
					t.Errorf("%s %dx%d: screen row %d width %d, want %d", tab.ID, w, h, i, got, m.contentWidth())
					break
				}
			}
		}
	}
}

// TestAuthBannerRendersOnce pins finding 6: the re-auth banner is rendered
// ONCE — when the pane/rail already carries the inline retry state, the
// global dock banner is suppressed.
func TestAuthBannerRendersOnce(t *testing.T) {
	m := phase3App(120, 40)
	m.convErr = "unauthenticated: bad token"
	m.setChatError("send", &stubAuthErr{})
	if m.dock.Err != "" {
		t.Fatalf("inline rail retry must suppress the duplicate dock banner: %q", m.dock.Err)
	}
	m.convErr = ""
	m.setChatError("send", &stubAuthErr{})
	if !strings.Contains(m.dock.Err, "/connect") {
		t.Fatalf("without an inline retry the single banner must render: %q", m.dock.Err)
	}
	// Exactly one occurrence of the re-auth copy in the rendered frame.
	m.dock.SetError("")
	m.convErr = "unauthenticated"
	m.setChatErrorPlain("unauthenticated: bad token")
	if got := strings.Count(lipglossStrip(m.View()), "needs re-authentication"); got > 1 {
		t.Fatalf("re-auth copy rendered %d times in one frame, want at most 1", got)
	}
}
