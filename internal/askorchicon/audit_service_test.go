package askorchicon

// Ask-Orchicon service-level audit tests (design §5 item 3 + D8): the user
// send RPCs (ChatStream AND InterjectConversationTurn) each write exactly
// one conversation.message_sent row (interject marks superseded=true) in
// the same tx as the message persistence; the read RPCs (GetConversation,
// ListMessages) write zero; AbortConversationTurn writes one
// conversation.turn_aborted row. Skipped unless ORCHICON_TEST_DSN points at
// a disposable database:
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/askorchicon/ -run TestAuditService -v

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	apiv1connect "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/auth"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

func auditEventCount(t *testing.T, pool *db.Pool, tenantID, action, targetType, targetID string) int {
	t.Helper()
	rows, err := pool.ListAuditEvents(context.Background(), tenantID, action, "", targetType, targetID, "", 100, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("ListAuditEvents: %v", err)
	}
	return len(rows)
}

// auditCreateConversation is createConversation scoped to the caller's
// tenant (the shared helper hardcodes tnt_dev).
func auditCreateConversation(t *testing.T, pool *db.Pool, tenantID string) string {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	row, err := db.CreateConversation(ctx, ttx.Tx, db.ConversationRow{
		ID: db.NewID(), TenantID: tenantID, ModelRef: "",
	})
	if err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return row.ID
}

// auditConvEnv wires the service over a migration-applied pool with the
// fake session client and seeds the actor identity + a conversation, all in
// the given tenant.
func auditConvEnv(t *testing.T, tenantID string) (*db.Pool, *Service, *fakeSessionClient, string, string, string) {
	t.Helper()
	pool := chatDBTestPool(t)
	client := &fakeSessionClient{}
	s := newChatService(t, pool, client)

	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	actor, _, err := db.GetOrCreateIdentity(ctx, ttx.Tx, tenantID, "audit-chat-"+strings.ToLower(db.NewID()), "Chat Actor", "user")
	if err != nil {
		t.Fatalf("ensure identity: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit identity: %v", err)
	}
	convID := auditCreateConversation(t, pool, tenantID)
	return pool, s, client, tenantID, actor.ID, convID
}

