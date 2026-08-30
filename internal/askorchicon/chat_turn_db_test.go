package askorchicon

// Task 2 DB-backed integration tests. They verify the turn lifecycle against
// a real Postgres: ChatStream's ack returns before the reply is collected,
// the detached collector persists the reply (and errors) after the RPC has
// returned, and the abort RPC cancels the turn + aborts the serve session.
// Skipped unless ORCHICON_TEST_DSN points at a disposable database (the
// pattern used across the repo):
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/askorchicon/ -run 'TestStartConversationTurn|TestAbortConversationTurn' -v

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	assets "github.com/beardedparrott/orchicon"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/migrate"
	"github.com/beardedparrott/orchicon/internal/opencode"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

func chatDBTestPool(t *testing.T) *db.Pool {
	t.Helper()
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed turn lifecycle tests")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := migrate.Run(ctx, pool, assets.MigrationsFS, assets.MigrationsDir); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return pool
}

// newChatService builds a Service with a real pool, a fresh turn registry, and
// the injected fake session client (bypassing the real host serve).
func newChatService(t *testing.T, pool *db.Pool, client *fakeSessionClient) *Service {
	t.Helper()
	s := New(pool, slog.Default(), nil, nil, nil)
	s.testServeClient = client
	return s
}

func createConversation(t *testing.T, pool *db.Pool, modelRef string) string {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	row, err := db.CreateConversation(ctx, ttx.Tx, db.ConversationRow{
		ID:       db.NewID(),
		TenantID: "tnt_dev",
		ModelRef: modelRef,
	})
	if err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return row.ID
}

