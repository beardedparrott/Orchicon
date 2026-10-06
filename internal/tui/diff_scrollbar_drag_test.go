package tui

// diff_scrollbar_drag_test.go — GRABBING THE DIFF RAIL'S SCROLLBAR.
//
// The operator: "the scroll only works in the TUI by using the scroll wheel. I can't grab onto the scroll bar
// and drag it up and down like you can in the GUI."
//
// There were TWO defects behind that sentence, and this file pins both:
//
//   1. The bar occupied the pane's LAST cell, which is the shell's rail-RESIZE handle. A press there was
//      claimed as a resize before the pane saw anything — so the bar could not be clicked either, and
//      "grabbing" it resized the rail.
//   2. Even with a column of its own, a drag cannot be handled inside the pane: a left press in the rail sets
//      a SELECTION REGION and clipState consumes the motion that follows, so the pane would win the press and
//      lose every step after it.
//
// So the gesture is the shell's, claimed above the clipboard layer — exactly like the divider's resize, whose
// own comment documents the same trap.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/diffs"
)

// openRailWithContent opens the diff pane on an execution and gives it a body that overflows, so the
// scrollbar is live. It returns the App plus the terminal X of the bar's column.
func openRailWithContent(t *testing.T) (*App, int) {
	t.Helper()
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
	if !m.diffOpen || m.diffPane == nil {
		t.Fatal("ctrl+d did not open the diff rail")
	}

	// A diff long enough to overflow the pane at this size, built here rather than taken from fixtures so
	// the test states its own premise.
	var sb strings.Builder
	sb.WriteString("@@ -1,60 +1,60 @@\n")
	for i := 0; i < 60; i++ {
		sb.WriteString("-old line ")
		sb.WriteString(strings.Repeat("x", 20))
		sb.WriteString("\n+new line ")
		sb.WriteString(strings.Repeat("y", 20))
		sb.WriteString("\n")
	}
	m.diffPane.Update(diffs.FetchDoneMsg{Snapshot: &diffs.Snapshot{Edits: []*apiv1.FileEdit{{
		Id: "1", Path: "src/main.go", Kind: "modify", Tool: "edit", Seq: 1, UnifiedDiff: sb.String(),
	}}}})
	if m.diffPane.View() == "" {
		t.Fatal("the pane rendered nothing, so the bar assertions below would prove nothing")
	}
	barX := m.diffPaneWidth() - 2
	if !m.diffScrollbarHit(barX, 10) {
		t.Fatalf("the bar is not grabbable at x=%d (pane width %d)", barX, m.diffPaneWidth())
	}
	return m, barX
}

// THE REPORTED BUG: a press on the bar was claimed as a RAIL RESIZE. It must jump the viewport instead, and
// must not touch the rail's width.
func TestPressOnTheScrollbarJumpsInsteadOfResizingTheRail(t *testing.T) {
	m, barX := openRailWithContent(t)
	widthBefore := m.diffPaneWidth()
	viewBefore := m.diffPane.View()

	nm, _ := m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: barX, Y: 10})
	m = nm.(*App)

	if m.diffResizing {
		t.Error("a press on the scrollbar started a RAIL RESIZE — the two columns must not be the same, or " +
			"the operator cannot grab the bar at all")
	}
	if !m.diffScrollDragging {
		t.Fatal("a press on the scrollbar did not start a drag")
	}
	if m.diffPaneWidth() != widthBefore {
		t.Errorf("the rail width changed from %d to %d on a bar press", widthBefore, m.diffPaneWidth())
	}
	if m.diffPane.View() == viewBefore {
		t.Error("the press did not move the viewport — the grab did not reach the pane")
	}
}

// THE DRAG ITSELF: motion tracks the pointer, and it keeps tracking even when a diagonal hand movement takes
// the pointer out of the rail's columns (the gesture was claimed at the press, so no X test applies to it).
func TestDraggingTheScrollbarFollowsThePointer(t *testing.T) {
	m, barX := openRailWithContent(t)

	nm, _ := m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: barX, Y: 30})
	m = nm.(*App)
	atBottom := m.diffPane.View()

	// Drag UP: the viewport must move back towards the top.
	nm, _ = m.Update(tea.MouseMsg{Action: tea.MouseActionMotion, Button: tea.MouseButtonLeft, X: barX, Y: 6})
	m = nm.(*App)
	if m.diffPane.View() == atBottom {
		t.Error("motion during the bar drag did not move the viewport")
	}
	midway := m.diffPane.View()

	// …and keep dragging with the pointer OUTSIDE the rail's columns, which is what a real drag does when the
	// hand drifts. The gesture is the shell's now, so the bar must still follow.
	nm, _ = m.Update(tea.MouseMsg{Action: tea.MouseActionMotion, Button: tea.MouseButtonLeft, X: m.diffPaneWidth() + 25, Y: 34})
	m = nm.(*App)
	if m.diffPane.View() == midway {
		t.Error("the drag stopped when the pointer left the rail's columns — a diagonal drag must keep " +
			"following, since the gesture was claimed at the press")
	}
	if !m.diffScrollDragging {
		t.Error("the drag ended early")
	}
}

// The release ends the gesture, so the pane stops following a pointer that is no longer held.
func TestReleasingTheScrollbarEndsTheDrag(t *testing.T) {
	m, barX := openRailWithContent(t)
	nm, _ := m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: barX, Y: 10})
	m = nm.(*App)
	if !m.diffScrollDragging {
		t.Fatal("the press did not start a drag")
	}

	nm, _ = m.Update(tea.MouseMsg{Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft, X: barX, Y: 20})
	m = nm.(*App)
	if m.diffScrollDragging {
		t.Fatal("the release did not end the drag")
	}
	settled := m.diffPane.View()
	nm, _ = m.Update(tea.MouseMsg{Action: tea.MouseActionMotion, Button: tea.MouseButtonLeft, X: barX, Y: 35})
	m = nm.(*App)
	if m.diffPane.View() != settled {
		t.Error("the pane kept following the pointer after the release")
	}
}

// NO REGRESSION: the rail's resize edge still resizes. It is the pane's LAST cell, one cell right of the bar,
// and the two must stay distinguishable.
func TestTheResizeEdgeStillResizesNextToTheBar(t *testing.T) {
	m, barX := openRailWithContent(t)
	divX := m.diffPaneWidth() - 1

	if barX == divX {
		t.Fatal("the bar and the resize edge are the same column — this is the defect, not the fix")
	}
	if m.diffScrollbarHit(divX, 10) {
		t.Error("the pane claims a grab on the resize edge")
	}
	nm, _ := m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: divX, Y: 10})
	m = nm.(*App)
	if !m.diffResizing {
		t.Error("a press on the rail's edge no longer starts a resize")
	}
	if m.diffScrollDragging {
		t.Error("a press on the resize edge also started a scrollbar drag")
	}
}
