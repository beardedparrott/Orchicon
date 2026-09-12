package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// panelApp builds an app on a NON-Ask tab with the composer focused, which is
// where the slide-out strip lives.
func panelApp(t *testing.T) *App {
	t.Helper()
	m := newTestApp()
	m.RegisterScreen(TabWork, &stubScreen{id: "work"})
	m.dispatch(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.SwitchTo(TabWork)
	m.setFocus(focusComposer)
	return m
}

// Engaging the composer on a non-Ask screen slides the strip out, so a send's
// destination is visible instead of the message landing somewhere off-screen.
func TestSlideOutPanelOpensOnTyping(t *testing.T) {
	m := panelApp(t)
	if m.panelVisible() {
		t.Fatal("the strip must start minimised")
	}
	nm, _ := m.dispatch(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h'}})
	m = nm
	if !m.panelVisible() {
		t.Fatal("typing must slide the conversation strip out")
	}
	if rows := m.panelRows(); rows < 3 {
		t.Fatalf("strip rows = %d, want a usable height", rows)
	}
}

// It slides out for a click on the composer too (a mouse-first operator).
func TestSlideOutPanelOpensOnComposerClick(t *testing.T) {
	m := panelApp(t)
	nm, _ := m.Update(tea.MouseMsg{
		Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
		X: 20, Y: m.height - 2,
	})
	m = nm.(*App)
	if !m.panelVisible() {
		t.Fatal("clicking the composer must slide the strip out")
	}
	if m.chatFocus != focusComposer {
		t.Fatal("clicking the composer must focus it")
	}
}

// Esc minimises it back into the prompt — the operator's ask — and a SECOND esc
// then moves focus to the content.
func TestSlideOutPanelMinimisesWithEsc(t *testing.T) {
	m := panelApp(t)
	nm, _ := m.dispatch(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h'}})
	m = nm
	if !m.panelVisible() {
		t.Fatal("precondition: the strip is out")
	}
	nm, _ = m.dispatch(tea.KeyMsg{Type: tea.KeyEsc})
	m = nm
	if m.panelVisible() {
		t.Fatal("esc must minimise the strip")
	}
	if m.chatFocus != focusComposer {
		t.Fatal("minimising must leave the composer focused (back into the prompt)")
	}
	nm, _ = m.dispatch(tea.KeyMsg{Type: tea.KeyEsc})
	m = nm
	if m.chatFocus != focusContent {
		t.Fatal("a second esc must move focus to the content")
	}
}

// It is NOT drawn on Ask (the real transcript is already on screen) nor while a
// running execution is selected (that reply streams into the session view).
func TestSlideOutPanelHiddenOnAsk(t *testing.T) {
	m := newTestApp()
	m.RegisterScreen(TabAsk, &stubScreen{id: "ask"})
	m.dispatch(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.SwitchTo(TabAsk)
	m.setFocus(focusComposer)
	nm, _ := m.dispatch(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h'}})
	m = nm
	if m.panelVisible() {
		t.Fatal("the strip must not duplicate the Ask transcript")
	}
}

// The strip takes its rows from the screen's budget (never overlaying content),
// and the frame stays exact with it out at both floor sizes.
func TestSlideOutPanelTakesScreenRowsAndKeepsFrameExact(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {120, 40}} {
		w, h := size[0], size[1]
		m := newTestApp()
		m.RegisterScreen(TabWork, &stubScreen{id: "work"})
		m.dispatch(tea.WindowSizeMsg{Width: w, Height: h})
		m.SwitchTo(TabWork)
		m.setFocus(focusComposer)

		before := m.screenRows()
		nm, _ := m.dispatch(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h'}})
		m = nm
		if !m.panelVisible() {
			t.Fatalf("%dx%d: strip did not open", w, h)
		}
		if after := m.screenRows(); after != before-m.panelRows() {
			t.Fatalf("%dx%d: screen rows %d -> %d, want %d (strip takes its rows)",
				w, h, before, after, before-m.panelRows())
		}
		lines := strings.Split(m.View(), "\n")
		if len(lines) != h {
			t.Fatalf("%dx%d: frame has %d rows, want exactly %d", w, h, len(lines), h)
		}
		for i, l := range lines {
			if got := len([]rune(lipglossStrip(l))); got != w {
				t.Fatalf("%dx%d: row %d visible width %d, want %d", w, h, i, got, w)
			}
		}
	}
}

// ctrl+z escalates: Ask → Conversations, with the strip's conversation selected
// so the operator lands mid-stream in the real view. The escalation must also
// minimise the strip.
func TestSlideOutPanelCtrlZEscalates(t *testing.T) {
	m := panelApp(t)
	m.chatConvID = "conv-panel"
	nm, _ := m.dispatch(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h'}})
	m = nm
	if !m.panelVisible() {
		t.Fatal("precondition: the strip is out")
	}
	nm, _ = m.dispatch(tea.KeyMsg{Type: tea.KeyCtrlZ})
	m = nm
	if m.panelVisible() {
		t.Fatal("escalating must minimise the strip")
	}
	if m.ActiveTab() != TabAsk {
		t.Fatalf("escalating must land on Ask, got %q", m.ActiveTab())
	}
	if m.askMode != askConversations {
		t.Fatal("escalating must land in the Conversations view")
	}
	if m.chatConvID != "conv-panel" {
		t.Fatalf("escalating must keep the conversation selected, got %q", m.chatConvID)
	}
}

// Clicking the bracketed affordance escalates, and the strip consumes clicks in
// its own rows rather than letting them fall through to the pane beneath.
func TestSlideOutPanelClickEscalatesAndConsumes(t *testing.T) {
	m := panelApp(t)
	m.chatConvID = "conv-click"
	nm, _ := m.dispatch(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h'}})
	m = nm
	top := m.panelTopRow()
	if top < 0 {
		t.Fatal("strip has no top row while visible")
	}
	x0, x1 := m.panelEscalateX(m.width)
	upd, _ := m.Update(tea.MouseMsg{
		Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
		X: (x0 + x1) / 2, Y: top,
	})
	m = upd.(*App)
	if m.ActiveTab() != TabAsk || m.askMode != askConversations {
		t.Fatalf("clicking the affordance must escalate (tab=%q mode=%v)", m.ActiveTab(), m.askMode)
	}
}

// A body click in the strip must not be treated as a pane click (it is part of
// the compose area).
func TestSlideOutPanelBodyClickDoesNotReachThePane(t *testing.T) {
	m := panelApp(t)
	nm, _ := m.dispatch(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h'}})
	m = nm
	top := m.panelTopRow()
	// A body row, centre column — would be a pane click without the guard.
	upd2, _ := m.Update(tea.MouseMsg{
		Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
		X: 40, Y: top + 2,
	})
	m = upd2.(*App)
	if m.ActiveTab() != TabWork {
		t.Fatalf("a body click must not navigate away (tab=%q)", m.ActiveTab())
	}
	if m.chatFocus != focusComposer {
		t.Fatal("a body click keeps the composer focus (it is the compose area)")
	}
}
