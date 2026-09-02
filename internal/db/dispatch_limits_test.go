package db_test

// DB-backed tests for the dispatch-limits data-access surface
// (per-project/tenant max-concurrent-runs): the tenant/project settings
// round-trip, the min(tenant, project) effective-limit resolution, and the
// atomic in-place admission counter. Guarded by ORCHICON_TEST_DSN like the
// other DB-backed tests:
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/db/ -run TestDispatchLimits -v

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/migrate"
)

func strp(s string) *string { return &s }

func openTestPool(t *testing.T) *db.Pool {
	t.Helper()
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed dispatch-limits test")
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

// TestUpdateTenantSettingsNilBudgetOverrides verifies the upsert tolerates a
// caller that leaves DefaultBudgetOverrides unset (nil). The column is jsonb
// NOT NULL, and a literal NULL in the INSERT branch violates the constraint
// BEFORE the ON CONFLICT arbiter engages — so the fix coerces nil/empty to
// '{}'. Regression for the MCP update_settings tool (which builds the row
// without the budget field): every write returned "no row returned".
func TestUpdateTenantSettingsNilBudgetOverrides(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	const tenant = "tnt_dev"

	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)

	// Nil budget overrides (the former bug) + a stall change.
	row, err := db.UpdateTenantSettings(ctx, ttx.Tx, tenant, db.TenantSettingsRow{
		StallNoProgressWindowSeconds: 600,
	})
	if err != nil {
		t.Fatalf("update with nil budget overrides failed: %v", err)
	}
	if row.StallNoProgressWindowSeconds != 600 {
		t.Fatalf("stall_no_progress = %d, want 600", row.StallNoProgressWindowSeconds)
	}
	// The budget column must still hold valid JSON ('{}') — never NULL.
	if len(row.DefaultBudgetOverrides) == 0 {
		t.Fatalf("DefaultBudgetOverrides came back empty after nil-input update")
	}
	// Round-trip: GetTenantSettings must return the persisted stall value.
	got, err := db.GetTenantSettings(ctx, ttx.Tx, tenant)
	if err != nil {
		t.Fatalf("get tenant settings: %v", err)
	}
	if got.StallNoProgressWindowSeconds != 600 {
		t.Fatalf("reloaded stall_no_progress = %d, want 600", got.StallNoProgressWindowSeconds)
	}
	// Reset the field so the shared tnt_dev row stays clean.
	if _, err := db.UpdateTenantSettings(ctx, ttx.Tx, tenant, db.TenantSettingsRow{
		StallNoProgressWindowSeconds: 0,
	}); err != nil {
		t.Fatalf("reset tenant settings: %v", err)
	}
}

// TestDispatchLimitSettingsRoundTrip verifies tenant_settings and projects
// persist max_concurrent_runs and GetDispatchLimitValues reads both back.
func TestDispatchLimitSettingsRoundTrip(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	const tenant = "tnt_dev"

	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)

	// Tenant settings: set a ceiling, read it back. The budget overrides
	// column is jsonb NOT NULL — the settings service always sends "{}".
	row, err := db.UpdateTenantSettings(ctx, ttx.Tx, tenant, db.TenantSettingsRow{
		MaxConcurrentRuns:      4,
		MaxConcurrentRunsSet:   true,
		DefaultBudgetOverrides: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("update tenant settings: %v", err)
	}
	if row.MaxConcurrentRuns != 4 {
		t.Fatalf("tenant max_concurrent_runs = %d, want 4", row.MaxConcurrentRuns)
	}
	got, err := db.GetTenantSettings(ctx, ttx.Tx, tenant)
	if err != nil {
		t.Fatalf("get tenant settings: %v", err)
	}
	if got.MaxConcurrentRuns != 4 {
		t.Fatalf("reloaded tenant max_concurrent_runs = %d, want 4", got.MaxConcurrentRuns)
	}
	// Reset to 0 (no cap) so we leave the shared tnt_dev clean.
	if _, err := db.UpdateTenantSettings(ctx, ttx.Tx, tenant, db.TenantSettingsRow{
		MaxConcurrentRuns:      0,
		MaxConcurrentRunsSet:   true,
		DefaultBudgetOverrides: []byte("{}"),
	}); err != nil {
		t.Fatalf("reset tenant settings: %v", err)
	}

	// Project: set 2, effective limit = min(0 tenant, 2 project) = 2.
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: tenant,
		Name: "Dispatch Limits DB", Slug: "dl-db-" + db.NewID()[:8],
		Status: "drafting", Goals: []byte("{}"),
		MaxConcurrentRuns: 2,
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	// Leave the shared tenant clean even if the test aborts mid-way.
	t.Cleanup(func() {
		ctx := context.Background()
		ttx, err := pool.BeginTenantTx(ctx, tenant)
		if err != nil {
			return
		}
		defer ttx.Rollback(ctx)
		_ = db.DeleteProject(ctx, ttx.Tx, tenant, proj.ID)
		_, _ = db.UpdateTenantSettings(ctx, ttx.Tx, tenant, db.TenantSettingsRow{
			MaxConcurrentRuns:      0,
			MaxConcurrentRunsSet:   true,
			DefaultBudgetOverrides: []byte("{}"),
		})
		_ = ttx.Commit(ctx)
	})
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	ttx2, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx2.Rollback(ctx)
	tl, pl, err := db.GetDispatchLimitValues(ctx, ttx2.Tx, tenant, proj.ID)
	if err != nil {
		t.Fatalf("get dispatch limit values: %v", err)
	}
	if tl != 0 {
		t.Fatalf("tenant limit = %d, want 0 (reset)", tl)
	}
	if pl != 2 {
		t.Fatalf("project limit = %d, want 2", pl)
	}
	eff, err := db.GetEffectiveDispatchLimit(ctx, ttx2.Tx, tenant, proj.ID)
	if err != nil {
		t.Fatalf("get effective dispatch limit: %v", err)
	}
	if eff != 2 {
		t.Fatalf("effective limit = %d, want 2", eff)
	}
	if inPlace := db.InPlaceLimit(tl, pl); inPlace != 2 {
		t.Fatalf("in-place limit = %d, want 2 (project opted in at 2)", inPlace)
	}
	// Project opted out (0): effective follows tenant (0 = no cap), in-place
	// default serializes at 1.
	if eff := db.EffectiveDispatchLimit(0, 0); eff != 0 {
		t.Fatalf("effective limit (0,0) = %d, want 0", eff)
	}
	if inPlace := db.InPlaceLimit(0, 0); inPlace != 1 {
		t.Fatalf("in-place limit (0,0) = %d, want 1", inPlace)
	}
}

