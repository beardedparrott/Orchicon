package db_test

// telemetry_outcomes_test.go — the outcome counters behind the Reports view's
// "what happened to the work" roll-up.
//
// Three things are pinned here, and each one is a mistake that would have
// shipped silently because the numbers still look plausible when they are
// wrong:
//
//  1. workflow_runs' success state is 'completed', NOT 'succeeded'. A counter
//     written against 'succeeded' reads zero on a healthy instance.
//  2. 'aborted' is terminal and distinct: folding it into failed reports
//     cancelled work as a failure.
//  3. work_open must not absorb archived items, the Idea Cloud ('idea') or
//     recurring templates ('recurring'). All three live in work_items with
//     non-terminal-looking statuses, so a naive NOT IN (terminal) denylist
//     counts them as work in flight.
//
// Guarded by ORCHICON_TEST_DSN like the rest of the package's DB tests, and
// rolled back so the instance is left exactly as found.

import (
	"context"
	"os"
	"testing"
	"time"

	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/migrate"
)

// outcomeTestPool opens the test database and applies migrations. This file is
// in package db_test (not package db) because migrate imports db, so an
// internal test file that imported migrate would be an import cycle — the same
// reason credentials_test.go lives out here.
func outcomeTestPool(t *testing.T) *db.Pool {
	t.Helper()
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed outcome test")
	}
	pool, err := db.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := migrate.Run(context.Background(), pool, assets.MigrationsFS, assets.MigrationsDir); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return pool
}

