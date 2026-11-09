package tui

// ask_longconv_test.go — A LONG CONVERSATION MUST STILL SHOW WHAT WAS JUST SENT.

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
