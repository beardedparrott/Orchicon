package db

// project_cascade_test.go — DeleteProject must leave NOTHING behind.
//
// WHY THIS EXISTS. DeleteProject cascaded the work hierarchy (step runs, runs, versions, workflows,
// dependencies, work items) but NOT the telemetry tables — and none of them has a foreign key to
// projects, so a missing delete was SILENT rather than an error. Measured on the live dev tenant after
// every project had been removed: 1264 orphaned worker_executions, 150 orphaned recovery_executions,
// 9959 orphaned usage_records and 132 orphaned recurring_run_history rows, every one of them carrying
// a project_id that no longer existed. Deleting a project in the GUI left all of it; the DB-backed
// tests, which create and drop projects constantly, left a large share of the rest.
//
// The test builds a project that owns one row of EVERY kind the cascade is responsible for, runs the
// real DeleteProject, and asserts the whole set is gone — so adding a table to the schema without
// adding it to the cascade shows up here rather than as orphans in production.

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

// cascadeTestPool opens the shared DB (skipping without a DSN) and applies migrations, so the schema
// the test exercises is the one the binary would build.
func cascadeTestPool(t *testing.T) *Pool {
	t.Helper()
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed project cascade test")
	}
	ctx := context.Background()
	pool, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// The whole point: after DeleteProject, a project that owned one row of every kind leaves nothing.