func setConversationSessionID(t *testing.T, pool *db.Pool, convID, sessionID string) {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	if err := db.UpdateConversationSessionID(ctx, ttx.Tx, "tnt_dev", convID, sessionID); err != nil {
		t.Fatalf("set session id: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// listMessages returns a conversation's messages (oldest first).
func listMessages(t *testing.T, pool *db.Pool, convID string) []db.MessageRow {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	rows, err := db.ListMessages(ctx, ttx.Tx, "tnt_dev", convID, 100, "")
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	// ListMessages returns DESC; reverse for chronological order.
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	return rows
}

// busToolWithArgs builds a completed tool-part bus event with the given
// tool name and args (the repetition-signature input).
func busToolWithArgs(sessionID, tool string, input map[string]any) opencode.BusEvent {
	return opencode.BusEvent{
		Type: "message.part.updated",
		Properties: map[string]any{
			"sessionID": sessionID,
			"part": map[string]any{
				"type":  "tool",
				"tool":  tool,
				"input": input,
				"state": map[string]any{"status": "completed", "output": "[]"},
			},
		},
	}
}

// TestStartConversationTurnStallAborts verifies ADR-ASK-1: a turn fed no
// activity within the no-progress window aborts the serve session and
// persists a "stuck" error under the acked id — NOT the 30-minute
// reply-window timeout. This is the "spins forever" fix.
func TestStartConversationTurnStallAborts(t *testing.T) {
	t.Setenv("ORCHICON_ASK_STALL_NO_PROGRESS_WINDOW", "100ms")
	pool := chatDBTestPool(t)
	client := &fakeSessionClient{}
	s := newChatService(t, pool, client)

	convID := createConversation(t, pool, "")
	ctx := context.Background()
	ackID, _, err := s.startConversationTurn(ctx, "tnt_dev", convID, "hello", nil)
	if err != nil {
		t.Fatalf("startConversationTurn: %v", err)
	}

	// No reply events are fed — the stall monitor trips, aborts the session,
	// and persists a clear retryable error (not a 30-minute timeout wait).
	msg := waitForMessage(t, pool, convID, ackID)
	if msg.Role != "assistant" {
		t.Fatalf("acked message role = %q, want assistant", msg.Role)
	}
	var meta map[string]any
	if err := json.Unmarshal(msg.Metadata, &meta); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	errText, _ := meta["error"].(string)
	if !strings.Contains(errText, "stuck") || !strings.Contains(errText, "stalled") {
		t.Errorf("metadata.error = %q, want a stall message mentioning 'stuck' and 'stalled'", errText)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.aborted) == 0 {
		t.Error("stall must abort the serve session (interrupt the model now)")
	}
}

// TestStartConversationTurnRepetitionStallAborts verifies the repetition
// signal: the same tool_use signature repeated beyond the count within the
// window aborts the turn with a stall error instead of looping forever.
func TestStartConversationTurnRepetitionStallAborts(t *testing.T) {
	t.Setenv("ORCHICON_ASK_STALL_REPETITION_WINDOW", "2s")
	t.Setenv("ORCHICON_ASK_STALL_REPETITION_COUNT", "3")
	pool := chatDBTestPool(t)
	client := &fakeSessionClient{}
	s := newChatService(t, pool, client)

	convID := createConversation(t, pool, "")
	ctx := context.Background()
	ackID, _, err := s.startConversationTurn(ctx, "tnt_dev", convID, "hello", nil)
	if err != nil {
		t.Fatalf("startConversationTurn: %v", err)
	}

	// Feed the SAME tool call repeatedly (after the send was accepted).
	waitForSend(t, client, 1)
	time.Sleep(100 * time.Millisecond) // let the collector flip sent before the tool events
	input := map[string]any{"dir": "src"}
	for i := 0; i < 5; i++ {
		client.sub.feed(busToolWithArgs("ses_1", "orchicon_list_project_dir", input))
		time.Sleep(20 * time.Millisecond)
	}

	msg := waitForMessage(t, pool, convID, ackID)
	var meta map[string]any
	if err := json.Unmarshal(msg.Metadata, &meta); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	errText, _ := meta["error"].(string)
	if !strings.Contains(errText, "stuck") || !strings.Contains(errText, "stalled:repetition") {
		t.Errorf("metadata.error = %q, want a repetition stall message", errText)
	}
}

// TestInterjectConversationTurnSupersedes verifies ADR-ASK-2 end to end: an
// interjection on a conversation with an in-flight turn cancels the old
// collector (whose partial content is persisted as a PLAIN assistant
// message, not an error bubble), aborts the serve session, dispatches a
// fresh turn that acks a DIFFERENT assistant id and persists its own reply,
// and leaves exactly the new turn's registry entry (the stale-token guard).
func TestInterjectConversationTurnSupersedes(t *testing.T) {
	t.Setenv("ORCHICON_ASK_REATTACH_BACKOFF", "1ms")
	pool := chatDBTestPool(t)
	client := &fakeSessionClient{}
	s := newChatService(t, pool, client)

	convID := createConversation(t, pool, "")
	setConversationSessionID(t, pool, convID, "ses_keep")
	ctx := context.Background()

	// 1. Start a turn; feed a bit of partial content but never idle.
	ack1, _, err := s.startConversationTurn(ctx, "tnt_dev", convID, "first", nil)
	if err != nil {
		t.Fatalf("first send: %v", err)
	}
	waitForSend(t, client, 1)
	client.sub.feed(busText("ses_keep", "partial answer"))
	time.Sleep(100 * time.Millisecond) // let the old collector process the text before the cancel races it

	// 2. Interject (supersede) with a second message.
	ack2, _, err := s.startConversationTurnOpts(ctx, "tnt_dev", convID, "stop and focus on X", nil, turnDispatchOpts{supersede: true})
	if err != nil {
		t.Fatalf("interject: %v", err)
	}
	if ack2 == ack1 {
		t.Fatal("interject must ack a DIFFERENT assistant message id")
	}

	// 3. The superseded turn's partial content is persisted as a plain
	// (non-error) assistant message; the serve session was aborted.
	partial := waitForMessage(t, pool, convID, ack1)
	if partial.Role != "assistant" {
		t.Fatalf("superseded message role = %q, want assistant", partial.Role)
	}
	if strings.TrimSpace(partial.Content) != "partial answer" {
		t.Errorf("superseded content = %q, want %q", partial.Content, "partial answer")
	}
	var meta map[string]any
	if err := json.Unmarshal(partial.Metadata, &meta); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	if _, isErr := meta["error"]; isErr {
		t.Errorf("superseded message metadata carries an error: %v", meta)
	}

	// 5. The serve session was aborted by the supersede, and the registry
	// holds exactly the NEW turn — the superseded collector's deferred
	// remove (token-guarded) could not clobber the replacement entry while
	// it is still live.
	client.mu.Lock()
	aborted := len(client.aborted)
	client.mu.Unlock()
	if aborted == 0 {
		t.Error("supersede must abort the serve session to interrupt the model")
	}
	s.turns.mu.Lock()
	_, present := s.turns.turns[convID]
	s.turns.mu.Unlock()
	if !present {
		t.Fatal("expected the interjection turn to still be registered (stale-token guard failed)")
	}

	// 6. The interjection reply is persisted under ack2, then BOTH
	// collectors finalize and the registry drains to empty (each removes
	// only its own token).
	waitForSend(t, client, 2)
	time.Sleep(100 * time.Millisecond) // let the new collector flip sent before the reply events
	client.sub.feed(busText("ses_keep", "focusing on X"))
	client.sub.feed(busIdle("ses_keep"))
	reply := waitForMessage(t, pool, convID, ack2)
	if strings.TrimSpace(reply.Content) != "focusing on X" {
		t.Errorf("interjection reply = %q, want %q", reply.Content, "focusing on X")
	}
	deadline := time.After(5 * time.Second)
	for {
		s.turns.mu.Lock()
		_, present := s.turns.turns[convID]
		s.turns.mu.Unlock()
		if !present {
			break
		}
		select {
		case <-time.After(20 * time.Millisecond):
		case <-deadline:
			t.Fatal("registry must drain to empty after both turns finalize")
		}
	}
}

// waitForMessage polls for a message by id and returns it.
func waitForMessage(t *testing.T, pool *db.Pool, convID, id string) db.MessageRow {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		for _, m := range listMessages(t, pool, convID) {
			if m.ID == id {
				return m
			}
		}
		select {
		case <-time.After(20 * time.Millisecond):
		case <-deadline:
			t.Fatalf("message %s never persisted", id)
		}
	}
}

// TestStartConversationTurnReturnsAckAndPersistsReplyAfterReturn verifies the
// acceptance criteria: the send RPC returns immediately (an ack with the
// assistant message id) and the reply is collected on a detached context and
// persisted AFTER the RPC has returned — i.e. a browser disconnect / tab
// close cannot cancel it.
func TestStartConversationTurnReturnsAckAndPersistsReplyAfterReturn(t *testing.T) {
	pool := chatDBTestPool(t)
	client := &fakeSessionClient{}
	s := newChatService(t, pool, client)

	convID := createConversation(t, pool, "")

	// The RPC returns the ack before any reply exists.
	ctx := context.Background()
	ackID, _, err := s.startConversationTurn(ctx, "tnt_dev", convID, "hello", nil)
	if err != nil {
		t.Fatalf("startConversationTurn: %v", err)
	}
	if ackID == "" {
		t.Fatal("ack assistant message id must not be empty")
	}

	// The user message is persisted synchronously (before the reply).
	msgs := listMessages(t, pool, convID)
	if len(msgs) != 1 || msgs[0].Role != "user" || msgs[0].Content != "hello" {
		t.Fatalf("expected exactly the persisted user message, got %+v", msgs)
	}

	// Now the model replies (the collector is draining on a detached
	// context). Feed reasoning + text + idle and wait for the persisted reply.
	waitForSend(t, client, 1)
	if got := client.sendCalls[0]; got.sessionID != "ses_1" {
		t.Fatalf("first-message send session = %q, want created ses_1", got.sessionID)
	}
	client.sub.feed(busReasoning("ses_1", "analyzing the request"))
	client.sub.feed(busText("ses_1", "Hello back"))
	client.sub.feed(busIdle("ses_1"))

	reply := waitForMessage(t, pool, convID, ackID)
	if reply.Role != "assistant" {
		t.Fatalf("acked message role = %q, want assistant", reply.Role)
	}
	if strings.TrimSpace(reply.Content) != "Hello back" {
		t.Errorf("persisted reply = %q, want %q", reply.Content, "Hello back")
	}
	// Reasoning chunks from the SSE bus are persisted on the assistant
	// message (Task 3): the reasoning bubble data path.
	if len(reply.Reasoning) != 1 || reply.Reasoning[0] != "analyzing the request" {
		t.Errorf("persisted reasoning = %v, want [analyzing the request]", reply.Reasoning)
	}
	var meta map[string]any
	_ = json.Unmarshal(reply.Metadata, &meta)
	if meta["model_ref"] == "" {
		t.Errorf("reply metadata missing model_ref: %v", meta)
	}

	// The user message carries no reasoning (empty array, not nil).
	msgs = listMessages(t, pool, convID)
	if len(msgs) != 2 || msgs[0].Role != "user" {
		t.Fatalf("expected user + assistant messages, got %+v", msgs)
	}
	if msgs[0].Reasoning == nil || len(msgs[0].Reasoning) != 0 {
		t.Errorf("user message reasoning = %v, want empty non-nil slice", msgs[0].Reasoning)
	}
}

// TestStartConversationTurnRejectsSecondSend verifies the one-turn-per-
// conversation gate: a second send while a turn is pending is rejected with
// FailedPrecondition BEFORE any user message is persisted.
func TestStartConversationTurnRejectsSecondSend(t *testing.T) {
	pool := chatDBTestPool(t)
	client := &fakeSessionClient{}
	s := newChatService(t, pool, client)

	convID := createConversation(t, pool, "")

	// A first send starts a turn (the collector registers it).
	ctx := context.Background()
	firstAck, _, err := s.startConversationTurn(ctx, "tnt_dev", convID, "first", nil)
	if err != nil {
		t.Fatalf("first send: %v", err)
	}
	// The collector is live; a second send must be rejected.
	_, _, err = s.startConversationTurn(ctx, "tnt_dev", convID, "second", nil)
	var cerr *connect.Error
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeFailedPrecondition {
		t.Fatalf("second send error = %v, want FailedPrecondition", err)
	}
	// The rejected send must not have persisted a second user message.
	msgs := listMessages(t, pool, convID)
	if len(msgs) != 1 || msgs[0].Content != "first" {
		t.Fatalf("expected only the first user message, got %d messages", len(msgs))
	}
	// Finalize the first turn so the collector exits cleanly and releases
	// the registry entry (no leaked goroutine).
	waitForSend(t, client, 1)
	client.sub.feed(busText("ses_1", "done"))
	client.sub.feed(busIdle("ses_1"))
	waitForMessage(t, pool, convID, firstAck)
}

// TestPartialReplyMirroredWhileTurnRuns verifies a refreshed / second-tab
// client can watch the reply grow: the collector mirrors collected text into
// the acked assistant message row WHILE the turn is still in flight (the
// ListMessages poll renders it), and the finalize upserts the complete reply
// over the partial — exactly one row, terminal content.
func TestPartialReplyMirroredWhileTurnRuns(t *testing.T) {
	pool := chatDBTestPool(t)
	client := &fakeSessionClient{}
	s := newChatService(t, pool, client)

	convID := createConversation(t, pool, "")
	ctx := context.Background()
	ackID, _, err := s.startConversationTurn(ctx, "tnt_dev", convID, "hello", nil)
	if err != nil {
		t.Fatalf("startConversationTurn: %v", err)
	}
	waitForSend(t, client, 1)

	// First text chunk — the acked row appears with partial content while the
	// turn is still running (no idle yet) and still reports in flight.
	client.sub.feed(busText("ses_1", "part one"))
	partial := waitForMessage(t, pool, convID, ackID)
	if strings.TrimSpace(partial.Content) != "part one" {
		t.Fatalf("partial content = %q, want %q (mirrored while the turn runs)", partial.Content, "part one")
	}
	if _, ok := s.turns.get(convID); !ok {
		t.Fatal("turn must still be in flight while the partial row is visible")
	}

	// Second chunk + idle — the SAME row is upserted to the complete reply.
	client.sub.feed(busText("ses_1", "part two"))
	client.sub.feed(busIdle("ses_1"))
	deadline := time.After(5 * time.Second)
	for {
		msgs := listMessages(t, pool, convID)
		var final *db.MessageRow
		for i := range msgs {
			if msgs[i].ID == ackID {
				final = &msgs[i]
				break
			}
		}
		if final != nil && strings.TrimSpace(final.Content) == "part one\n\npart two" {
			break
		}
		select {
		case <-time.After(20 * time.Millisecond):
		case <-deadline:
			t.Fatal("final reply never upserted over the partial")
		}
	}

	// Exactly one row for the acked id (the partial was upserted, not
	// duplicated) and the turn released.
	if count := countMessageRows(t, pool, convID, ackID); count != 1 {
		t.Errorf("acked id has %d rows, want exactly 1 (partial upserted)", count)
	}
	if _, ok := s.turns.get(convID); ok {
		t.Error("turn must be released once the reply finalizes")
	}
}

// countMessageRows returns how many rows exist for a message id (partial
// mirroring must upsert, never duplicate).
func countMessageRows(t *testing.T, pool *db.Pool, convID, id string) int {
	t.Helper()
	n := 0
	for _, m := range listMessages(t, pool, convID) {
		if m.ID == id {
			n++
		}
	}
	return n
}

// TestPartialReplyMirrorsTokenDeltas verifies the live mirror streams
// mid-generation token deltas: a long single part with no completion still
// grows the partial row, so a re-attached client sees output without waiting
// for the part to complete (the "nothing until the final message" report).
// Reasoning deltas/parts grow the thinking tail; deltas never corrupt the
// durable record (the finalize overwrites the row with the authoritative
// reply).
func TestPartialReplyMirrorsTokenDeltas(t *testing.T) {
	pool := chatDBTestPool(t)
	client := &fakeSessionClient{}
	s := newChatService(t, pool, client)

	convID := createConversation(t, pool, "")
	ctx := context.Background()
	ackID, _, err := s.startConversationTurn(ctx, "tnt_dev", convID, "hello", nil)
	if err != nil {
		t.Fatalf("startConversationTurn: %v", err)
	}
	waitForSend(t, client, 1)

	// A completed reasoning part first (the "last reasoning block" a refresher
	// sees), then raw deltas for a text part that never completes before the
	// assertions run.
	client.sub.feed(busReasoning("ses_1", "thinking hard"))
	client.sub.feed(busDelta("ses_1", "The "))
	client.sub.feed(busDelta("ses_1", "answer"))
	client.sub.feed(busDelta("ses_1", " is "))

	// The partial row grows with the deltas (no part completion, no idle).
	deadline := time.After(5 * time.Second)
	for {
		msgs := listMessages(t, pool, convID)
		var row *db.MessageRow
		for i := range msgs {
			if msgs[i].ID == ackID {
				row = &msgs[i]
				break
			}
		}
		if row != nil && strings.Contains(row.Content, "The answer is") {
			break
		}
		select {
		case <-time.After(20 * time.Millisecond):
		case <-deadline:
			t.Fatal("delta text never mirrored into the partial row")
		}
	}
	// The completed reasoning part is mirrored as the thinking tail.
	msgs := listMessages(t, pool, convID)
	var row *db.MessageRow
	for i := range msgs {
		if msgs[i].ID == ackID {
			row = &msgs[i]
			break
		}
	}
	if len(row.Reasoning) == 0 || row.Reasoning[0] != "thinking hard" {
		t.Errorf("reasoning mirror = %v, want [thinking hard]", row.Reasoning)
	}
	if _, ok := s.turns.get(convID); !ok {
		t.Fatal("turn must still be in flight (deltas are not a completed reply)")
	}

	// Finalize cleanly so the collector exits: the authoritative reply
	// replaces the delta text and the turn releases.
	client.sub.feed(busText("ses_1", "The answer is 42"))
	client.sub.feed(busIdle("ses_1"))
	deadline = time.After(5 * time.Second)
	for {
		if _, ok := s.turns.get(convID); !ok {
			break
		}
		select {
		case <-time.After(20 * time.Millisecond):
		case <-deadline:
			t.Fatal("turn never released after finalize")
		}
	}
	final := waitForMessage(t, pool, convID, ackID)
	if strings.TrimSpace(final.Content) != "The answer is 42" {
		t.Errorf("final reply = %q, want %q (deltas must not leak into the durable record)", final.Content, "The answer is 42")
	}
}

// TestAbortConversationTurn verifies the Stop path: it cancels the registered
// turn and aborts the conversation's opencode session via SessionClient,
// keeping the session alive for the next message.
func TestAbortConversationTurn(t *testing.T) {
	pool := chatDBTestPool(t)
	client := &fakeSessionClient{}
	s := newChatService(t, pool, client)

	convID := createConversation(t, pool, "")
	setConversationSessionID(t, pool, convID, "ses_keep")

	// Register a turn manually (as the collector would) with a recording
	// cancel so the abort's cancellation is observable.
	turnCtx, cancelTurn := context.WithCancelCause(context.Background())
	if _, ok := s.turns.register(convID, "tnt_dev", "msg_abort", cancelTurn); !ok {
		t.Fatal("register turn")
	}
	cancelled := make(chan struct{})
	go func() { <-turnCtx.Done(); close(cancelled) }()

	ctx := tenant.WithID(context.Background(), "tnt_dev")
	_, err := s.AbortConversationTurn(ctx, connect.NewRequest(
		&apiv1.AbortConversationTurnRequest{ConversationId: convID},
	))
	if err != nil {
		t.Fatalf("abort: %v", err)
	}

	// The collector's cancel fired.
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("abort did not cancel the registered turn")
	}
	// The serve session was aborted via the session client (no subprocess to
	// kill); the session itself is kept — no delete call exists.
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.aborted) != 1 || client.aborted[0] != "ses_keep" {
		t.Errorf("aborted sessions = %v, want [ses_keep]", client.aborted)
	}
}

