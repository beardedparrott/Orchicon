package webhook

// Service-level audit tests for the webhook RPCs whose delivery-row
// mutation and audit row commit in ONE tenant transaction
// (TestSubscription / ReplayDelivery — transactional outbox, AC1). Read-
// only RPCs (ListDeliveries) write zero. Skipped unless
// ORCHICON_TEST_DSN points at a disposable database (repo convention):
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/webhook/ -run TestAuditService -v

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"connectrpc.com/connect"
	assets "github.com/beardedparrott/orchicon"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/auth"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/migrate"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

func webhookAuditEnv(t *testing.T, tenantID string) (*db.Pool, *Service, context.Context, string, string) {
	t.Helper()
	pool := webhookAuditPool(t)
	ident := webhookSeedIdentity(t, pool, tenantID, "audit-wk-"+strings.ToLower(db.NewID()))
	ctx := tenant.WithID(context.Background(), tenantID)
	ctx = auth.WithIdentity(ctx, auth.ResolvedIdentity{
		IdentityID: ident.ID,
		TenantID:   tenantID,
		Subject:    ident.Subject,
		AuthMethod: "apikey",
		IsAdmin:    true,
	})
	log := slog.New(slog.DiscardHandler)
	dispatcher := NewDispatcher(pool, nil, log)
	s := NewService(pool, log, dispatcher, nil)
	return pool, s, ctx, tenantID, ident.ID
}

func webhookAuditPool(t *testing.T) *db.Pool {
	t.Helper()
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed webhook audit tests")
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

func webhookSeedIdentity(t *testing.T, pool *db.Pool, tenantID, subject string) db.IdentityRow {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	ident, _, err := db.GetOrCreateIdentity(ctx, ttx.Tx, tenantID, subject, "Audit Actor", "user")
	if err != nil {
		t.Fatalf("ensure identity: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit identity: %v", err)
	}
	return ident
}

func webhookSeedSubscription(t *testing.T, pool *db.Pool, tenantID, targetURL string) db.EventSubscriptionRow {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	sub, err := db.CreateSubscription(ctx, ttx.Tx, db.EventSubscriptionRow{
		ID: db.NewID(), TenantID: tenantID, Name: "Audit Test Sub " + strings.ToLower(db.NewID()),
		TargetURL: targetURL, EventFilter: "orchicon.events.>",
		Scope: "tenant", Status: "active", MaxRetries: 3,
	})
	if err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit subscription: %v", err)
	}
	return sub
}

func webhookSeedDeadLetter(t *testing.T, pool *db.Pool, tenantID, subscriptionID string) db.WebhookDeliveryRow {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	d, err := db.CreateDelivery(ctx, ttx.Tx, db.WebhookDeliveryRow{
		TenantID: tenantID, SubscriptionID: subscriptionID,
		EventID: "evt-" + db.NewID(), EventType: "orchicon.events.test",
		Payload: []byte("{}"), Status: "dead_letter", StatusCode: 500,
	})
	if err != nil {
		t.Fatalf("create dead-letter delivery: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit delivery: %v", err)
	}
	return d
}

func webhookAuditCount(t *testing.T, pool *db.Pool, tenantID, action, targetID string) int {
	t.Helper()
	rows, err := pool.ListAuditEvents(context.Background(), tenantID, action, "", "webhook_subscription", targetID, "", 100)
	if err != nil {
		t.Fatalf("ListAuditEvents: %v", err)
	}
	return len(rows)
}

// webhookDeliveriesInState lists deliveries of the subscription in the
// given status via a short read tx.
func webhookDeliveriesInState(t *testing.T, pool *db.Pool, tenantID, subID, status string) []db.WebhookDeliveryRow {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	rows, err := db.ListDeliveries(ctx, ttx.Tx, tenantID, subID, status, 10, "")
	if err != nil {
		t.Fatalf("list deliveries: %v", err)
	}
	return rows
}

// TestAuditServiceTestSubscription asserts TestSubscription writes exactly
// one webhook_subscription.tested row and the delivery row it created
// committed in the same transaction (a delivered delivery row exists iff
// the audit row does).
func TestAuditServiceTestSubscription(t *testing.T) {
	tenantID := "tnt_audit_wk_test_" + strings.ToLower(db.NewID())
	pool, s, ctx, _, actorID := webhookAuditEnv(t, tenantID)

	// A local receiver so postOnce succeeds (delivery -> delivered).
	received := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case received <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sub := webhookSeedSubscription(t, pool, tenantID, srv.URL)

	if _, err := s.TestSubscription(ctx, connect.NewRequest(&apiv1.TestSubscriptionRequest{Id: sub.ID})); err != nil {
		t.Fatalf("TestSubscription: %v", err)
	}
	<-received // the POST happened before the RPC returned

	if n := webhookAuditCount(t, pool, tenantID, "webhook_subscription.tested", sub.ID); n != 1 {
		t.Fatalf("webhook_subscription.tested rows = %d, want 1", n)
	}
	rows, err := pool.ListAuditEvents(context.Background(), tenantID, "webhook_subscription.tested", "", "webhook_subscription", sub.ID, "", 10)
	if err != nil {
		t.Fatalf("ListAuditEvents: %v", err)
	}
	if rows[0].ActorType != "user" || rows[0].AuthMethod != "apikey" || rows[0].ActorIdentityID != actorID {
		t.Fatalf("actor fields = type:%s auth:%s id:%s, want user/apikey/%s",
			rows[0].ActorType, rows[0].AuthMethod, rows[0].ActorIdentityID, actorID)
	}
	// The delivery the test created committed with the audit row.
	dlv := webhookDeliveriesInState(t, pool, tenantID, sub.ID, "delivered")
	if len(dlv) != 1 {
		t.Fatalf("delivered deliveries = %d, want 1", len(dlv))
	}
}

// TestAuditServiceReplayDelivery asserts ReplayDelivery writes exactly one
// webhook_delivery.replayed row and the replayed delivery committed with it.
func TestAuditServiceReplayDelivery(t *testing.T) {
	tenantID := "tnt_audit_wk_replay_" + strings.ToLower(db.NewID())
	pool, s, ctx, _, _ := webhookAuditEnv(t, tenantID)
	sub := webhookSeedSubscription(t, pool, tenantID, "http://127.0.0.1:1/unreachable")
	orig := webhookSeedDeadLetter(t, pool, tenantID, sub.ID)

	if _, err := s.ReplayDelivery(ctx, connect.NewRequest(&apiv1.ReplayDeliveryRequest{Id: orig.ID})); err != nil {
		t.Fatalf("ReplayDelivery: %v", err)
	}
	if n := webhookAuditCount(t, pool, tenantID, "webhook_delivery.replayed", sub.ID); n != 1 {
		t.Fatalf("webhook_delivery.replayed rows = %d, want 1", n)
	}
	// The replayed (retrying) delivery committed with the audit row.
	dlv := webhookDeliveriesInState(t, pool, tenantID, sub.ID, "retrying")
	if len(dlv) != 1 {
		t.Fatalf("retrying deliveries = %d, want 1", len(dlv))
	}

	// Read-only call writes nothing.
	if _, err := s.ListDeliveries(ctx, connect.NewRequest(&apiv1.ListDeliveriesRequest{SubscriptionId: sub.ID})); err != nil {
		t.Fatalf("ListDeliveries: %v", err)
	}
	if n := webhookAuditCount(t, pool, tenantID, "webhook_delivery.replayed", sub.ID); n != 1 {
		t.Fatalf("ListDeliveries wrote audit rows: replayed rows = %d, want still 1", n)
	}
}
