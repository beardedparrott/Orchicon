package scheduler

// Late verdicts and stale-version retries.
//
// THE FIELD FAILURE THESE PIN (2026-10-17, run 01M54CJA5RZRNNRPY9410XRVJX for
// "Image path under Podman", sort 4 of the Podman feature):
//
//	03:10:22.556 that run COMPLETED with PR #736 merged (the item went succeeded)
//	03:10:22.754 the next child armed — 198ms later, so the item WAS succeeded
//	03:11:33     WARN reaping lost execution  execution=01M54DVWN… status="unhealthy"
//	             → FailLostExecution → transitionWorkItemOnResult(…, false, …)
//	             → boundToActiveRun = false (the run is "completed", not active)
//	             → the else branch wrote WorkItemFailed OVER succeeded
//
// The failed child then halted its sequence parent's entire chain in the
// failure pre-scan, so sort 6 never fired — for ~7.7 hours — until an operator
// manually marked sort 4 succeeded. Nothing logged a word about the overwrite.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
)

// TestLateVerdictContradictsRun enumerates every (run status, verdict) pair.
// The rule is small enough that exhaustiveness is cheap, and the pair that
// caused the incident must be the loud one.
func TestLateVerdictContradictsRun(t *testing.T) {
	cases := []struct {
		runStatus string
		verdict   bool
		want      bool
		why       string
	}{
		{domain.WorkflowRunCompleted, false, true,
			"THE INCIDENT: a reaped execution's failure verdict over a run that completed (and merged its PR)"},
		{domain.WorkflowRunCompleted, true, false,
			"agrees — a success verdict over a completed run is ordinary bookkeeping"},
		{domain.WorkflowRunFailed, true, true,
			"the other direction: a late success must not relitigate a failed run either"},
		{domain.WorkflowRunFailed, false, false, "agrees — failure over failure"},
		{domain.WorkflowRunAborted, true, true, "a late success over an aborted run is still a contradiction"},
		{domain.WorkflowRunAborted, false, false, "agrees"},
	}
	for _, c := range cases {
		if got := lateVerdictContradictsRun(c.runStatus, c.verdict); got != c.want {
			t.Errorf("lateVerdictContradictsRun(%q, %v) = %v, want %v — %s",
				c.runStatus, c.verdict, got, c.want, c.why)
		}
	}
}

// TestStaleVersionRetryConverges: the retry must succeed once the closure stops
// losing the race — the whole point of retrying at all.
func TestStaleVersionRetryConverges(t *testing.T) {
	t.Setenv("ORCHICON_STALE_VERSION_RETRY_DELAY", "0s")
	nopLog := slog.New(slog.NewTextHandler(io.Discard, nil))

	calls := 0
	got, err := retryStaleVersion(context.Background(), nopLog, "test op", func() (int, error) {
		calls++
		if calls < 3 {
			return 0, db.ErrNotFound // lost the optimistic-concurrency race
		}
		return 42, nil
	})
	if err != nil {
		t.Fatalf("retry did not converge: %v", err)
	}
	if got != 42 || calls != 3 {
		t.Fatalf("got (%d,%v) after %d calls, want (42,nil) after 3", got, err, calls)
	}
}

// TestStaleVersionRetryDoesNotRetryOtherErrors: a genuine failure must surface
// immediately — retrying it would turn a fast error into a slow one and could
// mask a real problem behind the backoff.
func TestStaleVersionRetryDoesNotRetryOtherErrors(t *testing.T) {
	t.Setenv("ORCHICON_STALE_VERSION_RETRY_DELAY", "0s")
	nopLog := slog.New(slog.NewTextHandler(io.Discard, nil))

	calls := 0
	boom := errors.New("connection reset")
	_, err := retryStaleVersion(context.Background(), nopLog, "test op", func() (int, error) {
		calls++
		return 0, boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the original error", err)
	}
	if calls != 1 {
		t.Fatalf("called %d times, want 1 — a non-race error must not be retried", calls)
	}
}

// TestStaleVersionRetryExhausts: a row that genuinely cannot be written is
// still reported after the bounded attempts, not retried forever.
func TestStaleVersionRetryExhausts(t *testing.T) {
	t.Setenv("ORCHICON_STALE_VERSION_RETRY_DELAY", "0s")
	nopLog := slog.New(slog.NewTextHandler(io.Discard, nil))

	calls := 0
	_, err := retryStaleVersion(context.Background(), nopLog, "test op", func() (int, error) {
		calls++
		return 0, db.ErrNotFound
	})
	if !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound after exhaustion", err)
	}
	if calls != staleVersionAttempts {
		t.Fatalf("called %d times, want %d", calls, staleVersionAttempts)
	}
}

