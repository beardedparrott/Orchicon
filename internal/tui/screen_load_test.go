package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// loadCountScreen counts first-load (Init) calls.
type loadCountScreen struct {
	stubScreen
	inits int
}

func (s *loadCountScreen) Init() tea.Cmd {
	s.inits++
	return func() tea.Msg { return nil }
}

// Regression: activating a tab MUST run that screen's first load exactly
// once. Screens are constructed eagerly for the nav registry, so before
// ensureLoaded only the startup tab ever fetched — every other tab
// rendered "nothing here" with no error (the operator's Control
// screenshot).
func TestSwitchToRunsScreenFirstLoadOnce(t *testing.T) {
	m := newTestApp()
	ctrl := &loadCountScreen{}
	m.RegisterScreen(TabControl, ctrl)
	if ctrl.inits != 0 {
		t.Fatalf("screen inited before activation: %d", ctrl.inits)
	}
	_, cmd := m.dispatch(keyFor(tabChord(TabControl)))
	if cmd == nil {
		t.Fatal("activating a screen must return its first-load cmd")
	}
	if ctrl.inits != 1 {
		t.Fatalf("inits = %d, want 1", ctrl.inits)
	}
	// Leaving and returning must not re-run the load (state is kept).
	m.dispatch(keyFor(tabChord(TabAsk)))
	m.dispatch(keyFor(tabChord(TabControl)))
	if ctrl.inits != 1 {
		t.Fatalf("re-activation re-ran the load: inits = %d, want 1", ctrl.inits)
	}
}

// The tab ring walks composer → every area → composer, and Shift+Tab toggles
// the diff pane.
func TestTabRingAndRailToggle(t *testing.T) {
	m := newTestApp()
	m.RegisterScreen(TabAsk, &stubScreen{id: "ask"})
	m.RegisterScreen(TabOverview, &stubScreen{id: "overview"})
	m.RegisterScreen(TabWork, &stubScreen{id: "work"})
	m.width, m.height = 120, 40
	m.SwitchTo(TabAsk)

	if m.chatFocus != focusComposer {
		t.Fatal("launch focus must be the composer")
	}
	// Tab from the composer lands on the TAB BAR — not in the content, which is
	// what let a pane take the arrows before anything was chosen.
	m.tabRingNext()
	if m.chatFocus != focusTabs || m.ActiveTab() != TabAsk {
		t.Fatalf("tab from composer = (%v, %s), want (tab bar, ask)", m.chatFocus, m.ActiveTab())
	}
	// Each further Tab advances one area, in tab-bar order (Overview is second).
	m.tabRingNext()
	if m.ActiveTab() != TabOverview {
		t.Fatalf("tab advanced to %s, want overview", m.ActiveTab())
	}
	// …and after the last area it WRAPS back round to the first: "once you hit the
	// end of the tab menu it stops. It should rotate back around."
	m.SwitchTo(TabControl)
	m.setFocus(focusTabs)
	m.tabRingNext()
	if m.ActiveTab() != Tabs[0].ID {
		t.Fatalf("tab after the last area must wrap to the first, got %s", m.ActiveTab())
	}
	// Shift+Tab reverses the ring: from the first tab it wraps to the last.
	m.tabRingPrev()
	if m.ActiveTab() != Tabs[len(Tabs)-1].ID {
		t.Fatalf("shift+tab from the first tab must wrap to the last, got %s", m.ActiveTab())
	}
	// Shift+Tab does NOT touch the diff pane any more (ctrl+d owns it).
	m.diffOpen = false
	m.rightRailOpen = true
	m.toggleSideRails()
	if !m.diffOpen {
		t.Fatal("shift+tab must open the diff pane")
	}
	if !m.rightRailOpen {
		t.Fatal("shift+tab must leave the conversations rail alone")
	}
	m.toggleSideRails()
	if m.diffOpen {
		t.Fatal("shift+tab must close the diff pane again")
	}
}

// /new returns Ask to a fresh conversation so the next send creates one.
func TestNewChatSlashResetsConversation(t *testing.T) {
	m := newTestApp()
	m.chatConvID = "conv-existing"
	if handled, _ := m.dispatchSlash("/new"); !handled {
		t.Fatal("/new must dispatch")
	}
	if m.chatConvID != "" {
		t.Fatalf("chatConvID = %q after /new, want empty (fresh conversation)", m.chatConvID)
	}
	if m.ActiveTab() != TabAsk {
		t.Fatalf("active tab = %s after /new, want ask", m.ActiveTab())
	}
}

// Regression for "themes are still not saving". The shell reference was injected
// by hand inside each factory and CONTROL's was missing, so Control.Shell()
// returned nil: its Themes pane switched the palette in-session but could never
// persist it (applyTheme fell through to theme.Use), and its notices were
// silently dropped. Every screen must be constructed with a shell.
func TestEveryScreenGetsTheShellReference(t *testing.T) {
	m := newTestApp()
	for _, tab := range Tabs {
		s := m.newScreen(tab.ID)
		if s == nil {
			t.Fatalf("tab %q has no screen factory", tab.ID)
		}
		sh, ok := s.(interface{ Shell() any })
		if !ok {
			t.Fatalf("tab %q screen has no Shell()", tab.ID)
		}
		if sh.Shell() == nil {
			t.Errorf("tab %q screen was built WITHOUT a shell reference — persistence and notices would silently fail", tab.ID)
		}
	}
}
