package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/diffs"
)

// TestDiffToggleRequiresContentFocus: D / shift+D toggles the pane only
// when focus is on the content (never while composing — `d` stays a literal
// character in the composer; Ctrl+D is never bound).
func TestDiffToggleRequiresContentFocus(t *testing.T) {
	m := newTestApp()

	// While composing, the `d` key must NOT open the pane (dispatch returns
	// to the dock input path before routes).
	m.setFocus(focusComposer)
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	if nm.(*App).diffOpen {
		t.Fatal("composer-focused 'd' must not open the diff pane")
	}

	// Content-focused `D` opens the pane on a diff-relevant owner. Wire a
	// stub execution screen exposing DetailID so diffOwner resolves.
	ex := &diffStubOwner{detailID: "exec-1"}
	m.RegisterScreen(TabExecution, ex)
	m.setFocus(focusContent)
	m.SwitchTo(TabExecution)
	nm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'D'}})
	if !nm.(*App).diffOpen {
		t.Fatal("content-focused 'D' should open the diff pane")
	}
	// Re-toggle closes it.
	nm, _ = nm.(*App).Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	if nm.(*App).diffOpen {
		t.Fatal("re-toggle 'd' should close the diff pane")
	}
}

// TestDiffPaneStatePersistsAcrossSwitch: the shell-owned open/tab/selected
// state survives SwitchTo (the GUI persists it at the host).
func TestDiffPaneStatePersistsAcrossSwitch(t *testing.T) {
	m := newTestApp()
	m.diffOpen = true
	m.diffTab = diffs.TabTree
	m.diffPath = "cmd/new.go"
	m.syncDiffPaneState()
	// Switch away and back; state must not reset.
	m.SwitchTo(TabWork)
	m.SwitchTo(TabExecution)
	if !m.diffOpen {
		t.Fatal("diffOpen must persist across SwitchTo")
	}
	if m.diffTab != diffs.TabTree {
		t.Fatalf("diffTab = %s, want tree", m.diffTab)
	}
}

// TestContentWidthReflow: opening the pane shrinks the content area (the
// screen + dock reflow) and closing restores the full width.
func TestContentWidthReflow(t *testing.T) {
	m := newTestApp()
	full := m.contentWidth() // 120 (width), no pane
	if full != 120 {
		t.Fatalf("full width = %d, want 120", full)
	}
	m.diffOpen = true
	reflowed := m.contentWidth()
	if reflowed != 120-DiffPaneWidth {
		t.Fatalf("reflowed width = %d, want %d", reflowed, 120-DiffPaneWidth)
	}
	m.diffOpen = false
	if again := m.contentWidth(); again != 120 {
		t.Fatalf("after close width = %d, want 120", again)
	}
}

// diffStubOwner is a minimal execution-screen stub exposing DetailID so the
// diffOwner derivation resolves a execution owner.
type diffStubOwner struct {
	stubScreen
	detailID string
}

func (s *diffStubOwner) DetailID() string { return s.detailID }
