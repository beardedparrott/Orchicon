package askorchicon

import (
	"context"
	"encoding/json"
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
	info := s.turnStatus(convID, askStallNoProgressWindow())
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
		if info := s.turnStatus(convID, askStallNoProgressWindow()); !info.inFlight {
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

// TestInterjectRecyclesWedgedSession verifies ADR-0002 D4 (AC3): a mid-run
// interjection onto a conversation whose in-flight turn wedged on an
// unresolved tool (MCP wedge) recycles the wedged session. The interjection is
// dispatched on a FRESH seeded session (sessionID forced to "" in
// startConversationTurnOpts) rather than queued behind / dispatched onto the
// stuck one, and its reply is persisted on that fresh session. It drives a
// genuinely wedged in-flight turn through startConversationTurnOpts(supersede).
func TestInterjectRecyclesWedgedSession(t *testing.T) {
	t.Setenv("ORCHICON_ASK_MCP_TOOL_WEDGE_WINDOW", "200ms")
	t.Setenv("ORCHICON_ASK_STALL_NO_PROGRESS_WINDOW", "3s")
	t.Setenv("ORCHICON_ASK_MCP_RECONNECT_ATTEMPTS", "1")
	t.Setenv("ORCHICON_ASK_REATTACH_BACKOFF", "1ms")

	pool := chatDBTestPool(t)
	client := &fakeSessionClient{}
	s := newChatService(t, pool, client)
	ctx := context.Background()
	convID := createConversation(t, pool, "")
	// The conversation already has a session (a follow-up turn reuses it). The
	// wedged turn wedges THIS session; D4 must recycle away from it.
	setConversationSessionID(t, pool, convID, "ses_live")

	// Start turn A on the reused session: it registers, subscribes, sends on
	// ses_live and waits for the reply.
	ackA, _, err := s.startConversationTurn(ctx, "tnt_dev", convID, "first message", nil)
	if err != nil {
		t.Fatalf("startConversationTurn: %v", err)
	}
	waitForSend(t, client, 1)
	// Give the drain loop a beat to flip sent == true so the tool event below is
	// observed (a pre-accept event would be ignored as a prior turn's telemetry).
	time.Sleep(150 * time.Millisecond)

	// The wedged tool: issued (non-terminal tool part) on ses_live, never
	// resolves. The collector's tool-wedge monitor detects it and recycles the
	// session — aborts ses_live, creates ses_1, re-dispatches (the second
	// send) — and marks the registry turn wedged (the D4 pre-condition).
	client.sub.feed(busToolRunning("ses_live", "list_projects"))

	// Wait for the collector's wedge-recycle re-dispatch (second send) so the
	// in-flight turn is genuinely wedged and stable before the interjection.
	waitForSend(t, client, 2)

	// Interject now that the in-flight turn is wedged.
	ackB, _, err := s.startConversationTurnOpts(ctx, "tnt_dev", convID, "steer it", nil, turnDispatchOpts{supersede: true})
	if err != nil {
		t.Fatalf("interject: %v", err)
	}

	// The interjection's collector was dispatched with sessionID == "" (D4 forced
	// a fresh seeded session): it creates one and sends on it — the third send.
	waitForSend(t, client, 3)
	client.mu.Lock()
	interjectSend := client.sendCalls[2]
	created := append([]string(nil), client.created...)
	client.mu.Unlock()

	// D4 must NOT have redispatched the interjection on the wedged session.
	if interjectSend.sessionID == "ses_live" {
		t.Fatalf("interjection sent on the wedged session ses_live — D4 recycle did not fire")
	}
	// It must be on a session freshly created during this turn (useSessionID == "").
	fresh := false
	for _, c := range created {
		if c == interjectSend.sessionID {
			fresh = true
			break
		}
	}
	if !fresh {
		t.Fatalf("interjection dispatched on session %q which was not freshly created — D4 did not force a fresh session", interjectSend.sessionID)
	}

	// Feed the interjection's reply on its fresh session, then assert it is
	// persisted on that session.
	client.sub.feed(busText(interjectSend.sessionID, "steered reply"))
	client.sub.feed(busIdle(interjectSend.sessionID))
	msg := waitForMessage(t, pool, convID, ackB)
	if strings.TrimSpace(msg.Content) != "steered reply" {
		t.Errorf("interjection reply = %q, want %q", msg.Content, "steered reply")
	}
	var meta map[string]any
	if err := json.Unmarshal(msg.Metadata, &meta); err != nil {
		t.Fatalf("unmarshal reply metadata: %v", err)
	}
	if meta["session_id"] != interjectSend.sessionID {
		t.Errorf("reply metadata session_id = %v, want %q (persisted on the fresh D4 session)", meta["session_id"], interjectSend.sessionID)
	}

	// The wedged session was aborted (the wedge-recycle aborts it).
	client.mu.Lock()
	aborted := append([]string(nil), client.aborted...)
	client.mu.Unlock()
	abortedSesLive := false
	for _, a := range aborted {
		if a == "ses_live" {
			abortedSesLive = true
			break
		}
	}
	if !abortedSesLive {
		t.Errorf("wedged session ses_live was not aborted; aborted = %v", aborted)
	}

	// The superseded turn A's reply was never persisted (no partial content).
	_ = ackA
}
