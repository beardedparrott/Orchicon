package project

// Service-level audit tests (design §5 item 3): project mutations write
// exactly one audit_events row in the same tx, Get/List write zero.
// Skipped unless ORCHICON_TEST_DSN points at a disposable database:
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/project/ -run TestAuditService -v

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
	ident := seedAuditIdentity(t, pool, tenantID, "audit-pj-"+strings.ToLower(db.NewID()))
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

// TestAuditServiceProjectMutations covers create/update/archive (the
// project lifecycle) and asserts reads write nothing.
func TestAuditServiceProjectMutations(t *testing.T) {
	pool, s, ctx, tenantID, _ := auditServiceEnv(t, "tnt_audit_pj_mut")

	resp, err := s.CreateProject(ctx, connect.NewRequest(&apiv1.CreateProjectRequest{
		Name: "Audit Project " + strings.ToLower(db.NewID()),
	}))
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	projID := resp.Msg.Project.Id
	if n := auditEventCount(t, pool, tenantID, "project.created", "project", projID); n != 1 {
		t.Fatalf("project.created rows = %d, want 1", n)
	}

	// Get/List write nothing.
	if _, err := s.GetProject(ctx, connect.NewRequest(&apiv1.GetProjectRequest{Id: projID})); err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if n := auditEventCount(t, pool, tenantID, "project.created", "project", projID); n != 1 {
		t.Fatalf("GetProject wrote audit rows: created rows = %d, want still 1", n)
	}
	if _, err := s.ListProjects(ctx, connect.NewRequest(&apiv1.ListProjectsRequest{})); err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if n := auditEventCount(t, pool, tenantID, "project.created", "project", projID); n != 1 {
		t.Fatalf("ListProjects wrote audit rows: created rows = %d, want still 1", n)
	}

	// Update → 1 project.updated row (before + after populated).
	newName := "Renamed audit project"
	if _, err := s.UpdateProject(ctx, connect.NewRequest(&apiv1.UpdateProjectRequest{
		Id:   projID,
		Name: &newName,
	})); err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}
	if n := auditEventCount(t, pool, tenantID, "project.updated", "project", projID); n != 1 {
		t.Fatalf("project.updated rows = %d, want 1", n)
	}

	// Archive → 1 project.archived row.
	if _, err := s.ArchiveProject(ctx, connect.NewRequest(&apiv1.ArchiveProjectRequest{Id: projID})); err != nil {
		t.Fatalf("ArchiveProject: %v", err)
	}
	if n := auditEventCount(t, pool, tenantID, "project.archived", "project", projID); n != 1 {
		t.Fatalf("project.archived rows = %d, want 1", n)
	}
}

// TestAuditServiceProjectCreateRollback pins atomicity: a create rejected
// by validation leaves no audit row.
func TestAuditServiceProjectCreateRollback(t *testing.T) {
	pool, s, ctx, tenantID, _ := auditServiceEnv(t, "tnt_audit_pj_rb_"+strings.ToLower(db.NewID()))

	_, err := s.CreateProject(ctx, connect.NewRequest(&apiv1.CreateProjectRequest{
		Name: strings.Repeat("x", 300),
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("over-long name: err = %v, want InvalidArgument", err)
	}
	rows, err := pool.ListAuditEvents(context.Background(), tenantID, "", "", "", "", "", 100, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("ListAuditEvents: %v", err)
	}
	for _, r := range rows {
		if strings.HasPrefix(r.Action, "project.") {
			t.Fatalf("failed create left an audit row: %+v", r)
		}
	}
}
