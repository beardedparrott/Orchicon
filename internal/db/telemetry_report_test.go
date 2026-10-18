package db

// telemetry_report_test.go — the duration / failure aggregation behind the
// native Telemetry page's Reports view.
//
// Two things are pinned here because both were cheap to get wrong:
//
//  1. durations use EXTRACT(EPOCH FROM (ended_at - started_at)) * 1000 —
//     EXTRACT(MILLISECOND …) returns only the milliseconds FIELD of the
//     interval, so a 90 s execution reported 30000 ms.
//  2. an execution that has not ended (ended_at IS NULL) counts toward the
//     execution total but contributes no duration and is never "failed".
//
// Guarded by ORCHICON_TEST_DSN like the rest of the package's DB tests; it
// deletes the rows it inserts.

import (
	"context"
	"os"
	"testing"
	"time"
)

func telemetryReportTestPool(t *testing.T) *Pool {
	t.Helper()
	if os.Getenv("ORCHICON_TEST_DSN") == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed telemetry report test")
	}
	pool, err := Open(context.Background(), os.Getenv("ORCHICON_TEST_DSN"))
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestGetProjectDurationStatsCountsFailuresAndMillis(t *testing.T) {
	pool := telemetryReportTestPool(t)
	ctx := context.Background()
	const tenantID = "tnt_report_test"
	const projectID = "prj_report_test"

	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)

	base := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	rows := []struct {
		id      string
		status  string
		started time.Time
		ended   *time.Time
	}{
		{"exe_report_ok", "succeeded", base, durPtr(base, 90*time.Second)},
		{"exe_report_fail", "failed", base.Add(time.Minute), durPtr(base.Add(time.Minute), 30*time.Second)},
		{"exe_report_running", "running", base.Add(2 * time.Minute), nil},
	}
	for _, r := range rows {
		if _, err := ttx.Tx.Exec(ctx, `INSERT INTO worker_executions
			(id, tenant_id, project_id, task_id, worker_id, worker_version, status, started_at, ended_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			r.id, tenantID, projectID, "wi_report_test", "wrk_report_test", 1, r.status, r.started, r.ended,
		); err != nil {
			t.Fatalf("insert execution %s: %v", r.id, err)
		}
	}
	// The whole test runs inside one rolled-back tenant tx, so the shared
	// instance is left exactly as found (no cleanup DELETE needed).
	stats, err := GetProjectDurationStats(ctx, ttx.Tx, tenantID, projectID, "", "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("GetProjectDurationStats: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("want 1 group, got %d", len(stats))
	}
	got := stats[0]
	if got.Executions != 3 {
		t.Errorf("executions: want 3, got %d", got.Executions)
	}
	if got.Failed != 1 {
		t.Errorf("failed: want 1 (running is not failed), got %d", got.Failed)
	}
	// 90 s + 30 s = 120000 ms. The EXTRACT(MILLISECOND …) bug would have
	// produced 0 + 0 here because both intervals have a 0 ms field.
	if got.DurationMsTotal != 120000 {
		t.Errorf("duration total: want 120000 ms, got %d", got.DurationMsTotal)
	}
}

func durPtr(start time.Time, d time.Duration) *time.Time {
	end := start.Add(d)
	return &end
}

// TestWorkflowReportScopesBothHalves guards the workflow-grouped report's
// filters: the project / work-item / execution scope must narrow the COST
// aggregate exactly like it narrows the duration/failure aggregate beside it.
//
// Before this, db.GetWorkflowAggregateCosts took no scope at all while
// GetWorkflowDurationStats took the project, so one report row could carry
// project-scoped run times next to tenant-wide spend. A filter that applies
// to half a row is worse than no filter: the numbers look authoritative and
// are not.
func TestWorkflowReportScopesBothHalves(t *testing.T) {
	pool := telemetryReportTestPool(t)
	ctx := context.Background()
	const tenantID = "tnt_report_wf_scope"

	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	// Rolled back, so the shared instance is left exactly as found.
	defer ttx.Rollback(ctx)

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := ttx.Tx.Exec(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	started := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)
	ended := started.Add(5 * time.Minute) // 300000 ms per run
	seeds := []struct {
		proj, wf, item, run, exe, rec string
		cost                          float64
	}{
		{"prj_wf_scope_a", "wf_scope_a", "item_wf_a", "run_wf_a", "exe_wf_a", "rec_wf_a", 0.10},
		{"prj_wf_scope_b", "wf_scope_b", "item_wf_b", "run_wf_b", "exe_wf_b", "rec_wf_b", 0.05},
	}
	for _, s := range seeds {
		exec(`INSERT INTO projects (id, tenant_id, name, slug, status, version, created_at, updated_at)
			VALUES ($1,$2,$1,$1,'active',1,now(),now())`, s.proj, tenantID)
		exec(`INSERT INTO workflows (id, tenant_id, project_id, name, current_version, status, version, created_at, updated_at, type)
			VALUES ($1,$2,$3,'Workflow',1,'published',1,now(),now(),'template')`, s.wf, tenantID, s.proj)
		exec(`INSERT INTO work_items (id, tenant_id, project_id, kind, title, status, version, created_at, updated_at)
			VALUES ($1,$2,$3,'task','Item','succeeded',1,now(),now())`, s.item, tenantID, s.proj)
		exec(`INSERT INTO workflow_runs (id, tenant_id, workflow_id, workflow_version, project_id, status, version, created_at, updated_at, work_item_id, started_at, ended_at)
			VALUES ($1,$2,$3,1,$4,'completed',1,now(),now(),$5,$6,$7)`,
			s.run, tenantID, s.wf, s.proj, s.item, started, ended)
		exec(`INSERT INTO worker_executions (id, tenant_id, project_id, task_id, worker_id, worker_version, status, version, created_at, updated_at, workflow_run_id, started_at, ended_at)
			VALUES ($1,$2,$3,$4,'wrk_scope',1,'succeeded',1,now(),now(),$5,$6,$7)`,
			s.exe, tenantID, s.proj, s.item, s.run, started, ended)
		exec(`INSERT INTO usage_records (id, tenant_id, project_id, task_id, execution_id, worker_id, provider, model, prompt_tokens, completion_tokens, total_tokens, cost_usd, occurred_at, created_at, workflow_run_id)
			VALUES ($1,$2,$3,$4,$5,'wrk_scope','anthropic','claude',100,50,150,$6,now(),now(),$7)`,
			s.rec, tenantID, s.proj, s.item, s.exe, s.cost, s.run)
	}

	// Costs: unscoped sees both workflows; each scope narrows to exactly one.
	cases := []struct {
		name            string
		proj, item, exe string
		wantWF          string
		wantCost        float64
	}{
		{"unscoped", "", "", "", "", 0},
		{"project", "prj_wf_scope_a", "", "", "wf_scope_a", 0.10},
		{"work item", "", "item_wf_b", "", "wf_scope_b", 0.05},
		{"execution", "", "", "exe_wf_a", "wf_scope_a", 0.10},
	}
	for _, c := range cases {
		t.Run("cost "+c.name, func(t *testing.T) {
			rows, err := GetWorkflowAggregateCosts(ctx, ttx.Tx, tenantID, c.proj, c.item, c.exe, time.Time{}, time.Time{})
			if err != nil {
				t.Fatalf("GetWorkflowAggregateCosts: %v", err)
			}
			if c.wantWF == "" {
				if len(rows) != 2 {
					t.Fatalf("unscoped: want both workflows, got %d rows", len(rows))
				}
				return
			}
			if len(rows) != 1 {
				t.Fatalf("want 1 row for %s, got %d", c.wantWF, len(rows))
			}
			if rows[0].WorkflowID != c.wantWF {
				t.Errorf("workflow: want %s, got %s", c.wantWF, rows[0].WorkflowID)
			}
			if rows[0].TotalCostUSD != c.wantCost {
				t.Errorf("cost: want %v, got %v", c.wantCost, rows[0].TotalCostUSD)
			}
		})
	}

	// Durations answer to the same scopes — including the execution filter,
	// which has to travel run → worker_executions.
	for _, c := range cases {
		if c.wantWF == "" {
			continue
		}
		t.Run("duration "+c.name, func(t *testing.T) {
			stats, err := GetWorkflowDurationStats(ctx, ttx.Tx, tenantID, c.proj, c.item, c.exe, time.Time{}, time.Time{})
			if err != nil {
				t.Fatalf("GetWorkflowDurationStats: %v", err)
			}
			if len(stats) != 1 {
				t.Fatalf("want 1 group, got %d", len(stats))
			}
			if stats[0].GroupKey != c.wantWF {
				t.Errorf("workflow: want %s, got %s", c.wantWF, stats[0].GroupKey)
			}
			if stats[0].DurationMsTotal != 300000 {
				t.Errorf("duration: want 300000 ms, got %d", stats[0].DurationMsTotal)
			}
		})
	}
}