// TestConversationSurfacesRunningTurn verifies GetConversation and
// ListConversations report an in-flight turn (turn_in_flight + the pending
// assistant message id) while the turn registry holds it — the data a
// refreshed frontend / another tab uses to restore the Stop button — and
// report idle again once the turn is released.
func TestConversationSurfacesRunningTurn(t *testing.T) {
	pool := chatDBTestPool(t)
	client := &fakeSessionClient{}
	s := newChatService(t, pool, client)

	convID := createConversation(t, pool, "")
	ctx := tenant.WithID(context.Background(), "tnt_dev")

	// No running turn yet: both reads report idle.
	get, err := s.GetConversation(ctx, connect.NewRequest(&apiv1.GetConversationRequest{Id: convID}))
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if get.Msg.Conversation.TurnInFlight || get.Msg.Conversation.PendingAssistantMessageId != "" {
		t.Fatalf("idle conversation reported running: turn_in_flight=%v pending=%q",
			get.Msg.Conversation.TurnInFlight, get.Msg.Conversation.PendingAssistantMessageId)
	}

	// Register a running turn (as the collector would) carrying its acked
	// assistant message id.
	assistantMsgID := "msg_pending_1"
	token, ok := s.turns.register(convID, "tnt_dev", assistantMsgID, func(error) {})
	if !ok {
		t.Fatal("register turn")
	}
	t.Cleanup(func() { s.turns.remove(convID, token) })

	get, err = s.GetConversation(ctx, connect.NewRequest(&apiv1.GetConversationRequest{Id: convID}))
	if err != nil {
		t.Fatalf("GetConversation (running): %v", err)
	}
	if !get.Msg.Conversation.TurnInFlight || get.Msg.Conversation.PendingAssistantMessageId != assistantMsgID {
		t.Fatalf("running conversation not surfaced: turn_in_flight=%v pending=%q",
			get.Msg.Conversation.TurnInFlight, get.Msg.Conversation.PendingAssistantMessageId)
	}

	list, err := s.ListConversations(ctx, connect.NewRequest(&apiv1.ListConversationsRequest{}))
	if err != nil {
		t.Fatalf("ListConversations: %v", err)
	}
	var listed *apiv1.Conversation
	for i := range list.Msg.Conversations {
		if list.Msg.Conversations[i].Id == convID {
			listed = list.Msg.Conversations[i]
			break
		}
	}
	if listed == nil {
		t.Fatal("conversation missing from list")
	}
	if !listed.TurnInFlight || listed.PendingAssistantMessageId != assistantMsgID {
		t.Fatalf("list reported wrong turn state: turn_in_flight=%v pending=%q",
			listed.TurnInFlight, listed.PendingAssistantMessageId)
	}

	// Release the turn (the collector's finalize) — both reads go idle again.
	s.turns.remove(convID, token)
	get, err = s.GetConversation(ctx, connect.NewRequest(&apiv1.GetConversationRequest{Id: convID}))
	if err != nil {
		t.Fatalf("GetConversation (released): %v", err)
	}
	if get.Msg.Conversation.TurnInFlight || get.Msg.Conversation.PendingAssistantMessageId != "" {
		t.Fatalf("released conversation still running: turn_in_flight=%v pending=%q",
			get.Msg.Conversation.TurnInFlight, get.Msg.Conversation.PendingAssistantMessageId)
	}
}

