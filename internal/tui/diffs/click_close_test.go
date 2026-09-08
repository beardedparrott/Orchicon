package diffs

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

// TestClickCloseButton: the docked "✕" is the mouse toggle area. A left
// click on it (terminal X = glyphX()+1, content-relative + the left border)
// sets a close request the shell consumes to close the pane. A click on a
// tab label still switches the tab and must NOT close. A click on the
// trailing inert padding (well right of the glyph) must NOT close.
func TestClickCloseButton(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	t.Cleanup(func() { lipgloss.SetColorProfile(termenv.Ascii) })
	m := NewModel(nil, nil)
	m.Width = 48
	m.Height = 24

	// Click on the glyph: terminal X = content-relative glyphX + 1 (border).
	m.closeReq = false
	m.click(m.glyphX()+1, 2)
	if !m.TakeCloseRequest() {
		t.Fatalf("click on ✕ (contentX=%d) did not set a close request", m.glyphX())
	}

	// Click on the trailing inert padding must NOT close.
	m.closeReq = false
	m.click(m.Width+1, 2)
	if m.TakeCloseRequest() {
		t.Fatalf("click on inert padding set a close request")
	}

	// Click on a tab label still switches the tab (does not close).
	m.closeReq = false
	m.Tab = TabDiff
	m.click(10, 2)
	if m.TakeCloseRequest() {
		t.Fatalf("click on a tab label set a close request")
	}
	if m.Tab != TabTree {
		t.Fatalf("tab click did not switch to tree (got %s)", m.Tab)
	}
}

// TestClickCloseRequestClearedOnce: a consumed close request is cleared so a
// later unrelated click does not spuriously close the pane.
func TestClickCloseRequestClearedOnce(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	t.Cleanup(func() { lipgloss.SetColorProfile(termenv.Ascii) })
	m := NewModel(nil, nil)
	m.Width = 48
	m.Height = 24
	m.click(m.glyphX()+1, 2)
	if !m.TakeCloseRequest() {
		t.Fatal("first TakeCloseRequest should be true")
	}
	if m.TakeCloseRequest() {
		t.Fatal("TakeCloseRequest returned true twice — request was not cleared")
	}
	_ = tea.KeyMsg{}
}

// TestClickCloseTabBarNotWide: the tab bar width (glyph included) must not
// exceed the pane content width — the close button must never push the pane
// beyond its rail (no horizontal overflow / tearing).
func TestClickCloseTabBarNotWide(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	t.Cleanup(func() { lipgloss.SetColorProfile(termenv.Ascii) })
	m := NewModel(nil, nil)
	m.Width = 48
	tb := m.tabBar()
	if w := ansi.StringWidth(tb); w > m.Width {
		t.Fatalf("tab bar width %d exceeds pane content width %d (overflow)", w, m.Width)
	}
}
