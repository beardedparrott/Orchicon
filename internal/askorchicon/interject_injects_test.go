package askorchicon

// interject_injects_test.go — interjecting ADDS to the running turn; it does not interrupt it.
//
// THE OPERATOR: "I would much rather just do the injecting instead. It should still look like a chat message
// in the stream to the user, and it should inject in the context that the model is constantly pulling back in
// … I want it to 'interject' by adding it to the overall context as a user message but NOT abort."
//
// THE OLD BEHAVIOUR, and why it broke: supersede cancelled the collector and ABORTED the provider session
// mid-generation. What survives an abort is the EVIDENCE of the cut-off turn — tool results, file changes,
// transcript — without the assistant turn that produced it, so the model inherits finished work it has no
// record of doing. Reported as "seems to think the current work may have been done by someone else" and "loses
// track of what it was already working on".
//
// These drive the real collector through the fake serve bus, so they assert the behaviour rather than the
// wiring.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
)

// testInterjectModelRef is set on the conversation rather than left to the tenant default, so these tests are
// self-contained: an unseeded tenant has no DefaultAskOrchiconModel and every dispatch would fail before it
// reached the behaviour under test.
const testInterjectModelRef = "opencode/test-model"

// TestInterjectInjectsWithoutAborting is the core contract change.
func TestInterjectInjectsWithoutAborting(t *testing.T) {
	t.Setenv("ORCHICON_ASK_REATTACH_BACKOFF", "1ms")
	pool := chatDBTestPool(t)
	client := &fakeSessionClient{}
	s := newChatService(t, pool, client)

	convID := createConversation(t, pool, testInterjectModelRef)
	setConversationSessionID(t, pool, convID, "ses_keep")
	ctx := context.Background()

	// 1. A turn is running, mid-reply.
	ack1, _, _, err := s.startConversationTurn(ctx, "tnt_dev", convID, "first", nil)
	if err != nil {
		t.Fatalf("first send: %v", err)
	}
	waitForSend(t, client, 1)
	client.sub.feed(busText("ses_keep", "working on it"))
	time.Sleep(100 * time.Millisecond) // let the collector record the partial before we inject

	entry, running := s.turns.get(convID)
	if !running {
		t.Fatal("the first turn should be registered")
	}

	// 2. Inject. NO STREAM: this drives the delivery half, which is where the behaviour lives (the caller then
	// attaches to the running reply — see watchRunningTurn, exercised by the watch path).
	if err := s.deliverInjection(ctx, "tnt_dev", convID, "also check the logs", nil, entry); err != nil {
		t.Fatalf("inject: %v", err)
	}

	// 3. THE SESSION WAS NOT ABORTED — this is the whole change.
	client.mu.Lock()
	aborted := len(client.aborted)
	sends := len(client.sendCalls)
	injected := ""
	if sends >= 2 {
		injected = client.sendCalls[1].text
	}
	client.mu.Unlock()
	if aborted != 0 {
		t.Fatalf("the injection ABORTED the session %d time(s). That is the supersede behaviour the operator "+
			"rejected: an aborted turn leaves its work behind without the assistant turn that produced it", aborted)
	}

	// 4. IT WAS DELIVERED AS A USER MESSAGE on the live session.
	if sends != 2 {
		t.Fatalf("send calls = %d, want 2 (the turn's own message, then the injection)", sends)
	}
	if !strings.Contains(injected, "also check the logs") {
		t.Errorf("second send = %q, want the injected text", injected)
	}

	// 5. THE SAME TURN IS STILL RUNNING — no new assistant message, no new turn, same token.
	after, stillRunning := s.turns.get(convID)
	if !stillRunning {
		t.Fatal("the turn ended on injection; it must keep running")
	}
	if after.token != entry.token || after.assistantMsgID != ack1 {
		t.Fatalf("the turn was replaced (token %d→%d, ack %q→%q); an injection must join the running turn, "+
			"not supersede it", entry.token, after.token, ack1, after.assistantMsgID)
	}

	// 6. IT IS A CHAT MESSAGE IN THE TRANSCRIPT — persisted like any other user message, so it is visible, and so
	// a recreated session replays it into the model's context.
	found := false
	for _, m := range listMessages(t, pool, convID) {
		if m.Role == "user" && strings.TrimSpace(m.Content) == "also check the logs" {
			found = true
		}
	}
	if !found {
		t.Error("the injected message was not persisted as a user message, so it is invisible in the " +
			"transcript and missing from the history a recreated session would replay")
	}

	// 7. THE COLLECTOR EXPECTS WORK FROM IT.
	if seq := s.turns.injectionSeq(convID, entry.token); seq != 1 {
		t.Errorf("injection counter = %d, want 1 — without it the collector can finalise on an idle that "+
			"predates the injection, and the operator's message is answered by nobody", seq)
	}

	// 8. THE TURN THEN CONTINUES AND FINISHES WITH BOTH PARTS under the ORIGINAL assistant message.
	//
	// IN THE ORDER THE SERVE PRODUCES IT: the idle for the step the injection landed on comes first (and is
	// deferred by the guard), then the work for the injected message, then the idle that completes the turn.
	client.sub.feed(busIdle("ses_keep"))
	client.sub.feed(busText("ses_keep", "and the logs are clean"))
	client.sub.feed(busIdle("ses_keep"))
	waitForNoTurn(t, s, convID)
	reply := waitForMessage(t, pool, convID, ack1)
	for _, want := range []string{"working on it", "and the logs are clean"} {
		if !strings.Contains(reply.Content, want) {
			t.Errorf("final reply = %q, want it to contain %q — the injected message must continue the SAME "+
				"reply rather than start another", reply.Content, want)
		}
	}
}

