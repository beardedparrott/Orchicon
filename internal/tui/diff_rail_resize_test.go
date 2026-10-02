package tui

// diff_rail_resize_test.go — the drag-resizable diff rail, end to end.
//
// The operator: "The diff pane in the GUI should be resizable by dragging it. Not sure if we can pull that
// off in the TUI as well, but if we can, that would be great and we should."
//
// Three things have to hold together, and each is asserted through the COMPOSED view rather than the
// field, because the field agreeing with itself proves nothing:
//
//  1. the drag RESIZES — and the width, the content area and the drawn pane all move by the same delta;
//  2. the drag is resolved AHEAD OF THE CLIPBOARD — a divider drag must not start a text selection;
//  3. the drag is claimed — it must not leak into the pane as a click (tab switch / file select / ✕).

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/beardedparrott/orchicon/internal/tui/diffs"
)

// markerScreen renders a fixed marker run so the CONTENT area's first column can be located in the
// composed frame. That is what makes "the pane is exactly diffPaneWidth() wide" checkable on the drawn
// output rather than on the number the width method returns.
type markerScreen struct{ stubScreen }

const markerRun = "MARKERMARKER"

func (s *markerScreen) View() string { return strings.Repeat(markerRun, 20) }

// newDiffRailApp builds a shell with the diff pane OPEN over a marker screen on a diff-relevant tab.
func newDiffRailApp(t *testing.T, w, h int) *App {
	t.Helper()
	m := newTestApp()
	m.RegisterScreen(TabExecution, &markerScreen{stubScreen: stubScreen{id: "exec"}})
	m.dispatch(tea.WindowSizeMsg{Width: w, Height: h})
	m.SwitchTo(TabExecution)
	m.diffOpen = true
	m.refreshLayout()
	if m.diffPane == nil {
		t.Fatal("the diff pane is not constructed — the shell cannot resize what it does not have")
	}
	return m
}

// contentStartColumn returns the frame COLUMN the content area begins at, read off the composed view: the
// display column of the first marker in a screen body row. This is the pane's drawn width.
//
// The column is counted in CELLS (lipgloss.Width of the prefix), never in BYTES: the pane's frame carries
// multi-byte runes (│ and ✕), so a byte offset would over-report by 2 cells per such rune — which is
// exactly the ~4-cell drift that made this look like the drawn pane disagreed with the width method.
func contentStartColumn(t *testing.T, m *App) int {
	t.Helper()
	row := strings.Split(m.View(), "\n")[tabBarRows+1]
	i := strings.Index(row, "MARKER")
	if i < 0 {
		t.Fatalf("no marker in the composed body row — the pane or the screen did not render: %q", row)
	}
	return lipgloss.Width(row[:i])
}

// mouse builds a mouse message the way bubbletea delivers one.
func mouse(action tea.MouseAction, x, y int) tea.MouseMsg {
	return tea.MouseMsg{Action: action, Button: tea.MouseButtonLeft, X: x, Y: y}
}

// DRAGGING THE DIVIDER RESIZES THE RAIL, and the composed view follows: the pane grows by the delta the
// drag travelled, and contentWidth shrinks by exactly that much.
func TestDiffRailDragResizesThePaneAndTheComposedView(t *testing.T) {
	m := newDiffRailApp(t, 120, 40)

	before := m.diffPaneWidth()
	if got := contentStartColumn(t, m); got != before {
		t.Fatalf("composed view disagrees with diffPaneWidth before the drag: pane starts at column %d, "+
			"field says %d", got, before)
	}
	beforeContent := m.contentWidth()

	// Press ON the divider column (the pane's right-most cell) on a body row.
	y := tabBarRows + 2
	nm, _ := m.Update(mouse(tea.MouseActionPress, before-1, y))
	m = nm.(*App)

	// Drag four cells to the right.
	want := before + 4
	nm, _ = m.Update(mouse(tea.MouseActionMotion, want-1, y))
	m = nm.(*App)

	if got := m.diffPaneWidth(); got != want {
		t.Fatalf("after a drag of +4 the rail is %d cells, want %d", got, want)
	}
	if got, wantC := contentStartColumn(t, m), want; got != wantC {
		t.Errorf("the COMPOSED view starts the content at column %d after the drag, want %d — the drawn "+
			"pane and the width method disagree", got, wantC)
	}
	if delta := beforeContent - m.contentWidth(); delta != 4 {
		t.Errorf("contentWidth moved by %d, want exactly the drag's 4 — the pane and content do not "+
			"share one source of width", delta)
	}
	// The pane was actually re-sized, not just re-measured.
	if m.diffPane.Width != want {
		t.Errorf("the pane's own SetSize width is %d, want %d", m.diffPane.Width, want)
	}

	// The release ENDS the gesture.
	nm, _ = m.Update(mouse(tea.MouseActionRelease, want-1, y))
	m = nm.(*App)
	if m.diffResizing {
		t.Error("the release did not end the resize gesture")
	}
	// And a motion AFTER the release must not keep resizing (the held flag is what carries it).
	nm, _ = m.Update(mouse(tea.MouseActionMotion, want+10, y))
	m = nm.(*App)
	if got := m.diffPaneWidth(); got != want {
		t.Errorf("motion after the release resized the rail to %d (want %d) — the gesture did not end", got, want)
	}
}

