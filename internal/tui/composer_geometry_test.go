package tui

// composer_geometry_test.go — the ONE invariant every composer click depends on:
//
//	the shell's idea of where the composer is MUST equal where it is painted
//
// The click conversion is `y - composerTopRow()`, and the dock then uses TextOrigin() to find the
// text row inside the box. Two independent computations have to agree with a third — the paint — or
// every click in the composer is displaced by exactly the disagreement.
//
// These are MEASUREMENTS, not descriptions: they render the real frame and search it for the painted
// box and the painted draft text, then compare those rows to what the click path would compute. That
// is the only way to check this class of bug, because a geometry error is invisible to a test that
// recomputes the same arithmetic it is testing.

import (
	"strings"
	"testing"
)

const geomMarker = "DRAFTMARKER"

// paintedRows returns the frame row of the composer's top border and of the draft text, or -1.
func paintedRows(frame []string) (boxRow, textRow int) {
	boxRow, textRow = -1, -1
	for i, l := range frame {
		if boxRow < 0 && strings.Contains(l, "╭") {
			boxRow = i
		}
		if strings.Contains(l, geomMarker) {
			textRow = i
		}
	}
	return boxRow, textRow
}

// THE DOCKED GEOMETRY: a conversation is open, so the composer is pinned to the bottom.
func TestDockedComposerGeometryMatchesThePaint(t *testing.T) {
	m, _ := consentApp(t)
	m.chatConvID = "c1" // leaves welcome mode
	m.dock.Focus()
	m.dock.SetValue(geomMarker)

	boxRow, textRow := paintedRows(strings.Split(m.View(), "\n"))
	if boxRow < 0 || textRow < 0 {
		t.Fatalf("the fixture did not paint a composer (box=%d text=%d)", boxRow, textRow)
	}
	if boxRow != m.composerTopRow() {
		t.Errorf("composerTopRow()=%d but the box is painted at row %d (off by %+d) — every click in the "+
			"composer is displaced by that much", m.composerTopRow(), boxRow, boxRow-m.composerTopRow())
	}
	originRow, _ := m.dock.TextOrigin()
	if want := m.composerTopRow() + originRow; textRow != want {
		t.Errorf("the draft is painted at row %d, but a click there resolves to dock row %d (want %d) — "+
			"the caret would land %+d rows off", textRow, textRow-m.composerTopRow(), originRow, want-textRow)
	}
}

// THE CENTERED GEOMETRY: the launch page, where the composer is far from composerTopRow() and the
// click path uses welcomeLayout instead. An error here is "fairly far" rather than one row, which is
// why it is measured at several terminal sizes — the wordmark falls back to one row on a narrow one.
func TestCenteredComposerGeometryMatchesThePaint(t *testing.T) {
	for _, size := range [][2]int{{120, 40}, {100, 30}, {140, 50}, {80, 24}} {
		m, _ := consentApp(t)
		m.width, m.height = size[0], size[1]
		m.chatConvID = ""
		m.dock.Focus()
		m.dock.SetValue(geomMarker)
		if !m.welcomeMode() {
			t.Fatalf("fixture: %v is not the launch page", size)
		}
		_, g := m.welcomeLayout(m.contentWidth(), m.contentHeight()+m.dock.Lines())
		boxRow, textRow := paintedRows(strings.Split(m.View(), "\n"))
		if boxRow != g.Top {
			t.Errorf("%dx%d: welcomeLayout reports Top=%d but the box is painted at %d (off by %+d)",
				size[0], size[1], g.Top, boxRow, boxRow-g.Top)
		}
		originRow, _ := m.dock.TextOrigin()
		if want := g.Top + originRow; textRow != want {
			t.Errorf("%dx%d: the draft is painted at %d, but a click there resolves to dock row %d (want %d)",
				size[0], size[1], textRow, textRow-g.Top, originRow)
		}
		// The painted text must be INSIDE the reported box, or the box's rows and the paint disagree
		// and the click would be refused outright rather than misplaced.
		if !g.contains(g.Left+2, textRow) {
			t.Errorf("%dx%d: the painted text row %d is not inside the reported box — clicks there are refused",
				size[0], size[1], textRow)
		}
	}
}
