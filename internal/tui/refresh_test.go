package tui

// refresh_test.go — the ROLLING REFRESH WINDOW.
//
// The operator: "when you send a message, nothing happens unless you move away from the execution and
// then come back ... We need this to be refreshing every few seconds. That also brings up a good point
// about all of the other screens. Are we currently implementing a rolling refresh window? If not we
// should do that."
//
// The answer was no: before this, the ONLY timer in the TUI was the clipboard toast, and a pane could
// repaint only on a NATS event poke — which five screens subscribed to and which the follow-up path
// does not emit at all. These tests pin the window itself: that it exists, that it re-arms, that it
// follows the operator's tab, that it does NOT disrupt an open form, and that it reaches the screen.
//
// They drive the SHELL's own dispatch, because the whole point of the feature is that the shell
// owns the trigger — a test that called a screen's refresh directly would prove nothing about
// whether anything ever asks it to.

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
)

// refreshProbeScreen is a screen that records how often the shell asked it to refresh.
type refreshProbeScreen struct {
	id     string
	calls  int
	refuse bool
}

func (s *refreshProbeScreen) Init() tea.Cmd                              { return nil }
func (s *refreshProbeScreen) View() string                               { return "" }
func (s *refreshProbeScreen) Name() string                               { return s.id }
func (s *refreshProbeScreen) SetSize(int, int)                           {}
func (s *refreshProbeScreen) Close()                                     {}
func (s *refreshProbeScreen) Update(tea.Msg) (screenkit.Screen, tea.Cmd) { return s, nil }
func (s *refreshProbeScreen) RefreshView() tea.Cmd {
	if s.refuse {
		return nil
	}
	s.calls++
	return nil
}

// refreshProbeApp builds an App carrying two probe screens, so "follows the active tab" is testable.
func refreshProbeApp(t *testing.T) (*App, *refreshProbeScreen, *refreshProbeScreen) {
	t.Helper()
	m := newTestApp()
	a := &refreshProbeScreen{id: "a"}
	b := &refreshProbeScreen{id: "b"}
	m.RegisterScreen(TabAsk, a)
	m.RegisterScreen(TabWork, b)
	m.SwitchTo(TabAsk)
	return m, a, b
}

// THE WINDOW EXISTS AND RE-ARMS: a tick refreshes the screen and produces another tick.
//
// Re-arming is the property that makes it a window rather than a one-shot, and it is the thing a
// naive implementation gets wrong (a tick that fires once and never re-arms looks identical on the
// first pass and is dead for the rest of the session).
func TestRefreshTickRefreshesAndReArms(t *testing.T) {
	brisk(t)
	m, a, _ := refreshProbeApp(t)

	_, cmd := m.Update(refreshTickMsg{gen: m.refreshGen})
	if a.calls != 1 {
		t.Fatalf("the tick did not refresh the active screen (calls=%d)", a.calls)
	}
	if cmd == nil {
		t.Fatal("the tick did not re-arm — the window would fire once and stop")
	}
	// The re-armed command must itself produce the next tick.
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		t.Fatalf("re-arm produced %T, want a batch containing the next tick", msg)
	}
	found := false
	for _, c := range batch {
		if c == nil {
			continue
		}
		if _, ok := runCmdBounded(c, runCtxCmdBudget).(refreshTickMsg); ok {
			found = true
		}
	}
	if !found {
		t.Error("the re-armed batch contains no next tick — the chain would die after one fire")
	}
}

// THE WINDOW FOLLOWS THE OPERATOR: a tick refreshes the tab that is ACTIVE, not a fixed one.
func TestRefreshTickFollowsTheActiveTab(t *testing.T) {
	m, a, b := refreshProbeApp(t)

	m.Update(refreshTickMsg{gen: m.refreshGen})
	if a.calls != 1 || b.calls != 0 {
		t.Fatalf("first tick: a=%d b=%d, want only the active tab refreshed", a.calls, b.calls)
	}

	m.SwitchTo(TabWork)
	m.Update(refreshTickMsg{gen: m.refreshGen})
	if b.calls != 1 {
		t.Errorf("after switching, the new tab was not refreshed (b=%d)", b.calls)
	}
	if a.calls != 1 {
		t.Errorf("the tab the operator LEFT was refreshed again (a=%d) — wasted work and a flicker", a.calls)
	}
}

