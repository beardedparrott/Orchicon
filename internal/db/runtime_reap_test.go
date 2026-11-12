package db

// runtime_reap_test.go — THE REAP CANDIDATE SET MUST CONVERGE, AGAINST A REAL DATABASE.
//
// The log noise the operator reported ("every second ... reaped a terminal run's container") was this query
// returning the same run forever: its evidence predicate is a condition that never becomes false, because
// runtime_image is the run's history and runtime_ready is a gate, and the sweep must clear neither. The reap
// marker is what makes the set terminate — so it is asserted here, where a wrong predicate is visible.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func reapTestPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping the DB-backed reap proof")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, ctx
}

// reapTenant makes a fresh tenant with one COMPLETED run that carries runtime evidence.
func reapTenant(t *testing.T, ctx context.Context, pool *pgxpool.Pool, runID string) (tenantID string, tx pgx.Tx) {
	t.Helper()
	tenantID = "tnt_reap_" + runID
	if _, err := pool.Exec(ctx, `INSERT INTO tenants (id, name, slug) VALUES ($1,$1,$1)
		ON CONFLICT (id) DO NOTHING`, tenantID); err != nil {
		t.Fatalf("tenant: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workflow_runs
		(id, tenant_id, workflow_id, workflow_version, project_id, status, runtime_image, runtime_ready, ended_at)
		VALUES ($1,$2,'wf',1,'proj','completed','orchicon-runtime:dev',true, now())`,
		runID, tenantID); err != nil {
		t.Fatalf("run: %v", err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID); err != nil {
		t.Fatalf("set tenant: %v", err)
	}
	return tenantID, tx
}

func inCandidates(t *testing.T, ctx context.Context, tx pgx.Tx, tenantID, runID string) bool {
	t.Helper()
	runs, err := ListTerminalRunsWithRuntimeEvidence(ctx, tx, tenantID, 50)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, r := range runs {
		if r.ID == runID {
			return true
		}
	}
	return false
}

// THE REPORTED LOOP: a terminal run with runtime evidence is a candidate, the sweep reaps it, and it must then
// STOP being one.
func TestAReapedRunLeavesTheCandidateSetForever(t *testing.T) {
	pool, ctx := reapTestPool(t)
	runID := "run_reap_" + time.Now().Format("150405.000000000")
	tenantID, tx := reapTenant(t, ctx, pool, runID)

	if !inCandidates(t, ctx, tx, tenantID, runID) {
		t.Fatal("premise wrong: a completed run with runtime evidence is not a candidate")
	}

	// THE SWEEP REAPS AND MARKS.
	if err := MarkRunRuntimeReaped(ctx, tx, tenantID, runID); err != nil {
		t.Fatalf("mark reaped: %v", err)
	}
	if inCandidates(t, ctx, tx, tenantID, runID) {
		t.Fatal("the run is STILL a candidate after being marked reaped — the sweep would reap it again and log " +
			"`reaped a terminal run's container` again, once a second, forever: exactly the reported noise")
	}

	// AND THE MARKER IS IDEMPOTENT: marking twice is not an error and does not resurrect the row.
	if err := MarkRunRuntimeReaped(ctx, tx, tenantID, runID); err != nil {
		t.Fatalf("second mark: %v", err)
	}
	if inCandidates(t, ctx, tx, tenantID, runID) {
		t.Fatal("a second mark put the run back in the candidate set")
	}
}

// A LIVE RUN IS NEVER A CANDIDATE — the sweep must not reap a container a run is using.
func TestARunningRunIsNeverReaped(t *testing.T) {
	pool, ctx := reapTestPool(t)
	runID := "run_live_" + time.Now().Format("150405.000000000")
	tenantID, tx := reapTenant(t, ctx, pool, runID)
	if _, err := tx.Exec(context.Background(),
		`UPDATE workflow_runs SET status = 'running' WHERE id = $1`, runID); err != nil {
		t.Fatalf("set running: %v", err)
	}
	if inCandidates(t, ctx, tx, tenantID, runID) {
		t.Fatal("a RUNNING run is a reap candidate — its container would be destroyed under a live execution")
	}
}