// TestStaleVersionRetryHonoursContext: the retry must not outlive its context —
// a cancelled pass has to stop, or slow contention holds a reconcile open.
func TestStaleVersionRetryHonoursContext(t *testing.T) {
	t.Setenv("ORCHICON_STALE_VERSION_RETRY_DELAY", "30s")
	nopLog := slog.New(slog.NewTextHandler(io.Discard, nil))

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	start := time.Now()
	_, err := retryStaleVersion(ctx, nopLog, "test op", func() (int, error) {
		return 0, db.ErrNotFound
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("retry waited %s on a cancelled context, want an immediate return", elapsed)
	}
}

// seedBoundItemWithExecution builds the minimum shape the guard reads: a work
// item bound to a workflow run, with one execution of that item.
func seedBoundItemWithExecution(t *testing.T, pool *db.Pool, projectID, runStatus, itemStatus string, bound bool) (itemID, execID string) {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)

	wf, err := db.CreateWorkflow(ctx, ttx.Tx, db.WorkflowRow{
		ID: db.NewID(), TenantID: approvalTestTenant, ProjectID: projectID,
		Name: "Late Verdict WF " + db.NewID()[:6], CurrentVersion: 1,
		Status: "published", Type: "template",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateWorkflowVersion(ctx, ttx.Tx, db.WorkflowVersionRow{
		ID: db.NewID(), TenantID: approvalTestTenant, WorkflowID: wf.ID,
		Version: 1, Status: "published", Steps: []byte("[]"),
		Inputs: []byte("{}"), Outputs: []byte("{}"),
	}); err != nil {
		t.Fatal(err)
	}
	runID := ""
	if bound {
		run, err := db.CreateWorkflowRun(ctx, ttx.Tx, db.WorkflowRunRow{
			ID: db.NewID(), TenantID: approvalTestTenant, WorkflowID: wf.ID,
			WorkflowVersion: 1, ProjectID: projectID, Status: runStatus,
			RunContext: []byte("{}"),
		})
		if err != nil {
			t.Fatal(err)
		}
		runID = run.ID
	}
	item, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: approvalTestTenant, ProjectID: projectID,
		Kind: domain.WorkItemKindTask, Title: "Late Verdict Item " + db.NewID()[:6],
		Status: itemStatus, WorkflowRunID: runID, WorkflowID: &wf.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	exec, err := db.CreateExecution(ctx, ttx.Tx, db.ExecutionRow{
		ID: db.NewID(), TenantID: approvalTestTenant, ProjectID: projectID,
		TaskID: item.ID, WorkerID: "w_se_devops_engineer", WorkerVersion: 1,
		Status: domain.ExecutionFailed, HealthState: domain.HealthUnhealthy,
		StartedAt: &now, EndedAt: &now,
		WorkflowRunID: runID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return item.ID, exec.ID
}

func itemStatusNow(t *testing.T, pool *db.Pool, itemID string) string {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	wi, err := db.GetWorkItem(ctx, ttx.Tx, approvalTestTenant, itemID)
	if err != nil {
		t.Fatal(err)
	}
	return wi.Status
}

// TestLateFailureVerdictDoesNotOverwriteCompletedRun is the incident itself:
// the reaper fails a superseded execution of a run that already COMPLETED and
// whose item already succeeded. The item must stay succeeded.
func TestLateFailureVerdictDoesNotOverwriteCompletedRun(t *testing.T) {
	purgeScanTenant(t, approvalTestPool(t))
	env := newSequenceTestEnv(t)

	itemID, execID := seedBoundItemWithExecution(t, env.pool, env.proj.ID,
		domain.WorkflowRunCompleted, domain.WorkItemSucceeded, true)

	rec := NewTaskReconciler(env.pool, slog.Default(), testDispatcher(&manifestCaptureBridge{}))
	// What FailLostExecution does to the work item, with the reaper's verdict.
	rec.transitionWorkItemOnResult(context.Background(), execID, false, "")

	if got := itemStatusNow(t, env.pool, itemID); got != domain.WorkItemSucceeded {
		t.Fatalf("item status = %q, want %q.\n"+
			"A late failure verdict from a SUPERSEDED execution overwrote a run that had already\n"+
			"completed. That is what halted the sequence chain for 7.7 hours: the failed child made\n"+
			"the parent's failure pre-scan stop the whole chain.", got, domain.WorkItemSucceeded)
	}
}

// The control: the guard must not break the standalone path. An UNBOUND item
// still takes its execution's verdict — that is the only signal it has.
func TestFailureVerdictStillAppliesToUnboundItem(t *testing.T) {
	purgeScanTenant(t, approvalTestPool(t))
	env := newSequenceTestEnv(t)

	itemID, execID := seedBoundItemWithExecution(t, env.pool, env.proj.ID,
		"", domain.WorkItemRunning, false)

	rec := NewTaskReconciler(env.pool, slog.Default(), testDispatcher(&manifestCaptureBridge{}))
	rec.transitionWorkItemOnResult(context.Background(), execID, false, "")

	if got := itemStatusNow(t, env.pool, itemID); got != domain.WorkItemFailed {
		t.Fatalf("unbound item status = %q, want %q — the guard must not suppress the only verdict an\n"+
			"unbound item ever gets", got, domain.WorkItemFailed)
	}
}

// And a bound item whose run is still RUNNING keeps the old behaviour: the run
// is active, so the per-execution path propagates rather than deciding.
func TestVerdictForActiveRunDoesNotDecideTheItem(t *testing.T) {
	purgeScanTenant(t, approvalTestPool(t))
	env := newSequenceTestEnv(t)

	itemID, execID := seedBoundItemWithExecution(t, env.pool, env.proj.ID,
		domain.WorkflowRunRunning, domain.WorkItemRunning, true)

	rec := NewTaskReconciler(env.pool, slog.Default(), testDispatcher(&manifestCaptureBridge{}))
	rec.transitionWorkItemOnResult(context.Background(), execID, false, "")

	if got := itemStatusNow(t, env.pool, itemID); got != domain.WorkItemRunning {
		t.Fatalf("item bound to a RUNNING run became %q, want %q — the run decides, not the execution",
			got, domain.WorkItemRunning)
	}
}

// A source-level pin on PLACEMENT: the guard must consult the terminal-run
// check BEFORE any work-item status write. The helper existing is not enough —
// a version placed after the write would leave the defect fully intact.
func TestTerminalRunGuardPrecedesTheStatusWrite(t *testing.T) {
	raw, err := os.ReadFile("reconciler.go")
	if err != nil {
		t.Fatalf("read reconciler.go: %v", err)
	}
	src := string(raw)
	// Scope to the FUNCTION — "status := domain.WorkItemFailed" also appears in
	// earlier functions on this file, so a whole-file index would compare the
	// guard against the wrong write.
	fnStart := strings.Index(src, "func (r *TaskReconciler) transitionWorkItemOnResult(")
	if fnStart < 0 {
		t.Fatal("transitionWorkItemOnResult is gone")
	}
	fnEnd := strings.Index(src[fnStart+1:], "\nfunc ")
	if fnEnd < 0 {
		t.Fatal("could not delimit transitionWorkItemOnResult")
	}
	body := src[fnStart : fnStart+1+fnEnd]
	guardIdx := strings.Index(body, "if runTerminal, terminal := r.boundRunTerminalStatus(")
	writeIdx := strings.Index(body, "status := domain.WorkItemFailed")
	if guardIdx < 0 {
		t.Fatal("transitionWorkItemOnResult no longer consults boundRunTerminalStatus — a late verdict can\n" +
			"again overwrite a run that already completed")
	}
	if writeIdx < 0 {
		t.Fatal("could not locate the failed-status write to compare against")
	}
	if guardIdx > writeIdx {
		t.Fatalf("the terminal-run guard appears AFTER the failed-status write (guard at %d, write at %d —\n"+
			"checking too late leaves the overwrite in place", guardIdx, writeIdx)
	}
	// And the verdict must be DROPPED, not merely logged.
	if !strings.Contains(body, "lateVerdictContradictsRun") {
		t.Error("the guard no longer distinguishes a contradicting verdict — the incident case would be silent again")
	}
}
