package tui

// selection_region_test.go — A COPY STAYS IN THE PANE IT STARTED IN, AND CHROME IS NOT CONTENT.
//
// The operator: "When copying something in conversations it is copying the ENTER line even past the
// conversations window into the conversation list."
//
// They were describing two things at once, and both are mechanical:
//
//  1. A selection is a RECTANGLE over the whole frame — deliberate, since the shell is the only layer that
//     owns the frame — so a drag from the transcript into the conversations rail copied the RAIL's rows
//     too, interleaved line by line.
//  2. The pane's own BORDER cells sit inside every row, so a selection across the transcript put a `│` at
//     the start of every copied line — the same glyph the code block used to draw.
//
// So a drag is confined to the pane its ANCHOR falls in, and the border cells are never content. Both are
// asserted against a REAL rendered frame, because the geometry is the part that can drift.
//
// ⚠️ EVERY TEST HERE HAS TO DRAG ON A ROW WHERE BOTH PANES HAVE CONTENT, and that is not a detail — it is
// what makes them mean anything. The first version of this file dragged along the transcript's body row,
// where the rail happens to be BLANK, and then asserted the rail was absent: both assertions passed with
// the confinement switched OFF, so the file "proved" a fix it never exercised. A drag only demonstrates a
// leak if there is something to leak. rowWith() is what keeps that honest, and the check is in the test
// rather than in a comment.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// dragPlane builds a conversation with a distinctive message and a rail of distinctive titles, so a copy
// that leaked across the boundary is unmissable.
func dragPlane(t *testing.T) *App {
	t.Helper()
	m := askRelaunched(t, "c1")
	healthyPlane(m)
	m.chatStore.append("c1", chat.ChatItem{Kind: chat.KindUser, Text: "TRANSCRIPT-ONLY-TEXT", Key: "u1", At: 1})
	m.onChatWake()
	m.conversations = []chat.Conversation{
		{ID: "c1", Title: "RAIL-TITLE-ONE"},
		{ID: "c2", Title: "RAIL-TITLE-TWO"},
	}
	m.convRailOpen = true
	m.refreshLayout()
	// PAINT ONCE, THROUGH View().
	_ = m.View()
	return m
}

// rowWith is the frame row carrying needle, or a fatal — a fixture row that vanished must fail loudly
// rather than silently turn the assertion below it into a tautology.
func rowWith(t *testing.T, frame, needle string) int {
	t.Helper()
	for i, row := range strings.Split(frame, "\n") {
		if strings.Contains(row, needle) {
			return i
		}
	}
	t.Fatalf("fixture: no frame row carries %q, so a leak or a stray glyph on that row could not be "+
		"detected and this test would pass vacuously", needle)
	return -1
}

// selection drives a press and a motion through the real mouse path and returns the text a release
// would copy.
func selection(t *testing.T, m *App, fromX, fromY, toX, toY int) string {
	t.Helper()
	feed(t, m, tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: fromX, Y: fromY})
	feed(t, m, tea.MouseMsg{Action: tea.MouseActionMotion, Button: tea.MouseButtonLeft, X: toX, Y: toY})
	return m.clip.text()
}

func feed(t *testing.T, m *App, msg tea.Msg) {
	t.Helper()
	nm, _ := m.Update(msg)
	if nm == nil {
		t.Fatal("Update returned a nil model")
	}
}

// THE RAIL IS NOT PART OF A TRANSCRIPT SELECTION — the gesture in the report, exactly: start in the
// transcript, drag out to the far right, end inside the conversations rail.
func TestATranscriptSelectionDoesNotTakeTheRail(t *testing.T) {
	m := dragPlane(t)
	if !m.railVisible() {
		t.Fatal("fixture: the rail is not visible, so nothing could leak into it and this would pass " +
			"vacuously")
	}
	frame := m.viewFrame()
	railRow := rowWith(t, frame, "RAIL-TITLE-ONE")
	if got := frame; !strings.Contains(got, "TRANSCRIPT-ONLY-TEXT") {
		t.Fatal("fixture: the message is not on screen")
	}

	// BOTH ENDS SIT ON REAL CONTENT: column 1 is the pane's first CONTENT column (0 is its border), and
	// the far end is inside the rail.
	got := selection(t, m, 1, railRow, m.width-4, railRow)
	if got == "" {
		t.Fatal("the drag copied nothing at all")
	}
	// THE ASSERTION IS AN ABSENCE, which is the whole point.
	if strings.Contains(got, "RAIL-TITLE") {
		t.Errorf("a selection that started in the transcript copied the conversations rail:\n%q", got)
	}
	// AND THE SELECTION IS NOT EMPTY: the transcript's own text on that same row has to be in it, or the
	// absence above would be satisfied by having copied nothing.
	if !strings.Contains(got, "Conversation: c1") {
		t.Errorf("the selection lost the transcript content it started on:\n%q", got)
	}
}

// AND THE PANE'S BORDER IS NOT CONTENT — no `│` anywhere in a copied line.
func TestATranscriptSelectionHasNoBorderCharacters(t *testing.T) {
	m := dragPlane(t)
	row := m.transcriptBodyTopRow()
	got := selection(t, m, 0, row, m.width-1, row)
	if got == "" {
		t.Fatal("the drag copied nothing")
	}
	if strings.ContainsAny(got, "│┃║") {
		t.Errorf("the copied line carries the pane's border, which is the \"weird pipes and characters\" the "+
			"operator pasted:\n%q", got)
	}
	if !strings.Contains(got, "TRANSCRIPT-ONLY-TEXT") {
		t.Errorf("the copied text does not contain the message:\n%q", got)
	}
}

