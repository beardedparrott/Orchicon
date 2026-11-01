package askorchicon

// undo_revert_db_test.go — THE DELETION SEMANTICS, AGAINST A REAL POSTGRES.
//
// The two verbs remove different amounts of history and one of them is the only destructive operation an operator
// has on a transcript, so the SQL is not something to trust to a mock: `(created_at, id) >= ($3, $4)` is a ROW
// COMPARISON, and a row comparison that silently degrades to `created_at >= $3` would take extra messages with it.
//
// It also pins the half that has no visible symptom: THE MODEL SESSION IS RECYCLED. Turns are continued on the
// stored session id, so without the reset a removed message would leave the transcript while staying in what the
// model reasons about — the screen saying one thing and the model knowing another.
//
// Skipped unless ORCHICON_TEST_DSN points at a DISPOSABLE database (the repo-wide convention):
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon_undo_feature_test?sslmode=disable'
//	go test ./internal/askorchicon/ -run 'TestDeleteConversationMessage' -v

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// seedTranscript writes n alternating messages with EXPLICIT timestamps, so the ordering the deletion relies on is
// the ordering the test asserts rather than whatever `now()` produced in the same millisecond.
func seedTranscript(t *testing.T, pool *db.Pool, conversationID string, n int) []db.MessageRow {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	var out []db.MessageRow
	for i := 0; i < n; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		row, err := db.CreateMessage(ctx, ttx.Tx, db.MessageRow{
			ID:             db.NewID(),
			TenantID:       "tnt_dev",
			ConversationID: conversationID,
			Role:           role,
			Content:        role + " message",
			// THE JSONB COLUMNS ARE NOT NULL. sanitizeMessage fills the ones it owns; these are the caller's,
			// and an insert without them is a constraint violation rather than an empty ledger.
			ToolCalls:   []byte("[]"),
			ToolResults: []byte("[]"),
			Attachments: []byte("[]"),
			Metadata:    []byte("{}"),
		})
		if err != nil {
			t.Fatalf("create message: %v", err)
		}
		// Force the timestamp: the insertion order and the created_at order must agree, and `now()` inside one
		// transaction does not distinguish rows.
		if _, err := ttx.Tx.Exec(ctx,
			`UPDATE ask_orchicon_messages SET created_at = $1 WHERE tenant_id = $2 AND id = $3`,
			base.Add(time.Duration(i)*time.Second), "tnt_dev", row.ID); err != nil {
			t.Fatalf("stamp created_at: %v", err)
		}
		out = append(out, row)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return out
}

func messagesOf(t *testing.T, pool *db.Pool, conversationID string) []db.MessageRow {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	rows, err := db.ListMessages(ctx, ttx.Tx, "tnt_dev", conversationID, 100, "")
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	return rows
}

func TestDeleteConversationMessageOnlyRemovesOneRow(t *testing.T) {
	pool := chatDBTestPool(t)
	conv := createConversation(t, pool, "m")
	msgs := seedTranscript(t, pool, conv, 5)
	svc := newChatService(t, pool, &fakeSessionClient{})
	ctx := tenant.WithID(context.Background(), "tnt_dev")

	// Give the conversation a session id, so the reset has something to clear.
	setSession(t, pool, conv, "sess-1")

	res, err := svc.DeleteConversationMessage(ctx, connect.NewRequest(&apiv1.DeleteConversationMessageRequest{
		ConversationId: conv,
		MessageId:      msgs[2].ID,
		Mode:           apiv1.MessageDeleteMode_MESSAGE_DELETE_MODE_ONLY,
	}))
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := res.Msg.GetDeletedCount(); got != 1 {
		t.Errorf("deleted %d, want 1 — undo removes exactly one message", got)
	}
	left := messagesOf(t, pool, conv)
	if len(left) != 4 {
		t.Fatalf("%d messages remain, want 4", len(left))
	}
	for _, m := range left {
		if m.ID == msgs[2].ID {
			t.Error("the named message survived")
		}
	}
	if !res.Msg.GetSessionReset() {
		t.Error("session_reset is false — the model's context would still hold the removed message")
	}
	if got := sessionOf(t, pool, conv); got != "" {
		t.Errorf("session id is still %q, so the next turn would CONTINUE a session containing the removed text", got)
	}
}

func TestDeleteConversationMessageAndFollowingTruncates(t *testing.T) {
	pool := chatDBTestPool(t)
	conv := createConversation(t, pool, "m")
	msgs := seedTranscript(t, pool, conv, 5)
	svc := newChatService(t, pool, &fakeSessionClient{})
	ctx := tenant.WithID(context.Background(), "tnt_dev")

	res, err := svc.DeleteConversationMessage(ctx, connect.NewRequest(&apiv1.DeleteConversationMessageRequest{
		ConversationId: conv,
		MessageId:      msgs[2].ID,
		Mode:           apiv1.MessageDeleteMode_MESSAGE_DELETE_MODE_AND_FOLLOWING,
	}))
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	// msgs[2], [3] and [4] — the named message INCLUDED, which is the operator's "revert to that message
	// removing anything that has come after it".
	if got := res.Msg.GetDeletedCount(); got != 3 {
		t.Errorf("deleted %d, want 3", got)
	}
	left := messagesOf(t, pool, conv)
	if len(left) != 2 {
		t.Fatalf("%d messages remain, want the 2 BEFORE the named one (%v)", len(left), idsOf(left))
	}
	for _, m := range left {
		if m.ID != msgs[0].ID && m.ID != msgs[1].ID {
			t.Errorf("unexpected survivor %s (role %s)", m.ID, m.Role)
		}
	}
}

