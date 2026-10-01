package diffs

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
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
		m.click(c.termX, paneTopRow)
		if m.Tab != c.want {
			t.Errorf("tab click at terminal (%d,2): got %s, want %s", c.termX, m.Tab, c.want)
		}
	}

	// A click on the shell tab bar (y=0) or its bottom border (y=1) must not
	// change the pane tab.
	m.Tab = TabDiff
	m.click(10, paneTopRow-3)
	if m.Tab != TabDiff {
		t.Errorf("click at y=0 (shell tab bar) changed tab to %s", m.Tab)
	}
	m.click(10, paneTopRow-2)
	if m.Tab != TabDiff {
		t.Errorf("click at y=1 (shell tab bar border) changed tab to %s", m.Tab)
	}
	// A click right of the pane (x >= width+border) must not change the tab.
	m.click(48+5, paneTopRow)
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

	// terminal row 3 = body row 0 → group[0] ("create"). The tab is reset
	// before EVERY click because a body-row click now also switches to the
	// Diff tab (the select-and-focus parity fix); without the reset the next
	// click would hit the Diff-tab no-op guard.
	m.Tab = TabTree
	m.click(5, paneBodyRow)
	if m.SelectedPath != vecs["create-new-file"].Path {
		t.Errorf("file click (5,3) selected %q, want %q", m.SelectedPath, vecs["create-new-file"].Path)
	}
	// body row 1 = terminal row 4 → group[1] ("modify").
	m.Tab = TabTree
	m.click(5, paneBodyRow+1)
	if m.SelectedPath != vecs["modify-two-lines"].Path {
		t.Errorf("file click (5,4) selected %q, want %q", m.SelectedPath, vecs["modify-two-lines"].Path)
	}
	// Out-of-range terminal row (beyond the file list) must not change
	// selection, and must not switch tabs (the guard short-circuits SetTab).
	m.Tab = TabTree
	m.click(5, 99)
	if m.SelectedPath != vecs["modify-two-lines"].Path {
		t.Errorf("out-of-range click changed selection to %q", m.SelectedPath)
	}
	if m.Tab != TabTree {
		t.Errorf("out-of-range click switched tab to %s, want tree", m.Tab)
	}
}

// TestClickFileRowFocusesDiff is the anti-drift parity test: a file-row click
// in EITHER the Tree or the Timeline tab must select the file AND move the
// pane to the Diff tab with that file's parsed diff loaded — all in one event,
// both tabs through the same loop (the gap that produced the operator report:
// Timeline clicks selected invisibly and never focused the diff).
func TestClickFileRowFocusesDiff(t *testing.T) {
	vecs := vecByName(t)
	create := vecs["create-new-file"]
	modify := vecs["modify-two-lines"]
	wantRows := ParseUnifiedDiff(deref(modify.ExpectedUnifiedDiff))
	for _, tab := range []Tab{TabTree, TabTimeline} {
		t.Run(string(tab), func(t *testing.T) {
			m := NewModel(nil, nil)
			m.Width = 48
			m.Height = 24
			m.groups = GroupByFile([]*apiv1.FileEdit{
				makeEdit(create, 1),
				makeEdit(modify, 2),
			})
			m.SelectedPath = create.Path
			m.rows = m.rowsForSelected()
			m.Tab = tab
			// Body row 1 → group[1] ("modify"), a DIFFERENT path than the
			// pre-selected one.
			m.click(5, paneBodyRow+1)
			if m.Tab != TabDiff {
				t.Errorf("row click from %s left tab %s, want diff", tab, m.Tab)
			}
			if m.SelectedPath != modify.Path {
				t.Errorf("row click selected %q, want %q", m.SelectedPath, modify.Path)
			}
			if !reflect.DeepEqual(m.rows, wantRows) {
				t.Errorf("rows after click are not the clicked file's parsed diff")
			}
		})
	}
}

// TestClickSelectedRowStillFocusesDiff pins the unconditional SetTab: clicking
// the ALREADY-selected row must still bounce the pane to the Diff tab —
// SelectPath early-returns on the same path, so a conditional switch would
// leave a re-click inert (the GUI calls onTabChange unconditionally).
func TestClickSelectedRowStillFocusesDiff(t *testing.T) {
	vecs := vecByName(t)
	create := vecs["create-new-file"]
	m := NewModel(nil, nil)
	m.Width = 48
	m.Height = 24
	m.groups = GroupByFile([]*apiv1.FileEdit{makeEdit(create, 1)})
	m.SelectedPath = create.Path
	m.Tab = TabTree
	m.click(5, paneBodyRow) // row 0 is already selected
	if m.Tab != TabDiff {
		t.Errorf("re-click on selected row left tab %s, want diff", m.Tab)
	}
	if m.SelectedPath != create.Path {
		t.Errorf("re-click changed selection to %q, want %q", m.SelectedPath, create.Path)
	}
}

// TestClickDiffTabBodyStaysNoOp pins the m.Tab != TabDiff guard: on the Diff
// tab there is no file list, so a body click must change nothing.
func TestClickDiffTabBodyStaysNoOp(t *testing.T) {
	vecs := vecByName(t)
	create := vecs["create-new-file"]
	modify := vecs["modify-two-lines"]
	m := NewModel(nil, nil)
	m.Width = 48
	m.Height = 24
	m.groups = GroupByFile([]*apiv1.FileEdit{makeEdit(create, 1), makeEdit(modify, 2)})
	m.SelectedPath = create.Path
	m.Tab = TabDiff
	m.click(5, paneBodyRow+1)
	if m.Tab != TabDiff {
		t.Errorf("Diff-tab body click changed tab to %s", m.Tab)
	}
	if m.SelectedPath != create.Path {
		t.Errorf("Diff-tab body click changed selection to %q, want %q", m.SelectedPath, create.Path)
	}
}

