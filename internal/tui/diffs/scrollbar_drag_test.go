package diffs

// scrollbar_drag_test.go — THE BAR'S COLUMN AND THE GRAB MAPPING.
//
// The operator: "the scroll only works in the TUI by using the scroll wheel. I can't grab onto the scroll
// bar and drag it up and down like you can in the GUI."
//
// The drag ITSELF is the shell's (a left press in the rail sets a selection region, and the clipboard layer
// consumes the motion that follows — see App.dispatchMouse), but the two things the gesture depends on live
// here: WHICH COLUMN the bar occupies, and HOW a grab row maps to a scroll offset.

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// overflowingModel is a pane whose body overflows, so the bar is live.
func overflowingModel() *Model {
	m := NewModel(nil, nil)
	m.Width, m.Height = 48, 8
	m.rows = []Row{longWrapRow()}
	m.Tab = TabDiff
	return m
}

// THE BAR IS NOT ON THE RESIZE EDGE. It used to occupy the pane's LAST cell — which is where the shell's
// rail-resize handle lives (diffDividerHit matches x == diffPaneWidth()-1) — so the shell claimed the press
// and the pane never saw a click or a drag on the bar at all: clicking did nothing and dragging RESIZED the
// rail. The bar now sits one cell inside, and this pins the separation.
func TestTheScrollbarColumnIsNotTheResizeEdge(t *testing.T) {
	m := overflowingModel()
	body := 1 + m.bodyWidth() // the bar's terminal X: the left border, then the content

	if !m.ScrollbarHit(body, paneBodyRow) {
		t.Errorf("the bar is not grabbable at its own column (x=%d, bodyWidth=%d)", body, m.bodyWidth())
	}
	// The pane's LAST cell is the shell's handle, so a press there is not a grab — asserting it keeps the
	// two affordances from silently converging again.
	if m.ScrollbarHit(m.Width-1, paneBodyRow) {
		t.Error("the pane claims a grab on its LAST cell, which is the shell's rail-resize handle: a press " +
			"there resizes the rail, so the pane would never receive it")
	}
	if m.ScrollbarHit(body, paneBodyRow-1) {
		t.Errorf("a press on the bar's column ABOVE the body (row %d) is not a grab", paneBodyRow-1)
	}
	if m.ScrollbarHit(body+1, paneBodyRow) {
		t.Error("the bar's hit region extends past its own cell")
	}
}

// The bar is drawn in the column the hit-test grabs, and the row still fills the pane — a body row is
// [content][bar][divider], so moving the bar did not change the pane's width or the shell's budget.
func TestABodyRowCarriesTheBarInTheGrabbableColumn(t *testing.T) {
	m := overflowingModel()
	// Scrolled to the BOTTOM, where the thumb sits at the bottom of the track — so the last rendered body
	// row is the one to read. (Reading the first row would read the track above the thumb and fail for a
	// reason that has nothing to do with the geometry under test.)
	m.scroll = m.maxScroll()
	lines := strings.Split(m.diffBody(), "\n")
	if len(lines) == 0 {
		t.Fatal("the diff body rendered nothing")
	}
	row := []rune(ansi.Strip(lines[len(lines)-1]))

	barX := m.bodyWidth()
	if len(row) != barX+2 {
		t.Fatalf("a body row is %d cells, want bodyWidth+2 = %d (content + bar + the shell's divider cell)",
			len(row), barX+2)
	}
	if strings.TrimSpace(string(row[barX])) != scrollThumbGlyph {
		t.Errorf("the cell at the bar's column is %q, want the thumb — the hit-test and the drawing must "+
			"agree about where the bar is", string(row[barX]))
	}
	// The shell's handle column is NOT the bar — it carries the resize grip instead, in its own style.
	//
	// IT IS RENDERED, deliberately. It was blank when the bar first got its own column, and that is exactly
	// what the operator reported as broken resizing: the grip was invisible, and the only thing in that
	// region they could see to grab was the BAR next to it.
	if got := string(row[barX+1]); got != scrollHandleGlyph {
		t.Errorf("the resize-handle cell is %q, want the grip glyph %q — an invisible handle reads as no "+
			"handle at all", got, scrollHandleGlyph)
	}
	if strings.ContainsAny(string(row[barX+1]), scrollThumbGlyph+scrollTrackGlyph) {
		t.Errorf("the handle cell carries the bar (%q) — the two affordances must stay distinguishable",
			string(row[barX+1]))
	}
}