// A drag arriving with MouseButtonNone still resizes: with cell-motion reporting the button often does not
// survive into the motion event, and the gesture must not depend on it.
func TestDiffRailDragHoldsWhenMotionReportsNoButton(t *testing.T) {
	m := newDiffRailApp(t, 120, 40)
	before := m.diffPaneWidth()
	y := tabBarRows + 2

	nm, _ := m.Update(mouse(tea.MouseActionPress, before-1, y))
	m = nm.(*App)
	// Motion with NO button, the shape the clipboard code already tolerates.
	nm, _ = m.Update(tea.MouseMsg{Action: tea.MouseActionMotion, Button: tea.MouseButtonNone, X: before + 5, Y: y})
	m = nm.(*App)

	if got, want := m.diffPaneWidth(), before+6; got != want {
		t.Fatalf("a button-less motion did not resize the rail: got %d, want %d", got, want)
	}
}

// THE DRAG DOES NOT START A TEXT SELECTION. This is the regression the press-order change could introduce:
// clipState begins a drag on ANY left press, so a divider press routed through it would select text
// instead of resizing.
func TestDiffRailDragDoesNotStartASelection(t *testing.T) {
	m := newDiffRailApp(t, 120, 40)
	y := tabBarRows + 2
	x := m.diffPaneWidth() - 1

	nm, _ := m.Update(mouse(tea.MouseActionPress, x, y))
	m = nm.(*App)
	if m.clip.drag {
		t.Fatal("the divider press began a clipboard drag — a resize would select text")
	}
	nm, _ = m.Update(mouse(tea.MouseActionMotion, x+4, y+2))
	m = nm.(*App)
	if m.clip.drag {
		t.Error("the divider drag was picked up by the clipboard")
	}
	if _, _, _, _, ok := m.clip.bounds(); ok {
		t.Error("a clipboard selection region exists after a divider drag")
	}
	nm, _ = m.Update(mouse(tea.MouseActionRelease, x+4, y+2))
	m = nm.(*App)
	if m.clip.drag {
		t.Error("the release left the clipboard in a dragging state")
	}

	// And the drag did not rewrite the selection region either.
	if m.clip.hasRegion {
		t.Error("the divider drag set a clipboard region (selectionRegionAt was reached)")
	}
}

// THE DRAG DOES NOT LEAK INTO THE PANE AS A CLICK: no tab switch, no file select, no close request.
func TestDiffRailDragDoesNotLeakIntoThePane(t *testing.T) {
	m := newDiffRailApp(t, 120, 40)
	m.diffPane.SetTab(diffs.TabTree)
	m.diffPane.SelectPath("cmd/new.go")
	m.syncDiffPaneState()

	tabBefore, pathBefore := m.diffPane.Tab, m.diffPane.SelectedPath
	y := tabBarRows + 2
	x := m.diffPaneWidth() - 1

	nm, _ := m.Update(mouse(tea.MouseActionPress, x, y))
	m = nm.(*App)
	nm, _ = m.Update(mouse(tea.MouseActionMotion, x+4, y))
	m = nm.(*App)
	nm, _ = m.Update(mouse(tea.MouseActionRelease, x+4, y))
	m = nm.(*App)

	if m.diffPane.Tab != tabBefore {
		t.Errorf("the divider drag switched the pane's tab (%s -> %s)", tabBefore, m.diffPane.Tab)
	}
	if m.diffPane.SelectedPath != pathBefore {
		t.Errorf("the divider drag changed the selected file (%q -> %q)", pathBefore, m.diffPane.SelectedPath)
	}
	if !m.diffOpen {
		t.Error("the divider drag closed the pane (the ✕ was triggered)")
	}
	if m.diffPane.TakeCloseRequest() {
		t.Error("the divder drag left a close request on the pane")
	}
	// The shell-owned mirror of the pane state is untouched too (a leak would have synced it).
	if m.diffTab != tabBefore {
		t.Errorf("the shell's diffTab changed to %s during a divider drag", m.diffTab)
	}
}

// A press on a NON-divider column inside the pane is still a pane click — the drag claims ONE column.
func TestDiffRailPaneClickIsNotADividerDrag(t *testing.T) {
	m := newDiffRailApp(t, 120, 40)
	y := tabBarRows + 2

	nm, _ := m.Update(mouse(tea.MouseActionPress, 2, y))
	m = nm.(*App)
	if m.diffResizing {
		t.Error("a press inside the pane's body started a resize — the whole pane would be a divider")
	}
	// And it was NOT consumed by the resize path, so the pane still gets its click.
	if m.diffPaneW != 0 {
		t.Errorf("a body press set an override width (%d) — only the divider column may", m.diffPaneW)
	}
}

