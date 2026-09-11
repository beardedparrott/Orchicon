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
	_, cmd := m.dispatch(tea.KeyMsg{Type: tea.KeyCtrlT})
	if cmd == nil {
		t.Fatal("activating a screen must return its first-load cmd")
	}
	if ctrl.inits != 1 {
		t.Fatalf("inits = %d, want 1", ctrl.inits)
	}
	// Leaving and returning must not re-run the load (state is kept).
	m.dispatch(tea.KeyMsg{Type: tea.KeyCtrlO})
	m.dispatch(tea.KeyMsg{Type: tea.KeyCtrlT})
	if ctrl.inits != 1 {
		t.Fatalf("re-activation re-ran the load: inits = %d, want 1", ctrl.inits)
	}
}

// The tab ring walks composer → six areas → composer, and Shift+Tab
// toggles both side rails.
func TestTabRingAndRailToggle(t *testing.T) {
	m := newTestApp()
	m.RegisterScreen(TabAsk, &stubScreen{id: "ask"})
	m.RegisterScreen(TabWork, &stubScreen{id: "work"})
	m.width, m.height = 120, 40
	m.SwitchTo(TabAsk)

	if m.chatFocus != focusComposer {
		t.Fatal("launch focus must be the composer")
	}
	// Tab from the composer drops into the Ask content.
	m.tabRingNext()
	if m.chatFocus != focusContent || m.ActiveTab() != TabAsk {
		t.Fatalf("tab from composer = (%v, %s), want (content, ask)", m.chatFocus, m.ActiveTab())
	}
	// Each further Tab advances one area.
	m.tabRingNext()
	if m.ActiveTab() != TabWork {
		t.Fatalf("tab advanced to %s, want work", m.ActiveTab())
	}
	// …and after the last area it wraps back to the prompt.
	m.SwitchTo(TabControl)
	m.setFocus(focusContent)
	m.tabRingNext()
	if m.chatFocus != focusComposer {
		t.Fatal("tab after the last area must wrap back to the chat prompt")
	}
	// Shift+Tab pops both rails together.
	m.rightRailOpen = true
	m.toggleSideRails()
	if m.rightRailOpen || m.diffOpen {
		t.Fatal("shift+tab must collapse both side rails")
	}
	m.toggleSideRails()
	if !m.rightRailOpen {
		t.Fatal("shift+tab must restore the conversations rail")
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