// A STALE tick is ignored rather than acted on: it cannot refresh the tab the operator has left, and
// it does not start a second chain (which would climb the refresh rate with every tab switch).
func TestStaleTickDoesNotRefreshOrMultiply(t *testing.T) {
	brisk(t)
	m, a, _ := refreshProbeApp(t)
	stale := m.refreshGen
	m.SwitchTo(TabWork) // bumps the generation

	_, cmd := m.Update(refreshTickMsg{gen: stale})
	if a.calls != 0 {
		t.Errorf("a stale tick refreshed the old tab (calls=%d)", a.calls)
	}
	if cmd == nil {
		t.Fatal("a stale tick ended the window — the chain must survive a tab switch")
	}
	// And it must re-arm with the CURRENT generation, so exactly one chain remains live.
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		t.Fatalf("re-arm produced %T, want a batch", msg)
	}
	for _, c := range batch {
		if c == nil {
			continue
		}
		if tick, ok := runCmdBounded(c, runCtxCmdBudget).(refreshTickMsg); ok && tick.gen != m.refreshGen {
			t.Errorf("re-armed with the stale generation %d, want the current %d — chains would multiply",
				tick.gen, m.refreshGen)
		}
	}
}

// AN OPEN OVERLAY PAUSES THE WINDOW. Repainting under a modal the operator is choosing from would
// move the rows out from under them.
func TestOpenOverlayPausesTheRefresh(t *testing.T) {
	m, a, _ := refreshProbeApp(t)

	m.help.open = true
	m.Update(refreshTickMsg{gen: m.refreshGen})
	if a.calls != 0 {
		t.Error("the screen was refreshed while the help overlay was open")
	}
	m.help.open = false

	// …and the window RESUMES, which matters: a skipped tick must not end the chain, or one
	// accidental overlay would leave the screen stale for the rest of the session.
	m.Update(refreshTickMsg{gen: m.refreshGen})
	if a.calls != 1 {
		t.Errorf("the window did not resume after the overlay closed (calls=%d)", a.calls)
	}
}

// A SCREEN THAT REFUSES still leaves the chain alive. The screen decides whether a refresh is safe;
// the shell must not treat a refusal as the end of the window.
func TestRefusingScreenDoesNotEndTheWindow(t *testing.T) {
	m, a, _ := refreshProbeApp(t)
	a.refuse = true

	_, cmd := m.Update(refreshTickMsg{gen: m.refreshGen})
	if cmd == nil {
		t.Fatal("a refusing screen ended the window")
	}
	if a.calls != 0 {
		t.Error("a refusing screen was counted as refreshed")
	}
	// Resume and confirm the chain is still healthy.
	a.refuse = false
	m.Update(refreshTickMsg{gen: m.refreshGen})
	if a.calls != 1 {
		t.Errorf("the window did not survive a refusal (calls=%d)", a.calls)
	}
}

// EVERY kit2 screen is refreshable WITHOUT each one writing the method: Base is embedded, so the
// hook is promoted. This is the property that makes the window cover "all of the other screens"
// rather than only the ones someone remembered to wire.
func TestKit2ScreensAreRefreshableByEmbedding(t *testing.T) {
	// The assertion the shell makes (tui.Refresher). A type that embeds kit2.Base — which every screen
	// Model does — satisfies it with no method of its own.
	type refresher interface{ RefreshView() tea.Cmd }
	var _ refresher = (*kit2BaseProbe)(nil)

	// And the embedded method is the real one, not a stub: calling it on a Base with no sources and
	// no selection must be a harmless no-op rather than a panic.
	var probe kit2BaseProbe
	if cmd := probe.RefreshView(); cmd != nil {
		t.Errorf("a Base with nothing to refresh produced a command (%T) — it must be a no-op", cmd)
	}
}

// kit2BaseProbe embeds kit2.Base exactly as every screen Model does.
type kit2BaseProbe struct {
	kit2.Base
}

// brisk shortens the window for the duration of one test. The re-arm chain genuinely waits (tea.Tick
// is a real timer), and five seconds per test is a cost the feature does not justify — the TIMING is
// not what is under test, the re-arm, the generation and the guards are.
func brisk(t *testing.T) {
	t.Helper()
	prev := refreshPeriod
	refreshPeriod = time.Millisecond
	t.Cleanup(func() { refreshPeriod = prev })
}