// A GRAB AT EITHER END REACHES IT. The mapping is end-inclusive because a DRAG needs it to be: the operator
// holds the thumb at the bottom and expects the bottom of the content, not 95% of the way there.
func TestAGrabAtEitherEndReachesTheEnd(t *testing.T) {
	m := overflowingModel()
	viewH := m.viewHeight()
	if m.maxScroll() == 0 {
		t.Fatal("the fixture does not overflow, so the bar is inert and this proves nothing")
	}

	m.ScrollbarJumpTo(paneBodyRow) // the first body row
	if m.scroll != 0 {
		t.Errorf("grabbing the top of the bar scrolled to %d, want 0", m.scroll)
	}
	m.ScrollbarJumpTo(paneBodyRow + viewH - 1) // the last body row
	if m.scroll != m.maxScroll() {
		t.Errorf("grabbing the bottom of the bar scrolled to %d, want maxScroll=%d — the bar must go all "+
			"the way down when the operator drags it there", m.scroll, m.maxScroll())
	}
	// Monotonic in between, so the drag tracks the pointer rather than jumping about.
	prev := -1
	for row := 0; row < viewH; row++ {
		m.ScrollbarJumpTo(paneBodyRow + row)
		if m.scroll < prev {
			t.Fatalf("dragging down moved the viewport UP at body row %d (%d after %d)", row, m.scroll, prev)
		}
		prev = m.scroll
	}
	// Grabbing past the end clamps rather than overshooting.
	m.ScrollbarJumpTo(paneBodyRow + viewH + 10)
	if m.scroll != m.maxScroll() {
		t.Errorf("a grab past the bottom scrolled to %d, want maxScroll=%d", m.scroll, m.maxScroll())
	}
	m.ScrollbarJumpTo(paneBodyRow - 5)
	if m.scroll != 0 {
		t.Errorf("a grab above the top scrolled to %d, want 0", m.scroll)
	}
}

// GRABBING THE THUMB DOES NOT JUMP — the "jumps a bit" report.
//
// A GUI scrollbar picks the content up where it is when you grab the thumb, and only jumps when you press
// the track (clicking the track is asking to GO somewhere). Treating both as a jump yanked the content to
// wherever the pointer happened to be before the drag had started, which is what made the bar feel like it
// leapt about.
func TestGrabbingTheThumbDoesNotJump(t *testing.T) {
	m := overflowingModel()
	m.scroll = 0
	start, length := scrollbarThumb(0, m.viewHeight(), m.visibleLines())
	if length == 0 || length >= m.viewHeight() {
		t.Fatalf("fixture thumb is %d rows of %d — need a partial thumb", length, m.viewHeight())
	}

	thumb := paneBodyRow + start
	if !m.ScrollbarThumbAt(thumb) {
		t.Fatalf("row %d is not recognised as the thumb (start=%d len=%d)", thumb, start, length)
	}
	if m.ScrollbarDragStart(thumb) != true {
		t.Error("ScrollbarDragStart did not report the grab as a thumb grab")
	}
	if m.scroll != 0 {
		t.Errorf("grabbing the thumb jumped the content to %d, want it left where it was (0)", m.scroll)
	}
	m.ScrollbarDragEnd()

	// The TRACK is the jumping gesture, and it is the rows outside the thumb.
	track := paneBodyRow + m.viewHeight() - 1
	if m.ScrollbarThumbAt(track) {
		t.Fatalf("row %d is on the thumb; the fixture needs a track row below it", track)
	}
	if m.ScrollbarDragStart(track) != false {
		t.Error("ScrollbarDragStart reported a track grab as a thumb grab")
	}
	if m.scroll == 0 {
		t.Error("grabbing the track did not jump the viewport")
	}
}

// DRAGGING FROM THE THUMB TRACKS THE POINTER: relative to where it was grabbed, and scaled by the thumb's
// own travel range, so one row of pointer movement is one row of thumb movement.
func TestDraggingFromTheThumbTracksThePointer(t *testing.T) {
	m := overflowingModel()
	m.scroll = 0
	start, length := scrollbarThumb(0, m.viewHeight(), m.visibleLines())
	travel := m.viewHeight() - length
	reach := m.maxScroll()
	if travel < 4 || reach < 4 {
		t.Fatalf("fixture too small to measure a drag: travel=%d reach=%d", travel, reach)
	}
	grab := paneBodyRow + start
	m.ScrollbarDragStart(grab)

	// One row down: the content advances by exactly the thumb's ratio.
	m.ScrollbarDragTo(grab + 1)
	if want := 1 * reach / travel; m.scroll != want {
		t.Errorf("one row of drag scrolled to %d, want %d (reach/travel)", m.scroll, want)
	}
	// Three rows down: three times the distance, because the anchor is where it was GRABBED.
	m.ScrollbarDragTo(grab + 3)
	if want := 3 * reach / travel; m.scroll != want {
		t.Errorf("three rows of drag scrolled to %d, want %d", m.scroll, want)
	}
	// Back to the anchor: exactly where it started.
	m.ScrollbarDragTo(grab)
	if m.scroll != 0 {
		t.Errorf("dragging back to the grab row left the viewport at %d, want the original 0", m.scroll)
	}
	// Upward past the start, and down past the end, both clamp.
	m.ScrollbarDragTo(grab - 50)
	if m.scroll != 0 {
		t.Errorf("dragging above the top scrolled to %d, want 0", m.scroll)
	}
	m.ScrollbarDragTo(grab + 500)
	if m.scroll != m.maxScroll() {
		t.Errorf("dragging below the bottom scrolled to %d, want maxScroll=%d", m.scroll, m.maxScroll())
	}

	// The gesture ends with the release: no further motion moves anything.
	m.ScrollbarDragEnd()
	settled := m.scroll
	m.ScrollbarDragTo(grab)
	if m.scroll != settled {
		t.Errorf("the pane followed the pointer after the drag ended (%d -> %d)", settled, m.scroll)
	}
}
