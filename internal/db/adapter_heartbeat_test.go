package db

// adapter_heartbeat_test.go — the regression guard for a defect that reached production data.
//
// CreateAdapter's INSERT dropped `last_heartbeat_at`: the column list omitted it and the field was
// only READ BACK, so a caller that passed a heartbeat silently got a row with NULL. The dispatcher's
// candidate query treats NULL as FRESH (`last_heartbeat_at IS NULL OR last_heartbeat_at >= now() -
// ttl`), so every test fixture that created an adapter with `LastHeartbeatAt: &now` in fact created
// one that could NEVER EXPIRE. In the live dev tenant that accumulated 156 phantom adapters with
// endpoint `localhost:0`, and because selectAdapter breaks ties on the adapter ID, they entered the
// same candidate pool as the real adapter — 993 real executions were dispatched onto them and came
// back failed / failed_to_start.
//
// The INSERT is the contract; this test asserts it directly, so the next edit to the column list
// cannot quietly drop a caller's heartbeat again.
//
// These run only with ORCHICON_TEST_DSN set (the package's DB-backed convention), and they create
// and DELETE their own row so the shared instance is left exactly as found.

import (
	"context"
	"os"
	"testing"
	"time"
)

// The heartbeat a caller supplies is the heartbeat that is STORED.
func TestCreateAdapterPersistsTheHeartbeat(t *testing.T) {
	pool := adapterTestPool(t)
	ctx := context.Background()
	tenant := adapterTestTenant
	now := time.Now().UTC().Truncate(time.Second)

	id := NewID()
	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	created, err := CreateAdapter(ctx, ttx.Tx, AdapterRow{
		ID: id, TenantID: tenant,
		Kind: "opencode", Version: "test", Endpoint: "localhost:0",
		Capabilities: []byte("{}"), Status: "ready",
		MaxConcurrentExecutions: 8, LastHeartbeatAt: &now,
	})
	if err != nil {
		_ = ttx.Rollback(ctx)
		t.Fatalf("create adapter: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	t.Cleanup(func() {
		cctx := context.Background()
		cttx, cerr := pool.BeginTenantTx(cctx, tenant)
		if cerr != nil {
			t.Logf("cleanup: begin: %v", cerr)
			return
		}
		defer cttx.Rollback(cctx)
		_ = DeleteAdapter(cctx, cttx.Tx, tenant, id)
		_ = cttx.Commit(cctx)
	})

	// The RETURNED row carries it…
	if created.LastHeartbeatAt == nil {
		t.Fatal("the returned row lost the heartbeat the caller passed")
	}
	// …and so does a fresh READ, which is what actually matters: the caller's field is only a
	// convenience, the STORED value is what the dispatcher's query sees.
	ttx2, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin read tx: %v", err)
	}
	defer ttx2.Rollback(ctx)
	got, err := GetAdapter(ctx, ttx2.Tx, tenant, id)
	if err != nil {
		t.Fatalf("get adapter: %v", err)
	}
	if got.LastHeartbeatAt == nil {
		t.Fatal("the STORED row has a NULL heartbeat — a caller's heartbeat was dropped, which makes " +
			"the row a permanent dispatch candidate (the query treats NULL as fresh)")
	}
	if d := got.LastHeartbeatAt.Sub(now); d > time.Second || d < -time.Second {
		t.Errorf("stored heartbeat = %v, want ~%v", got.LastHeartbeatAt, now)
	}
}

// An OMITTED heartbeat stays NULL, which is the meaning the dispatcher's query relies on (it accepts
// NULL as fresh). This pins the other half of the contract: persisting a supplied value must not
// invent one when none was given.
func TestCreateAdapterLeavesAnOmittedHeartbeatNull(t *testing.T) {
	pool := adapterTestPool(t)
	ctx := context.Background()
	tenant := adapterTestTenant

	id := NewID()
	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if _, err := CreateAdapter(ctx, ttx.Tx, AdapterRow{
		ID: id, TenantID: tenant,
		Kind: "opencode", Version: "test", Endpoint: "localhost:0",
		Capabilities: []byte("{}"), Status: "ready", MaxConcurrentExecutions: 1,
	}); err != nil {
		_ = ttx.Rollback(ctx)
		t.Fatalf("create adapter: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	t.Cleanup(func() {
		cctx := context.Background()
		cttx, cerr := pool.BeginTenantTx(cctx, tenant)
		if cerr != nil {
			return
		}
		defer cttx.Rollback(cctx)
		_ = DeleteAdapter(cctx, cttx.Tx, tenant, id)
		_ = cttx.Commit(cctx)
	})

	ttx2, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin read tx: %v", err)
	}
	defer ttx2.Rollback(ctx)
	got, err := GetAdapter(ctx, ttx2.Tx, tenant, id)
	if err != nil {
		t.Fatalf("get adapter: %v", err)
	}
	if got.LastHeartbeatAt != nil {
		t.Errorf("an omitted heartbeat became %v — it must stay NULL", got.LastHeartbeatAt)
	}
}