func TestDeletingAMessageFromAnotherConversationIsRefused(t *testing.T) {
	pool := chatDBTestPool(t)
	convA := createConversation(t, pool, "m")
	convB := createConversation(t, pool, "m")
	other := seedTranscript(t, pool, convB, 2)
	svc := newChatService(t, pool, &fakeSessionClient{})
	ctx := tenant.WithID(context.Background(), "tnt_dev")

	_, err := svc.DeleteConversationMessage(ctx, connect.NewRequest(&apiv1.DeleteConversationMessageRequest{
		ConversationId: convA,
		MessageId:      other[0].ID,
		Mode:           apiv1.MessageDeleteMode_MESSAGE_DELETE_MODE_AND_FOLLOWING,
	}))
	if err == nil {
		t.Fatal("a message from ANOTHER conversation was deleted — the handler would be a deletion primitive " +
			"over any row a caller can name")
	}
	if len(messagesOf(t, pool, convB)) != 2 {
		t.Error("the other conversation lost a message")
	}
}

func TestAModeMustBeChosen(t *testing.T) {
	pool := chatDBTestPool(t)
	conv := createConversation(t, pool, "m")
	msgs := seedTranscript(t, pool, conv, 2)
	svc := newChatService(t, pool, &fakeSessionClient{})
	ctx := tenant.WithID(context.Background(), "tnt_dev")

	_, err := svc.DeleteConversationMessage(ctx, connect.NewRequest(&apiv1.DeleteConversationMessageRequest{
		ConversationId: conv,
		MessageId:      msgs[0].ID,
		// UNSPECIFIED: an unnamed intent must not default to the destructive one.
	}))
	if err == nil {
		t.Fatal("an unspecified mode was accepted — a caller that forgot to name the scope would silently get " +
			"whichever default, and the truncating one is irreversible")
	}
	if len(messagesOf(t, pool, conv)) != 2 {
		t.Error("a refused request still deleted something")
	}
}

func idsOf(rows []db.MessageRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ID + ":" + r.Role
	}
	return out
}

func setSession(t *testing.T, pool *db.Pool, conversationID, session string) {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	if err := db.UpdateConversationSessionID(ctx, ttx.Tx, "tnt_dev", conversationID, session); err != nil {
		t.Fatalf("set session: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func sessionOf(t *testing.T, pool *db.Pool, conversationID string) string {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	row, err := db.GetConversation(ctx, ttx.Tx, "tnt_dev", conversationID)
	if err != nil {
		t.Fatalf("get conversation: %v", err)
	}
	return row.SessionID
}

// TestTruncationBreaksATimestampTieByID — the case the row comparison EXISTS for.
//
// created_at is `now()`, which is not unique: a user message and the reply that follows it can share a
// millisecond, and rows written in one transaction always do. Without the (created_at, id) TIEBREAK a revert at the
// SECOND of a tied pair would also delete the first — silently taking an extra message the operator asked to keep.
//
// This is why the seed above stamps DISTINCT timestamps for the ordering test and this one deliberately ties
// them: the two properties are different, and a test with distinct timestamps cannot see this one at all.
func TestTruncationBreaksATimestampTieByID(t *testing.T) {
	pool := chatDBTestPool(t)
	conv := createConversation(t, pool, "m")
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	tied := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	var rows []db.MessageRow
	for i := 0; i < 3; i++ {
		row, err := db.CreateMessage(ctx, ttx.Tx, db.MessageRow{
			ID: db.NewID(), TenantID: "tnt_dev", ConversationID: conv, Role: "user",
			Content: "m", ToolCalls: []byte("[]"), ToolResults: []byte("[]"),
			Attachments: []byte("[]"), Metadata: []byte("{}"),
		})
		if err != nil {
			t.Fatalf("create message: %v", err)
		}
		// THE FIRST TWO SHARE A TIMESTAMP; the third is later. So the tiebreak alone decides whether the
		// second is the start of the truncation.
		at := tied
		if i == 2 {
			at = tied.Add(time.Second)
		}
		if _, err := ttx.Tx.Exec(ctx,
			`UPDATE ask_orchicon_messages SET created_at = $1 WHERE tenant_id = $2 AND id = $3`,
			at, "tnt_dev", row.ID); err != nil {
			t.Fatalf("stamp: %v", err)
		}
		rows = append(rows, row)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Order the tied pair by ID, so the target is deterministically the SECOND of the two.
	first, second := rows[0], rows[1]
	if first.ID > second.ID {
		first, second = second, first
	}
	svc := newChatService(t, pool, &fakeSessionClient{})
	tctx := tenant.WithID(context.Background(), "tnt_dev")
	if _, err := svc.DeleteConversationMessage(tctx, connect.NewRequest(&apiv1.DeleteConversationMessageRequest{
		ConversationId: conv,
		MessageId:      second.ID,
		Mode:           apiv1.MessageDeleteMode_MESSAGE_DELETE_MODE_AND_FOLLOWING,
	})); err != nil {
		t.Fatalf("delete: %v", err)
	}
	left := messagesOf(t, pool, conv)
	if len(left) != 1 || left[0].ID != first.ID {
		t.Fatalf("survivors = %v, want only the FIRST of the tied pair (%s).\n"+
			"A truncation at the second of two messages sharing a timestamp must not also delete the first — "+
			"that is what the (created_at, id) tiebreak is for, and `created_at >= $3` alone loses this.",
			idsOf(left), first.ID)
	}
}