func TestDeleteProjectCascadesEveryOwnedTable(t *testing.T) {
	pool := cascadeTestPool(t)
	ctx := context.Background()
	tenant := "tnt_dev"
	proj := "zz_cascade_probe_" + NewID()[:10]
	wi := "zz_wi_" + NewID()[:10]
	wi2 := "zz_wi2_" + NewID()[:10]
	run := "zz_run_" + NewID()[:10]
	exec := "zz_exec_" + NewID()[:10]
	rec := "zz_rec_" + NewID()[:10]

	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	// Cleanup is belt-and-braces: if an assertion fails the rows must still not survive the test.
	t.Cleanup(func() {
		cctx := context.Background()
		cttx, cerr := pool.BeginTenantTx(cctx, tenant)
		if cerr != nil {
			return
		}
		defer cttx.Rollback(cctx)
		_ = DeleteProject(cctx, cttx.Tx, tenant, proj)
		_ = cttx.Commit(cctx)
	})

	// Seed one row of every kind the cascade owns. Written as raw SQL because the point is the
	// SCHEMA's breadth, not any one store function's API.
	seed := []struct {
		what string
		q    string
		args []any
	}{
		{"project", `INSERT INTO projects (id, tenant_id, name, slug, status, goals, version, project_dir)
			VALUES ($1,$2,'Cascade Probe',$3,'active','{}',1,'')`, []any{proj, tenant, "cascade-probe-" + proj}},
		{"work item", `INSERT INTO work_items (id, tenant_id, project_id, kind, title, status, version)
			VALUES ($1,$2,$3,'task','probe item','pending',1)`, []any{wi, tenant, proj}},
		{"second work item", `INSERT INTO work_items (id, tenant_id, project_id, kind, title, status, version)
			VALUES ($1,$2,$3,'task','probe item 2','pending',1)`, []any{wi2, tenant, proj}},
		// A dependency between TWO items: a self-edge is rejected by the DAG guard, which is the
		// schema protecting itself and is not what this test is about.
		{"work item dependency", `INSERT INTO work_item_dependencies (id, tenant_id, project_id, from_id, to_id, type)
			VALUES ($1,$2,$3,$4,$5,'depends_on')`, []any{"zz_dep_" + NewID()[:10], tenant, proj, wi, wi2}},
		{"workflow run", `INSERT INTO workflow_runs (id, tenant_id, project_id, workflow_id, workflow_version, status, version)
			VALUES ($1,$2,$3,'wf_x',1,'running',1)`, []any{run, tenant, proj}},
		{"workflow step run", `INSERT INTO workflow_step_runs (id, tenant_id, workflow_run_id, step_id, step_kind, status, version)
			VALUES ($1,$2,$3,'s1','task','running',1)`, []any{"zz_sr_" + NewID()[:10], tenant, run}},
		{"worker execution", `INSERT INTO worker_executions (id, tenant_id, project_id, task_id, worker_id, worker_version, status, adapter_id, version, workflow_run_id)
			VALUES ($1,$2,$3,$4,'w_1',1,'running','adp_orchicon_dev',1,$5)`, []any{exec, tenant, proj, wi, run}},
		{"execution session part", `INSERT INTO execution_session_parts (execution_id, tenant_id, seq, kind, payload, created_at)
			VALUES ($1,$2,1,'text','{}',now())`, []any{exec, tenant}},
		{"usage record", `INSERT INTO usage_records (id, tenant_id, project_id, execution_id, provider, model, total_tokens, cost_usd, occurred_at)
			VALUES ($1,$2,$3,$4,'deepseek','deepseek-flash',100,0.01,now())`, []any{"zz_usage_" + NewID()[:10], tenant, proj, exec}},
		{"recovery execution", `INSERT INTO recovery_executions (id, tenant_id, project_id, task_id, failed_execution_id, recovery_workflow_id, trigger_reason, level, status, current_step, resumption_path, continuation_plan_id, reviewer_worker_id, summary, version, triggered_at, strategy, max_retries, retry_delay_seconds)
			VALUES ($1,$2,$3,$4,$5,'','probe',1,'running','','summarize_resume','','','',1,now(),'summarize_restart',5,10)`, []any{rec, tenant, proj, wi, exec}},
		{"recovery step run", `INSERT INTO recovery_step_runs (id, tenant_id, recovery_id, step_id, status, version)
			VALUES ($1,$2,$3,'capture','succeeded',1)`, []any{"zz_rsr_" + NewID()[:10], tenant, rec}},
		{"continuation plan", `INSERT INTO continuation_plans (id, tenant_id, recovery_id, version)
			VALUES ($1,$2,$3,1)`, []any{"zz_plan_" + NewID()[:10], tenant, rec}},
		{"recurring run history", `INSERT INTO recurring_run_history (id, tenant_id, workflow_run_id, work_item_id, fire_at, status)
			VALUES ($1,$2,$3,$4,now(),'succeeded')`, []any{"zz_hist_" + NewID()[:10], tenant, run, wi}},
	}
	for _, s := range seed {
		if _, err := ttx.Exec(ctx, s.q, s.args...); err != nil {
			_ = ttx.Rollback(ctx)
			t.Fatalf("seed %s: %v", s.what, err)
		}
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}

	// Sanity: the fixture really did create the rows, or the assertions below would pass vacuously.
	before := cascadeCounts(t, pool, tenant, proj, run, wi, exec, rec)
	for what, n := range before {
		if n == 0 {
			t.Fatalf("fixture %s was not created — the test would pass vacuously", what)
		}
	}

	// THE CALL UNDER TEST.
	dttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin delete tx: %v", err)
	}
	if err := DeleteProject(ctx, dttx.Tx, tenant, proj); err != nil {
		_ = dttx.Rollback(ctx)
		t.Fatalf("DeleteProject: %v", err)
	}
	if err := dttx.Commit(ctx); err != nil {
		t.Fatalf("commit delete: %v", err)
	}

	after := cascadeCounts(t, pool, tenant, proj, run, wi, exec, rec)
	for what, n := range after {
		if n != 0 {
			t.Errorf("%s SURVIVED DeleteProject (%d row(s)) — the cascade does not cover it, and with no "+
				"foreign key to projects nothing else will ever clean it up", what, n)
		}
	}
}

