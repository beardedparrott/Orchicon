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
	// PAINT ONCE, THROUGH View(). Only View() hands the painted frame to clipState (setFrame), and a drag
	// expressed in an empty frame copies nothing — which every assertion here would have read as "the
	// confinement lost the text" rather than "the fixture never painted".
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

// selection drives a press and a motion through the real mouse path and returns the text a release would
// copy.
//
// It reads clipState.text() rather than the clipboard CMD, because the cmd writes OSC 52 to stdout — and
// the selection itself is what this is about. App is a value model but m.clip is a POINTER, so the object
// the handlers mutated is the one read back here.
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
//
// The row is the one carrying a rail TITLE, because that is the row where this can be wrong: it has
// content in both panes, so a drag that is not confined copies both. On the transcript's BODY row the rail
// is blank and this test would pass with the confinement removed.
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

	// BOTH ENDS SIT ON REAL CONTENT: column 1 is the pane's first CONTENT column (0 is its border), and the
	// far end is inside the rail. Starting further in would clip the leading `C` off "Conversation: c1" and
	// the presence assertion below would fail for a reason that has nothing to do with confinement.
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
//
// THE DRAG RUNS EDGE TO EDGE, which is what puts the border cells inside the selection at all: inset by two
// columns it would never touch them and this would assert nothing. And it spans the pane because the
// operator's own message is RIGHT-ALIGNED (the label "You" sits at the left edge, the text at the right), so
// a drag that stopped mid-pane would copy the label and miss the message. (Measured: columns 2..40 copy
// exactly "You".)
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

// A DRAG INSIDE THE RAIL STAYS IN THE RAIL, so the confinement is symmetric rather than a special case.
//
// Started from the rail TITLE row for the same reason as above — and it asserts the title IS copied, so
// "the transcript is absent" cannot be satisfied by an empty selection.
func TestARailSelectionStaysInTheRail(t *testing.T) {
	m := dragPlane(t)
	if !m.railVisible() {
		t.Fatal("fixture: no rail at this size")
	}
	railRow := rowWith(t, m.viewFrame(), "RAIL-TITLE-ONE")
	// THE DRAG ENDS ON THE TRANSCRIPT'S FIRST CONTENT COLUMN (1, not 0), so a leak would bring the whole of
	// "Conversation: c1" with it. Ending at 2 clips the leading `C`, and the absence assertion below would
	// then hold for the wrong reason — measured, with the confinement switched off: that version passed.
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
//
// The operator's worry was that the block "is printing tab/spaces all the way to the end of the width" — and
// the copy is where that would actually hurt, since a paste carrying a run of blanks is a paste nobody can
// use. Two defences, and this is the second: the band no longer runs to the pane, and the padding a row DOES
// carry is stripped from the copy. It is asserted on clipState directly because that is the layer that decides
// what the clipboard receives, and the behaviour is easy to lose without noticing — the selection would still
// look right on screen.
//
// (The FIRST defence, that there is no leading space to trim, is md's and is asserted there:
// TestCodeBlockBandIsTheCodesWidthNotThePanes. A leading space cannot be trimmed here — it is
// indistinguishable from real indentation — which is exactly why the renderer must not emit one.)
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
