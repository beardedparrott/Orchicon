package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/config"
	"github.com/beardedparrott/orchicon/internal/tui/stream"
)

func newTestApp() *App {
	m := NewApp(&client.Clients{}, &config.Profile{Name: "default", URL: "https://x.example.com"}, "v9.9.9")
	m.width, m.height = 120, 40
	return m
}

// stubScreen is a minimal Screen implementation for shell tests.
type stubScreen struct {
	id      string
	closed  bool
	width   int
	height  int
	chip    string
	lastKey string
	ensure  int
}

func (s *stubScreen) Init() tea.Cmd        { return nil }
func (s *stubScreen) View() string         { return "view:" + s.id }
func (s *stubScreen) Name() string         { return s.id }
func (s *stubScreen) SetSize(w, h int)     { s.width, s.height = w, h }
func (s *stubScreen) Close()               { s.closed = true }
func (s *stubScreen) ContextChip() string  { return s.chip }
func (s *stubScreen) EnsureSubscriptions() { s.ensure++ }
func (s *stubScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	if k, ok := msg.(tea.KeyMsg); ok {
		s.lastKey = k.String()
	}
	return s, nil
}

func TestTabOrderMirrorsGUINav(t *testing.T) {
	want := []string{"Ask Orchicon", "Overview", "Work", "Execution", "Automation", "Enforcement", "Control"}
	if len(Tabs) != len(want) {
		t.Fatalf("tabs = %d, want %d", len(Tabs), len(want))
	}
	for i, title := range want {
		if Tabs[i].Title != title {
			t.Fatalf("tab %d = %q, want %q", i, Tabs[i].Title, title)
		}
	}
	chords := []string{"ctrl+o", "ctrl+v", "ctrl+w", "ctrl+e", "ctrl+a", "ctrl+f", "ctrl+t"}
	for i, chord := range chords {
		if Tabs[i].Chord != chord {
			t.Fatalf("tab %d chord = %q, want %q", i, Tabs[i].Chord, chord)
		}
	}
}

func TestChordSwitching(t *testing.T) {
	m := newTestApp()
	for _, tab := range Tabs {
		s := &stubScreen{id: string(tab.ID)}
		m.RegisterScreen(tab.ID, s)
	}
	for _, tab := range Tabs {
		nm, _ := m.Update(keyFor(tab.Chord))
		m2 := nm.(*App)
		if m2.ActiveTab() != tab.ID {
			t.Fatalf("chord %s: active = %q, want %q", tab.Chord, m2.ActiveTab(), tab.ID)
		}
	}
}

// keyFor builds the KeyMsg whose String() equals s.
func keyFor(s string) tea.KeyMsg {
	switch s {
	case "ctrl+a":
		return tea.KeyMsg{Type: tea.KeyCtrlA}
	case "ctrl+w":
		return tea.KeyMsg{Type: tea.KeyCtrlW}
	case "ctrl+e":
		return tea.KeyMsg{Type: tea.KeyCtrlE}
	case "ctrl+f":
		return tea.KeyMsg{Type: tea.KeyCtrlF}
	case "ctrl+t":
		return tea.KeyMsg{Type: tea.KeyCtrlT}
	case "ctrl+o":
		return tea.KeyMsg{Type: tea.KeyCtrlO}
	case "ctrl+v":
		return tea.KeyMsg{Type: tea.KeyCtrlV}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func TestArrowTabCycling(t *testing.T) {
	m := newTestApp()
	m.RegisterScreen(TabWork, &stubScreen{id: "work"})
	if m.ActiveTab() != TabWork {
		t.Fatalf("initial tab = %q", m.ActiveTab())
	}
	// Arrow tab cycling is a structural chord — it works even while the
	// composer is focused (the Phase-2a launch default).
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRight})
	if got := nm.(*App).ActiveTab(); got != TabExecution {
		t.Fatalf("right from work = %q, want execution", got)
	}
	nm, _ = nm.(*App).Update(tea.KeyMsg{Type: tea.KeyLeft})
	if got := nm.(*App).ActiveTab(); got != TabWork {
		t.Fatalf("left back = %q, want work", got)
	}
}

func TestSwitchClosesPreviousScreen(t *testing.T) {
	m := newTestApp()
	a := &stubScreen{id: "work"}
	b := &stubScreen{id: "execution"}
	m.RegisterScreen(TabWork, a)
	m.RegisterScreen(TabExecution, b)
	m.SwitchTo(TabWork)
	m.SwitchTo(TabExecution)
	if !a.closed {
		t.Fatal("previous screen must be closed on switch (unsubscribe)")
	}
	if b.closed {
		t.Fatal("new screen must not be closed")
	}
}

