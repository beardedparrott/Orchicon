package db_test

import (
	"context"
	"os"
	"testing"

	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/migrate"
)

// These tests exercise the canned-worker seeder's version handling against
// a real Postgres. They are skipped unless ORCHICON_TEST_DSN points at a
// disposable database (migrations + dev workers are applied on every run):
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/db/ -run 'TestSeed.*' -v
//
// They guard the draft-preservation contract: a user draft on a canned
// worker must never be force-published by a boot re-seed.

func seedTestPool(t *testing.T) *db.Pool {
	t.Helper()
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed seed tests")
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
	if err := db.SeedDevWorkers(ctx, pool); err != nil {
		t.Fatalf("seed dev workers: %v", err)
	}
	return pool
}

func insertDraftVersion(t *testing.T, pool *db.Pool, workerID string, version int) {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	if _, err := db.CreateWorkerVersion(ctx, ttx.Tx, db.WorkerVersionRow{
		ID:               db.NewID(),
		TenantID:         "tnt_dev",
		WorkerID:         workerID,
		Version:          version,
		Status:           "draft",
		RuntimeRef:       "opencode",
		ModelRef:         "opencode-go/deepseek-v4-flash",
		ContextSources:   []byte("[]"),
		Permissions:      []byte("{}"),
		GatedTools:       []byte("[]"),
		BudgetOverrides:  []byte("{}"),
		Labels:           []byte("{}"),
		ConcurrencyLimit: 1,
	}); err != nil {
		t.Fatalf("insert draft v%d: %v", version, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func workerVersionStatus(t *testing.T, pool *db.Pool, workerID string, version int) string {
	t.Helper()
	var status string
	err := pool.QueryRow(context.Background(),
		`SELECT status FROM worker_versions WHERE worker_id = $1 AND tenant_id = 'tnt_dev' AND version = $2`,
		workerID, version).Scan(&status)
	if err != nil {
		t.Fatalf("query v%d status: %v", version, err)
	}
	return status
}

// resetWorker deletes the canned worker (cascade versions) and re-seeds it so
// each test starts from a clean, fresh seed state — independent of residue
// from a previous run against the same disposable DB.
func resetWorker(t *testing.T, pool *db.Pool, workerID string) {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if _, err := ttx.Exec(ctx,
		`DELETE FROM worker_versions WHERE worker_id = $1 AND tenant_id = 'tnt_dev'`, workerID); err != nil {
		t.Fatalf("delete versions: %v", err)
	}
	if _, err := ttx.Exec(ctx,
		`DELETE FROM workers WHERE id = $1 AND tenant_id = 'tnt_dev'`, workerID); err != nil {
		t.Fatalf("delete worker: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := db.SeedDevWorkers(ctx, pool); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
}

// TestSeedLeavesUserDraftUntouched: a user-created draft on a canned worker
// (alongside the seed's published v1) must survive a boot re-seed as a
// draft — never force-published.
func TestSeedLeavesUserDraftUntouched(t *testing.T) {
	pool := seedTestPool(t)
	const workerID = "w_ui_developer"
	resetWorker(t, pool, workerID)

	insertDraftVersion(t, pool, workerID, 2)

	if err := db.SeedDevWorkers(context.Background(), pool); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	if got := workerVersionStatus(t, pool, workerID, 1); got != "published" {
		t.Errorf("seed v1 should stay published, got %q", got)
	}
	if got := workerVersionStatus(t, pool, workerID, 2); got != "draft" {
		t.Errorf("user draft v2 must NOT be force-published, got %q", got)
	}
}

// TestSeedPublishesLatestDraftWhenNoPublishedVersion: when a canned worker
// has lost every published version, the seeder promotes the latest draft
// (and only that one) so the worker stays dispatchable, and current_version
// follows.
func TestSeedPublishesLatestDraftWhenNoPublishedVersion(t *testing.T) {
	pool := seedTestPool(t)
	ctx := context.Background()
	const workerID = "w_ui_design_architect"
	resetWorker(t, pool, workerID)

	// Remove the seed's published v1 so the worker has no published version.
	ttx, err := pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if _, err := ttx.Exec(ctx,
		`DELETE FROM worker_versions WHERE worker_id = $1 AND tenant_id = 'tnt_dev' AND status = 'published'`,
		workerID); err != nil {
		t.Fatalf("delete published versions: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	insertDraftVersion(t, pool, workerID, 2)
	insertDraftVersion(t, pool, workerID, 3)

	if err := db.SeedDevWorkers(context.Background(), pool); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	if got := workerVersionStatus(t, pool, workerID, 3); got != "published" {
		t.Errorf("latest draft v3 should be promoted when nothing is published, got %q", got)
	}
	if got := workerVersionStatus(t, pool, workerID, 2); got != "draft" {
		t.Errorf("non-latest draft v2 should stay draft, got %q", got)
	}
	var curVer int
	if err := pool.QueryRow(ctx,
		`SELECT current_version FROM workers WHERE id = $1 AND tenant_id = 'tnt_dev'`, workerID).Scan(&curVer); err != nil {
		t.Fatalf("query current_version: %v", err)
	}
	if curVer != 3 {
		t.Errorf("current_version should follow the promoted draft, got %d want 3", curVer)
	}
}
