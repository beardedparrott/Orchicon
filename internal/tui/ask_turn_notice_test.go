package tui

// ask_turn_notice_test.go — the activity line ("Orchicon is thinking…", and its watchdog age) must show
// whenever a turn is IN FLIGHT, not only while THIS client happens to hold a live stream for it.
//
// The operator: "I am no longer seeing the 'Orchicon is thinking...' and the watchdog countdown in the
// TUI. The conversation text is going to the bottom."
//
// BOTH SYMPTOMS, ONE CAUSE. The notice was keyed to the client's own stream slot alone
// (chat.IsStreaming), while a turn's real state lives on the PLANE and reaches the shell on the
// conversation row (Conversation.TurnInFly, from turn_in_flight) — the same field the rail already uses
// to mark the row as running (rightrail.go) and the same one re-attach reads (reattachRunningTurn). So
// whenever the slot was not armed in THIS client — a turn started in the GUI, a re-attach that ran
// before the row landed, a slot cleared by a superseded stream — the pane showed no line at all while
// the rail beside it said the conversation was running.
//
// "THE TEXT IS GOING TO THE BOTTOM" IS THE SAME BUG, SEEN FROM THE OTHER SIDE. The notice takes a row
// from the body (kit2.Stream.bodyRows), so an EMPTY notice hands the transcript the whole pane: the text
// runs one row further down, to the pane's bottom edge — which is where the operator saw it land.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// THE LINE SHOWS FOR A TURN THE SERVER REPORTS, even when this client streams nothing.
func TestActivityLineShowsForAServerReportedTurn(t *testing.T) {
	m, _ := askWithTranscript(t, "c1")
	// The server says a turn is in flight (the rail row carries it, and the rail marks it). This client
	// holds NO live stream slot — never sent, or the slot was lost and not re-attached.
	m.conversations = []chat.Conversation{{ID: "c1", Title: "a chat", TurnInFly: true, PendingReplyID: "m1"}}
	if m.chat.IsStreaming("c1") {
		t.Fatal("fixture: this client must hold no stream slot for the turn")
	}
	m.onChatWake()

	str := m.TranscriptStream("c1")
	if str == nil {
		t.Fatal("no transcript stream for the open conversation")
	}
	if !strings.Contains(str.Notice, "Orchicon is") {
		t.Errorf("no activity line while the PLANE reports a turn in flight (notice = %q). The rail "+
			"marks this conversation as running, so the pane and the rail contradict each other — and "+
			"an empty notice also hands the transcript the row the line would have reserved, which is "+
			"the operator's \"the conversation text is going to the bottom\"", str.Notice)
	}
	if frame := m.View(); !strings.Contains(frame, "Orchicon is") {
		t.Errorf("the activity line is not in the PAINTED frame:\n%s", tailOf(frame, 1200))
	}
}

// AND IT GOES AWAY WHEN THE PLANE SAYS THE TURN IS OVER — the fallback must not become a permanent
// claim that something is happening, which would be worse than the silence it fixes.
func TestActivityLineClearsWhenTheTurnEnds(t *testing.T) {
	m, _ := askWithTranscript(t, "c1")
	m.conversations = []chat.Conversation{{ID: "c1", Title: "a chat", TurnInFly: true, PendingReplyID: "m1"}}
	m.onChatWake()
	if str := m.TranscriptStream("c1"); str == nil || str.Notice == "" {
		t.Fatal("fixture: the line should be showing")
	}

	// The turn finishes: the conversation row no longer reports it.
	m.conversations = []chat.Conversation{{ID: "c1", Title: "a chat"}}
	m.onChatWake()
	if str := m.TranscriptStream("c1"); str != nil && str.Notice != "" {
		t.Errorf("the activity line outlived the turn it describes: notice = %q", str.Notice)
	}
}

// THE LINE'S ROW IS RESERVED, which is what stops the transcript running to the pane's bottom edge.
// Asserted through the stream's own visible-row count, so it holds whatever the pane's height is.
func TestTheActivityLineReservesItsRow(t *testing.T) {
	m, _ := askWithTranscript(t, "c1")
	for i := 0; i < 60; i++ {
		m.chatStore.append("c1", chat.ChatItem{
			Kind: chat.KindUser, Text: fmt.Sprintf("history-%02d", i), Key: fmt.Sprintf("h%d", i), At: int64(i),
		})
	}
	m.conversations = []chat.Conversation{{ID: "c1", Title: "a chat", TurnInFly: true, PendingReplyID: "m1"}}
	m.onChatWake()

	str := m.TranscriptStream("c1")
	if str == nil || str.Notice == "" {
		t.Fatalf("fixture: the notice must be showing (notice = %v)", str.Notice)
	}
	withNotice := len(str.Visible())
	if withNotice >= str.Height {
		t.Errorf("the transcript takes %d of the stream's %d rows with a notice set — the notice's row "+
			"must come OUT of the body, or the line is what gets clipped", withNotice, str.Height)
	}

	// With the turn over, the body gets that row back.
	m.conversations = []chat.Conversation{{ID: "c1", Title: "a chat"}}
	m.onChatWake()
	if got := len(str.Visible()); got <= withNotice {
		t.Errorf("with no notice the body should get the row back: %d visible, was %d", got, withNotice)
	}
}