// TestStartConversationTurnTimeoutPersistsError verifies a turn that never
// completes within the reply window persists an empty-content assistant
// message with metadata.error set (the frontend's error bubble + Retry).
func TestStartConversationTurnTimeoutPersistsError(t *testing.T) {
	t.Setenv("ORCHICON_ASK_REPLY_WINDOW", "100ms")
	pool := chatDBTestPool(t)
	client := &fakeSessionClient{}
	s := newChatService(t, pool, client)

	convID := createConversation(t, pool, "")
	ctx := context.Background()
	ackID, _, err := s.startConversationTurn(ctx, "tnt_dev", convID, "hello", nil)
	if err != nil {
		t.Fatalf("startConversationTurn: %v", err)
	}

	// No reply events are fed — the reply window expires and the collector
	// persists the error under the acked id.
	msg := waitForMessage(t, pool, convID, ackID)
	if msg.Role != "assistant" {
		t.Fatalf("acked message role = %q, want assistant", msg.Role)
	}
	if msg.Content != "" {
		t.Errorf("error message content = %q, want empty", msg.Content)
	}
	// No reasoning parts arrived before the timeout — the persisted array is
	// empty (a model that emits no reasoning yields no events).
	if msg.Reasoning == nil || len(msg.Reasoning) != 0 {
		t.Errorf("error message reasoning = %v, want empty non-nil slice", msg.Reasoning)
	}
	var meta map[string]any
	if err := json.Unmarshal(msg.Metadata, &meta); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	errText, _ := meta["error"].(string)
	if !strings.Contains(errText, "timed out") {
		t.Errorf("metadata.error = %q, want a timeout message", errText)
	}
}

