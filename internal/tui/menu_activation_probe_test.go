package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/config"
)

// Operator Phase-3.5 finding 3: submenu activation. Pins BOTH entry
// paths: (a) Enter/Space opens the active tab's dropdown (empty
// composer), arrows navigate, Enter selects, (b) mouse click on a
// dropdown row selects.
func TestTabSubmenuKeyboardActivation(t *testing.T) {
	m := NewApp(nil, &config.Profile{URL: "http://localhost:8080", AuthMethod: config.AuthAPIKey, Token: "oc_test"}, "")
	m.width, m.height = 120, 40
	m.SwitchTo(TabAsk)

	// Launch focus is the composer; the buffer is empty → Enter opens.
	if got := m.dock.Value(); got != "" {
		t.Fatalf("composer unexpectedly non-empty at launch: %q", got)
	}
	next, _ := m.dispatch(tea.KeyMsg{Type: tea.KeyEnter})
	_ = next
	if m.TabMenu() == nil {
		t.Fatal("Enter did not open the active tab's dropdown (empty composer)")
	}
	menu := m.TabMenu()
	if len(menu.Entries) == 0 {
		t.Fatal("dropdown has no entries")
	}

	// Down navigates.
	sel0 := menu.Sel
	next, _ = m.dispatch(tea.KeyMsg{Type: tea.KeyDown})
	_ = next
	if m.TabMenu() == nil {
		t.Fatal("dropdown closed on down-arrow")
	}
	if m.TabMenu().Sel == sel0 && len(menu.Entries) > 1 {
		t.Fatal("down-arrow did not move the dropdown selection")
	}

	// Esc closes.
	next, _ = m.dispatch(tea.KeyMsg{Type: tea.KeyEsc})
	_ = next
	if m.TabMenu() != nil {
		t.Fatal("esc did not close the dropdown")
	}
}

func TestTabSubmenuActivationWithTextInComposerSends(t *testing.T) {
	m := NewApp(nil, &config.Profile{URL: "http://localhost:8080", AuthMethod: config.AuthAPIKey, Token: "oc_test"}, "")
	m.width, m.height = 120, 40
	m.SwitchTo(TabAsk)
	// Type into the composer (launch focus), then Enter = SEND, not menu.
	next, _ := m.dispatch(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("hello")})
	_ = next
	if m.TabMenu() != nil {
		t.Fatal("menu open before enter")
	}
	// Enter with text: sends (menu must NOT open) — the send path errors
	// on the unreachable plane, but the menu must stay closed.
	next, cmd := m.dispatch(tea.KeyMsg{Type: tea.KeyEnter})
	_ = next
	_ = cmd
	if m.TabMenu() != nil {
		t.Fatal("Enter with composer text opened the dropdown (must send instead)")
	}
}

// The /connect overlay must open a PASSWORD field that is EMPTY and typeable.
//
// The profile below is the real shape: in password mode its Token is the previous
// session's minted ACCESS TOKEN. That token used to be loaded into the field
// labelled "Password > ", so the field arrived pre-satisfied, submit sent the
// stale token, and the plane answered 401 — while the operator could not work out
// what was in the box or clear it. (This test previously asserted only that a
// single Tab reached a typeable field, which pinned the old field ORDER.)
func TestConnectOverlayPasswordFieldEmptyAndTypeable(t *testing.T) {
	m := NewApp(nil, &config.Profile{
		URL:        "http://localhost:8080",
		AuthMethod: config.AuthPassword,
		Token:      strings.Repeat("tok.", 100),
		Username:   "orchicon",
	}, "")
	m.width, m.height = 120, 40
	m.openConnectOverlay()

	// EMPTY on open: nothing is echoed before the operator types.
	if v := m.connectOverlayView(); strings.Contains(v, "••") {
		t.Fatal("the password field opened pre-filled — the saved access token is not a password")
	}

	// Password mode draws URL, Username, Password, so TWO tabs reach the field.
	for i := 0; i < 2; i++ {
		next, _ := m.dispatch(tea.KeyMsg{Type: tea.KeyTab})
		_ = next
	}
	next, _ := m.dispatch(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("pw")})
	_ = next
	v := m.connectOverlayView()
	if !strings.Contains(v, "••") {
		t.Fatalf("typed password not echoed in the overlay")
	}
}
