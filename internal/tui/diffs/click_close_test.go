package diffs

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
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
	m.click(m.glyphX()+1, paneTopRow)
	if !m.TakeCloseRequest() {
		t.Fatalf("click on ✕ (contentX=%d) did not set a close request", m.glyphX())
	}

	// Click on the trailing inert padding must NOT close.
	m.closeReq = false
	m.click(m.Width+1, paneTopRow)
	if m.TakeCloseRequest() {
		t.Fatalf("click on inert padding set a close request")
	}

	// Click on a tab label still switches the tab (does not close).
	m.closeReq = false
	m.Tab = TabDiff
	m.click(10, paneTopRow)
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
	m.click(m.glyphX()+1, paneTopRow)
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

// TestClickRowThenClose pins that the pane's hit-tests still hit AFTER the
// automatic select-and-focus tab switch: a Timeline row click switches the
// pane to the Diff tab, which re-renders the tab bar with a DIFFERENT active
// tab — the docked ✕ and the three tab labels must still land at their
// coordinates. glyphX() is called AFTER the row click so the assertion
// measures the re-rendered bar (that is the point of the test).
func TestClickRowThenClose(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	t.Cleanup(func() { lipgloss.SetColorProfile(termenv.Ascii) })
	vecs := vecByName(t)
	m := NewModel(nil, nil)
	m.Width = 48
	m.Height = 24
	m.groups = GroupByFile([]*apiv1.FileEdit{
		makeEdit(vecs["create-new-file"], 1),
		makeEdit(vecs["modify-two-lines"], 2),
	})
	m.SelectedPath = vecs["create-new-file"].Path

	// Sequence: Timeline row click (→ Diff tab) THEN ✕ — the pane must close.
	m.Tab = TabTimeline
	m.click(5, paneBodyRow)
	if m.Tab != TabDiff {
		t.Fatalf("timeline row click left tab %s, want diff", m.Tab)
	}
	m.closeReq = false
	m.click(m.glyphX()+1, paneTopRow) // re-measured AFTER the tab switch
	if !m.TakeCloseRequest() {
		t.Fatalf("✕ click after a row click (contentX=%d) did not close the pane", m.glyphX())
	}

	// And each tab label still hits its tab after the switch.
	cases := []struct {
		termX int
		want  Tab
	}{{3, TabDiff}, {10, TabTree}, {17, TabTimeline}}
	for _, c := range cases {
		m.Tab = TabTimeline
		m.click(5, paneBodyRow) // row click → now on Diff tab
		m.click(c.termX, paneTopRow)
		if m.Tab != c.want {
			t.Errorf("tab label click at term X=%d after row click: got %s, want %s", c.termX, m.Tab, c.want)
		}
	}
}