// TestStartConversationTurnServeLossFreshSessionFallback verifies the
// serve-loss path end to end: the bus dies mid-reply (serve restart), the
// re-attached session 404s (data dir wiped), and a fresh session seeded from
// the DB transcript takes over — the reply is still persisted in the same
// conversation.
func TestStartConversationTurnServeLossFreshSessionFallback(t *testing.T) {
	t.Setenv("ORCHICON_ASK_REATTACH_BACKOFF", "1ms")
	pool := chatDBTestPool(t)
	client := &fakeSessionClient{sendErrs: []error{nil, opencode.ErrSessionNotFound}}
	s := newChatService(t, pool, client)

	convID := createConversation(t, pool, "")
	setConversationSessionID(t, pool, convID, "ses_orig")
	ctx := context.Background()
	ackID, _, err := s.startConversationTurn(ctx, "tnt_dev", convID, "hello", nil)
	if err != nil {
		t.Fatalf("startConversationTurn: %v", err)
	}

	// 1. First dispatch accepted on the original session, then the bus dies.
	waitForSend(t, client, 1)
	// The steady-state follow-up system carries NO DB history block; the
	// original session already holds the conversation (buildSystemPrompt
	// includeHistory=false). Assert on prompt content, not literals — the
	// handler passes real buildSystemPrompt output.
	if got := client.sendCalls[0]; got.sessionID != "ses_orig" || strings.Contains(got.system, "## Conversation history") {
		t.Fatalf("first send = session %q, want ses_orig with reuse system (no history block); system len %d", got.sessionID, len(got.system))
	}
	client.sub.Close()

	// 2. Re-attach: the re-dispatched send 404s (data dir wiped) → fresh
	// seeded session ses_1 (the fake's first CreateSession).
	waitForSend(t, client, 2)
	if got := client.sendCalls[1]; got.sessionID != "ses_orig" {
		t.Fatalf("re-attach send session = %q, want ses_orig (re-attach, not recreate yet)", got.sessionID)
	}
	waitForSend(t, client, 3)
	// The fallback dispatch runs on the FRESH session with the SEED system:
	// the DB transcript is injected, so the history block MUST be present.
	if got := client.sendCalls[2]; got.sessionID != "ses_1" || !strings.Contains(got.system, "## Conversation history") {
		t.Fatalf("fallback send = session %q, want fresh ses_1 with seed system (history block present)", got.sessionID)
	}
	client.sub.feed(busText("ses_1", "recovered"))
	client.sub.feed(busIdle("ses_1"))

	// 3. The reply is persisted in the SAME conversation (fresh session
	// seeded from the DB transcript).
	msg := waitForMessage(t, pool, convID, ackID)
	if strings.TrimSpace(msg.Content) != "recovered" {
		t.Errorf("persisted reply = %q, want recovered", msg.Content)
	}
	var meta map[string]any
	_ = json.Unmarshal(msg.Metadata, &meta)
	if sid, _ := meta["session_id"].(string); sid != "ses_1" {
		t.Errorf("metadata.session_id = %q, want ses_1", sid)
	}
}
