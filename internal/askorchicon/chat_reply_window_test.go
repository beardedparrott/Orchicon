package askorchicon

// chat_reply_window_test.go — the turn's reply window is a SILENCE budget, not
// a stopwatch on the turn's age.
//
// The operator: "Sessions seem to be timing out on me... it may be model related
// but it seems to be occuring quite frequently. We need to investigate: 'reply
// timed out after 30m0s on model orchicon/deepseek/deepseek-flash — the model
// may be overloaded or unavailable.'"
//
// It was not the model. The window was created once per turn and never reset, so
// a turn doing more than thirty minutes of honest work — exactly the work this
// product exists to run — was killed at the wall, and the message it produced
// sent the operator to Settings → Default models to fix a model that was fine.
//
// The pair below is the whole contract: progress keeps the turn alive, and
// silence still ends it. The second half is what keeps the fix honest — bounding
// a turn was never the bug. What the bound MEASURED was.

import (
	"strings"
	"testing"
	"time"
)

// TestReplyWindowSurvivesATurnThatKeepsMakingProgress drives deltas into the
// collector for four times the reply window. Under the old absolute cap this
// turn died at the window and reported a model fault; with an inactivity window
// every delta restarts it, so the turn finishes and its reply is kept.
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

// TestReplyWindowStillBoundsATurnThatGoesQuiet is the other half: progress must
// not DISABLE the bound. A turn that streams one delta and then goes silent is
// still bounded, with the same retryable timeout message — so this narrows what
// the window measures without widening it.
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
