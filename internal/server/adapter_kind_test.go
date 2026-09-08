package server

// Tests for mid-run adapter-kind resolution (epic: a nudge sent while the
// worker's latest version changed adapter kind since dispatch must route to
// the bridge that actually ran the execution — the execution's recorded
// adapter_id wins over the worker's current model_ref).
//
// DB-backed; skips without ORCHICON_TEST_DSN (adapter_seed_test pattern).
// All ids are unique per run (db.NewID) so re-runs never collide.

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
)

func TestResolveAdapterKind(t *testing.T) {
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed adapter-kind test")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	tenant := "tnt_dev"
	now := time.Now()
	u := db.NewID()[:10]

	// Orchicon adapter row (the dispatching adapter for the exec under
	// test) and an opencode row that the worker's latest version would
	// mislead us toward.
	orchiconID := "adp_ak_orchicon_" + u
	seedDevAdapterKind(ctx, pool, logger, orchiconID, "orchicon", func() string { return "{}" })
	opencodeID := "adp_ak_opencode_" + u
	seedDevAdapterKind(ctx, pool, logger, opencodeID, "opencode", func() string { return "{}" })

	// Worker whose LATEST published version now refs opencode.
	workerID := "w_ak_resolve_" + u
	{
		ttx, err := pool.BeginTenantTx(ctx, tenant)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.CreateWorker(ctx, ttx.Tx, db.WorkerRow{
			ID: workerID, TenantID: tenant, Name: "ak-resolve-" + u, Slug: workerID,
			Status: domain.WorkerPublished,
		}); err != nil {
			t.Fatalf("create worker: %v", err)
		}
		if _, err := db.CreateWorkerVersion(ctx, ttx.Tx, db.WorkerVersionRow{
			ID: "v_ak_v1_" + u, TenantID: tenant, WorkerID: workerID, Version: 1,
			Status: domain.WorkerVersionPublished, ModelRef: "orchicon/deepseek/deepseek-v4-flash",
			ContextSources: []byte("[]"), Permissions: []byte("{}"), GatedTools: []byte("[]"),
			BudgetOverrides: []byte("{}"), Labels: []byte("{}"), ConcurrencyLimit: 1,
			PublishedAt: &now,
		}); err != nil {
			t.Fatalf("create version v1: %v", err)
		}
		// v2 (latest, published) refs opencode — would misroute if trusted.
		if _, err := db.CreateWorkerVersion(ctx, ttx.Tx, db.WorkerVersionRow{
			ID: "v_ak_v2_" + u, TenantID: tenant, WorkerID: workerID, Version: 2,
			Status: domain.WorkerVersionPublished, ModelRef: "anthropic/claude-sonnet-4",
			ContextSources: []byte("[]"), Permissions: []byte("{}"), GatedTools: []byte("[]"),
			BudgetOverrides: []byte("{}"), Labels: []byte("{}"), ConcurrencyLimit: 1,
			PublishedAt: &now,
		}); err != nil {
			t.Fatalf("create version v2: %v", err)
		}
		if err := ttx.Commit(ctx); err != nil {
			t.Fatalf("commit worker/versions: %v", err)
		}
	}

	// Execution bound to the ORCHICON adapter row via adapter_id.
	var execID string
	{
		ttx, err := pool.BeginTenantTx(ctx, tenant)
		if err != nil {
			t.Fatal(err)
		}
		adpOrchicon := orchiconID
		row := db.ExecutionRow{
			ID: "exec_ak_orchicon_" + u, TenantID: tenant, ProjectID: "proj_ak_" + u, TaskID: "task_ak_" + u,
			WorkerID: workerID, WorkerVersion: 2, AdapterID: &adpOrchicon,
			Status: "running", HealthState: "healthy", StartedAt: &now,
		}
		created, err := db.CreateExecution(ctx, ttx.Tx, row)
		if err != nil {
			t.Fatalf("create orchicon execution: %v", err)
		}
		execID = created.ID
		if err := ttx.Commit(ctx); err != nil {
			t.Fatalf("commit orchicon execution: %v", err)
		}
	}

	// The recorded adapter_id names the orchicon row → kind "orchicon",
	// even though the worker's latest version now refs opencode.
	got, err := resolveAdapterKind(ctx, pool, tenant, execID)
	if err != nil {
		t.Fatalf("resolveAdapterKind: %v", err)
	}
	if got != "orchicon" {
		t.Errorf("orchicon-bound execution kind = %q, want \"orchicon\" (adapter_id row must win over latest-worker model_ref)", got)
	}

	// Legacy execution WITHOUT an adapter_id falls back to the worker's
	// latest published model_ref → "opencode".
	var legacyID string
	{
		ttx, err := pool.BeginTenantTx(ctx, tenant)
		if err != nil {
			t.Fatal(err)
		}
		created, err := db.CreateExecution(ctx, ttx.Tx, db.ExecutionRow{
			ID: "exec_ak_legacy_" + u, TenantID: tenant, ProjectID: "proj_ak_" + u, TaskID: "task_ak_" + u,
			WorkerID: workerID, WorkerVersion: 2, Status: "running",
			HealthState: "healthy", StartedAt: &now,
		})
		if err != nil {
			t.Fatalf("create legacy execution: %v", err)
		}
		legacyID = created.ID
		if err := ttx.Commit(ctx); err != nil {
			t.Fatalf("commit legacy execution: %v", err)
		}
	}
	got, err = resolveAdapterKind(ctx, pool, tenant, legacyID)
	if err != nil {
		t.Fatalf("resolveAdapterKind(legacy): %v", err)
	}
	if got != "opencode" {
		t.Errorf("legacy (no adapter_id) execution kind = %q, want \"opencode\" (worker latest model_ref fallback)", got)
	}
}
