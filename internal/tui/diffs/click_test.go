package diffs

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// TestClickTabCoordinates verifies mouse tab-click resolution against the
// terminal layout the shell produces (the pane is a left rail rendered below
// the shell tab bar, so its content starts at (col+1, row+1)). The tab bar is
// terminal row 1; a click on a tab label anywhere in its hit region (the
// label text plus its 1-cell padding) must switch to that tab, and a click on
// the shell tab bar (y=0) must not.
func TestClickTabCoordinates(t *testing.T) {
	vecs := vecByName(t)
	m := NewModel(nil, nil)
	m.Width = 48
	m.Height = 24
	m.groups = GroupByFile([]*apiv1.FileEdit{makeEdit(vecs["create-new-file"], 1)})
	m.SelectedPath = vecs["create-new-file"].Path

	cases := []struct {
		termX int
		want  Tab
	}{ // terminal X = contentX + 1 (left border column)
		{3, TabDiff}, {6, TabDiff}, // "diff" text cols 2-5 → term 3-6
		{10, TabTree}, {13, TabTree}, // "tree" text cols 9-12 → term 10-13
		{17, TabTimeline}, {24, TabTimeline}, // "timeline" text cols 16-23 → term 17-24
	}
	for _, c := range cases {
		m.Tab = TabDiff
		m.click(c.termX, 2)
		if m.Tab != c.want {
			t.Errorf("tab click at terminal (%d,2): got %s, want %s", c.termX, m.Tab, c.want)
		}
	}

	// A click on the shell tab bar (y=0) or its bottom border (y=1) must not
	// change the pane tab.
	m.Tab = TabDiff
	m.click(10, 0)
	if m.Tab != TabDiff {
		t.Errorf("click at y=0 (shell tab bar) changed tab to %s", m.Tab)
	}
	m.click(10, 1)
	if m.Tab != TabDiff {
		t.Errorf("click at y=1 (shell tab bar border) changed tab to %s", m.Tab)
	}
	// A click right of the pane (x >= width+border) must not change the tab.
	m.click(48+5, 2)
	if m.Tab != TabDiff {
		t.Errorf("out-of-pane click changed tab to %s", m.Tab)
	}
}

// TestClickFileRowCoordinates verifies mouse click-through from the tree tab
// to a file's diff: terminal row 2 is body row 0 (first file), row 3 is the
// second file, etc.
func TestClickFileRowCoordinates(t *testing.T) {
	vecs := vecByName(t)
	m := NewModel(nil, nil)
	m.Width = 48
	m.Height = 24
	m.groups = GroupByFile([]*apiv1.FileEdit{
		makeEdit(vecs["create-new-file"], 1),
		makeEdit(vecs["modify-two-lines"], 2),
	})
	m.SelectedPath = vecs["create-new-file"].Path
	m.Tab = TabTree

	// terminal row 3 = body row 0 → group[0] ("create").
	m.click(5, 3)
	if m.SelectedPath != vecs["create-new-file"].Path {
		t.Errorf("file click (5,3) selected %q, want %q", m.SelectedPath, vecs["create-new-file"].Path)
	}
	// body row 1 = terminal row 4 → group[1] ("modify").
	m.click(5, 4)
	if m.SelectedPath != vecs["modify-two-lines"].Path {
		t.Errorf("file click (5,4) selected %q, want %q", m.SelectedPath, vecs["modify-two-lines"].Path)
	}
	// Out-of-range terminal row (beyond the file list) must not change selection.
	m.click(5, 99)
	if m.SelectedPath != vecs["modify-two-lines"].Path {
		t.Errorf("out-of-range click changed selection to %q", m.SelectedPath)
	}
}

// TestHandleKeyTabSwitchAsymmetric verifies `l` steps the tab forward and `h`
// steps it backward — the two directions are symmetric and reach every tab.
func TestHandleKeyTabSwitchAsymmetric(t *testing.T) {
	m := NewModel(nil, nil)
	order := []Tab{TabDiff, TabTree, TabTimeline}
	// `l` (next) cycles forward through the three tabs.
	tab := TabDiff
	for _, want := range []Tab{TabTree, TabTimeline, TabDiff} {
		m.SetTab(tab)
		m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
		if m.Tab != want {
			t.Errorf("l from %s -> %s, want %s", tab, m.Tab, want)
		}
		tab = m.Tab
	}
	// `h` (previous) cycles backward.
	tab = TabDiff
	for _, want := range []Tab{TabTimeline, TabTree, TabDiff} {
		m.SetTab(tab)
		m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h'}})
		if m.Tab != want {
			t.Errorf("h from %s -> %s, want %s", tab, m.Tab, want)
		}
		tab = m.Tab
	}
	_ = order
}

// TestRenderSideBySideColumnsAlign verifies the side-by-side renderer keeps
// the old|new separator in a fixed cell column across every row (the
// acceptance criteria: paired line numbers / no tearing or column drift).
func TestRenderSideBySideColumnsAlign(t *testing.T) {
	vecs := vecByName(t)
	rows := ParseUnifiedDiff(deref(vecs["modify-two-lines"].ExpectedUnifiedDiff))
	sepCellCol := -1
	for _, line := range strings.Split(renderSideBySide(rows, 48), "\n") {
		s := ansi.Strip(line)
		idx := strings.Index(s, "│")
		if idx < 0 {
			continue
		}
		col := ansi.StringWidth(s[:idx])
		if sepCellCol == -1 {
			sepCellCol = col
		} else if col != sepCellCol {
			t.Fatalf("separator drifted: first=%d this=%d line=%q", sepCellCol, col, s)
		}
		if ansi.StringWidth(s) > 48 {
			t.Fatalf("render exceeded pane width %d: %q", 48, s)
		}
	}
}
