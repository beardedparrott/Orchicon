package askorchicon

// chat_reply_window_test.go — the turn's reply window is a SILENCE budget, not a stopwatch on the
// turn's age.

import (
	"strings"
	"testing"
	"time"
)

// TestReplyWindowSurvivesATurnThatKeepsMakingProgress drives deltas into the collector for four
// times the reply window.
func TestReplyWindowSurvivesATurnThatKeepsMakingProgress(t *testing.T) {
	t.Setenv("ORCHICON_ASK_REPLY_WINDOW", "100ms")
	client := &fakeSessionClient{}
	opts := turnCollectOpts{
		client: client, sessionID: "ses_live", reuseSystem: "REUSE_SYSTEM",
		modelRef: "opencode/deepseek-v4-flash-free", userMsg: "hello",
	}
	go func() {
		waitForSend(t, client, 1)
		// 20 deltas at 20ms = 400ms of steady progress against a 100ms window.
		for i := 0; i < 20; i++ {
			client.sub.feed(busDelta("ses_live", "tok"))
			time.Sleep(20 * time.Millisecond)
		}
		client.sub.feed(busIdle("ses_live"))
	}()

	reply, _, _, err := collectTurn(t, client, opts)
	if err != nil {
		t.Fatalf("a turn that made progress throughout must not time out: %v", err)
	}
	if !strings.Contains(reply, "tok") {
		t.Errorf("reply = %q, want the deltas the turn streamed", reply)
	}
}

// TestReplyWindowStillBoundsATurnThatGoesQuiet is the other half: progress must not DISABLE the
// bound.
func TestReplyWindowStillBoundsATurnThatGoesQuiet(t *testing.T) {
	t.Setenv("ORCHICON_ASK_REPLY_WINDOW", "100ms")
	client := &fakeSessionClient{}
	opts := turnCollectOpts{
		client: client, sessionID: "ses_live", reuseSystem: "REUSE_SYSTEM",
		modelRef: "opencode/deepseek-v4-flash-free", userMsg: "hello",
	}
	go func() {
		waitForSend(t, client, 1)
		// One delta of progress, then silence. The window must still fire.
		client.sub.feed(busDelta("ses_live", "starting"))
	}()

	_, _, _, err := collectTurn(t, client, opts)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error = %v, want the reply-window timeout", err)
	}
}

// TestReplyWindowTouchRestartsTheDeadline pins the mechanism on its own, with no
// turn around it: Touch moves the deadline, and stopping Touch lets it fire.
func TestReplyWindowTouchRestartsTheDeadline(t *testing.T) {
	w := newTurnReplyWindow(120 * time.Millisecond)
	defer w.Stop()
	// Keep touching at half the window for 4x the window: it must not fire.
	for i := 0; i < 8; i++ {
		select {
		case <-w.C():
			t.Fatal("the window fired while progress was being reported")
		case <-time.After(60 * time.Millisecond):
			w.Touch()
		}
	}
	// Then stop touching, and the same window fires.
	select {
	case <-w.C():
	case <-time.After(2 * time.Second):
		t.Fatal("the window never fired after progress stopped")
	}
}