// cascadeCounts reads how many rows each table still holds for the probe.
func cascadeCounts(t *testing.T, pool *Pool, tenant, proj, run, wi, exec, rec string) map[string]int {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin read tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	queries := map[string]struct {
		q    string
		args []any
	}{
		"the project":             {`SELECT count(*) FROM projects WHERE id=$1 AND tenant_id=$2`, []any{proj, tenant}},
		"work items":              {`SELECT count(*) FROM work_items WHERE project_id=$1 AND tenant_id=$2`, []any{proj, tenant}},
		"work item dependencies":  {`SELECT count(*) FROM work_item_dependencies WHERE project_id=$1 AND tenant_id=$2`, []any{proj, tenant}},
		"workflow runs":           {`SELECT count(*) FROM workflow_runs WHERE project_id=$1 AND tenant_id=$2`, []any{proj, tenant}},
		"workflow step runs":      {`SELECT count(*) FROM workflow_step_runs WHERE workflow_run_id=$1 AND tenant_id=$2`, []any{run, tenant}},
		"worker executions":       {`SELECT count(*) FROM worker_executions WHERE project_id=$1 AND tenant_id=$2`, []any{proj, tenant}},
		"execution session parts": {`SELECT count(*) FROM execution_session_parts WHERE execution_id=$1 AND tenant_id=$2`, []any{exec, tenant}},
		"usage records":           {`SELECT count(*) FROM usage_records WHERE project_id=$1 AND tenant_id=$2`, []any{proj, tenant}},
		"recovery executions":     {`SELECT count(*) FROM recovery_executions WHERE project_id=$1 AND tenant_id=$2`, []any{proj, tenant}},
		"recovery step runs":      {`SELECT count(*) FROM recovery_step_runs WHERE recovery_id=$1 AND tenant_id=$2`, []any{rec, tenant}},
		"continuation plans":      {`SELECT count(*) FROM continuation_plans WHERE recovery_id=$1 AND tenant_id=$2`, []any{rec, tenant}},
		"recurring run history":   {`SELECT count(*) FROM recurring_run_history WHERE workflow_run_id=$1 AND tenant_id=$2`, []any{run, tenant}},
	}
	out := make(map[string]int, len(queries))
	for what, q := range queries {
		var n int
		if err := ttx.QueryRow(ctx, q.q, q.args...).Scan(&n); err != nil && err != pgx.ErrNoRows {
			t.Fatalf("count %s: %v", what, err)
		}
		out[what] = n
	}
	return out
}

// A delete of something already gone is not an error.
//
// The GUI's delete and a test's cleanup can both run, and the contract DeleteProject owes is "the
// project and its data are gone" — which is true whether this call or an earlier one removed it.
func TestDeleteProjectIsIdempotent(t *testing.T) {
	pool := cascadeTestPool(t)
	ctx := context.Background()
	tenant := "tnt_dev"
	proj := "zz_idem_probe_" + NewID()[:10]

	// CLEANUP IS NOT REDUNDANT WITH THE DELETES BELOW, and its absence was a real leak: this test
	// asserts that DeleteProject is harmless when called twice, so a FAILURE anywhere before the
	// second call — or a failure of the second call itself — left the probe project behind in the
	// shared tenant. It was the only test in this file without a t.Cleanup, and it did leak: two
	// probes survived in the dev tenant from a single run while this was being written.
	t.Cleanup(func() {
		cctx := context.Background()
		if err := DeleteProjectByID(cctx, pool, tenant, proj); err != nil {
			t.Logf("cleanup: delete idempotency probe %s: %v", proj, err)
		}
	})

	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if _, err := ttx.Exec(ctx, `INSERT INTO projects (id, tenant_id, name, slug, status, goals, version, project_dir)
		VALUES ($1,$2,'Idem Probe',$3,'active','{}',1,'')`, proj, tenant, "idem-"+proj); err != nil {
		_ = ttx.Rollback(ctx)
		t.Fatalf("seed project: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	for i := 1; i <= 2; i++ {
		dttx, err := pool.BeginTenantTx(ctx, tenant)
		if err != nil {
			t.Fatalf("begin delete tx %d: %v", i, err)
		}
		if err := DeleteProject(ctx, dttx.Tx, tenant, proj); err != nil {
			_ = dttx.Rollback(ctx)
			t.Fatalf("DeleteProject call %d returned %v — a repeated delete must be harmless", i, err)
		}
		if err := dttx.Commit(ctx); err != nil {
			t.Fatalf("commit delete %d: %v", i, err)
		}
	}
}
