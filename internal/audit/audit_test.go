package audit_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/audit"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/migrate"
)

// openTestPool opens a DB pool against ORCHICON_TEST_DSN (mirrors the
// other DB-backed tests) and applies migrations so the audit_events
// table + auth_method column exist.
func openTestPool(t *testing.T) *db.Pool {
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

// TestAuditRecordCommitted verifies a row lands in the tx and is visible
// after commit (transactional outbox: audit row exists iff mutation
// committed).
func TestAuditRecordCommitted(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	// Unique target id per run so the test is idempotent on a shared DB
	// (the tenant accumulates rows across runs).
	targetID := "wi-" + db.NewID()

	ttx, err := pool.BeginTenantTx(ctx, "tnt_audit_commit")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	ident, _, err := db.GetOrCreateIdentity(ctx, ttx.Tx, "tnt_audit_commit", "audit-actor@orchicon.local", "Audit Actor", "user")
	if err != nil {
		t.Fatalf("ensure identity: %v", err)
	}
	if err := audit.Record(ctx, ttx.Tx, audit.Entry{
		TenantID:        "tnt_audit_commit",
		ActorIdentityID: ident.ID,
		ActorType:       "user",
		AuthMethod:      "oidc",
		Action:          "work_item.created",
		TargetType:      "work_item",
		TargetID:        targetID,
		After:           json.RawMessage(`{"title":"hi"}`),
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	rows, err := pool.ListAuditEvents(ctx, "tnt_audit_commit", "", "", "work_item", targetID, "", 10, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row after commit, got %d", len(rows))
	}
	r := rows[0]
	if r.Action != "work_item.created" || r.TargetType != "work_item" || r.TargetID != targetID {
		t.Fatalf("unexpected row: %+v", r)
	}
	if r.AuthMethod != "oidc" || r.ActorType != "user" || r.ActorIdentityID != ident.ID {
		t.Fatalf("unexpected actor fields: %+v", r)
	}
	// jsonb normalizes whitespace; compare semantically.
	var after map[string]any
	if err := json.Unmarshal(r.After, &after); err != nil {
		t.Fatalf("unmarshal after: %v", err)
	}
	if after["title"] != "hi" {
		t.Fatalf("unexpected after: %s", r.After)
	}
}

// TestAuditRecordRollback verifies the row rolls back with the tx.
func TestAuditRecordRollback(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	ttx, err := pool.BeginTenantTx(ctx, "tnt_audit_rollback")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := audit.Record(ctx, ttx.Tx, audit.Entry{
		TenantID:   "tnt_audit_rollback",
		ActorType:  "system",
		Action:     "project.created",
		TargetType: "project",
		TargetID:   "p-1",
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := ttx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	rows, err := pool.ListAuditEvents(ctx, "tnt_audit_rollback", "", "", "", "", "", 10, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected 0 rows after rollback, got %d", len(rows))
	}
}

// TestAuditSystemRowNullActorScan verifies a system-actor row (no
// identity -> actor_identity_id stored NULL) lists cleanly: the read
// path must scan the nullable column, not assume a non-null string.
func TestAuditSystemRowNullActorScan(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, "tnt_sys")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	targetID := "sys-" + db.NewID()
	if err := audit.Record(ctx, ttx.Tx, audit.Entry{
		TenantID:   "tnt_sys",
		ActorType:  "system",
		AuthMethod: "system",
		Action:     "x.system",
		TargetType: "x",
		TargetID:   targetID,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	rows, err := pool.ListAuditEvents(ctx, "tnt_sys", "", "", "x", targetID, "", 10, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("ListAuditEvents: %v", err)
	}
	if len(rows) != 1 || rows[0].ActorIdentityID != "" {
		t.Fatalf("expected 1 system row with empty actor id, got %+v", rows)
	}
}

// TestAuditNoTenantRejected verifies Record rejects an entry without a
// tenant (RLS backstop requires the session variable).
func TestAuditNoTenantRejected(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	ttx, err := pool.BeginTenantTx(ctx, "tnt_audit")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	if err := audit.Record(ctx, ttx.Tx, audit.Entry{
		Action:     "tenant.created",
		TargetType: "tenant",
	}); err == nil {
		t.Fatal("expected error for entry without tenant")
	}
}

// TestAuditCrossTenantRLS verifies RLS rejects a cross-tenant read.
func TestAuditCrossTenantRLS(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	ttx, err := pool.BeginTenantTx(ctx, "tnt_a")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	if err := audit.Record(ctx, ttx.Tx, audit.Entry{
		TenantID:   "tnt_a",
		ActorType:  "system",
		Action:     "x.created",
		TargetType: "x",
		TargetID:   "1",
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Read from tnt_b: RLS must hide the tnt_a row.
	rows, err := pool.ListAuditEvents(ctx, "tnt_b", "", "", "", "", "", 10, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected 0 cross-tenant rows, got %d", len(rows))
	}
}