// A press on the divider with NO pane open must not resize anything (and must not panic).
func TestDiffRailDividerIsInertWithThePaneClosed(t *testing.T) {
	m := newDiffRailApp(t, 120, 40)
	x := m.diffPaneWidth() - 1
	m.diffOpen = false
	m.diffResizing = false

	nm, _ := m.Update(mouse(tea.MouseActionPress, x, tabBarRows+2))
	m = nm.(*App)
	if m.diffResizing || m.diffPaneW != 0 {
		t.Error("a divider press with the pane closed started a resize")
	}
}

// The floor is real: a drag that would go far below DiffRailMinWidth still leaves a usable rail.
func TestDiffRailWidthFloorHolds(t *testing.T) {
	m := newDiffRailApp(t, 160, 40)
	m.setDiffPaneW(3)
	if got := m.diffPaneWidth(); got != DiffRailMinWidth {
		t.Errorf("a 3-cell width applied as %d, want the %d floor", got, DiffRailMinWidth)
	}
	// And the cap: a drag past half the terminal leaves the content at least half.
	m.setDiffPaneW(200)
	avail := 160
	if got := m.diffPaneWidth(); got > avail/2 {
		t.Errorf("a 200-cell width applied as %d, above half of %d", got, avail/2)
	}
}

// AUTO MODE IS BYTE-IDENTICAL: with no override, every size gives exactly what the proportional width has
// always produced (the existing scaling test pins the monotonic shape; this pins the equality).
func TestDiffRailAutoWidthIsUnchangedByTheOverrideField(t *testing.T) {
	for _, w := range []int{80, 120, 160, 190, 240} {
		m := newDiffRailApp(t, w, 40)
		if m.diffPaneW != 0 {
			t.Fatalf("a fresh app has override %d, want 0 (auto)", m.diffPaneW)
		}
		if got, want := m.diffPaneWidth(), m.diffPaneAutoWidth(); got != want {
			t.Errorf("auto mode at %d columns: diffPaneWidth = %d, auto = %d — the override changed the "+
				"proportional width", w, got, want)
		}
	}
}

// THE WIDTH IS CLAMPED IN ONE PLACE, and a terminal shrink RE-CLAMPS a stored width that no longer fits.
// The clamp is applied on READ (diffPaneWidth), so a resize that never touches the override still re-clamps.
//
// NOTE ON THE SHRINK SIZE. The pre-existing clamp is floor-then-cap, so on a terminal so narrow that half of
// it is BELOW DiffRailMinWidth the CAP is the binding bound (an 80-column terminal gives 40 — what auto mode
// has always produced, which TestDiffPaneWidthScalesWithTheTerminal pins as `got <= w/2`). The override
// obeys the SAME one function, so this test shrinks to 120 columns where half the width (60) is still above
// the floor, making BOTH bounds live and the re-clamp unambiguous.
func TestDiffRailWidthClampsOnAShrink(t *testing.T) {
	m := newDiffRailApp(t, 240, 40)
	// The operator drags the rail wide on a big terminal.
	m.setDiffPaneW(110)
	if got := m.diffPaneWidth(); got != 110 {
		t.Fatalf("a 110-cell override on a 240-column terminal applied as %d, want 110", got)
	}

	// The terminal shrinks to 120 columns with no further drag: the stored width must re-clamp.
	m.dispatch(tea.WindowSizeMsg{Width: 120, Height: 24})
	got := m.diffPaneWidth()
	if got > 120/2 {
		t.Errorf("after shrinking to 120 columns the rail is %d cells, above half the available %d — the "+
			"stored width did not re-clamp", got, 120/2)
	}
	if got < DiffRailMinWidth {
		t.Errorf("after shrinking the rail is %d cells, below the %d floor", got, DiffRailMinWidth)
	}
	// The STORED intent survives, so growing the terminal back restores what the operator chose.
	if m.diffPaneW != 110 {
		t.Errorf("the stored override became %d — the clamp must not overwrite the operator's intent", m.diffPaneW)
	}
	m.dispatch(tea.WindowSizeMsg{Width: 240, Height: 40})
	if got := m.diffPaneWidth(); got != 110 {
		t.Errorf("growing the terminal back gave %d cells, want the stored 110", got)
	}

	// AND AT 80 COLUMNS the override obeys EXACTLY what auto mode does — an override must not invent a
	// second width policy just because the operator set one.
	m.dispatch(tea.WindowSizeMsg{Width: 80, Height: 24})
	if got, want := m.diffPaneWidth(), m.clampDiffPaneWidth(110); got != want {
		t.Errorf("at 80 columns the override applied %d, want the clamp's %d", got, want)
	}
	if got := m.diffPaneWidth(); got > 80/2 {
		t.Errorf("at 80 columns the rail is %d cells, above half the terminal (%d)", got, 80/2)
	}
}
