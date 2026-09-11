package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/config"
)

// claimStub is a screen that opts into owning every key (an open form).
type claimStub struct {
	id    TabID
	claim bool
	keys  []string
}

func (c *claimStub) Init() tea.Cmd { return nil }
func (c *claimStub) Update(msg tea.Msg) (Screen, tea.Cmd) {
	if k, ok := msg.(tea.KeyMsg); ok {
		c.keys = append(c.keys, k.String())
	}
	return c, nil
}
func (c *claimStub) View() string     { return "" }
func (c *claimStub) Name() string     { return string(c.id) }
func (c *claimStub) SetSize(int, int) {}
func (c *claimStub) Close()           {}

// ClaimsKeys reports the screen owns every key right now.
func (c *claimStub) ClaimsKeys() bool { return c.claim }

// TestScreenKeyClaimBypassesShellRoutes pins the input-modal contract: a
// screen whose form is open claims every key, so a typed character is
// never a shell shortcut ('q' would quit the process, space would open the
// tab menu, '/' the palette) — and stops claiming when the form closes.
func TestScreenKeyClaimBypassesShellRoutes(t *testing.T) {
	m := NewApp(&client.Clients{}, &config.Profile{Name: "default", URL: "https://x.example.com"}, "v9.9.9")
	m.width, m.height = 120, 40
	cs := &claimStub{id: TabAutomation, claim: true}
	m.RegisterScreen(TabAutomation, cs)
	m.RegisterScreen(TabWork, &claimStub{id: TabWork})
	m.SwitchTo(TabAutomation)

	keys := []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune("q")},
		{Type: tea.KeySpace},
		{Type: tea.KeyRunes, Runes: []rune("/")},
	}
	for _, k := range keys {
		nm, _ := m.Update(k)
		m = nm.(*App)
		if m.quitting {
			t.Fatalf("a claimed %q must not quit", k.String())
		}
		if m.TabMenu() != nil {
			t.Fatalf("a claimed %q must not open the tab menu", k.String())
		}
		if m.palette.PaletteOpen() {
			t.Fatalf("a claimed %q must not open the palette", k.String())
		}
	}
	if len(cs.keys) != len(keys) {
		t.Fatalf("claimed keys must reach the screen, got %v", cs.keys)
	}

	// With the claim released the shell routes own their keys again.
	cs.claim = false
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if !nm.(*App).quitting {
		t.Fatal("with no claim, q must quit")
	}
}
