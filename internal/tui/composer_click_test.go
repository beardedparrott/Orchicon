package tui

// composer_click_test.go — A CLICK IN THE COMPOSER MOVES THE CARET, end to end.
//
// The operator: "I also just realized you can't use your mouse in the composer to change the position of your
// cursor."
//
// The dock places the caret (dock.ClickAt); the SHELL supplies the coordinates, because it is the only layer
// that knows where the dock sits in the frame. That conversion is the part that can drift silently, so both
// tests here find the composer by what the frame actually PAINTS rather than by recomputing the layout.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// composerRow finds the frame row carrying needle and the CELL it starts at.
//
// THE COLUMN IS IN CELLS, NOT BYTES. `strings.Index` answers a byte offset, and the composer's chrome is full
// of multi-byte runes — the box's `│` and the prompt's `❯` are three bytes each — so a byte offset overstated
// the column by four and made a correct caret look wrong. Every coordinate in this file is a terminal cell,
// because that is the space the mouse arrives in; ansi.StringWidth is what converts.
func composerRow(t *testing.T, m *App, needle string) (row, col int) {
	t.Helper()
	for i, r := range strings.Split(m.viewFrame(), "\n") {
		stripped := ansi.Strip(r)
		if at := strings.Index(stripped, needle); at >= 0 {
			return i, ansi.StringWidth(stripped[:at])
		}
	}
	t.Fatalf("fixture: %q is not on screen, so nothing could be clicked", needle)
	return -1, -1
}

// A CLICK ON A WORD PLACES THE CARET ON IT.
func TestAClickInTheComposerMovesTheCaret(t *testing.T) {
	m := askRelaunched(t, "c1")
	healthyPlane(m)
	m.refreshLayout()
	m.dock.SetValue("hello world")

	row, col := composerRow(t, m, "hello world")
	// Click the "w" of "world": six cells into the text.
	nm, _ := m.Update(tea.MouseMsg{
		Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: col + 6, Y: row,
	})
	app, ok := nm.(*App)
	if !ok || app == nil {
		t.Fatal("Update returned a non-App model")
	}
	line, caret := app.dock.Caret()
	if line != 0 || caret != 6 {
		t.Errorf("the click placed the caret at line %d column %d, want line 0 column 6 (the start of \"world\")",
			line, caret)
	}
}

// AND A CLICK ELSEWHERE LEAVES IT ALONE. The composer rule runs ahead of the generic click handling, so this is
// the boundary that stops it swallowing the rest of the frame's clicks.
func TestAClickOnTheTranscriptLeavesTheCaretAlone(t *testing.T) {
	m := askRelaunched(t, "c1")
	healthyPlane(m)
	m.chatStore.append("c1", chat.ChatItem{Kind: chat.KindText, Text: "Just prose here.", Key: "a1", At: 1})
	m.onChatWake()
	m.refreshLayout()
	m.dock.SetValue("hello world")
	_ = m.View()

	before, _ := m.dock.Caret()
	nm, _ := m.Update(tea.MouseMsg{
		Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 10, Y: m.transcriptBodyTopRow(),
	})
	app, ok := nm.(*App)
	if !ok || app == nil {
		t.Fatal("Update returned a non-App model")
	}
	if after, _ := app.dock.Caret(); after != before {
		t.Errorf("a click on the transcript moved the composer's caret from line %d to line %d", before, after)
	}
}

// A CLICK ON THE LAUNCH PAGE'S CENTERED COMPOSER PLACES THE CARET TOO.
//
// This is the operator's "This WAS working but is no longer working again", and the mechanism is the one
// thing about the launch page that differs from every other screen: the composer is CENTERED in the body
// region and narrower than the content column, so composerTopRow() — which is where a DOCKED composer
// starts — names a row the box is not on. The conversion therefore handed ClickAt a negative row, ClickAt
// refused it, and the caret never moved. The conversation view was working the whole time, which is why
// the report reads as a regression of something that used to be fine.
func TestAClickInTheLaunchComposerMovesTheCaret(t *testing.T) {
	m := phase3App(120, 40)
	m.refreshLayout()
	m.dock.Focus()
	m.dock.SetValue("hello world")

	// Find the box wherever the LAUNCH layout actually painted it — never by recomputing the centering.
	row, col := composerRow(t, m, "hello world")
	if row >= m.composerTopRow() {
		t.Fatalf("fixture: the launch composer is at frame row %d and composerTopRow() is %d — this test is "+
			"only meaningful while the centered box sits ABOVE the docked row (and if the two ever agree, "+
			"the conversion below has no centering left to account for)", row, m.composerTopRow())
	}
	beforeLine, beforeCol := m.dock.Caret()
	if beforeLine == 0 && beforeCol == 6 {
		t.Fatalf("fixture: the caret already sits on the cell this test clicks, so the assertion could not " +
			"tell a placed caret from an untouched one")
	}

	// Click the "w" of "world": six cells into the text.
	nm, _ := m.Update(tea.MouseMsg{
		Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: col + 6, Y: row,
	})
	app, ok := nm.(*App)
	if !ok || app == nil {
		t.Fatal("Update returned a non-App model")
	}
	if line, caret := app.dock.Caret(); line != 0 || caret != 6 {
		t.Errorf("the click placed the caret at line %d column %d, want line 0 column 6 (the start of "+
			"\"world\", from line %d column %d) — the launch page's centering was not accounted for",
			line, caret, beforeLine, beforeCol)
	}
	// The dock's width is restored, so the layout the next frame measures is the real one.
	if app.dock.Width != app.contentWidth() {
		t.Errorf("after a launch-page click the dock width is %d, want the content width %d — the click "+
			"must not leave the composer measured at the centered box's width", app.dock.Width, app.contentWidth())
	}
	// And the gesture FOCUSES, like every other composer click (a caret is only drawn while focused).
	if !app.dock.Focused {
		t.Error("a click in the launch composer did not focus it, so the caret it placed is invisible")
	}
}

// A CLICK IN THE LAUNCH PAGE'S MARGINS IS NOT A COMPOSER CLICK — the box is visibly narrower than the
// frame, so the cells beside it must keep their ordinary meaning rather than silently placing a caret.
func TestAClickBesideTheLaunchComposerIsNotAComposerClick(t *testing.T) {
	m := phase3App(120, 40)
	m.refreshLayout()
	m.dock.Focus()
	m.dock.SetValue("hello world")

	row, col := composerRow(t, m, "hello world")
	if col <= 0 {
		t.Fatalf("fixture: the launch composer starts at cell %d, so there is no margin to click", col)
	}
	before, _ := m.dock.Caret()
	nm, _ := m.Update(tea.MouseMsg{
		Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: col - 1, Y: row,
	})
	app, ok := nm.(*App)
	if !ok || app == nil {
		t.Fatal("Update returned a non-App model")
	}
	if after, _ := app.dock.Caret(); after != before {
		t.Errorf("a click in the margin left of the launch composer moved the caret from %d to %d", before, after)
	}
}
