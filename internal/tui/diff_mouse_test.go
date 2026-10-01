package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/testfixtures"
	"github.com/beardedparrott/orchicon/internal/tui/diffs"
)

// TestDiffPaneMouseTabSwitchPersistsThroughNav: a mouse click on the pane's
// "tree" tab switches the tab, and that choice survives navigating to another
// screen and back (the acceptance criterion: sidebar open state persists
// while navigating between screens). This exercises the terminal-global
// row/col offsets the shell forwards for pane clicks (pane tab bar sits at
// terminal row 2, below the shell tab bar + its bottom border).
func TestDiffPaneMouseTabSwitchPersistsThroughNav(t *testing.T) {
	m := newTestApp()
	ex := &diffStubOwner{detailID: "exec-1"}
	m.RegisterScreen(TabExecution, ex)
	m.setFocus(focusContent)
	m.width, m.height = 120, 40
	m.SwitchTo(TabExecution)
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	m = nm.(*App)
	m.width, m.height = 120, 40
	m.refreshLayout()

	// Click the "tree" tab at the pane's tab-bar row (terminal row 2) and
	// terminal column 10 (the "tree" label text, right of the left border).
	// Click the "tree" tab at the pane's tab-bar row (terminal row 3: the
	// shell paints tab bar / underline / gap on rows 0-2) and terminal column
	// 10 (the "tree" label text, right of the left border).
	m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 10, Y: 3})
	m.syncDiffPaneState()
	if m.diffTab != "tree" {
		t.Fatalf("mouse click did not switch pane to tree (shell diffTab=%s)", m.diffTab)
	}

	// Navigate away and back; the pane must still be on the tree tab.
	m.SwitchTo(TabWork)
	m.SwitchTo(TabExecution)
	if m.diffPane.Tab != "tree" {
		t.Fatalf("pane tab %s after navigation; want tree (state did not persist)", m.diffPane.Tab)
	}
}

// TestDiffPaneRowClickFocusPersistsThroughNav: a file-row click forwarded
// through the SHELL (the rail) must select the file AND move the pane to the
// Diff tab, with the shell's persisted App.diffTab following automatically
// (diffMsg calls syncDiffPaneState after every forwarded mouse event — the
// point is that this test never calls it by hand), and the choice must
// survive navigating to another screen and back.
func TestDiffPaneRowClickFocusPersistsThroughNav(t *testing.T) {
	m := newTestApp()
	ex := &diffStubOwner{detailID: "exec-1"}
	m.RegisterScreen(TabExecution, ex)
	m.setFocus(focusContent)
	m.width, m.height = 120, 40
	m.SwitchTo(TabExecution)
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	m = nm.(*App)
	m.width, m.height = 120, 40
	m.refreshLayout()

	vecs, err := testfixtures.LoadFileEditVectors()
	if err != nil {
		t.Fatalf("load fixtures: %v", err)
	}
	byName := map[string]testfixtures.FileEditVector{}
	for _, v := range vecs {
		byName[v.Name] = v
	}
	create := byName["create-new-file"]
	modify := byName["modify-two-lines"]
	diffOf := func(v testfixtures.FileEditVector) string {
		if v.ExpectedUnifiedDiff == nil {
			return ""
		}
		return *v.ExpectedUnifiedDiff
	}
	// Load the pane's data by driving it directly: FetchDoneMsg populates the
	// groups and defaults SelectedPath to groups[0] (the create file). The
	// click below targets the SECOND row, so the selection genuinely changes.
	m.diffPane.Update(diffs.FetchDoneMsg{Snapshot: &diffs.Snapshot{Edits: []*apiv1.FileEdit{
		{Id: "1", Path: create.Path, Kind: create.ExpectedKind, Tool: "batch_write", Seq: 1, UnifiedDiff: diffOf(create)},
		{Id: "2", Path: modify.Path, Kind: modify.ExpectedKind, Tool: "batch_write", Seq: 2, UnifiedDiff: diffOf(modify)},
	}}})
	if m.diffPane.SelectedPath != create.Path {
		t.Fatalf("default selection %q, want %q", m.diffPane.SelectedPath, create.Path)
	}
	m.diffPane.SetTab(diffs.TabTimeline)
	m.syncDiffPaneState()

	// Click the second file row through the shell: per the pane's
	// paneTopRow/paneBodyRow contract the tab bar is terminal row 3 and body
	// row 0 is terminal row 4, so terminal Y=5 is body row 1 (the modify
	// file). X=5 is inside the rail. Same global-coordinate contract as the
	// tab-click test above. App is a VALUE model — the shell-owned diffTab
	// written by diffMsg's sync lands on the returned copy (only the pane
	// pointer is shared), so the returned App must be captured: this test
	// never calls syncDiffPaneState by hand, which is the point.
	NM, _ := m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 5, Y: 5})
	m = NM.(*App)
	if m.diffTab != diffs.TabDiff {
		t.Fatalf("shell diffTab=%s after rail row click, want diff (syncDiffPaneState did not follow)", m.diffTab)
	}
	if m.diffPath != modify.Path {
		t.Fatalf("shell diffPath=%q after rail row click, want %q", m.diffPath, modify.Path)
	}
	if m.diffPane.SelectedPath != modify.Path {
		t.Fatalf("pane selection %q after rail row click, want %q", m.diffPane.SelectedPath, modify.Path)
	}

	// Navigate away and back; restoreDiffPaneState must bring the pane back
	// to the Diff tab with the clicked file still selected.
	m.SwitchTo(TabWork)
	m.SwitchTo(TabExecution)
	if m.diffPane.Tab != diffs.TabDiff {
		t.Fatalf("pane tab %s after navigation; want diff (state did not persist)", m.diffPane.Tab)
	}
	if m.diffPane.SelectedPath != modify.Path {
		t.Fatalf("pane selection %q after navigation; want %q", m.diffPane.SelectedPath, modify.Path)
	}
}
