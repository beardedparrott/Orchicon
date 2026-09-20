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

	"github.com/beardedparrott/orchicon/internal/tui/chat"
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
	// Esc closes it, and Space re-opens it (the second activation key).
	//
	// Space runs HERE, on the LAUNCH page, where no conversations rail is up.
	// On Ask with the rail VISIBLE, Space/Enter are the rail's SELECT gesture
	// instead — the operator's "I should be able to move up/down with arrow
	// keys and space or enter selects". That override owns the key only while
	// the rail is drawn, and is pinned separately by
	// TestAskRailEnterSpaceSelectsConversation.
	nm, _ = m.dispatch(tea.KeyMsg{Type: tea.KeyEsc})
	m = nm
	if m.MenuOpenID() != "" {
		t.Fatal("esc must close the dropdown")
	}
	nm, _ = m.dispatch(tea.KeyMsg{Type: tea.KeySpace})
	m = nm
	if m.MenuOpenID() != m.active {
		t.Fatalf("space must open the active tab's submenu (menuOpen=%q)", m.MenuOpenID())
	}
	// Arrows move the selection; Enter selects + closes.
	nm, _ = m.dispatch(tea.KeyMsg{Type: tea.KeyDown})
	m = nm
	if m.TabMenu().Sel != 1 {
		t.Fatalf("down must move the dropdown selection (sel=%d)", m.TabMenu().Sel)
	}
	want := m.TabMenu().Entries[1]
	nm, _ = m.dispatch(tea.KeyMsg{Type: tea.KeyEnter})
	m = nm
	if m.MenuOpenID() != "" {
		t.Fatalf("enter must select + close the dropdown (menuOpen=%q)", m.MenuOpenID())
	}
	// A VERB row (Ask's New/Conversations) runs its action; a source row focuses
	// its source. Assert whichever contract applies to the row we picked.
	if want.Action != nil {
		if m.active == TabAsk && want.Cmd == "conversations" && !m.railVisible() {
			t.Fatal("selecting Conversations must switch Ask to its conversation view (rail visible)")
		}
	} else if s := m.screens[m.active]; s != nil {
		if ar, ok := s.(interface{ ActiveSourceName() string }); ok && ar.ActiveSourceName() != want.Source {
			t.Fatalf("selected entry must navigate: active source %q, want %q", ar.ActiveSourceName(), want.Source)
		}
	}
	// (Space/esc coverage lives above, on the launch page, so the rail's
	// select gesture cannot shadow the submenu activation.)
	// A non-empty composer keeps Enter as send (no surprise menu).
	m.dock.SetValue("hello")
	nm, _ = m.dispatch(tea.KeyMsg{Type: tea.KeyEnter})
	m = nm
	if m.MenuOpenID() != "" {
		t.Fatal("enter with a non-empty composer must send, not open the submenu")
	}
}

