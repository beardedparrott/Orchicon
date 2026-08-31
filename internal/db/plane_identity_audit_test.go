package db_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/migrate"
)

// TestPlaneAutomationIdentityAuditFK is the regression for the
// audit_events_actor_identity_fk failure (SQLSTATE 23503) that blocked every
// plane-channel WRITE (orchicon_plane_create_work_item) from worker
// executions: mintPlaneCredential stamped api_keys.identity_id with the
// WORKER's ID, but workers are not identities — recordAudit then stamped
// auth.ActorFromContext(ctx) (the worker ID) into audit_events, whose
// actor_identity_id is FK'd to identities. Reads worked; writes rolled back.
//
// The fix provisions a per-run service identity
// (subject "run:<runID>", identity_type "service") and stamps the key with
// ITS id. This test replays that contract: provision → mint key → audit
// write under the resolved actor must all succeed, and the key's identity
// must carry the automation subject.
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/db/ -run TestPlaneAutomationIdentityAuditFK -v
func TestPlaneAutomationIdentityAuditFK(t *testing.T) {
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed audit-FK regression")
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

	const (
		tenant = "tnt_dev"
		runID  = "run-test-auditfk"
	)
	subject := "run:" + runID

	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin tenant tx: %v", err)
	}
	// Mirrors mintPlaneCredential: ensure the per-run service identity, then
	// mint the API key against IT (never a worker ID).
	ident, created, err := db.GetOrCreateIdentity(ctx, ttx.Tx, tenant, subject, "Automation run "+runID, "service")
	if err != nil {
		t.Fatalf("ensure automation identity: %v", err)
	}
	if created && ident.ID == "" {
		t.Fatal("created identity has empty ID")
	}
	key, err := db.CreateApiKey(ctx, ttx.Tx, db.ApiKeyRow{
		TenantID:   tenant,
		IdentityID: ident.ID,
		Name:       "automation:" + runID,
		KeyPrefix:  "oc_test",
		KeyHash:    "hash-auditfk-regression",
		Scopes:     []string{"work-items:**"},
		Status:     "active",
	})
	if err != nil {
		t.Fatalf("mint api key: %v", err)
	}
	if key.IdentityID != ident.ID {
		t.Fatalf("key identity = %q, want the automation identity %q (never a worker ID)", key.IdentityID, ident.ID)
	}
	if key.IdentityID == "" || key.IdentityID == "wrk-nonexistent" {
		t.Fatalf("key identity must be a real identity row, got %q", key.IdentityID)
	}

	// The failing operation exactly: audit-write a work_item.created event
	// under the resolved actor. Before the fix this failed with 23503.
	if err := db.CreateAuditEvent(ctx, ttx.Tx, db.AuditEventRow{
		TenantID:        tenant,
		ActorIdentityID: ident.ID,
		ActorType:       "user",
		AuthMethod:      "apikey",
		Action:          "work_item.created",
		TargetType:      "work_item",
		TargetID:        "wi-auditfk-regression",
		OccurredAt:      time.Now().UTC(),
	}); err != nil {
		t.Fatalf("audit write under automation identity: %v", err)
	}
	// Idempotent second mint resolves the SAME identity row (no FK churn).
	ident2, created2, err := db.GetOrCreateIdentity(ctx, ttx.Tx, tenant, subject, "Automation run "+runID, "service")
	if err != nil || created2 {
		t.Fatalf("second ensure must reuse the row: created=%v err=%v", created2, err)
	}
	if ident2.ID != ident.ID {
		t.Fatalf("second ensure returned a different identity: %q vs %q", ident2.ID, ident.ID)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Sanity: a Worker-ID-stamped actor must STILL fail the audit write — the
	// exact bug class this guards against.
	ttx2, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin tenant tx: %v", err)
	}
	defer ttx2.Rollback(ctx)
	err = db.CreateAuditEvent(ctx, ttx2.Tx, db.AuditEventRow{
		TenantID:        tenant,
		ActorIdentityID: "wrk-never-an-identity",
		ActorType:       "user",
		AuthMethod:      "apikey",
		Action:          "work_item.created",
		TargetType:      "work_item",
		TargetID:        "wi-auditfk-worker-id",
		OccurredAt:      time.Now().UTC(),
	})
	var pgErr *pgconn.PgError
	if err == nil {
		t.Fatal("worker-ID actor must violate audit_events_actor_identity_fk")
	} else if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Fatalf("want FK violation 23503, got %v", err)
	}
}