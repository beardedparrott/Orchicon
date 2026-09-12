package askorchicon

// DB-backed tests for the conversation delete teardown: the resolved adapter's
// durable history is purged, and usage is DE-LINKED (never deleted).
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/askorchicon/ -run TestDeleteConversation -v

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// purgingFakeClient is a fakeSessionClient that ALSO satisfies the optional
// scheduler.ConversationHistoryPurger capability, recording every purge. It
// proves the delete path type-asserts the capability off the RESOLVED client
// (the same client it aborts), rather than reaching for the native bridge
// directly — which would break on any other adapter kind.
type purgingFakeClient struct {
	*fakeSessionClient

	mu     sync.Mutex
	purged []purgedConv
}

type purgedConv struct{ convID, sessionID string }

func (p *purgingFakeClient) PurgeConversationHistory(_ context.Context, conversationID, sessionID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.purged = append(p.purged, purgedConv{convID: conversationID, sessionID: sessionID})
	return nil
}

func (p *purgingFakeClient) purgeCalls() []purgedConv {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]purgedConv(nil), p.purged...)
}

func TestDeleteConversationPurgesHistoryAndDelinksUsage(t *testing.T) {
	pool := chatDBTestPool(t)

	// The outer type implements ChatTurnClient (promoted from the embed) AND
	// the purger capability — exactly like the native bridge does.
	client := &purgingFakeClient{fakeSessionClient: &fakeSessionClient{}}
	s := New(pool, slog.Default(), nil, nil, nil)
	s.testServeClient = client

	const tenantID = "tnt_dev"
	convID := createConversation(t, pool, "orchicon/deepseek/deepseek-flash")
	sessionID := "orchicon-ask:" + convID
	setConversationSessionID(t, pool, convID, sessionID)

	// Usage attributed to this conversation (the canonical Ask shape: the
	// conversation id in session_id).
	bg := context.Background()
	var usageID string
	ttx, err := pool.BeginTenantTx(bg, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	row, err := db.CreateUsageRecord(bg, ttx.Tx, db.UsageRecordRow{
		TenantID:     tenantID,
		AdapterKind:  "orchicon",
		SessionID:    convID,
		Provider:     "deepseek",
		Model:        "deepseek-flash",
		PromptTokens: 999,
		CostUSD:      0.25,
		OccurredAt:   time.Now().UTC(),
	})
	if err != nil {
		ttx.Rollback(bg)
		t.Fatalf("create usage record: %v", err)
	}
	if err := ttx.Commit(bg); err != nil {
		t.Fatalf("commit usage: %v", err)
	}
	usageID = row.ID
	t.Cleanup(func() {
		c := context.Background()
		ttx2, err := pool.BeginTenantTx(c, tenantID)
		if err != nil {
			return
		}
		defer ttx2.Rollback(c)
		_, _ = ttx2.Tx.Exec(c, `DELETE FROM usage_records WHERE tenant_id = $1 AND id = $2`, tenantID, usageID)
		_ = ttx2.Commit(c)
	})

	ctx := tenant.WithID(bg, tenantID)
	if _, err := s.DeleteConversation(ctx, connect.NewRequest(
		&apiv1.DeleteConversationRequest{Id: convID},
	)); err != nil {
		t.Fatalf("delete conversation: %v", err)
	}

	// 1. The adapter's durable history was purged, with the resolved session id.
	calls := client.purgeCalls()
	if len(calls) != 1 {
		t.Fatalf("purge calls = %d, want 1: %+v", len(calls), calls)
	}
	if calls[0].convID != convID || calls[0].sessionID != sessionID {
		t.Errorf("purge called with (%q, %q), want (%q, %q)",
			calls[0].convID, calls[0].sessionID, convID, sessionID)
	}

	// 2. The usage row SURVIVES the delete with its money intact, but is no
	//    longer linked to the deleted conversation.
	check, err := pool.BeginTenantTx(bg, tenantID)
	if err != nil {
		t.Fatalf("begin check tx: %v", err)
	}
	defer check.Rollback(bg)
	var sid string
	var cost float64
	if err := check.Tx.QueryRow(bg,
		`SELECT session_id, cost_usd FROM usage_records WHERE tenant_id = $1 AND id = $2`,
		tenantID, usageID).Scan(&sid, &cost); err != nil {
		t.Fatalf("deleted conversation's usage row must survive (the money is a ledger): %v", err)
	}
	if sid != "" {
		t.Errorf("usage session_id = %q, want cleared by the de-link", sid)
	}
	if cost != 0.25 {
		t.Errorf("usage cost = %v, want 0.25 preserved", cost)
	}

	// 3. The conversation itself is gone.
	var n int
	if err := check.Tx.QueryRow(bg,
		`SELECT count(*) FROM ask_orchicon_conversations WHERE tenant_id = $1 AND id = $2`,
		tenantID, convID).Scan(&n); err != nil {
		t.Fatalf("count conversations: %v", err)
	}
	if n != 0 {
		t.Errorf("conversation rows = %d, want 0 after delete", n)
	}
}

func TestDeleteConversationWithoutPurgerStillSucceeds(t *testing.T) {
	// An adapter that holds no durable history (opencode) does not implement
	// the purger. The delete must still succeed — the capability is optional
	// and its absence must never fail the RPC.
	pool := chatDBTestPool(t)
	s := newChatService(t, pool, &fakeSessionClient{})

	convID := createConversation(t, pool, "opencode/deepseek-v4-flash")
	ctx := tenant.WithID(context.Background(), "tnt_dev")
	if _, err := s.DeleteConversation(ctx, connect.NewRequest(
		&apiv1.DeleteConversationRequest{Id: convID},
	)); err != nil {
		t.Fatalf("delete conversation without a purger capability: %v", err)
	}
}
