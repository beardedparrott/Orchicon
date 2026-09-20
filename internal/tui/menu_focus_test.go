package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// Regression for the operator's "when in a sub menu it keeps focus on the tab
// bar so hitting enter or spacebar just opens the tab menu and nothing gets
// selected". Enter/Space belong to the CONTENT when the content has focus —
// menu activation is a composer-only affordance.
func TestEnterReachesContentNotMenuActivation(t *testing.T) {
	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyEnter},
		{Type: tea.KeySpace},
	} {
		m := newTestApp()
		s := &activatableScreen{}
		m.RegisterScreen(TabWork, s)
		m.dispatch(tea.WindowSizeMsg{Width: 120, Height: 40})
		m.SwitchTo(TabWork)
		m.setFocus(focusContent) // the operator clicked into the pane

		m2, _ := m.dispatch(key)
		if m2.MenuOpenID() != "" {
			t.Fatalf("%s with content focus opened the tab menu (%q) instead of reaching the screen",
				key.String(), m2.MenuOpenID())
		}
		if !s.activated {
			t.Fatalf("%s with content focus never reached the screen's row activation", key.String())
		}
	}
}

// The composer keeps the affordance: from an EMPTY composer, Enter still opens
// the dropdown (discoverability on the launch page).
func TestEmptyComposerStillOpensMenu(t *testing.T) {
	m := newTestApp()
	m.RegisterScreen(TabWork, &stubScreen{id: "work"})
	m.dispatch(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.SwitchTo(TabWork)
	m.setFocus(focusComposer)

	nm, _ := m.dispatch(tea.KeyMsg{Type: tea.KeyEnter})
	if nm.MenuOpenID() == "" {
		t.Fatal("Enter from an empty composer must still open the tab dropdown")
	}
}

// And with text in the composer, Enter still SENDS rather than opening a menu.
func TestComposerTextEnterDoesNotOpenMenu(t *testing.T) {
	m := newTestApp()
	m.RegisterScreen(TabWork, &stubScreen{id: "work"})
	m.dispatch(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.SwitchTo(TabWork)
	m.setFocus(focusComposer)

	for _, r := range "hello" {
		nm, _ := m.dispatch(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = nm
	}
	nm, _ := m.dispatch(tea.KeyMsg{Type: tea.KeyEnter})
	if nm.MenuOpenID() != "" {
		t.Fatal("Enter with composer text must send, not open the dropdown")
	}
}

// activatableScreen records that a row activation reached the screen.
type activatableScreen struct {
	stubScreen
	activated bool
}

func (s *activatableScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	if k, ok := msg.(tea.KeyMsg); ok && (k.String() == "enter" || k.String() == " ") {
		s.activated = true
	}
	return s, nil
}
