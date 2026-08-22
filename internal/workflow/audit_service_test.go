package workflow

// Service-level audit tests (design §5 item 3): workflow mutations write
// exactly one audit_events row in the same tx, Get/List write zero.
// Skipped unless ORCHICON_TEST_DSN points at a disposable database:
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/workflow/ -run TestAuditService -v

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	assets "github.com/beardedparrott/orchicon"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/auth"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/migrate"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

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

func auditServiceEnv(t *testing.T, tenantID string) (*db.Pool, *Service, context.Context, string, string) {
	t.Helper()
	pool := auditTestPool(t)
	ident := seedAuditIdentity(t, pool, tenantID, "audit-wf-"+strings.ToLower(db.NewID()))
	ctx := tenant.WithID(context.Background(), tenantID)
	ctx = auth.WithIdentity(ctx, auth.ResolvedIdentity{
		IdentityID: ident.ID,
		TenantID:   tenantID,
		Subject:    ident.Subject,
		AuthMethod: "oidc",
		IsAdmin:    true,
	})
	s := New(pool, slog.New(slog.DiscardHandler), nil)
	return pool, s, ctx, tenantID, ident.ID
}

func auditEventCount(t *testing.T, pool *db.Pool, tenantID, action, targetType, targetID string) int {
	t.Helper()
	rows, err := pool.ListAuditEvents(context.Background(), tenantID, action, "", targetType, targetID, "", 100, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("ListAuditEvents: %v", err)
	}
	return len(rows)
}

// TestAuditServiceWorkflowMutations covers the workflow create lifecycle
// (workflow.created) and asserts reads write nothing.
func TestAuditServiceWorkflowMutations(t *testing.T) {
	pool, s, ctx, tenantID, _ := auditServiceEnv(t, "tnt_audit_wf_mut")

	resp, err := s.CreateWorkflow(ctx, connect.NewRequest(&apiv1.CreateWorkflowRequest{
		Name: "Audit Workflow " + strings.ToLower(db.NewID()),
	}))
	if err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}
	wfID := resp.Msg.Workflow.Id
	if n := auditEventCount(t, pool, tenantID, "workflow.created", "workflow", wfID); n != 1 {
		t.Fatalf("workflow.created rows = %d, want 1", n)
	}

	// Get/List write nothing.
	if _, err := s.GetWorkflow(ctx, connect.NewRequest(&apiv1.GetWorkflowRequest{Id: wfID})); err != nil {
		t.Fatalf("GetWorkflow: %v", err)
	}
	if n := auditEventCount(t, pool, tenantID, "workflow.created", "workflow", wfID); n != 1 {
		t.Fatalf("GetWorkflow wrote audit rows: created rows = %d, want still 1", n)
	}
	if _, err := s.ListWorkflows(ctx, connect.NewRequest(&apiv1.ListWorkflowsRequest{})); err != nil {
		t.Fatalf("ListWorkflows: %v", err)
	}
	if n := auditEventCount(t, pool, tenantID, "workflow.created", "workflow", wfID); n != 1 {
		t.Fatalf("ListWorkflows wrote audit rows: created rows = %d, want still 1", n)
	}
}

// TestAuditServiceWorkflowCreateRollback pins atomicity: a create rejected
// by validation leaves no audit row.
func TestAuditServiceWorkflowCreateRollback(t *testing.T) {
	pool, s, ctx, tenantID, _ := auditServiceEnv(t, "tnt_audit_wf_rb_"+strings.ToLower(db.NewID()))

	_, err := s.CreateWorkflow(ctx, connect.NewRequest(&apiv1.CreateWorkflowRequest{
		Name: strings.Repeat("x", 600),
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("over-long name: err = %v, want InvalidArgument", err)
	}
	rows, err := pool.ListAuditEvents(context.Background(), tenantID, "", "", "", "", "", 100, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("ListAuditEvents: %v", err)
	}
	for _, r := range rows {
		if strings.HasPrefix(r.Action, "workflow.") {
			t.Fatalf("failed create left an audit row: %+v", r)
		}
	}
}
