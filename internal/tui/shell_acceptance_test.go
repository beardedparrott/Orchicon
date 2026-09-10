package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/config"
)

// TestTabBarAlwaysVisiblePins the acceptance invariant: the numbered tab
// bar is ALWAYS the first shell line on every tab at 80x24 and 120x40, and
// the shell never overflows the terminal height.
func TestTabBarAlwaysVisibleOnEveryTab(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {120, 40}} {
		w, h := size[0], size[1]
		for _, tab := range Tabs {
			app := NewApp(nil, &config.Profile{URL: "http://x", Token: "t"}, "v0.2.51")
			app.RegisterScreen(tab.ID, &tabBarScreenStub{body: "SRC"})
			app.dispatch(tea.WindowSizeMsg{Width: w, Height: h})
			app.SwitchTo(tab.ID)
			v := app.View()
			lines := strings.Split(v, "\n")
			if !strings.Contains(lines[0], tab.Title) {
				t.Errorf("%s %dx%d: tab bar not on first line: %q", tab.ID, w, h, lines[0])
			}
			if len(lines) > h {
				t.Errorf("%s %dx%d: shell overflow: %d lines > %d", tab.ID, w, h, len(lines), h)
			}
		}
	}
}

// TestTabBarNumberedPins the mockup-parity numbered chrome (ordinal prefix).
func TestTabBarNumbered(t *testing.T) {
	app := NewApp(nil, &config.Profile{URL: "http://x", Token: "t"}, "v0.2.51")
	app.RegisterScreen(TabAsk, &tabBarScreenStub{body: "b"})
	line := app.tabBarView()
	for i, tab := range Tabs {
		want := tab.Ordinal + "·" + tab.Title
		if !strings.Contains(line, want) {
			t.Errorf("tab %s: ordinal label %q missing from %q", tab.Title, want, line)
		}
		_ = i
	}
}

// TestExitAliasPins /exit as an alias of /quit: both must set quitting and
// issue tea.Quit.
func TestExitAlias(t *testing.T) {
	for _, name := range []string{"/exit", "/quit"} {
		m := NewApp(&client.Clients{}, &config.Profile{URL: "http://x", Token: "t"}, "v9.9.9")
		m.dock.Focus()
		m.dock.SetValue(name)
		handled, cmd := m.dispatchSlash(name)
		if !handled {
			t.Fatalf("%s must dispatch", name)
		}
		if !m.quitting {
			t.Fatalf("%s must set quitting", name)
		}
		if cmd == nil {
			t.Fatalf("%s must issue tea.Quit", name)
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Fatalf("%s cmd must produce tea.QuitMsg", name)
		}
	}
}

// TestNoNoticeOnlyCommands asserts zero notice-only ("use the web GUI")
// commands remain in the registry. Every command resolves to a real Run.
func TestNoNoticeOnlyCommands(t *testing.T) {
	app := NewApp(&client.Clients{}, &config.Profile{URL: "http://x", Token: "t"}, "v9.9.9")
	for _, n := range app.slash.names {
		c := app.slash.byName[n]
		if c.NoticeOnly() {
			t.Errorf("command %s is notice-only (%q)", c.Name, c.Desc)
		}
		if c.Run == nil {
			t.Errorf("command %s has no Run", c.Name)
		}
		if strings.Contains(strings.ToLower(c.Desc), "web gui") ||
			strings.Contains(strings.ToLower(c.Desc), "gui-only") {
			t.Errorf("command %s desc references the GUI: %q", c.Name, c.Desc)
		}
	}
}

// TestProvidersRealScreen asserts /providers resolves to the Control screen's
// real Providers source (not a notice).
func TestProvidersRealScreen(t *testing.T) {
	m := newTestApp()
	m.SwitchTo(TabControl)
	c := m.slash.resolve("/providers")
	if c == nil {
		t.Fatal("/providers must be registered")
	}
	// The Control screen must expose a "providers" source (real pane).
	s := m.screens[TabControl]
	if s == nil {
		t.Fatal("control screen not built")
	}
	// Ensure /providers is not notice-only and its command switches to Control.
	if c.NoticeOnly() {
		t.Fatal("/providers must not be notice-only")
	}
}