// TestAStaleIdleDoesNotEndTheTurn is the race injection introduces, and the reason for the counter.
//
// `idle` means "no work in flight" and the serve emits it between steps. An injection accepted at that boundary
// can arrive before the idle is consumed, in which case the idle describes the state BEFORE the operator's
// message existed. Finalising on it would leave the injected message in the transcript with its reply
// persisted nowhere and streamed to nobody.
func TestAStaleIdleDoesNotEndTheTurn(t *testing.T) {
	t.Setenv("ORCHICON_ASK_REATTACH_BACKOFF", "1ms")
	pool := chatDBTestPool(t)
	client := &fakeSessionClient{}
	s := newChatService(t, pool, client)

	convID := createConversation(t, pool, testInterjectModelRef)
	setConversationSessionID(t, pool, convID, "ses_keep")
	ctx := context.Background()

	ack1, _, _, err := s.startConversationTurn(ctx, "tnt_dev", convID, "first", nil)
	if err != nil {
		t.Fatalf("first send: %v", err)
	}
	waitForSend(t, client, 1)
	entry, _ := s.turns.get(convID)
	if err := s.deliverInjection(ctx, "tnt_dev", convID, "one more thing", nil, entry); err != nil {
		t.Fatalf("inject: %v", err)
	}

	// THE STALE IDLE: it arrives with nothing yet produced for the injection.
	client.sub.feed(busIdle("ses_keep"))
	time.Sleep(200 * time.Millisecond)

	if row := lookupMessage(t, pool, convID, ack1); row != nil {
		t.Fatalf("the turn FINALISED on an idle that predates the injection — the operator's message is now in "+
			"the transcript with no reply. Persisted content: %q", strings.TrimSpace(row.Content))
	}
	if _, stillRunning := s.turns.get(convID); !stillRunning {
		t.Fatal("the turn was released on a stale idle")
	}

	// And the real work then arrives and completes the SAME turn.
	client.sub.feed(busText("ses_keep", "done"))
	client.sub.feed(busIdle("ses_keep"))
	reply := waitForMessage(t, pool, convID, ack1)
	if !strings.Contains(reply.Content, "done") {
		t.Errorf("reply = %q, want the post-injection work", reply.Content)
	}
	waitForNoTurn(t, s, convID)
}

