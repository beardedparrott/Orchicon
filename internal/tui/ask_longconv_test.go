package tui

// ask_longconv_test.go — A LONG CONVERSATION MUST STILL SHOW WHAT WAS JUST SENT.
//
// The operator, reporting the exact shape: "So conversation ID 01M4114WHDAE0T4M8064MJEX55 is working.
// Your conversation 01M3XKG1JHE1WVYS5F4QZ02MT2 is not." — i.e. a SHORT conversation behaves and a LONG
// one does not: "user messages are being swallowed up when I send them and they don't print to the
// screen until the orchicon agent starts reasoning", and no activity line.
//
// SIZE-DEPENDENT, WHICH IS THE WHOLE CLUE. The transcript only becomes SCROLLABLE when it is taller than
// the pane — so the only difference between the working conversation and the broken one is that the
// broken one can be scrolled. And the stream's follow INTENT (kit2.Stream.follow) is cleared by exactly
// one thing: the operator's own wheel/keyboard scroll, which a long transcript invites and a short one
// cannot perform. Once follow is false, every later append — the optimistic echo, the reply, the
// activity line, which is drawn last — lands BELOW the fold, so the pane keeps showing the rows the
// operator scrolled to and nothing they just sent.
//
// The measured trace is unambiguous that the STORE and the PAINT are fine: the echo is appended 1ms
// after the send and the pane is repainted 3ms later with the notice set. So this is the remaining
// possibility, and it is the one the size-dependence points at.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// longConversation builds a transcript far taller than the pane, so the stream can be scrolled at all.
func longConversation(t *testing.T) *App {
	t.Helper()
	m, _ := askWithTranscript(t, "c1")
	for i := 0; i < 60; i++ {
		m.chatStore.append("c1", chat.ChatItem{
			Kind: chat.KindUser, Text: fmt.Sprintf("history-%02d", i), Key: fmt.Sprintf("h%d", i), At: int64(i),
		})
	}
	m.conversations = []chat.Conversation{{ID: "c1", Title: "a chat", TurnInFly: true, PendingReplyID: "m1"}}
	m.onChatWake()
	return m
}

// SENDING SHOWS THE MESSAGE EVEN WHEN THE OPERATOR HAS SCROLLED BACK.
//
// Scrolling up is a reading gesture on a long transcript, and it is deliberately sticky (see
// kit2.Stream.follow — an auto-follow that ignored it would yank a reader back down mid-sentence). But
// a SEND is a new, deliberate act: the operator is no longer reading, they have just asked something and
// the reply is about to arrive under it. Leaving the view parked in the middle of the history is what
// makes their own message invisible.
func TestSendingRepinsAScrolledTranscript(t *testing.T) {
	m := longConversation(t)

	// The operator scrolled up to re-read something — the gesture that clears the follow intent.
	str := m.TranscriptStream("c1")
	if str == nil {
		t.Fatal("fixture: no transcript stream")
	}
	str.Wheel(-10)
	if str.Following() {
		t.Fatal("fixture: the wheel did not clear the follow intent")
	}

	// Now they send. The echo and the activity line must both end up ON SCREEN.
	applyCmds(m, m.sendFromComposer("MY JUST-SENT MESSAGE"))
	frame := stripANSI(m.View())
	if !strings.Contains(frame, "MY JUST-SENT MESSAGE") {
		t.Errorf("the operator's own message is NOT on screen after sending into a scrolled "+
			"transcript — the pane stayed parked in the history, which is the reported \"my user "+
			"messages are being swallowed up when I send them\":\n%s", tailOf(frame, 1200))
	}
	if !strings.Contains(frame, "Orchicon is") {
		t.Errorf("no activity line in the frame after sending into a scrolled transcript:\n%s",
			tailOf(frame, 1200))
	}
	if !m.TranscriptStream("c1").Following() {
		t.Error("the send left the stream un-followed — the reply about to arrive would land off screen too")
	}
}
