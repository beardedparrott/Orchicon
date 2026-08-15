package worker

// Service-level audit tests (design §5 item 3): CreateWorker and
// PublishWorkerVersion each write exactly one audit_events row in the same
// tx as the mutation; GetWorker/ListWorkers write zero. Skipped unless
// ORCHICON_TEST_DSN points at a disposable database:
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/worker/ -run TestAuditService -v

import (
	"context"
	"log/slog"
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

// auditTestPool opens a migration-applied test pool (repo convention:
// skipped unless ORCHICON_TEST_DSN is set).
func auditTestPool(t *testing.T) *db.Pool {
	t.Helper()
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed audit test")
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

// seedAuditIdentity creates (or reuses) an identity row the audit FK can
// point at, returning its id.
func seedAuditIdentity(t *testing.T, pool *db.Pool, tenantID, subject string) db.IdentityRow {
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

// auditServiceEnv opens a migration-applied pool, seeds an identity (the
// audit FK target) and returns a service + context with tenant + resolved
// identity (the middleware-equivalent actor). tenantID is caller-chosen so
// sibling tests never share audit rows.
func auditServiceEnv(t *testing.T, tenantID string) (*db.Pool, *Service, context.Context, string, string) {
	t.Helper()
	pool := auditTestPool(t)
	ident := seedAuditIdentity(t, pool, tenantID, "audit-wk-"+strings.ToLower(db.NewID()))
	ctx := tenant.WithID(context.Background(), tenantID)
	ctx = auth.WithIdentity(ctx, auth.ResolvedIdentity{
		IdentityID: ident.ID,
		TenantID:   tenantID,
		Subject:    ident.Subject,
		AuthMethod: "oidc",
		IsAdmin:    true,
	})
	s := New(pool, slog.New(slog.DiscardHandler))
	return pool, s, ctx, tenantID, ident.ID
}

func auditEventCount(t *testing.T, pool *db.Pool, tenantID, action, targetType, targetID string) int {
	t.Helper()
	rows, err := pool.ListAuditEvents(context.Background(), tenantID, action, "", targetType, targetID, "", 100)
	if err != nil {
		t.Fatalf("ListAuditEvents: %v", err)
	}
	return len(rows)
}

// TestAuditServiceWorkerMutations covers worker.created and
// worker.published (create + the publish lifecycle action from the AC
// "assign/publish/deprecate") and asserts reads write nothing.
func TestAuditServiceWorkerMutations(t *testing.T) {
	pool, s, ctx, tenantID, _ := auditServiceEnv(t, "tnt_audit_wk_mut")

	// CreateWorker → exactly one worker.created row.
	resp, err := s.CreateWorker(ctx, connect.NewRequest(&apiv1.CreateWorkerRequest{
		Name:       "Audit Worker " + strings.ToLower(db.NewID()),
		ModelRef:   "opencode/deepseek-v4-flash-free",
		RuntimeRef: "opencode",
	}))
	if err != nil {
		t.Fatalf("CreateWorker: %v", err)
	}
	workerID := resp.Msg.Worker.Id
	if n := auditEventCount(t, pool, tenantID, "worker.created", "worker", workerID); n != 1 {
		t.Fatalf("worker.created rows = %d, want 1", n)
	}

	// GetWorker / ListWorkers write nothing.
	if _, err := s.GetWorker(ctx, connect.NewRequest(&apiv1.GetWorkerRequest{Id: workerID})); err != nil {
		t.Fatalf("GetWorker: %v", err)
	}
	if n := auditEventCount(t, pool, tenantID, "worker.created", "worker", workerID); n != 1 {
		t.Fatalf("GetWorker wrote audit rows: created rows = %d, want still 1", n)
	}
	if _, err := s.ListWorkers(ctx, connect.NewRequest(&apiv1.ListWorkersRequest{})); err != nil {
		t.Fatalf("ListWorkers: %v", err)
	}
	if n := auditEventCount(t, pool, tenantID, "worker.created", "worker", workerID); n != 1 {
		t.Fatalf("ListWorkers wrote audit rows: created rows = %d, want still 1", n)
	}

	// PublishWorkerVersion → one worker.published row (before = draft,
	// after = published status).
	if _, err := s.PublishWorkerVersion(ctx, connect.NewRequest(&apiv1.PublishWorkerVersionRequest{
		WorkerId: workerID,
	})); err != nil {
		t.Fatalf("PublishWorkerVersion: %v", err)
	}
	if n := auditEventCount(t, pool, tenantID, "worker.published", "worker", workerID); n != 1 {
		t.Fatalf("worker.published rows = %d, want 1", n)
	}
}

// TestAuditServiceWorkerCreateRollback pins atomicity: a create rejected by
// validation leaves no audit row.
func TestAuditServiceWorkerCreateRollback(t *testing.T) {
	pool, s, ctx, tenantID, _ := auditServiceEnv(t, "tnt_audit_wk_rb_"+strings.ToLower(db.NewID()))

	_, err := s.CreateWorker(ctx, connect.NewRequest(&apiv1.CreateWorkerRequest{
		Name: strings.Repeat("x", 300), // over the name bound
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("over-long name: err = %v, want InvalidArgument", err)
	}
	rows, err := pool.ListAuditEvents(context.Background(), tenantID, "", "", "", "", "", 100)
	if err != nil {
		t.Fatalf("ListAuditEvents: %v", err)
	}
	for _, r := range rows {
		if strings.HasPrefix(r.Action, "worker.") {
			t.Fatalf("failed create left an audit row: %+v", r)
		}
	}
}