// TestClickEmptyListInert pins the no-changed-files state: with no groups the
// body renders the hint text and a click must select nothing AND must not
// switch tabs (there is nothing to focus).
func TestClickEmptyListInert(t *testing.T) {
	m := NewModel(nil, nil)
	m.Width = 48
	m.Height = 24
	m.groups = nil
	m.Tab = TabTree
	m.click(5, paneBodyRow)
	if m.SelectedPath != "" {
		t.Errorf("empty-list click selected %q, want nothing", m.SelectedPath)
	}
	if m.Tab != TabTree {
		t.Errorf("empty-list click switched tab to %s, want tree", m.Tab)
	}
}

// TestClickNoDiffFileStillSelectsAndFocuses covers a file whose latest edit
// carries no unified diff (e.g. a binary): the selection must still highlight
// in BOTH Tree and Timeline (with DiffFileSel) and a click must still switch
// to the Diff tab, even though the Diff body itself shows the hint.
func TestClickNoDiffFileStillSelectsAndFocuses(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
	vecs := vecByName(t)
	create := vecs["create-new-file"]
	noDiff := vecs["binary-no-diff"] // expected_unified_diff is null
	m := NewModel(nil, nil)
	m.Width = 48
	m.Height = 24
	m.groups = GroupByFile([]*apiv1.FileEdit{makeEdit(create, 1), makeEdit(noDiff, 2)})
	m.SelectedPath = create.Path
	m.Tab = TabTimeline
	m.click(5, paneBodyRow+1) // the no-diff file's row
	if m.Tab != TabDiff {
		t.Errorf("no-diff row click left tab %s, want diff", m.Tab)
	}
	if m.SelectedPath != noDiff.Path {
		t.Errorf("no-diff row click selected %q, want %q", m.SelectedPath, noDiff.Path)
	}
	// The Diff tab shows its empty/hint state for this file...
	if body := m.diffBody(); !strings.Contains(ansi.Strip(body), "select a file") {
		t.Errorf("no-diff file's diff body is %q, want the hint", body)
	}
	// ...while the selection still highlights in both list tabs.
	selLine := fmt.Sprintf(" %s %s (%s)", m.groups[1].Path, m.groups[1].Kind, m.groups[1].LastTool)
	tl := strings.Split(m.timelineBody(), "\n")
	if len(tl) != 2 || tl[1] != theme.DiffFileSel.Render(selLine) {
		t.Errorf("timeline did not highlight the selected no-diff row")
	}
	wantTree := theme.DiffFileSel.Render(fmt.Sprintf(" %s +%d −%d", m.groups[1].Path, m.groups[1].Adds, m.groups[1].Dels))
	tr := strings.Split(m.treeBody(), "\n")
	if len(tr) != 2 || tr[1] != wantTree {
		t.Errorf("tree did not highlight the selected no-diff row")
	}
}

// TestTimelineBodySelection pins that the Timeline tab renders a VISIBLE
// selection identical to treeBody's treatment (theme.DiffFileSel on the
// selected row, theme.ListItem otherwise). It needs the TrueColor profile:
// Ascii strips styles, which would make the assertion vacuous (the
// click_close_test.go Ascii precedent is for width checks only). The profile
// is process-global state that leaks across tests in the package, so it is
// restored via t.Cleanup.
func TestTimelineBodySelection(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
	vecs := vecByName(t)
	create := vecs["create-new-file"]
	modify := vecs["modify-two-lines"]
	m := NewModel(nil, nil)
	m.Width = 48
	m.Height = 24
	m.groups = GroupByFile([]*apiv1.FileEdit{makeEdit(create, 1), makeEdit(modify, 2)})
	m.SelectedPath = modify.Path

	tl := strings.Split(m.timelineBody(), "\n")
	if len(tl) != 2 {
		t.Fatalf("timeline body has %d lines, want 2", len(tl))
	}
	wantSel := theme.DiffFileSel.Render(fmt.Sprintf(" %s %s (%s)", m.groups[1].Path, m.groups[1].Kind, m.groups[1].LastTool))
	wantUn := theme.ListItem.Render(fmt.Sprintf(" %s %s (%s)", m.groups[0].Path, m.groups[0].Kind, m.groups[0].LastTool))
	if tl[1] != wantSel {
		t.Errorf("selected timeline row is not styled with DiffFileSel:\n got %q\nwant %q", tl[1], wantSel)
	}
	if tl[0] != wantUn {
		t.Errorf("unselected timeline row is not styled with ListItem:\n got %q\nwant %q", tl[0], wantUn)
	}
	if tl[0] == tl[1] {
		t.Errorf("selected and unselected timeline rows are identical — no visible selection")
	}

	// Mirror the assertion on treeBody so the two tabs are pinned to the same
	// selected-row treatment and cannot drift again.
	tr := strings.Split(m.treeBody(), "\n")
	if len(tr) != 2 {
		t.Fatalf("tree body has %d lines, want 2", len(tr))
	}
	wantTreeSel := theme.DiffFileSel.Render(fmt.Sprintf(" %s +%d −%d", m.groups[1].Path, m.groups[1].Adds, m.groups[1].Dels))
	if tr[1] != wantTreeSel {
		t.Errorf("selected tree row is not styled with DiffFileSel:\n got %q\nwant %q", tr[1], wantTreeSel)
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
