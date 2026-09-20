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

	// Content-focused CTRL+D opens the pane on a diff-relevant owner. Wire a
	// stub execution screen exposing DetailID so diffOwner resolves.
	ex := &diffStubOwner{detailID: "exec-1"}
	m.RegisterScreen(TabExecution, ex)
	m.setFocus(focusContent)
	m.SwitchTo(TabExecution)
	nm, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	if !nm.(*App).diffOpen {
		t.Fatal("content-focused ctrl+d should open the diff pane")
	}
	// Re-toggle closes it.
	nm, _ = nm.(*App).Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	if nm.(*App).diffOpen {
		t.Fatal("re-toggle ctrl+d should close the diff pane")
	}
}

// TestDiffPaneStatePersistsAcrossSwitch: the shell-owned open/tab/selected
// state survives SwitchTo (the GUI persists it at the host).
func TestDiffPaneStatePersistsAcrossSwitch(t *testing.T) {
	m := newTestApp()
	m.diffOpen = true
	m.diffTab = diffs.TabTree
	m.diffPath = "cmd/new.go"
	// Seed the pane from the shell's saved state (the restore direction runs
	// on open/refresh); the shell state is the authority and must survive.
	m.restoreDiffPaneState()
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
	if want := 120 - m.diffPaneWidth(); reflowed != want {
		t.Fatalf("reflowed width = %d, want %d", reflowed, want)
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

// THE DIFF PANE IS SIZED TO THE TERMINAL, not fixed at 48.
//
// The operator: "the diff box is very tiny and cut off. Maybe we should increase the size of the diff
// pane." At a fixed 48 a side-by-side diff gets ~18 cells of code per column after the line numbers,
// which cannot show a line of Go — and on a 190-column terminal it wasted most of the width.
//
// This also pins the property that made the width a METHOD: the pane's drawn width and its clickable
// region must be the same number, so a click near the right edge cannot land on the content pane while
// looking like it is inside the diff.
func TestDiffPaneWidthScalesWithTheTerminal(t *testing.T) {
	m := newTestApp()
	m.diffOpen = true

	prev := 0
	for _, w := range []int{80, 120, 160, 190, 240} {
		m.width = w
		got := m.diffPaneWidth()
		if got <= prev {
			t.Errorf("terminal %d: diff pane %d did not grow from %d — the pane is not scaling", w, got, prev)
		}
		prev = got
		// The diff is a SIDEBAR: the content must keep at least half the terminal.
		if got > w/2 {
			t.Errorf("terminal %d: diff pane %d takes more than half the screen (%d), squeezing the "+
				"work item or execution beside it", w, got, w/2)
		}
		// And the pane must stay wide enough for the side-by-side layout it exists to show.
		if got < diffs.MinSideBySideWidth && w >= 120 {
			t.Errorf("terminal %d: diff pane %d is below MinSideBySideWidth (%d), so the pane would "+
				"collapse to the unified layout on a screen with plenty of room", w, got, diffs.MinSideBySideWidth)
		}
		// THE HIT REGION MATCHES THE DRAW: contentWidth is what the rest of the layout uses.
		if want := w - got; m.contentWidth() != want {
			t.Errorf("terminal %d: contentWidth = %d, want %d — the pane and the content disagree",
				w, m.contentWidth(), want)
		}
	}

	// An 80-column terminal is the smallest supported size: the pane still gets a usable share.
	m.width = 80
	if got := m.diffPaneWidth(); got < 32 {
		t.Errorf("at 80 columns the diff pane is %d cells — too narrow to read a diff", got)
	}
}