// A DRAG INSIDE THE RAIL STAYS IN THE RAIL, so the confinement is symmetric rather than a special
// case.
func TestARailSelectionStaysInTheRail(t *testing.T) {
	m := dragPlane(t)
	if !m.railVisible() {
		t.Fatal("fixture: no rail at this size")
	}
	railRow := rowWith(t, m.viewFrame(), "RAIL-TITLE-ONE")
	// THE DRAG ENDS ON THE TRANSCRIPT'S FIRST CONTENT COLUMN (1, not 0), so a leak would bring the
	// whole of "Conversation: c1" with it.
	got := selection(t, m, m.width-2, railRow, 1, railRow)
	if got == "" {
		t.Fatal("fixture: the rail drag copied nothing at this size")
	}
	if strings.Contains(got, "Conversation: c1") {
		t.Errorf("a selection that started in the rail copied the transcript:\n%q", got)
	}
	if !strings.Contains(got, "RAIL-TITLE-ONE") {
		t.Errorf("the selection lost the rail row it started on:\n%q", got)
	}
}

// A SELECTION THE SHELL CANNOT CLASSIFY IS NOT CLIPPED TO A GUESS.
func TestAnUnclassifiedSelectionIsNotClipped(t *testing.T) {
	m := dragPlane(t)
	if _, ok := m.selectionRegionAt(2, 0); ok {
		t.Error("a point in the tab chrome was classified to a pane, so a selection there would be clipped " +
			"to a region it does not belong to")
	}
	if _, ok := m.selectionRegionAt(2, m.transcriptBodyTopRow()); !ok {
		t.Error("a point inside the transcript pane was not classified to it, so a drag there would be " +
			"unbounded and could take the rail")
	}
}

// AND A SELECTION THAT NEVER MOVED IS STILL A CLICK, not a copy.
func TestAPressWithNoMotionCopiesNothing(t *testing.T) {
	m := dragPlane(t)
	feed(t, m, tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 4, Y: m.transcriptBodyTopRow()})
	if got := m.clip.text(); got != "" {
		t.Errorf("a press with no motion produced a selection %q — a click must keep meaning what it always "+
			"did", got)
	}
}

// A SELECTED CODE BLOCK COPIES CODE, NOT THE FILL'S PADDING.
func TestACopiedRowDropsTheTrailingPadding(t *testing.T) {
	c := &clipState{}
	// A row as the code block paints it: the code, then the fill's padding out to the band's width.
	c.setFrame("ls -la" + strings.Repeat(" ", 30) + "\n")
	c.setRegion(clipRegion{x0: 0, y0: 0, x1: 39, y1: 0})
	c.beginDrag(0, 0)
	c.extend(39, 0)
	got := c.text()
	if got != "ls -la" {
		t.Errorf("the copy carries the fill's padding: %q — selecting a code block must yield code, and a run "+
			"of trailing blanks is not part of what the operator selected", got)
	}
}

// AND THE SELECTION DOES NOT PAINT ACROSS THE PANE.
//
// The frame pads every row out to the pane width, so the highlight used to run to the edge even for a row
// holding two words. The operator, over a code block inside such a row:
//
//	"Here is a screenshot showing I still am copying the entire band versus just the code block area. We should
//	 lock how far it goes on the screen."
//
// The COPY was already tight — text() right-trims, which the test above pins — so this is about the LOOK, and
// the look was the complaint: the highlight is the only evidence the operator has of what they are about to copy.
func TestTheSelectionHighlightStopsAtTheContent(t *testing.T) {
	const pane = 40
	const code = "ls -la"
	c := &clipState{}
	c.setFrame(code + strings.Repeat(" ", pane-len(code)) + "\n")
	c.setRegion(clipRegion{x0: 0, y0: 0, x1: pane - 1, y1: 0})
	c.beginDrag(0, 0)
	c.extend(pane-1, 0)

	sx, ex, ok := c.span(0, 0, 0, pane-1, 0)
	if !ok {
		t.Fatal("fixture: the selection does not cover the row")
	}
	if sx != 0 {
		t.Errorf("the span starts at column %d, want 0", sx)
	}
	if want := len(code) - 1; ex != want {
		t.Errorf("the selection spans columns %d..%d of a %d-cell pane, but the content ends at column %d — a "+
			"highlight that runs to the pane edge is the operator's \"it copies the entire band\": %q",
			sx, ex, pane, want, c.frame[0])
	}
}

// AND A BLANK LINE INSIDE A SELECTION STILL BELONGS TO IT.
func TestABlankLineInsideASelectionIsKept(t *testing.T) {
	const pane = 40
	c := &clipState{}
	blank := strings.Repeat(" ", pane)
	c.setFrame("first" + strings.Repeat(" ", pane-5) + "\n" + blank + "\n" + "third" + strings.Repeat(" ", pane-5) + "\n")
	c.setRegion(clipRegion{x0: 0, y0: 0, x1: pane - 1, y1: 2})
	c.beginDrag(0, 0)
	c.extend(pane-1, 2)

	got := c.text()
	if got != "first\n\nthird" {
		t.Errorf("the copy is %q, want the blank line preserved as its own line — the paragraph gap was "+
			"swallowed by the highlight clamping", got)
	}
}