// TestAdmitInPlaceRun serializes admissions on the project-row lock: the
// first admitted run claims the single slot, the second is denied, and a
// third is admitted after the first reaches a terminal state.
func TestAdmitInPlaceRun(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	const tenant = "tnt_dev"

	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: tenant,
		Name: "Admit In-Place DB", Slug: "admit-inplace-" + db.NewID()[:8],
		Status: "drafting", Goals: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		ttx, err := pool.BeginTenantTx(ctx, tenant)
		if err != nil {
			return
		}
		defer ttx.Rollback(ctx)
		_ = db.DeleteProject(ctx, ttx.Tx, tenant, proj.ID)
		_ = ttx.Commit(ctx)
	})
	mkRun := func() db.WorkflowRunRow {
		run, err := db.CreateWorkflowRun(ctx, ttx.Tx, db.WorkflowRunRow{
			ID: db.NewID(), TenantID: tenant,
			WorkflowID: "wf-admit", WorkflowVersion: 1,
			ProjectID: proj.ID, Status: "running", RunContext: []byte("{}"),
		})
		if err != nil {
			t.Fatalf("create run: %v", err)
		}
		return run
	}
	run1 := mkRun()
	run2 := mkRun()
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	admit := func(run db.WorkflowRunRow) bool {
		ttx, err := pool.BeginTenantTx(ctx, tenant)
		if err != nil {
			t.Fatal(err)
		}
		defer ttx.Rollback(ctx)
		// Admit the run's in-place token atomically (project-row lock +
		// count within this tx) and, when admitted, mark it 'skipped'.
		admitted, err := db.AdmitInPlaceRun(ctx, ttx.Tx, tenant, proj.ID, 1)
		if err != nil {
			t.Fatalf("admit run %s: %v", run.ID, err)
		}
		if admitted {
			if _, err := db.UpdateWorkflowRun(ctx, ttx.Tx, tenant, run.ID, run.Version, db.UpdateWorkflowRunFields{
				WorktreeStatus: strp("skipped"),
			}); err != nil {
				t.Fatalf("mark run %s skipped: %v", run.ID, err)
			}
		}
		if err := ttx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return admitted
	}

	if !admit(run1) {
		t.Fatal("run1 not admitted (first in-place slot should be free)")
	}
	if admit(run2) {
		t.Fatal("run2 admitted while run1 holds the only in-place slot")
	}

	// Terminalize run1: the slot frees. Re-fetch its current version (the
	// admit above bumped it) for the optimistic-concurrency update.
	ttx, err = pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	cur, err := db.GetWorkflowRun(ctx, ttx.Tx, tenant, run1.ID)
	if err != nil {
		t.Fatalf("re-fetch run1: %v", err)
	}
	if _, err := db.UpdateWorkflowRun(ctx, ttx.Tx, tenant, run1.ID, cur.Version, db.UpdateWorkflowRunFields{
		Status: strp("completed"),
	}); err != nil {
		t.Fatalf("terminalize run1: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if !admit(run2) {
		t.Fatal("run2 not admitted after run1 terminalized")
	}
}