// TestAskRailEnterSpaceSelectsConversation pins the Ask rail's key contract:
// while the conversations rail is UP and the composer is empty, Enter/Space
// open the HIGHLIGHTED conversation — the operator's "I should be able to move
// up/down with arrow keys and space or enter selects". This deliberately
// overrides the tab-submenu activation on Ask, so it is asserted separately
// from TestSubmenuOpensByKeyAndSelects (which covers the launch page).
func TestAskRailEnterSpaceSelectsConversation(t *testing.T) {
	newRails := func() *App {
		m := newRailsApp(120, 40)
		m.conversations = []chat.Conversation{
			{ID: "c1", Title: "Alpha"},
			{ID: "c2", Title: "Beta"},
		}
		m.convSel, m.convScroll, m.convLoaded = 1, 0, true
		return m
	}

	m := newRails()
	if !m.railVisible() {
		t.Fatal("the conversations rail must be visible for this contract")
	}
	nm, _ := m.dispatch(tea.KeyMsg{Type: tea.KeyEnter})
	m = nm
	if m.MenuOpenID() != "" {
		t.Fatalf("enter on Ask with the rail up must select, not open the submenu (menuOpen=%q)", m.MenuOpenID())
	}
	if m.chatConvID != "c2" {
		t.Fatalf("enter must open the HIGHLIGHTED conversation: chatConvID=%q, want c2", m.chatConvID)
	}

	// SPACE NO LONGER OPENS — it MARKS, which is what "selects" means on every other list in this
	// client and what the operator asked for when they requested bulk operations ("Spacebar selects,
	// then we should be able to bulk delete or bulk assign"). ENTER remains the open gesture, so the
	// earlier "space or enter selects" wording is satisfied by the pair rather than by both keys.
	//
	// The whole point of asserting it HERE is that the two must stay distinguishable: a space that
	// still opened, or an enter that stopped opening, would both be regressions.
	m2 := newRails()
	nm2, _ := m2.dispatch(tea.KeyMsg{Type: tea.KeySpace})
	m2 = nm2
	if m2.MenuOpenID() != "" {
		t.Fatalf("space on Ask with the rail up must MARK, not open the submenu (menuOpen=%q)", m2.MenuOpenID())
	}
	if m2.chatConvID == "c2" {
		t.Fatal("space must not OPEN the highlighted conversation any more — enter is the open gesture")
	}
	if ids := m2.convMarkedIDs(); len(ids) != 1 || ids[0] != "c2" {
		t.Fatalf("space must MARK the highlighted conversation, got %v", ids)
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
// ONCE — where a surface already carries the inline retry state, the global
// dock banner is suppressed rather than duplicated.
//
// The dedupe is only valid while that surface is ON SCREEN. The fixture must
// therefore put the conversations rail UP: this test used to set convErr and
// assert suppression while sitting on the launch page, where the rail is not
// rendered at all (rightrail.go — railVisible is false in welcome mode). It was
// pinning the defect. TestLaunchPageAuthFailureIsVisible covers that half.
func TestAuthBannerRendersOnce(t *testing.T) {
	m := phase3App(120, 40)
	// The state the dedupe was written for: the rail is on screen, so its own
	// retry row IS the banner.
	m.askMode = askConversations
	if !m.railVisible() {
		t.Fatal("fixture: the conversations rail must be visible for the dedupe to apply")
	}
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

// TestLaunchPageAuthFailureIsVisible pins the launch-page half of the same
// finding: in welcome mode the conversations rail is NOT rendered, so its
// convErr is not an inline retry state and must not suppress the banner. Before
// this, a 401 on the first send from the launch page was reported NOWHERE — the
// composer showed a stuck "sending …" ack, the draft reappeared in the box, and
// no surface said why. That combination was read as "Enter does nothing".
func TestLaunchPageAuthFailureIsVisible(t *testing.T) {
	m := phase3App(120, 40)
	if !m.welcomeMode() {
		t.Fatal("fixture: expected the Ask launch page")
	}
	if m.railVisible() {
		t.Fatal("fixture: the launch page must not draw the conversations rail")
	}
	// The rail's own startup load failed with the same 401 — the state that
	// used to silence the banner on its own.
	m.convErr = "unauthenticated: bad token"
	m.dock.SetNotice("sending …") // the ack the composer writes when Enter fires
	m.setChatError("send", &stubAuthErr{})

	if !strings.Contains(m.dock.Err, "/connect") {
		t.Fatalf("a 401 on the launch page must be visible: dock.Err = %q", m.dock.Err)
	}
	if strings.Contains(m.dock.Notice, "sending") {
		t.Fatalf("the send ack must settle on failure: dock.Notice = %q", m.dock.Notice)
	}
}

// dockedAskApp builds an app on Ask with a session already open, so the shell
// renders the DOCKED layout (composer pinned at the bottom). Tests that assert
// the dock's geometry use this; the centered launch layout has its own test.
func dockedAskApp(w, h int) *App {
	m := phase3App(w, h)
	m.chatConvID = "conv-docked" // leaves welcome mode
	return m
}
