package antigravity

// ask_sendaccept_test.go — SendTurnMessage RETURNS ON ACCEPT, not at turn end.
//
// WHY THIS IS A CONTRACT AND NOT A PREFERENCE. scheduler.ChatTurnClient.SendTurnMessage means "append a
// user message to a conversation session", and the platform's turn loop reads its return as
// SEND-ACCEPT: internal/askorchicon/chat.go starts a handshake timer (ORCHICON_ASK_TIMEOUT) and, when it
// expires before `sent` is set, cancels the in-flight send and RE-DISPATCHES. Every other Ask adapter
// returns on accept — claude writes the turn to the child and returns, opencode's serve acknowledges the
// send — so the loop is entitled to that assumption.
//
// antigravity's prompt used to block until agy answered (the whole turn), which produced two defects:
//
//  1. A turn PARKED ON A PERMISSION CARD was cancelled and re-sent every handshake window, because the
//     operator had not answered yet and the send had not "returned". MEASURED on a local-mode plane:
//     `permissionAsk` for `run_command {echo card-probe-ok}` was raised immediately, then after the
//     handshake window `permissionAskResolved {outcome:"expired"}` and `turn_failed`.
//  2. A turn longer than the handshake window looked like a wedged serve even with nobody to ask.
//
// So the assertion is on TIME: a turn whose child accepts the prompt and answers nothing must still let
// SendTurnMessage return promptly, and the terminal event must still arrive afterwards (the turn is driven
// in the background, not abandoned).
//
// The fixture's `hang` script is exactly that shape: it holds the `session/prompt` open until `release()`
// or `session/cancel`, which is the same wire behaviour as a turn parked on a consent card.

import (
	"strings"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/askmode"
)

// TestAskSendTurnMessageReturnsOnAcceptNotTurnEnd is the pin.
func TestAskSendTurnMessageReturnsOnAcceptNotTurnEnd(t *testing.T) {
	h := newAskHarness(t, script{hang: true})
	ctx := h.turnCtx(askmode.Iteration)
	h.subscribe(ctx)
	sid := h.create(askmode.Iteration)
	col := drainSince(t, h)

	returned := h.sendTurn(ctx, sid, askmode.Iteration, "hello")
	select {
	case err := <-returned:
		if err != nil {
			t.Fatalf("SendTurnMessage returned an error for an ACCEPTED turn: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("SendTurnMessage did not return within 10s of a turn whose child accepted the prompt — " +
			"the contract is scheduler.ChatTurnClient's send-accept, and the platform's handshake timer " +
			"re-dispatches (cancelling the parked turn) when it does not return. A turn parked on a " +
			"permission card would have its card expire unanswered")
	}

	// The prompt really did reach the child, so the early return is acceptance and not a silent no-op.
	// It is REACHED asynchronously (that is the whole point), so this waits rather than sampling once.
	waitFor(t, "the prompt to reach the child", func() bool { return len(h.latestProc().promptsSeen()) >= 1 })

	// AND THE TURN IS STILL DRIVEN: releasing the held prompt must produce the terminal event, so an early
	// return did not orphan the turn.
	h.latestProc().release()
	waitFor(t, "the terminal event after the send returned", func() bool {
		return len(col.ofKind("idle"))+len(col.ofKind("error")) >= 1
	})
}

// TestAskSecondTurnWhileBusyReportsRatherThanRacing pins the other half: while a turn is in flight, a
// second SendTurnMessage must say the conversation is still answering rather than putting a second
// overlapping session/prompt on the wire (agy defines no behaviour for that).
func TestAskSecondTurnWhileBusyReportsRatherThanRacing(t *testing.T) {
	h := newAskHarness(t, script{hang: true})
	ctx := h.turnCtx(askmode.Iteration)
	h.subscribe(ctx)
	sid := h.create(askmode.Iteration)
	drainSince(t, h)

	if err := <-h.sendTurn(ctx, sid, askmode.Iteration, "first"); err != nil {
		t.Fatalf("the first turn was not accepted: %v", err)
	}
	waitFor(t, "the first prompt to reach the child", func() bool {
		return len(h.latestProc().promptsSeen()) >= 1
	})

	second := h.sendTurn(ctx, sid, askmode.Iteration, "second")
	select {
	case err := <-second:
		if err == nil {
			t.Fatal("a second turn was accepted while the first was still running — agy defines no " +
				"behaviour for overlapping session/prompt requests, so this can only corrupt the turn")
		}
		if !strings.Contains(strings.ToLower(err.Error()), "still answering") {
			t.Fatalf("the busy refusal does not say the conversation is still answering: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second SendTurnMessage neither returned nor reported the busy session")
	}
	if n := len(h.latestProc().promptsSeen()); n != 1 {
		t.Fatalf("the child received %d session/prompt requests, want 1", n)
	}
	h.latestProc().release()
}

// TestAskTerminalEventStillFollowsAnEarlyReturn is the third leg: the collector must still see the turn's
// terminal event, because it is what closes the turn (and what a client's spinner hangs on).
func TestAskTerminalEventStillFollowsAnEarlyReturn(t *testing.T) {
	h := newAskHarness(t, script{hang: true})
	ctx := h.turnCtx(askmode.Iteration)
	h.subscribe(ctx)
	sid := h.create(askmode.Iteration)
	col := drainSince(t, h)

	if err := <-h.sendTurn(ctx, sid, askmode.Iteration, "hi"); err != nil {
		t.Fatalf("SendTurnMessage: %v", err)
	}
	waitFor(t, "the prompt to reach the child", func() bool { return len(h.latestProc().promptsSeen()) >= 1 })
	// Cancel rather than release: the operator pressing Stop must also close the turn it was driving.
	if err := h.b.AbortConversationSession(ctx, sid); err != nil {
		t.Fatalf("AbortConversationSession: %v", err)
	}
	waitFor(t, "the terminal event after an abort", func() bool {
		return len(col.ofKind("idle"))+len(col.ofKind("error")) >= 1
	})
}