// TestCompactionMemoryPolicyDBRoundTrip verifies the typed compaction +
// memory policy columns (D4) persist through UpdateTenantSettings and come
// back through GetTenantSettings + BudgetJSON — the full F1 wiring. The
// BudgetJSON transport carries the context_compaction/memory keys the
// native session and worker overrides parse, so this round-trip is the
// proof that per-tenant policy is reachable at dispatch.
func TestCompactionMemoryPolicyDBRoundTrip(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	const tenant = "tnt_ccmp"

	// Write an explicit policy (disable compaction, keep memory with 3
	// digest entries) via the same row shape the Settings API sends.
	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	in := db.TenantSettingsRow{
		DefaultBudgetOverrides:        []byte("{}"),
		ContextCompactionEnabled:      false,
		ContextCompactionPressureFrac: 0.9,
		ContextRecentTurns:            4,
		MemoryEnabled:                 true,
		MemoryDigestEntries:           3,
	}
	row, err := db.UpdateTenantSettings(ctx, ttx.Tx, tenant, in)
	if err != nil {
		t.Fatalf("update tenant settings: %v", err)
	}
	if row.ContextCompactionEnabled || row.ContextCompactionPressureFrac != 0.9 || row.ContextRecentTurns != 4 {
		t.Fatalf("policy not written: enabled=%v frac=%v turns=%v",
			row.ContextCompactionEnabled, row.ContextCompactionPressureFrac, row.ContextRecentTurns)
	}
	if !row.MemoryEnabled || row.MemoryDigestEntries != 3 {
		t.Fatalf("memory not written: enabled=%v entries=%v", row.MemoryEnabled, row.MemoryDigestEntries)
	}

	// Read back through GetTenantSettings (fresh row) — the SELECT + scan
	// must carry the columns.
	got, err := db.GetTenantSettings(ctx, ttx.Tx, tenant)
	if err != nil {
		t.Fatalf("get tenant settings: %v", err)
	}
	if got.ContextCompactionEnabled || got.ContextCompactionPressureFrac != 0.9 || got.ContextRecentTurns != 4 {
		t.Fatalf("reloaded policy mismatch: enabled=%v frac=%v turns=%v",
			got.ContextCompactionEnabled, got.ContextCompactionPressureFrac, got.ContextRecentTurns)
	}
	if !got.MemoryEnabled || got.MemoryDigestEntries != 3 {
		t.Fatalf("reloaded memory mismatch: enabled=%v entries=%v", got.MemoryEnabled, got.MemoryDigestEntries)
	}
	// BudgetJSON (the dispatch transport) must carry the keys.
	var bj map[string]any
	if err := json.Unmarshal(got.BudgetJSON(), &bj); err != nil {
		t.Fatalf("BudgetJSON invalid: %v", err)
	}
	cc, _ := bj["context_compaction"].(map[string]any)
	mem, _ := bj["memory"].(map[string]any)
	if cc == nil || cc["enabled"] != false || cc["pressure_frac"] != 0.9 || cc["recent_turns"] != float64(4) {
		t.Fatalf("BudgetJSON context_compaction = %v", bj["context_compaction"])
	}
	if mem == nil || mem["enabled"] != true || mem["digest_entries"] != float64(3) {
		t.Fatalf("BudgetJSON memory = %v", bj["memory"])
	}

	// A partial update (only a stall field) must NOT clobber the stored
	// policy (the policyIsSet guard overlays the current row).
	part, err := db.UpdateTenantSettings(ctx, ttx.Tx, tenant, db.TenantSettingsRow{
		StallNoProgressWindowSeconds: 300,
	})
	if err != nil {
		t.Fatalf("partial update: %v", err)
	}
	// Preserved: the partial update must leave the policy exactly as
	// written (enabled=false, frac=0.9, turns=4, memory=true, entries=3).
	if part.ContextCompactionEnabled != false || part.ContextCompactionPressureFrac != 0.9 || part.ContextRecentTurns != 4 {
		t.Fatalf("partial update clobbered policy: enabled=%v frac=%v turns=%v",
			part.ContextCompactionEnabled, part.ContextCompactionPressureFrac, part.ContextRecentTurns)
	}
	if !part.MemoryEnabled || part.MemoryDigestEntries != 3 {
		t.Fatalf("partial update clobbered memory: enabled=%v entries=%v", part.MemoryEnabled, part.MemoryDigestEntries)
	}

	// Cleanup: drop the scratch tenant row so the shared dev DB stays tidy.
	if _, err := db.DeleteTenantSettings(ctx, ttx.Tx, tenant); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}
