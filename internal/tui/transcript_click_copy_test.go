package tui

// transcript_click_copy_test.go — CLICKING YOUR OWN MESSAGE COPIES IT.
//
// The operator: "I would like to make clicking on a user message in conversations auto copy to clipboard
// ... and we should add a hint in the composer saying as such."
//
// THREE COORDINATE SPACES have to agree for this to copy the right thing, and each is a place it can be
// wrong:
//
//	frame row → body row   (where the transcript's first line is drawn)
//	body row  → body LINE  (a wrapped row is not a line)
//	body line → the ITEM   (from the render that drew it)
//
// The first is DERIVED (transcriptBodyTopRow), so it is pinned here against a real rendered frame rather
// than trusted: if the pane's layout changes, this test fails instead of the click silently copying a
// different message.

import (
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// clickPlane builds a conversation with a known user message and reply, and returns the app.
func clickPlane(t *testing.T) *App {
	t.Helper()
	m := askRelaunched(t, "c1")
	healthyPlane(m)
	m.chatStore.append("c1", chat.ChatItem{Kind: chat.KindUser, Text: "MARKER-USER-MSG", Key: "u1", At: 1})
	m.chatStore.append("c1", chat.ChatItem{Kind: chat.KindText, Text: "MARKER-REPLY", Key: "t1", At: 2})
	m.onChatWake()
	return m
}

// THE DERIVED BODY-TOP ROW MATCHES WHERE THE TRANSCRIPT IS ACTUALLY DRAWN.
//
// This is the pin that matters: the formula is arithmetic over the pane's chrome, and arithmetic over
// someone else's layout is exactly the kind of thing that drifts without a test noticing.
func TestTheDerivedBodyTopRowMatchesTheDrawnFrame(t *testing.T) {
	m := clickPlane(t)
	want := m.transcriptBodyTopRow()

	frame := strings.Split(m.viewFrame(), "\n")
	if want < 0 || want >= len(frame) {
		t.Fatalf("transcriptBodyTopRow = %d, outside the frame (%d rows)", want, len(frame))
	}
	if !strings.Contains(frame[want], "MARKER-USER-MSG") {
		t.Errorf("transcriptBodyTopRow = %d, but frame row %d is %q — the first transcript line is somewhere "+
			"else, so every click resolves to the wrong line", want, want, strings.TrimSpace(frame[want]))
	}
}

// A CLICK ON THE OPERATOR'S MESSAGE RESOLVES TO ITS TEXT — found by SCANNING the frame for the marker,
// so the test does not depend on the formula it is checking.
func TestAClickOnTheOperatorsMessageResolvesToItsText(t *testing.T) {
	m := clickPlane(t)
	frame := strings.Split(m.viewFrame(), "\n")

	var found bool
	for i, l := range frame {
		if !strings.Contains(l, "MARKER-USER-MSG") {
			continue
		}
		found = true
		text, ok := m.transcriptUserMessageAtFrameRow(i)
		if !ok {
			t.Fatalf("a click at frame row %d (the operator's own message) resolved to nothing — the copy "+
				"gesture its own hint advertises would do nothing", i)
		}
		if text != "MARKER-USER-MSG" {
			t.Errorf("the click copied %q, want the message text %q — pasting it would paste the wrong thing",
				text, "MARKER-USER-MSG")
		}
	}
	if !found {
		t.Fatal("fixture: the user message was not rendered anywhere in the frame")
	}
}

// AND THE MODEL'S REPLY IS NOT CLICKABLE, because the gesture is "get MY message back". A hint that
// promised otherwise would be the interface lying, so this is the other half of the contract.
func TestAClickOnTheModelReplyCopiesNothing(t *testing.T) {
	m := clickPlane(t)
	frame := strings.Split(m.viewFrame(), "\n")

	for i, l := range frame {
		if !strings.Contains(l, "MARKER-REPLY") {
			continue
		}
		if text, ok := m.transcriptUserMessageAtFrameRow(i); ok {
			t.Errorf("a click at frame row %d (the model's reply) copied %q — only the operator's own "+
				"messages are part of this gesture", i, text)
		}
	}
}

// A CLICK ANYWHERE ELSE COPIES NOTHING — the tab chrome, and the blank rows the pane pads with.
func TestAClickOffTheTranscriptCopiesNothing(t *testing.T) {
	m := clickPlane(t)
	for _, row := range []int{0, 1, m.transcriptBodyTopRow() + 200} {
		if text, ok := m.transcriptUserMessageAtFrameRow(row); ok {
			t.Errorf("a click at frame row %d copied %q — it is not on a message", row, text)
		}
	}
}

// THE COMPOSER SAYS SO. The operator asked for the hint by name, and it names THEIR message rather than
// "a message" because that is what the code does.
func TestTheComposerAdvertisesTheCopyGesture(t *testing.T) {
	m := clickPlane(t)
	m.refreshComposerHint()
	got := m.dock.Context
	if !strings.Contains(got, transcriptCopyHint) {
		t.Errorf("the composer hint does not advertise the copy gesture: %q", got)
	}
	if !strings.Contains(strings.ToLower(transcriptCopyHint), "your message") {
		t.Errorf("the hint reads %q — it should name the operator's OWN messages, since a click on the "+
			"model's reply is deliberately inert", transcriptCopyHint)
	}
	// With no conversation open there is no transcript to click, so the hint must not appear.
	m.chatConvID = ""
	m.refreshComposerHint()
	if strings.Contains(m.dock.Context, transcriptCopyHint) {
		t.Errorf("the copy hint is advertised with no conversation open: %q", m.dock.Context)
	}
}
