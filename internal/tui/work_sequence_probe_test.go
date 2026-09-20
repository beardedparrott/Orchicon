package tui

// work_sequence_probe_test.go — the operator's EXACT sequence, step by step.
//
// Their model: Tab reaches the top menu → up/down moves the submenu → Enter selects an
// entry → arrows move the pane → Tab breaks back to the submenu. This drives that
// literally against the real Work screen and logs each stop, so a failure names the step
// that breaks rather than a guess.

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/screens/work"
)

func seqApp(t *testing.T) *App {
	t.Helper()
	m := newTestApp()
	m.RegisterScreen(TabWork, work.New(nil, m.reg, ""))
	m.dispatch(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.SwitchTo(TabWork)
	return m
}

func TestOperatorSequenceOnWork(t *testing.T) {
	m := seqApp(t)

	t.Logf("launch: tab=%q focus=%v menu=%v", m.ActiveTab(), m.chatFocus, m.TabMenu() != nil)

	// STEP 1 — Tab until the BAR is on Work.
	for i := 0; i < 12 && !(m.ActiveTab() == TabWork && m.chatFocus == focusTabs); i++ {
		m.dispatch(tea.KeyMsg{Type: tea.KeyTab})
	}
	t.Logf("step 1 (Tab to bar): tab=%q focus=%v", m.ActiveTab(), m.chatFocus)
	if m.ActiveTab() != TabWork {
		t.Fatalf("could not Tab onto the Work bar: tab=%q", m.ActiveTab())
	}

	// STEP 2 — Enter opens the submenu.
	m.dispatch(tea.KeyMsg{Type: tea.KeyEnter})
	t.Logf("step 2 (Enter): menu=%v", m.TabMenu() != nil)
	if m.TabMenu() == nil {
		t.Fatal("ENTER did not open the Work submenu from the bar — the operator's step 2")
	}

	// STEP 3 — down/up move the submenu selection.
	sel0 := m.TabMenu().Sel
	m.dispatch(tea.KeyMsg{Type: tea.KeyDown})
	t.Logf("step 3 (down): menu=%v sel %d -> %d", m.TabMenu() != nil, sel0, m.TabMenu().Sel)
	if m.TabMenu() == nil {
		t.Fatal("DOWN closed the submenu instead of moving it — the operator's 'can't use up/down on the Work menu'")
	}
	if m.TabMenu().Sel == sel0 {
		t.Fatal("DOWN did not move the Work submenu selection — the operator's step 3")
	}
	m.dispatch(tea.KeyMsg{Type: tea.KeyUp})
	if m.TabMenu().Sel != sel0 {
		t.Fatal("UP did not restore the Work submenu selection")
	}

	// STEP 4 — Enter selects the highlighted entry and moves focus into the pane.
	m.dispatch(tea.KeyMsg{Type: tea.KeyEnter})
	t.Logf("step 4 (Enter select): menu=%v focus=%v", m.TabMenu() != nil, m.chatFocus)
	if m.TabMenu() != nil {
		t.Fatal("ENTER did not select the highlighted Work submenu entry — the operator's 'can't hit enter on any submenu'")
	}
	if m.chatFocus != focusContent {
		t.Fatalf("selecting a submenu entry left focus at %v — the pane can never take the arrows", m.chatFocus)
	}

	// STEP 5 — arrows move the pane's list.
	sc, ok := m.screens[TabWork].(interface{ DetailID() string })
	_ = sc
	_ = ok
	m.dispatch(tea.KeyMsg{Type: tea.KeyDown})
	t.Logf("step 5 (arrows in pane): focus=%v", m.chatFocus)

	// STEP 6 — Tab breaks back to the submenu (focusTabs), which is the operator's
	// "Tab breaks that and moves through submenu again".
	m.dispatch(tea.KeyMsg{Type: tea.KeyTab})
	t.Logf("step 6 (Tab back to bar): focus=%v", m.chatFocus)
	if m.chatFocus != focusTabs {
		t.Fatalf("Tab from the pane left focus at %v, want the bar — the way back to the submenu", m.chatFocus)
	}

	// STEP 7 — shift+tab reverse must work from here.
	before := m.ActiveTab()
	m.dispatch(tea.KeyMsg{Type: tea.KeyShiftTab})
	t.Logf("step 7 (shift+tab): tab %q -> %q focus=%v", before, m.ActiveTab(), m.chatFocus)
	if m.ActiveTab() == before {
		t.Fatalf("SHIFT+TAB did not move the ring from the Work bar (still %q)", before)
	}
}