// The help overlay must render every route in the registry — parity
// between `?` and actual behavior is an acceptance criterion.
func TestHelpOverlayMatchesKeymap(t *testing.T) {
	m := newTestApp()
	_ = m
	routes := GlobalKeyRoutes(Tabs)
	lines := strings.Join(helpLines(routes), "\n")
	for _, r := range routes {
		if r.Keys == "" {
			continue
		}
		if !strings.Contains(lines, r.Keys) {
			t.Errorf("route %q (%s) missing from help overlay", r.Keys, r.Name)
		}
		if !strings.Contains(lines, r.Name) {
			t.Errorf("route name %q missing from help overlay", r.Name)
		}
	}
	// every tab chord appears
	for _, tab := range Tabs {
		found := false
		for _, r := range routes {
			if r.Keys == tab.Chord {
				found = true
			}
		}
		if !found {
			t.Errorf("no route registered for tab chord %s", tab.Chord)
		}
	}
}

func TestHelpOverlayToggle(t *testing.T) {
	m := newTestApp()
	m.RegisterScreen(TabWork, &stubScreen{id: "work"})
	// The help overlay opens from CONTENT focus (? is a literal character
	// while composing — typing "how?" into the composer must never open
	// an overlay). Esc also hands focus back: toggle content → composer.
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc}) // composer → content
	m2 := nm.(*App)
	if m2.chatFocus != focusContent {
		t.Fatal("esc must return focus to content (precondition)")
	}
	nm, _ = m2.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
	m2 = nm.(*App)
	if !m2.help.open {
		t.Fatal("? must open the overlay")
	}
	view := m2.View()
	if !strings.Contains(view, "orch — keys") {
		t.Fatalf("overlay not rendered: %q", view)
	}
	nm, _ = m2.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if nm.(*App).help.open {
		t.Fatal("esc must close the overlay")
	}
}

// Inside a text input (composer concern later), chords must fall through
// to editing: the router consumes ctrl-chords first, but plain keys and
// shift+letters reach the screen. The composer owns plain keys at launch
// (Phase 2a) — after esc (content focus) they reach the screen.
func TestRouterDefersPlainKeysToScreen(t *testing.T) {
	m := newTestApp()
	s := &stubScreen{id: "ask"}
	m.RegisterScreen(TabAsk, s)
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc}) // composer → content
	m2 := nm.(*App)
	nm, _ = m2.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	m2 = nm.(*App)
	got := m2.screens[TabAsk].(*stubScreen)
	if got.lastKey != "x" {
		t.Fatalf("screen got %q, want x", got.lastKey)
	}
}

func TestFooterVersionDrift(t *testing.T) {
	if versionDrift("v1.2.3", "v1.2.3") {
		t.Fatal("equal versions must not drift")
	}
	if !versionDrift("v1.2.3", "v1.2.4") {
		t.Fatal("mismatched versions must drift")
	}
	if versionDrift("v1.2.3", "dev") {
		t.Fatal("dev client never warns")
	}
	if versionDrift("", "v1.2.3") {
		t.Fatal("unknown server version never warns")
	}
}

func TestFooterRendersDriftWarning(t *testing.T) {
	f := footerModel{
		URL:           "https://x",
		ServerVersion: "v1.0.0",
		ClientVersion: "v2.0.0",
		StreamStatus:  stream.StatusOpen,
	}
	v := f.View()
	if !strings.Contains(v, "⚠ drift") {
		t.Fatalf("drift warning missing: %q", v)
	}
	if !strings.Contains(v, "https://x") {
		t.Fatalf("URL missing: %q", v)
	}
}

func TestFooterDisconnectedRetrying(t *testing.T) {
	f := footerModel{URL: "https://x", StreamStatus: stream.StatusReconnecting}
	v := f.View()
	if !strings.Contains(v, "disconnected, retrying") {
		t.Fatalf("graceful degradation label missing: %q", v)
	}
}

// Tab chords must drive the active screen's live streams: the chord
// doubles as the stream re-arm, and the opening tab starts live on the
// first WindowSizeMsg (the shell's EnsureSubscriptions contract).
func TestChordEnsuresSubscriptions(t *testing.T) {
	m := newTestApp()
	s := &stubScreen{id: "work"}
	m.RegisterScreen(TabWork, s)
	m.Update(keyFor("ctrl+w"))
	if s.ensure == 0 {
		t.Fatal("tab chord must ensure the screen's subscriptions")
	}
	first := s.ensure
	m.Update(keyFor("ctrl+w"))
	if s.ensure != first+1 {
		t.Fatalf("chord re-press must re-arm EnsureSubscriptions: ensure=%d", s.ensure)
	}
}

func TestWorseStatusWins(t *testing.T) {
	if statusRank(stream.StatusOpen) >= statusRank(stream.StatusReconnecting) {
		t.Fatal("reconnecting must outrank open")
	}
	if statusRank(stream.StatusError) <= statusRank(stream.StatusOpen) {
		t.Fatal("error must outrank open")
	}
	if statusRank(stream.StatusOpen) != 1 {
		t.Fatal("open rank pinned (footer default)")
	}
}