// auditChatStream drives the REAL ChatStream RPC through a connect handler
// (httptest + generated client) so the assertion covers the full handler
// path, not just the dispatch core. The turn is fed to completion so the
// stream closes and the RPC returns.
func auditChatStream(t *testing.T, s *Service, client *fakeSessionClient, tenantID, actorID, convID, message string) {
	t.Helper()
	handler := connect.NewServerStreamHandler(apiv1connect.AskOrchiconServiceChatStreamProcedure,
		func(ctx context.Context, req *connect.Request[apiv1.ChatStreamRequest], stream *connect.ServerStream[apiv1.ChatStreamResponse]) error {
			ctx = tenant.WithID(ctx, tenantID)
			ctx = auth.WithIdentity(ctx, auth.ResolvedIdentity{
				IdentityID: actorID, TenantID: tenantID, AuthMethod: "oidc", IsAdmin: true,
			})
			return s.ChatStream(ctx, req, stream)
		})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	client2 := apiv1connect.NewAskOrchiconServiceClient(srv.Client(), srv.URL)
	stream, err := client2.ChatStream(context.Background(), connect.NewRequest(&apiv1.ChatStreamRequest{
		ConversationId: convID,
		Message:        message,
	}))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	// Feed the reply to completion so the detached collector finalizes and
	// closes the stream channel (the RPC then returns).
	go func() {
		waitForSendNoFatal(client, 1, 5)
		if client.sub == nil {
			return // turn never dispatched; the stream drain will time out
		}
		client.sub.feed(busText("ses_1", "hi"))
		client.sub.feed(busIdle("ses_1"))
	}()
	for stream.Receive() {
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
}

// waitForSendNoFatal polls the fake client for n send calls without
// failing the test (callable from a goroutine; a stuck turn is caught by
// the stream-drain deadline instead).
func waitForSendNoFatal(client *fakeSessionClient, n int, seconds int) {
	deadline := time.Now().Add(time.Duration(seconds) * time.Second)
	for time.Now().Before(deadline) {
		client.mu.Lock()
		c := len(client.sendCalls)
		client.mu.Unlock()
		if c >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestAuditServiceChatStreamMessageSent asserts one conversation.message_sent
// row per ChatStream send with the actor fields and a non-secret snapshot.
func TestAuditServiceChatStreamMessageSent(t *testing.T) {
	pool, s, client, tenantID, actorID, convID := auditConvEnv(t, "tnt_audit_chat_mut")

	auditChatStream(t, s, client, tenantID, actorID, convID, "hello there")

	if n := auditEventCount(t, pool, tenantID, "conversation.message_sent", "conversation", convID); n != 1 {
		t.Fatalf("conversation.message_sent rows = %d, want 1", n)
	}
	rows, err := pool.ListAuditEvents(context.Background(), tenantID, "conversation.message_sent", "", "conversation", convID, "", 10, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("ListAuditEvents: %v", err)
	}
	if rows[0].ActorType != "user" || rows[0].AuthMethod != "oidc" || rows[0].ActorIdentityID != actorID {
		t.Fatalf("actor fields = type:%s auth:%s id:%s, want user/oidc/%s",
			rows[0].ActorType, rows[0].AuthMethod, rows[0].ActorIdentityID, actorID)
	}
	var after map[string]any
	if err := json.Unmarshal(rows[0].After, &after); err != nil {
		t.Fatalf("unmarshal after: %v", err)
	}
	if after["message_id"] == "" || after["role"] != "user" {
		t.Fatalf("message_sent after = %v, want message_id + role=user", after)
	}
	if strings.Contains(string(rows[0].After), "hello there") {
		t.Fatalf("message content leaked into the audit snapshot: %s", rows[0].After)
	}
}

// TestAuditServiceInterjectMessageSent asserts one conversation.message_sent
// row per InterjectConversationTurn send, with superseded=true in the after
// snapshot (single implementation point, D8).
func TestAuditServiceInterjectMessageSent(t *testing.T) {
	pool, s, client, tenantID, actorID, convID := auditConvEnv(t, "tnt_audit_chat_interj")

	// Interject drives startConversationTurnOpts(supersede=true) — the same
	// RPC core as ChatStream. Drive through the real handler path.
	handler := connect.NewServerStreamHandler(apiv1connect.AskOrchiconServiceInterjectConversationTurnProcedure,
		func(ctx context.Context, req *connect.Request[apiv1.InterjectConversationTurnRequest], stream *connect.ServerStream[apiv1.ChatStreamResponse]) error {
			ctx = tenant.WithID(ctx, tenantID)
			ctx = auth.WithIdentity(ctx, auth.ResolvedIdentity{
				IdentityID: actorID, TenantID: tenantID, AuthMethod: "oidc", IsAdmin: true,
			})
			return s.InterjectConversationTurn(ctx, req, stream)
		})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	client2 := apiv1connect.NewAskOrchiconServiceClient(srv.Client(), srv.URL)
	stream, err := client2.InterjectConversationTurn(context.Background(), connect.NewRequest(&apiv1.InterjectConversationTurnRequest{
		ConversationId: convID,
		Message:        "stop and focus",
	}))
	if err != nil {
		t.Fatalf("InterjectConversationTurn: %v", err)
	}
	go func() {
		waitForSendNoFatal(client, 1, 5)
		if client.sub == nil {
			return // turn never dispatched; the stream drain will time out
		}
		client.sub.feed(busText("ses_1", "ok"))
		client.sub.feed(busIdle("ses_1"))
	}()
	for stream.Receive() {
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("Interject: %v", err)
	}

	if n := auditEventCount(t, pool, tenantID, "conversation.message_sent", "conversation", convID); n != 1 {
		t.Fatalf("conversation.message_sent rows = %d, want 1", n)
	}
	rows, err := pool.ListAuditEvents(context.Background(), tenantID, "conversation.message_sent", "", "conversation", convID, "", 10, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("ListAuditEvents: %v", err)
	}
	var after map[string]any
	if err := json.Unmarshal(rows[0].After, &after); err != nil {
		t.Fatalf("unmarshal after: %v", err)
	}
	if after["superseded"] != true {
		t.Fatalf("interject after = %v, want superseded=true", after)
	}
}

// TestAuditServiceChatReadsWriteNothing pins the read-only side: the
// conversation view RPCs must not write audit rows.
func TestAuditServiceChatReadsWriteNothing(t *testing.T) {
	pool, s, _, tenantID, actorID, convID := auditConvEnv(t, "tnt_audit_chat_read")
	ctx := tenant.WithID(context.Background(), tenantID)
	ctx = auth.WithIdentity(ctx, auth.ResolvedIdentity{
		IdentityID: actorID, TenantID: tenantID, AuthMethod: "oidc", IsAdmin: true,
	})

	if _, err := s.GetConversation(ctx, connect.NewRequest(&apiv1.GetConversationRequest{Id: convID})); err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if _, err := s.ListMessages(ctx, connect.NewRequest(&apiv1.ListMessagesRequest{ConversationId: convID})); err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if n := auditEventCount(t, pool, tenantID, "conversation.message_sent", "conversation", convID); n != 0 {
		t.Fatalf("reads wrote message_sent rows: %d, want 0", n)
	}
	if n := auditEventCount(t, pool, tenantID, "conversation.created", "conversation", convID); n != 0 {
		t.Fatalf("reads wrote conversation.created rows: %d, want 0", n)
	}
}

// TestAuditServiceAbortConversationTurn asserts one conversation.turn_aborted
// row per AbortConversationTurn (the Stop button).
func TestAuditServiceAbortConversationTurn(t *testing.T) {
	pool, s, _, tenantID, actorID, convID := auditConvEnv(t, "tnt_audit_chat_abort")
	ctx := tenant.WithID(context.Background(), tenantID)
	ctx = auth.WithIdentity(ctx, auth.ResolvedIdentity{
		IdentityID: actorID, TenantID: tenantID, AuthMethod: "oidc", IsAdmin: true,
	})

	// A registered turn makes the abort path cancel it (exercise the full
	// handler, not just the no-op branch).
	turnCtx, cancelTurn := context.WithCancelCause(context.Background())
	if _, ok := s.turns.register(convID, tenantID, "msg_abort", cancelTurn); !ok {
		t.Fatal("register turn")
	}
	done := make(chan struct{})
	go func() { <-turnCtx.Done(); close(done) }()
	defer func() {
		select {
		case <-done:
		default:
			cancelTurn(errUserStop)
		}
	}()

	if _, err := s.AbortConversationTurn(ctx, connect.NewRequest(&apiv1.AbortConversationTurnRequest{
		ConversationId: convID,
	})); err != nil {
		t.Fatalf("AbortConversationTurn: %v", err)
	}
	if n := auditEventCount(t, pool, tenantID, "conversation.turn_aborted", "conversation", convID); n != 1 {
		t.Fatalf("conversation.turn_aborted rows = %d, want 1", n)
	}

	// Idempotent no-op for a conversation with no running turn must not
	// double-write.
	if _, err := s.AbortConversationTurn(ctx, connect.NewRequest(&apiv1.AbortConversationTurnRequest{
		ConversationId: convID,
	})); err != nil {
		t.Fatalf("second AbortConversationTurn: %v", err)
	}
	if n := auditEventCount(t, pool, tenantID, "conversation.turn_aborted", "conversation", convID); n != 2 {
		t.Fatalf("second abort wrote rows: turn_aborted rows = %d, want 2 (one per Stop action)", n)
	}
}