// TestInjectionCarriesTheRunningTurnsPromptAndModel pins what the injected message is delivered WITH.
//
// opencode applies `system` and `model` per message, so sending them as empty (or rebuilt) would hand this
// message a different persona or model than the turn it is joining.
func TestInjectionCarriesTheRunningTurnsPromptAndModel(t *testing.T) {
	t.Setenv("ORCHICON_ASK_REATTACH_BACKOFF", "1ms")
	pool := chatDBTestPool(t)
	client := &fakeSessionClient{}
	s := newChatService(t, pool, client)

	convID := createConversation(t, pool, testInterjectModelRef)
	setConversationSessionID(t, pool, convID, "ses_keep")
	ctx := context.Background()

	if _, _, _, err := s.startConversationTurn(ctx, "tnt_dev", convID, "first", nil); err != nil {
		t.Fatalf("first send: %v", err)
	}
	waitForSend(t, client, 1)
	entry, _ := s.turns.get(convID)
	if entry.reuseSystem == "" {
		t.Fatal("the running turn published no reuse prompt, so an injection cannot re-send it")
	}
	if err := s.deliverInjection(ctx, "tnt_dev", convID, "another", nil, entry); err != nil {
		t.Fatalf("inject: %v", err)
	}

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.sendCalls) < 2 {
		t.Fatalf("send calls = %d, want at least 2", len(client.sendCalls))
	}
	second := client.sendCalls[1]
	if second.system != entry.reuseSystem {
		t.Error("the injected message was sent with a DIFFERENT system prompt than the running turn — on " +
			"opencode that applies per message, so the message would reach the model under another persona")
	}
	if second.modelRef != entry.modelRef {
		t.Errorf("the injected message was sent to model %q, but the running turn is on %q",
			second.modelRef, entry.modelRef)
	}
}

// lookupMessage returns the message row with this id, or nil while it does not exist yet.
func lookupMessage(t *testing.T, pool *db.Pool, convID, id string) *db.MessageRow {
	t.Helper()
	for _, m := range listMessages(t, pool, convID) {
		if m.ID == id {
			row := m
			return &row
		}
	}
	return nil
}

// waitForNoTurn blocks until the conversation has no registered turn.
func waitForNoTurn(t *testing.T, s *Service, convID string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		if _, running := s.turns.get(convID); !running {
			return
		}
		select {
		case <-time.After(20 * time.Millisecond):
		case <-deadline:
			t.Fatal("the turn never finished")
		}
	}
}

// TestOnlyAWedgedTurnIsEverSuperseded pins the branch table, which is where this behaviour can regress
// silently: an injection that fell through to supersede would look identical to a working interjection until
// the model started re-doing work.
func TestOnlyAWedgedTurnIsEverSuperseded(t *testing.T) {
	src := readChatSource(t)

	// THE INJECTION PATH IS CHOSEN FIRST, on "running and not wedged" — so a healthy turn can never reach the
	// supersede branch by falling through.
	if !strings.Contains(src, "if running && !entry.wedged {") {
		t.Error("the injection path is not gated on `running && !entry.wedged`; a healthy running turn must " +
			"take the injection branch, or it is superseded and the model loses the thread again")
	}
	// AND THE DISPATCH BELOW CARRIES `running`, not a hardcoded true: with nothing running there is nothing to
	// supersede, and an interjection on an idle conversation must behave like ChatStream.
	if !strings.Contains(src, "turnDispatchOpts{supersede: running}") {
		t.Error("the fallback dispatch does not pass `supersede: running` — either an idle interjection takes " +
			"a path whose purpose is to cancel something, or the wedged case stops recycling its stuck session")
	}
	// THE WEDGED CASE STILL RECYCLES. It is the one case where injection cannot work: the session is blocked on
	// a tool call, so a queued message would never reach a turn boundary.
	if !strings.Contains(src, "prevEntry.wedged") {
		t.Error("the wedged-session recycle is gone; a stuck session would never answer the interjection")
	}
}

// TestTheInjectionDoesNotCancelOrAbort is the property, asserted at the source level as well as behaviourally
// above: the injection path must not reach cancel / AbortConversationSession / serve --stop machinery.
func TestTheInjectionDoesNotCancelOrAbort(t *testing.T) {
	src := readChatSource(t)
	start := strings.Index(src, "func (s *Service) deliverInjection(")
	if start < 0 {
		t.Fatal("deliverInjection is gone")
	}
	end := strings.Index(src[start:], "\n// loadConversation")
	if end < 0 {
		t.Fatal("cannot delimit deliverInjection")
	}
	body := src[start : start+end]
	for _, forbidden := range []string{"AbortConversationSession", "turns.cancel", "errTurnSuperseded", "turns.remove"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("deliverInjection calls %s. That is the supersede behaviour: the operator asked for the "+
				"message to be ADDED to the running context, explicitly NOT to abort", forbidden)
		}
	}
}