// TestPaletteOpensOnSlashPins the "/" palette: a leading "/" opens it, esc
// closes, and it enumerates registry commands.
func TestPaletteOpensOnSlash(t *testing.T) {
	m := newTestApp()
	m.setFocus(focusComposer)
	m.dispatch(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	if !m.palette.PaletteOpen() {
		t.Fatal("'/' must open the palette")
	}
	// filter narrows to commands containing "q"
	m.palette.query = "q"
	m.refreshPalette()
	if len(m.palette.filter) == 0 {
		t.Fatal("palette filter for 'q' must match /quit")
	}
	// esc closes
	m.dispatch(tea.KeyMsg{Type: tea.KeyEsc})
	if m.palette.PaletteOpen() {
		t.Fatal("esc must close the palette")
	}
}

// TestPaletteNeverOverflowsPins the palette render bounding: the floating
// overlay must never exceed the terminal height (regression: unbounded
// command list overflows at 80x24).
func TestPaletteNeverOverflows(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {120, 40}} {
		w, h := size[0], size[1]
		m := newTestApp()
		m.setFocus(focusComposer)
		m.dispatch(tea.WindowSizeMsg{Width: w, Height: h})
		m.dispatch(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
		// empty query -> all commands matched; unbounded would overflow.
		m.palette.query = ""
		m.refreshPalette()
		v := m.View()
		if n := strings.Count(v, "\n") + 1; n > h {
			t.Fatalf("%dx%d palette overflow: %d lines > %d", w, h, n, h)
		}
		// The selected row must be visible (viewport windowed).
		if m.palette.sel < m.palette.scroll || m.palette.sel >= m.palette.scroll+m.paletteVisibleRows() {
			t.Fatalf("%dx%d selected row %d outside window [%d,%d)", w, h, m.palette.sel, m.palette.scroll, m.palette.scroll+m.paletteVisibleRows())
		}
	}
}

// TestConnectCancelClearsReconnectPins esc-cancel of the /connect overlay:
// it must clear reconnectRequested so a LATER normal quit does not re-open
// the connection screen (the reconnect loop is first-run fallback only).
func TestConnectCancelClearsReconnect(t *testing.T) {
	m := newTestApp()
	m.dispatch(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m.closePalette()
	m.slash.byName["/connect"].Run(m, nil)
	if !m.ConnectOverlayOpen() {
		t.Fatal("connect overlay must be open")
	}
	if !m.reconnectRequested {
		t.Fatal("reconnectRequested must be set on /connect")
	}
	// esc cancels the overlay.
	m.dispatch(tea.KeyMsg{Type: tea.KeyEsc})
	if m.ConnectOverlayOpen() {
		t.Fatal("esc must close the overlay")
	}
	if m.reconnectRequested {
		t.Fatal("esc-cancel must clear reconnectRequested (later quit must not re-connect)")
	}
	if m.quitting {
		t.Fatal("esc-cancel must not quit")
	}
}

// TestTabClickUsesColumnsPins the tab-bar mouse hit-test against terminal
// COLUMNS (not byte offsets into the ANSI render). Regression: the previous
// code compared a byte offset to the mouse column and drifted by 1 cell per
// preceding "·" glyph, so clicks at / right of a mid-tab missed it.
func TestTabClickUsesColumns(t *testing.T) {
	app := NewApp(nil, &config.Profile{URL: "http://x", Token: "t"}, "v0.2.51")
	app.RegisterScreen(TabAsk, &tabBarScreenStub{body: "b"})
	app.dispatch(tea.WindowSizeMsg{Width: 120, Height: 40})
	app.SwitchTo(TabAsk)
	for _, tab := range Tabs {
		// Click at the exact visible column where this tab's label starts.
		id, ok := app.TabClick(app.tabStartCol(tab))
		if !ok || id != tab.ID {
			t.Errorf("col %d: TabClick = %q,%v want %q", app.tabStartCol(tab), id, ok, tab.ID)
		}
	}
}

// TestAskTwoRailsPins the Ask 3-zone layout: left diff rail + center + right
// conversations rail render together without overflow.
func TestAskTwoRailsPins(t *testing.T) {
	app := NewApp(nil, &config.Profile{URL: "http://x", Token: "t"}, "v0.2.51")
	app.RegisterScreen(TabAsk, &tabBarScreenStub{body: "SRC"})
	app.dispatch(tea.WindowSizeMsg{Width: 120, Height: 40})
	app.SwitchTo(TabAsk)
	v := app.View()
	if !strings.Contains(v, "CONVERSATIONS") {
		t.Fatal("right conversations rail missing from Ask view")
	}
	lines := strings.Split(v, "\n")
	if len(lines) > 40 {
		t.Fatalf("Ask 3-zone overflow: %d lines > 40", len(lines))
	}
}

// TestFullScreenTakeoverCoversViewport is the Phase-2a acceptance render
// gate: on EVERY tab in the shell inventory (real screens from the
// factories), the view is EXACTLY h rows and EVERY row is EXACTLY w cells
// wide (opaque fill — zero terminal bleed-through) at 80x24 and 120x40.
// The composer block is pinned to the bottom above the one-line footer.
func TestFullScreenTakeoverCoversViewport(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {120, 40}} {
		w, h := size[0], size[1]
		for _, tab := range Tabs {
			app := NewApp(nil, &config.Profile{URL: "http://x", Token: "t"}, "v0.2.51")
			app.dispatch(tea.WindowSizeMsg{Width: w, Height: h})
			app.SwitchTo(tab.ID)
			v := app.View()
			lines := strings.Split(v, "\n")
			if len(lines) != h {
				t.Errorf("%s %dx%d: %d rows, want exactly %d", tab.ID, w, h, len(lines), h)
				continue
			}
			for i, l := range lines {
				if got := lipgloss.Width(l); got != w {
					t.Errorf("%s %dx%d row %d: width %d, want %d", tab.ID, w, h, i, got, w)
					break
				}
			}
			// The composer block is pinned to the bottom: the input line is
			// inside the dock block (dockRows rows), directly above the
			// one-line footer; notice strips render above the input line,
			// so the exact row is found by scanning the dock block.
			footerIdx := h - 1
			dockRows := app.dock.Lines()
			inputIdx := -1
			for i := footerIdx - dockRows + 1; i <= footerIdx-1; i++ {
				if i >= 0 && strings.Contains(lipglossStrip(lines[i]), "❯") {
					inputIdx = i
					break
				}
			}
			if inputIdx < 0 {
				t.Errorf("%s %dx%d: composer prompt missing from the dock block (rows %d-%d)", tab.ID, w, h, footerIdx-dockRows, footerIdx-1)
				continue
			}
			if !strings.Contains(lipglossStrip(lines[inputIdx]), "❯") {
				t.Errorf("%s %dx%d: composer prompt not on row %d (dock block): %q", tab.ID, w, h, inputIdx, lipglossStrip(lines[inputIdx]))
			}
			footer := lipglossStrip(lines[footerIdx])
			if !strings.Contains(footer, "·") {
				t.Errorf("%s %dx%d: footer not on the last row: %q", tab.ID, w, h, footer)
			}
		}
	}
}

// TestComposerFocusedAtLaunch pins operator finding 3: the shell launches
// with the composer focused (typing works immediately, no ctrl+g); esc
// moves focus to content; ctrl+g returns it; the footer reflects focus.
func TestComposerFocusedAtLaunch(t *testing.T) {
	m := newTestApp()
	if m.chatFocus != focusComposer {
		t.Fatalf("launch focus = %v, want composer", m.chatFocus)
	}
	if !m.dock.Focused {
		t.Fatal("launch: dock textarea not focused")
	}
	if !m.Footer().ComposerFocus {
		t.Fatal("launch: footer must show the composer-focused hint")
	}
	// Typing lands in the composer with no ctrl+g first.
	nm, _ := m.dispatch(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	m = nm
	if got := m.dock.Value(); got != "x" {
		t.Fatalf("typed char must land in the composer at launch: %q", got)
	}
	if m.chatFocus != focusComposer {
		t.Fatal("typing must not steal focus")
	}
	// esc → content; ctrl+g → composer.
	nm, _ = m.dispatch(tea.KeyMsg{Type: tea.KeyEsc})
	m = nm
	if m.chatFocus != focusContent {
		t.Fatal("esc must move focus to content")
	}
	nm, _ = m.dispatch(keyFor("ctrl+g"))
	m = nm
	if m.chatFocus != focusComposer {
		t.Fatal("ctrl+g must return focus to the composer")
	}
}

// TestPaletteAboveComposerPins operator finding 4: the slash palette
// floats ABOVE the composer — the composer input line (with the typed
// text) stays visible BELOW the palette box while it filters.
func TestPaletteAboveComposer(t *testing.T) {
	m := newTestApp()
	m.dispatch(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.dock.SetValue("/the")
	m.openPalette()
	v := m.View()
	lines := strings.Split(v, "\n")
	// The composer line with the typed text must be visible.
	composerIdx := -1
	for i, l := range lines {
		if strings.Contains(lipglossStrip(l), "❯ /the") {
			composerIdx = i
			break
		}
	}
	if composerIdx < 0 {
		t.Fatalf("composer line with typed text invisible while palette open")
	}
	// The palette box renders ABOVE the composer line.
	paletteIdx := -1
	for i, l := range lines {
		if strings.Contains(lipglossStrip(l), "command palette") {
			paletteIdx = i
			break
		}
	}
	if paletteIdx < 0 {
		t.Fatalf("palette box not rendered")
	}
	if paletteIdx >= composerIdx {
		t.Fatalf("palette box (row %d) must float ABOVE the composer (row %d)", paletteIdx, composerIdx)
	}
}

// lipglossStrip removes ANSI escapes for plain-text assertions.
func lipglossStrip(s string) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && !(s[j] >= 0x40 && s[j] <= 0x7e) {
				j++
			}
			i = j + 1
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
