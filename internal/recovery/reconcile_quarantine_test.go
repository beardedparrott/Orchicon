package recovery

// Regression tests for the RC3 + R6 fixes:
//
//   - RC3: a recovery reconcile that hits a poisoned PostgreSQL tx
//     (SQLSTATE 25P01/25P02, "current transaction is aborted") must be
//     quarantined (marked terminal-failed) so ListPendingRecoveries stops
//     returning it — a single bad row can no longer wedge the sequential
//     scanRecoveries and flood the log (~20/s).
//   - R6: triggering recovery for a (task, failed_execution) whose prior
//     recovery already reached a TERMINAL status must be a no-op — no fresh
//     duplicate L1 recovery on the same dead run.
//
// These are DB-backed (skipped unless ORCHICON_TEST_DSN is set):
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/recovery/ -run 'TestIsPoisonedRecoveryTx|TestQuarantineRecovery|TestTriggerIdempotentTerminal' -v

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
)

// TestIsPoisonedRecoveryTx is the pure predicate truth table for RC3: only a
// tx-abort / failed-commit error is treated as poisoned; everything else is
// left to the normal retry path.
func TestIsPoisonedRecoveryTx(t *testing.T) {
	poisoned := []string{
		"current transaction is aborted",
		"ERROR: current transaction is aborted, commands ignored until end of transaction block (SQLSTATE 25P02)",
		"UPDATE recovery_executions ... ERROR: current transaction is aborted (SQLSTATE 25P01)",
		"foo SQLSTATE 25P01 bar",
		"foo SQLSTATE 25P02 bar",
	}
	for _, msg := range poisoned {
		if !isPoisonedRecoveryTx(errors.New(msg)) {
			t.Errorf("isPoisonedRecoveryTx(%q) = false, want true", msg)
		}
	}
	clean := []string{
		"",
		"connection refused",
		"db: update recovery: some business error",
		"recovery reconcile failed",
	}
	for _, msg := range clean {
		var err error
		if msg != "" {
			err = errors.New(msg)
		}
		if isPoisonedRecoveryTx(err) {
			t.Errorf("isPoisonedRecoveryTx(%q) = true, want false", msg)
		}
	}
}

// TestQuarantineRecoveryIsolatesPoisonedRow — RC3. A pending recovery that
// would deterministically re-fail on a poisoned tx is marked terminal-failed
// by quarantineRecovery so it drops out of the pending scan; a subsequent
// ListPendingRecoveries no longer returns it, so one bad row cannot wedge the
// (sequential) recovery reconciler.
func TestQuarantineRecoveryIsolatesPoisonedRow(t *testing.T) {
	if os.Getenv("ORCHICON_TEST_DSN") == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed quarantine test")
	}
	f := seedKeysSetup(t, true)
	ctx := context.Background()
	const tenant = "tnt_dev"
	rc := NewReconciler(newQuietEngine(f.pool))

	// A pending recovery for the failed execution (as seedKeysSetup leaves the
	// task ready for one).
	now := time.Now().UTC()
	ttx, err := f.pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	rec, err := db.CreateRecoveryExecution(ctx, ttx.Tx, db.RecoveryExecutionRow{
		ID: db.NewID(), TenantID: tenant, ProjectID: f.projectID,
		TaskID: f.taskID, FailedExecutionID: f.execID,
		RecoveryWorkflowID: "wf-recovery", TriggerReason: "step_recovery",
		Level: 1, Status: domain.RecoveryPending, CurrentStep: "capture",
		TriggeredAt: now,
	})
	if err != nil {
		_ = ttx.Rollback(ctx)
		t.Fatalf("create pending recovery: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit pending recovery: %v", err)
	}

	// Confirm it is currently pending (in the scan).
	if !recoveryIsPending(t, f.pool, tenant, rec.ID) {
		t.Fatalf("recovery %s should be pending before quarantine", rec.ID)
	}

	// Quarantine exactly as scanRecoveries does on a poisoned-tx error.
	cause := errors.New("update recovery: ERROR: current transaction is aborted (SQLSTATE 25P02)")
	if err := rc.quarantineRecovery(ctx, tenant, rec.ID, cause); err != nil {
		t.Fatalf("quarantineRecovery: %v", err)
	}

	// It must now be terminal-failed and dropped from the pending scan.
	if got := getRecoveryStatus(t, f.pool, tenant, rec.ID); got != domain.RecoveryFailed {
		t.Fatalf("quarantined recovery status = %q, want %q (terminal, out of scan)", got, domain.RecoveryFailed)
	}
	if recoveryIsPending(t, f.pool, tenant, rec.ID) {
		t.Fatalf("quarantined recovery still pending — it would wedge the scan")
	}

	// Quarantining an already-terminal row is a no-op (idempotent), never a
	// resurrection into a re-trigger.
	if err := rc.quarantineRecovery(ctx, tenant, rec.ID, cause); err != nil {
		t.Fatalf("re-quarantine terminal recovery should be a no-op, got %v", err)
	}
	if got := getRecoveryStatus(t, f.pool, tenant, rec.ID); got != domain.RecoveryFailed {
		t.Fatalf("re-quarantined recovery status = %q, want %q (unchanged)", got, domain.RecoveryFailed)
	}
}