func TestGetOutcomeCountsSeparatesEveryTerminalState(t *testing.T) {
	pool := outcomeTestPool(t)
	ctx := context.Background()
	const tenantID = "tnt_outcome_test"
	const projectID = "prj_outcome_test"

	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := ttx.Tx.Exec(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	exec(`INSERT INTO projects (id, tenant_id, name, slug, status, version, created_at, updated_at)
		VALUES ($1,$2,$1,$1,'active',1,now(),now())`, projectID, tenantID)

	// Work items: one of each terminal state, one in flight, and the three
	// that must NOT count as open.
	items := []struct {
		id, status string
		archived   bool
	}{
		{"wi_ok", "succeeded", false},
		{"wi_skipped", "skipped", false},
		{"wi_cancelled", "cancelled", false},
		{"wi_failed", "failed", false},
		{"wi_running", "running", false},
		{"wi_pending", "pending", false},
		// Must be excluded from work_open:
		{"wi_idea", "idea", false},
		{"wi_recurring", "recurring", false},
		// Archived keeps its terminal status but must not be counted at all.
		{"wi_archived_ok", "succeeded", true},
	}
	for _, it := range items {
		if it.archived {
			exec(`INSERT INTO work_items (id, tenant_id, project_id, kind, title, status, version,
					created_at, updated_at, archived_at, archived_from_status)
				VALUES ($1,$2,$3,'task','Item',$4,1,now(),now(),now(),$4)`,
				it.id, tenantID, projectID, it.status)
			continue
		}
		exec(`INSERT INTO work_items (id, tenant_id, project_id, kind, title, status, version, created_at, updated_at)
			VALUES ($1,$2,$3,'task','Item',$4,1,now(),now())`, it.id, tenantID, projectID, it.status)
	}

	// Workflow runs: completed is the SUCCESS state; aborted is its own.
	exec(`INSERT INTO workflows (id, tenant_id, project_id, name, current_version, status, version, created_at, updated_at, type)
		VALUES ('wf_outcome',$1,$2,'WF',1,'published',1,now(),now(),'template')`, tenantID, projectID)
	runs := []struct{ id, status string }{
		{"run_ok", "completed"},
		{"run_failed", "failed"},
		{"run_aborted", "aborted"},
		{"run_running", "running"},
		{"run_pending", "pending"},
	}
	started := time.Now().UTC().Add(-30 * time.Minute).Truncate(time.Second)
	for _, r := range runs {
		exec(`INSERT INTO workflow_runs (id, tenant_id, workflow_id, workflow_version, project_id,
				status, version, created_at, updated_at, started_at)
			VALUES ($1,$2,'wf_outcome',1,$3,$4,1,now(),now(),$5)`, r.id, tenantID, projectID, r.status, started)
	}

	// A zero start is the unbounded sentinel; end is now.
	got, err := db.GetOutcomeCounts(ctx, ttx.Tx, tenantID, projectID, "", time.Time{}, time.Now())
	if err != nil {
		t.Fatalf("GetOutcomeCounts: %v", err)
	}

	if got.WorkSucceeded != 1 {
		// wi_archived_ok is succeeded but archived, so it must NOT be here.
		t.Errorf("WorkSucceeded = %d, want 1 (archived succeeded item must not count)", got.WorkSucceeded)
	}
	if got.WorkSkipped != 1 {
		t.Errorf("WorkSkipped = %d, want 1 (skipped is terminal, not a success and not open)", got.WorkSkipped)
	}
	if got.WorkCancelled != 1 {
		t.Errorf("WorkCancelled = %d, want 1", got.WorkCancelled)
	}
	if got.WorkFailed != 1 {
		t.Errorf("WorkFailed = %d, want 1", got.WorkFailed)
	}
	if got.WorkOpen != 2 {
		// running + pending. 'idea' and 'recurring' are NOT work in flight.
		t.Errorf("WorkOpen = %d, want 2 (running+pending; idea/recurring must not count)", got.WorkOpen)
	}

	if got.WorkflowsCompleted != 1 {
		t.Errorf("WorkflowsCompleted = %d, want 1 (success state is 'completed', not 'succeeded')", got.WorkflowsCompleted)
	}
	if got.WorkflowsFailed != 1 {
		t.Errorf("WorkflowsFailed = %d, want 1", got.WorkflowsFailed)
	}
	if got.WorkflowsAborted != 1 {
		t.Errorf("WorkflowsAborted = %d, want 1 (aborted must not be folded into failed)", got.WorkflowsAborted)
	}
	if got.WorkflowsRunning != 2 {
		t.Errorf("WorkflowsRunning = %d, want 2 (running+pending)", got.WorkflowsRunning)
	}
}

// TestGetOutcomeCountsHonoursTheWindow pins the window rule: work that moved
// outside it is excluded, so a 24h report on a long-lived instance does not
// report all-time totals.
func TestGetOutcomeCountsHonoursTheWindow(t *testing.T) {
	pool := outcomeTestPool(t)
	ctx := context.Background()
	const tenantID = "tnt_outcome_window"
	const projectID = "prj_outcome_window"

	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := ttx.Tx.Exec(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	exec(`INSERT INTO projects (id, tenant_id, name, slug, status, version, created_at, updated_at)
		VALUES ($1,$2,$1,$1,'active',1,now(),now())`, projectID, tenantID)

	now := time.Now().UTC()
	old := now.Add(-72 * time.Hour)
	// One recent success, one three days old.
	exec(`INSERT INTO work_items (id, tenant_id, project_id, kind, title, status, version, created_at, updated_at)
		VALUES ('wi_recent',$1,$2,'task','Recent','succeeded',1,$3,$3)`, tenantID, projectID, now)
	exec(`INSERT INTO work_items (id, tenant_id, project_id, kind, title, status, version, created_at, updated_at)
		VALUES ('wi_old',$1,$2,'task','Old','succeeded',1,$3,$3)`, tenantID, projectID, old)

	// Last 24 hours: only the recent one.
	within, err := db.GetOutcomeCounts(ctx, ttx.Tx, tenantID, projectID, "", now.Add(-24*time.Hour), now.Add(time.Minute))
	if err != nil {
		t.Fatalf("GetOutcomeCounts within window: %v", err)
	}
	if within.WorkSucceeded != 1 {
		t.Errorf("24h WorkSucceeded = %d, want 1 (the 3-day-old item is outside the window)", within.WorkSucceeded)
	}

	// Unbounded: both.
	all, err := db.GetOutcomeCounts(ctx, ttx.Tx, tenantID, projectID, "", time.Time{}, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("GetOutcomeCounts unbounded: %v", err)
	}
	if all.WorkSucceeded != 2 {
		t.Errorf("unbounded WorkSucceeded = %d, want 2", all.WorkSucceeded)
	}
}
