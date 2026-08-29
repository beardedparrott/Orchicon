package askorchicon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
)

// These are DB-backed tests for ADR-0002 AC2/AC4 (refresh survival + the
// admission bound). They reuse the ORCHICON_TEST_DSN-gated helpers in
// chat_turn_db_test.go and skip unless a disposable database is configured
// (the repo pattern; `make ci` gates what runs in CI).

// TestRefreshKeepsTurnInFlightAndAccurate verifies the AC2 refresh-survival
// contract against a real DB: after the ChatStream RPC has returned (the
// client disconnect / page refresh), the detached collector keeps the turn in
// flight and the read path reports turn_in_flight + turn_progressing with the
// acked pending id until the reply persists, then clears both — so a refreshed
// UI re-attaches to a genuinely-running turn, not a stale one.
func TestRefreshKeepsTurnInFlightAndAccurate(t *testing.T) {
	t.Setenv("ORCHICON_ASK_STALL_NO_PROGRESS_WINDOW", "5s")
	pool := chatDBTestPool(t)
	client := &fakeSessionClient{}
	s := newChatService(t, pool, client)
	ctx := context.Background()
	convID := createConversation(t, pool, "")

	ackID, _, err := s.startConversationTurn(ctx, "tnt_dev", convID, "hello", nil)
	if err != nil {
		t.Fatalf("startConversationTurn: %v", err)
	}
	waitForSend(t, client, 1)

	// Refresh after the RPC returned: the turn is still live and progressing.
	info := s.turnStatus(convID)
	if !info.inFlight {
		t.Fatal("refresh must see the turn still in flight (detached collector)")
	}
	if !info.progressing {
		t.Error("a freshly-active turn must report progressing after refresh")
	}
	if info.pendingMsgID != ackID {
		t.Errorf("pending msg id = %q, want ackID %q", info.pendingMsgID, ackID)
	}

	// Complete the reply: the detached collector persists it and clears the
	// turn from the registry.
	time.Sleep(100 * time.Millisecond)
	client.sub.feed(busText("ses_1", "the refresh-survived reply"))
	client.sub.feed(busIdle("ses_1"))
	msg := waitForMessage(t, pool, convID, ackID)
	if strings.TrimSpace(msg.Content) != "the refresh-survived reply" {
		t.Errorf("reply = %q, want 'the refresh-survived reply'", msg.Content)
	}
	deadline := time.After(5 * time.Second)
	for {
		if info := s.turnStatus(convID); !info.inFlight {
			break
		}
		select {
		case <-time.After(20 * time.Millisecond):
		case <-deadline:
			t.Fatal("turn must clear in flight after the reply persists")
		}
	}
}

// TestStartConversationTurnAdmissionCapBusyError verifies ADR-0002 D7 (AC4):
// beyond the concurrent-turn cap a new dispatch is rejected with a clear
// CodeResourceExhausted error — and the rejected conversation is not left
// with an orphan turn — instead of degrading under contention.
func TestStartConversationTurnAdmissionCapBusyError(t *testing.T) {
	t.Setenv("ORCHICON_ASK_MAX_CONCURRENT_TURNS", "2")
	pool := chatDBTestPool(t)
	client := &fakeSessionClient{}
	s := newChatService(t, pool, client)
	ctx := context.Background()

	conv1 := createConversation(t, pool, "")
	conv2 := createConversation(t, pool, "")
	conv3 := createConversation(t, pool, "")

	for _, conv := range []string{conv1, conv2} {
		if _, _, err := s.startConversationTurn(ctx, "tnt_dev", conv, "busy", nil); err != nil {
			t.Fatalf("startConversationTurn %s: %v", conv, err)
		}
	}

	// Both slots are full; a third dispatch (a different conversation) exceeds
	// the cap.
	_, _, err := s.startConversationTurn(ctx, "tnt_dev", conv3, "third", nil)
	if err == nil {
		t.Fatal("expected a busy error when the concurrent-turn cap is exceeded")
	}
	var cerr *connect.Error
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeResourceExhausted {
		t.Fatalf("err = %v, want CodeResourceExhausted", err)
	}
	if _, ok := s.turns.get(conv3); ok {
		t.Fatal("a capped dispatch must not leave an orphan turn registered")
	}
}