// TestTriggerIdempotentForTerminalRecovery — R6. Triggering recovery for the
// SAME (task, failed_execution) whose prior recovery already reached a
// TERMINAL status (resumed / failed) must be a no-op — it must NOT create a
// fresh duplicate L1 recovery on the same dead execution.
func TestTriggerIdempotentForTerminalRecovery(t *testing.T) {
	if os.Getenv("ORCHICON_TEST_DSN") == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed idempotency test")
	}
	for _, terminal := range []string{domain.RecoveryResumed, domain.RecoveryFailed} {
		t.Run("terminal="+terminal, func(t *testing.T) {
			f := seedKeysSetup(t, true)
			ctx := context.Background()
			const tenant = "tnt_dev"
			engine := newQuietEngine(f.pool)

			if err := engine.TriggerOnFailure(ctx, tenant, f.taskID, f.execID, f.stepRunID, "test", nil); err != nil {
				t.Fatalf("first trigger: %v", err)
			}
			recID := recoveryIDFor(t, f.pool, f.taskID)
			now := time.Now().UTC()
			ttx, err := f.pool.BeginTenantTx(ctx, tenant)
			if err != nil {
				t.Fatalf("begin tx: %v", err)
			}
			// UpdateRecoveryExecution is optimistic-concurrency: pass the row's
			// actual Version, otherwise the UPDATE matches 0 rows → ErrNotFound.
			recRow, err := db.GetRecoveryExecution(ctx, ttx.Tx, tenant, recID)
			if err != nil {
				_ = ttx.Rollback(ctx)
				t.Fatalf("get recovery for version: %v", err)
			}
			if _, err := db.UpdateRecoveryExecution(ctx, ttx.Tx, tenant, recID, recRow.Version, db.UpdateRecoveryExecutionFields{
				Status:  strp(terminal),
				EndedAt: &now,
			}); err != nil {
				_ = ttx.Rollback(ctx)
				t.Fatalf("set recovery terminal: %v", err)
			}
			if err := ttx.Commit(ctx); err != nil {
				t.Fatalf("commit: %v", err)
			}

			// Second failure event for the SAME dead execution must be a no-op.
			if err := engine.TriggerOnFailure(ctx, tenant, f.taskID, f.execID, f.stepRunID, "test", nil); err != nil {
				t.Fatalf("second trigger must be a no-op, got %v", err)
			}
			if got := countRecoveries(t, f.pool, tenant, f.taskID); got != 1 {
				t.Fatalf("recovery rows = %d, want 1 (no duplicate re-seed for terminal recovery)", got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// small test helpers (recovery package)
// ---------------------------------------------------------------------------

func newQuietEngine(pool *db.Pool) *Engine {
	return New(pool, slog.New(slog.DiscardHandler))
}

func recoveryIsPending(t *testing.T, pool *db.Pool, tenant, recoveryID string) bool {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	recs, err := db.ListPendingRecoveries(ctx, ttx.Tx, tenant)
	if err != nil {
		t.Fatalf("list pending recoveries: %v", err)
	}
	for _, r := range recs {
		if r.ID == recoveryID {
			return true
		}
	}
	return false
}

func getRecoveryStatus(t *testing.T, pool *db.Pool, tenant, recoveryID string) string {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	rec, err := db.GetRecoveryExecution(ctx, ttx.Tx, tenant, recoveryID)
	if err != nil {
		t.Fatalf("get recovery: %v", err)
	}
	return rec.Status
}

func countRecoveries(t *testing.T, pool *db.Pool, tenant, taskID string) int {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	recs, err := db.ListRecoveries(ctx, ttx.Tx, db.ListRecoveriesFilter{TenantID: tenant, TaskID: taskID})
	if err != nil {
		t.Fatalf("list recoveries: %v", err)
	}
	return len(recs)
}

// strp returns a pointer to s (recovery test helper).
func strp(s string) *string { return &s }