// DeleteAdapter removes the row, and is idempotent (which is what a t.Cleanup needs — the row may
// already be gone if a test failed partway).
func TestDeleteAdapterRemovesAndIsIdempotent(t *testing.T) {
	pool := adapterTestPool(t)
	ctx := context.Background()
	tenant := adapterTestTenant

	id := NewID()
	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if _, err := CreateAdapter(ctx, ttx.Tx, AdapterRow{
		ID: id, TenantID: tenant,
		Kind: "opencode", Version: "test", Endpoint: "localhost:0",
		Capabilities: []byte("{}"), Status: "ready", MaxConcurrentExecutions: 1,
	}); err != nil {
		_ = ttx.Rollback(ctx)
		t.Fatalf("create adapter: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	ttx2, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin delete tx: %v", err)
	}
	if err := DeleteAdapter(ctx, ttx2.Tx, tenant, id); err != nil {
		t.Fatalf("delete adapter: %v", err)
	}
	if err := ttx2.Commit(ctx); err != nil {
		t.Fatalf("commit delete: %v", err)
	}

	ttx3, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin read tx: %v", err)
	}
	defer ttx3.Rollback(ctx)
	if _, err := GetAdapter(ctx, ttx3.Tx, tenant, id); err == nil {
		t.Error("the adapter is still readable after DeleteAdapter")
	}
	// Idempotent: deleting what is already gone is success, not an error.
	ttx4, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin second-delete tx: %v", err)
	}
	defer ttx4.Rollback(ctx)
	if err := DeleteAdapter(ctx, ttx4.Tx, tenant, id); err != nil {
		t.Errorf("a second DeleteAdapter returned %v — it must be idempotent", err)
	}
}

// --- the DB-backed test seam ----------------------------------------------------------------

const adapterTestTenant = "tnt_dev"

// adapterTestPool mirrors the package's other DB-backed helpers: skipped without a DSN, so the
// default `go test ./...` (no DSN) does not touch any instance.
func adapterTestPool(t *testing.T) *Pool {
	t.Helper()
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed adapter tests")
	}
	ctx := context.Background()
	pool, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// --- the batched usage enrichment (the executions page-load fix) -------------------------------

// SumUsageForExecutions must return exactly what the per-row SumUsageForExecution returns, for every
// id — the batched form replaced a loop that issued one query per execution, and a batching change
// that silently lost or mis-attributed a total would be worse than the slowness it fixed.
func TestSumUsageForExecutionsMatchesThePerRowForm(t *testing.T) {
	pool := adapterTestPool(t)
	ctx := context.Background()
	tenant := adapterTestTenant

	// A page of real ids, taken from the executions that actually have usage.
	ttx0, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	var ids []string
	rows, err := ttx0.Tx.Query(ctx, `SELECT DISTINCT execution_id FROM usage_records
		WHERE tenant_id = $1 AND execution_id <> '' ORDER BY execution_id LIMIT 25`, tenant)
	if err != nil {
		_ = ttx0.Rollback(ctx)
		t.Fatalf("sample ids: %v", err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			_ = ttx0.Rollback(ctx)
			t.Fatalf("scan id: %v", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	_ = ttx0.Rollback(ctx)
	if len(ids) == 0 {
		t.Skip("no usage records to compare against in this tenant")
	}

	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)

	batched, err := SumUsageForExecutions(ctx, ttx.Tx, tenant, ids)
	if err != nil {
		t.Fatalf("SumUsageForExecutions: %v", err)
	}
	for _, id := range ids {
		wantTokens, wantCost, err := SumUsageForExecution(ctx, ttx.Tx, tenant, id)
		if err != nil {
			t.Fatalf("SumUsageForExecution(%s): %v", id, err)
		}
		got, ok := batched[id]
		if !ok {
			t.Errorf("the batched result is MISSING %s — a row would render without its totals", id)
			continue
		}
		if got.Tokens != wantTokens {
			t.Errorf("%s tokens = %d, want %d (per-row form)", id, got.Tokens, wantTokens)
		}
		if got.CostUSD != wantCost {
			t.Errorf("%s cost = %v, want %v (per-row form)", id, got.CostUSD, wantCost)
		}
	}
	// An id with no usage is simply absent, not a zero entry — the caller distinguishes "no records"
	// from "zero cost", and a zero entry would claim the latter.
	if _, ok := batched["definitely-not-an-execution"]; ok {
		t.Error("a nonexistent execution produced an entry — absence must mean no records")
	}
	// Empty input is not an error and does no work.
	if got, err := SumUsageForExecutions(ctx, ttx.Tx, tenant, nil); err != nil || len(got) != 0 {
		t.Errorf("empty input = (%v, %v), want an empty map and no error", got, err)
	}
}
