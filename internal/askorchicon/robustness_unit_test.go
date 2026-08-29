package askorchicon

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"
)

// --- AC2 (refresh survival): server-confirmed progress signal ---

// TestTurnStatusReportsAccurateProgress verifies the server-confirmed status
// the frontend reconciles against after a refresh (ADR-0002 D3/AC2): a
// freshly-registered turn is in flight and progressing; a turn quiet past the
// no-progress window is not progressing (the server no longer claims "still
// working"); a wedged turn is never progressing even with recent activity; and
// a completed (removed) turn clears in flight. This is the source of the
// "still working…" vs "stalled — stop or retry" state.
func TestTurnStatusReportsAccurateProgress(t *testing.T) {
	s := &Service{turns: newTurnRegistry(), log: slog.Default()}
	cancel := func(error) {}

	// Freshly registered: in flight, progressing, pending id exposed.
	tok, ok := s.turns.register("conv", "tnt", "ack1", cancel)
	if !ok {
		t.Fatal("register failed")
	}
	info := s.turnStatus("conv")
	if !info.inFlight {
		t.Fatal("freshly registered turn must be in flight")
	}
	if info.pendingMsgID != "ack1" {
		t.Errorf("pendingMsgID = %q, want ack1", info.pendingMsgID)
	}
	if !info.progressing {
		t.Error("freshly registered turn must report progressing (recent activity)")
	}
	if info.lastActivity == nil {
		t.Error("turn must expose last_activity_at")
	}

	// Quiet past the no-progress window: still in flight but NOT progressing.
	s.turns.mu.Lock()
	e := s.turns.turns["conv"]
	e.lastActivity = time.Now().Add(-2 * time.Hour)
	s.turns.turns["conv"] = e
	s.turns.mu.Unlock()
	info = s.turnStatus("conv")
	if !info.inFlight {
		t.Fatal("a quiet-but-live turn is still in flight")
	}
	if info.progressing {
		t.Error("a turn quiet past the no-progress window must not report progressing")
	}

	// Wedged: never progressing even with recent activity.
	s.turns.markWedged("conv", tok)
	s.turns.markActivity("conv", tok)
	info = s.turnStatus("conv")
	if info.progressing {
		t.Error("a wedged turn must not report progressing regardless of activity")
	}

	// Completed (removed) turn: clears in flight.
	s.turns.remove("conv", tok)
	info = s.turnStatus("conv")
	if info.inFlight || info.pendingMsgID != "" || info.progressing {
		t.Errorf("completed turn status = %+v, want zero", info)
	}
}

// --- AC3 (mid-run interjection): one-turn gate is never left locked ---

// TestInterjectSupersedeReleasesTurnGate verifies the AC3 interject property
// at the registry level (the shared supersede path): interjecting on a
// conversation with an in-flight turn cancels + removes the old entry and is
// then free to register a fresh turn (the one-turn gate is never left locked),
// and a stale finalize from the superseded collector cannot clobber the
// replacement (token guard).
func TestInterjectSupersedeReleasesTurnGate(t *testing.T) {
	s := &Service{turns: newTurnRegistry(), log: slog.Default()}
	cancel := func(error) {}

	tokA, ok := s.turns.register("conv", "tnt", "ackA", cancel)
	if !ok {
		t.Fatal("register turn A failed")
	}

	// Interject supersedes: cancel + remove A's entry (as startConversationTurnOpts does).
	if _, ok := s.turns.cancel("conv", errTurnSuperseded); !ok {
		t.Fatal("supersede cancel reported no turn in flight")
	}
	s.turns.remove("conv", tokA)

	// The gate is free immediately: a fresh turn registers under a NEW token.
	tokB, ok := s.turns.register("conv", "tnt", "ackB", cancel)
	if !ok {
		t.Fatal("register turn B failed — the one-turn gate was left locked after an interject")
	}
	if tokB == tokA {
		t.Fatal("the interjection turn must register under a new token")
	}

	// A stale finalize from A (its own token) must not clobber B.
	s.turns.remove("conv", tokA)
	entry, present := s.turns.get("conv")
	if !present {
		t.Fatal("a stale finalize clobbered the replacement turn — token guard failed")
	}
	if entry.token != tokB || entry.assistantMsgID != "ackB" {
		t.Fatalf("registry holds %+v, want turn B (token %d, ackB)", entry, tokB)
	}
}

// --- AC4 (multi-session stability): per-turn isolation under concurrency ---

// TestConcurrentTurnsIsolated runs several independent turns concurrently over
// separate fake bus subscriptions and asserts each collects ONLY its own reply
// (no cross-session event leakage) and none hangs a reader. This exercises the
// ADR-0002 D5/D6 isolation + backpressure surface at the collector level.
func TestConcurrentTurnsIsolated(t *testing.T) {
	t.Setenv("ORCHICON_ASK_REATTACH_BACKOFF", "1ms")
	const n = 6
	type res struct {
		idx   int
		reply string
		err   error
	}
	results := make(chan res, n)
	for i := 0; i < n; i++ {
		i := i
		client := &fakeSessionClient{}
		sid := fmt.Sprintf("ses_%d", i)
		opts := turnCollectOpts{
			client: client, sessionID: sid, reuseSystem: "REUSE_SYSTEM",
			modelRef: "opencode/deepseek-v4-flash-free", userMsg: fmt.Sprintf("msg-%d", i),
		}
		go func() {
			waitForSendNoFatal(client, 1, 5)
			time.Sleep(100 * time.Millisecond)
			client.sub.feed(busText(sid, fmt.Sprintf("reply-%d", i)))
			client.sub.feed(busIdle(sid))
		}()
		go func() {
			reply, _, _, err := (&Service{turns: newTurnRegistry(), log: slog.Default()}).collectConversationReply(context.Background(), opts)
			results <- res{i, reply, err}
		}()
	}
	for i := 0; i < n; i++ {
		r := <-results
		if r.err != nil {
			t.Fatalf("turn %d errored: %v", r.idx, r.err)
		}
		if r.reply != fmt.Sprintf("reply-%d", r.idx) {
			t.Errorf("turn %d reply = %q, want reply-%d", r.idx, r.reply, r.idx)
		}
	}
}
