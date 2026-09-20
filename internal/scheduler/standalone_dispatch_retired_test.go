package scheduler

// standalone_dispatch_retired_test.go — the v0.1 standalone dispatch path is
// retired (workflow-first execution). These tests prove a work item with NO
// workflow binding can never reach a runnable/executed state through ANY
// path: the TaskReconciler scan (dispatch), the recurring-fire backstop, the
// scheduled-run backstop, and the sequence arm/validation path. Every path
// must fail LOUDLY with an actionable reason surfaced on the item, and must
// never create a WorkerExecution.
//
// DB-backed; skipped without ORCHICON_TEST_DSN.

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/workitem"
)

const noWorkflowReasonFragment = "no workflow is set"

// seedWorkflowLessReadyTask creates a ready task with an assigned worker and
// NO workflow binding — precisely the retired standalone dispatch shape
// (what ListReadyTasks used to pick up).
func seedWorkflowLessReadyTask(t *testing.T, pool *db.Pool, projectID string) db.WorkItemRow {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	created, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: approvalTestTenant, ProjectID: projectID,
		Kind: domain.WorkItemKindTask, Title: "No-Workflow Task",
		Status:            domain.WorkItemReady,
		AssignedWorkerRef: []byte(`{"worker_id":"w_se_devops_engineer","version":1}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return created
}

// setItemStatus flips a work item's status directly (test fixture only).
func setItemStatus(t *testing.T, pool *db.Pool, id, status string) {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	item, err := db.GetWorkItem(ctx, ttx.Tx, approvalTestTenant, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpdateWorkItem(ctx, ttx.Tx, approvalTestTenant, id, item.Version, db.UpdateWorkItemFields{
		Status: &status,
	}); err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestWorkflowLessItemCannotDispatch (AC 2 + AC 5): the TaskReconciler's scan
// dispatch creates NO execution for a no-workflow item and fails the item
// loudly with an actionable reason instead of silently arming a zombie.
func TestWorkflowLessItemCannotDispatch(t *testing.T) {
	purgeScanTenant(t, approvalTestPool(t))
	env := newSequenceTestEnv(t)
	seedReadyAdapter(t, env.pool)
	task := seedWorkflowLessReadyTask(t, env.pool, env.proj.ID)

	rec := NewTaskReconciler(env.pool, slog.Default(), testDispatcher(&manifestCaptureBridge{}))
	if err := rec.reconcileOne(context.Background(), task.ID, ""); err != nil {
		t.Fatalf("reconcileOne: %v", err)
	}
	if n := countExecutionsForTask(t, env.pool, task.ID); n != 0 {
		t.Fatalf("workflow-less item created %d execution(s), want 0", n)
	}
	got := mustGet(t, env.pool, task.ID)
	if got.Status != domain.WorkItemFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if !strings.Contains(string(got.Results), noWorkflowReasonFragment) {
		t.Fatalf("results must surface the actionable reason, got %s", got.Results)
	}
}

// TestWorkflowLessItemCannotFireRecurring (AC 4): the recurring-fire backstop
// no longer logs a warning and moves on — it fails the item loudly, records
// the failed fire in the ledger, and starts no workflow.
func TestWorkflowLessItemCannotFireRecurring(t *testing.T) {
	purgeScanTenant(t, approvalTestPool(t))
	env := newSequenceTestEnv(t)
	item := createRecurringItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "No-Workflow Recurring", nil, nil, nil)

	rec := &recurringFireRecorder{}
	r := NewRecurringFireReconciler(env.pool, slog.Default(), rec.startFn())
	if res := r.Reconcile(context.Background(), ""); res.Error != nil {
		t.Fatalf("scanAndFire: %v", res.Error)
	}
	rec.mu.Lock()
	starts := rec.startCalls
	rec.mu.Unlock()
	if starts != 0 {
		t.Fatalf("recurring fire started %d workflow(s), want 0", starts)
	}
	got := mustGet(t, env.pool, item.ID)
	if got.Status != domain.WorkItemFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if !strings.Contains(string(got.Results), noWorkflowReasonFragment) {
		t.Fatalf("results must surface the actionable reason, got %s", got.Results)
	}
	// The fire ledger records the failure (it is not a silent skip).
	ctx := context.Background()
	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	var failedFires int
	if err := ttx.Tx.QueryRow(ctx,
		`SELECT count(*) FROM recurring_run_history WHERE tenant_id=$1 AND work_item_id=$2 AND status='failed'`,
		approvalTestTenant, item.ID).Scan(&failedFires); err != nil {
		t.Fatal(err)
	}
	if failedFires != 1 {
		t.Fatalf("recurring ledger failed-fires = %d, want 1", failedFires)
	}
}

// TestWorkflowLessItemCannotFireScheduled (AC 4): the scheduled-run backstop
// fails a due workflow-less item loudly instead of skipping it. The scan's
// own WHERE admits only rows with a non-NULL workflow_id (or a sequence
// parent), so the observed branch is the legacy EMPTY-STRING binding — a
// cleared binding that is still "not NULL". That is precisely the zombie row
// the belt-and-suspenders backstop exists for: it must fail loudly, not
// silently skip.
func TestWorkflowLessItemCannotFireScheduled(t *testing.T) {
	purgeScanTenant(t, approvalTestPool(t))
	env := newSequenceTestEnv(t)
	wfID := seedPublishedWorkflow(t, env.pool, env.proj.ID)
	item := createScheduledItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "No-Workflow Scheduled", nil, &wfID)

	ctx := context.Background()
	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ttx.Exec(ctx, `UPDATE work_items SET workflow_id = '' WHERE tenant_id = $1 AND id = $2`,
		approvalTestTenant, item.ID); err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	rec := &fireRecorder{}
	r := NewScheduledRunReconciler(env.pool, slog.Default(), rec.startFn())
	if res := r.Reconcile(ctx, ""); res.Error != nil {
		t.Fatalf("scanAndFire: %v", res.Error)
	}
	rec.mu.Lock()
	starts := rec.startCalls
	rec.mu.Unlock()
	if starts != 0 {
		t.Fatalf("scheduled run started %d workflow(s), want 0", starts)
	}
	got := mustGet(t, env.pool, item.ID)
	if got.Status != domain.WorkItemFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if !strings.Contains(string(got.Results), noWorkflowReasonFragment) {
		t.Fatalf("results must surface the actionable reason, got %s", got.Results)
	}
}

// TestWorkflowLessItemCannotJoinSequence (AC 1 + AC 6): a workflow-less item
// is rejected by the sequence/schedule validation, and the sequence ARM path
// fails a workflow-less child loudly instead of arming a run that cannot
// exist (the delivered loud path, asserted here as a regression guard).
func TestWorkflowLessItemCannotJoinSequence(t *testing.T) {
	env := newSequenceTestEnv(t)
	ctx := context.Background()

	// A workflow-less LEAF has nothing to run: rejected at schedule time.
	leaf := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "Unbound Leaf", nil, nil)
	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	if err := workitem.ValidateSequenceSchedule(ctx, ttx.Tx, approvalTestTenant, leaf); err == nil {
		t.Fatal("workflow-less leaf must be rejected at schedule time")
	} else if !strings.Contains(err.Error(), noWorkflowReasonFragment) {
		t.Fatalf("leaf rejection must be actionable, got %v", err)
	}
	_ = ttx.Rollback(ctx)

	// A parent whose subtree holds a workflow-less child is rejected whole.
	parent := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindEpic, "Seq Parent", nil, nil)
	child := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "Unbound Child", &parent.ID, nil)
	parent = mustGet(t, env.pool, parent.ID)
	ttx2, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	if err := workitem.ValidateSequenceSchedule(ctx, ttx2.Tx, approvalTestTenant, parent); err == nil {
		t.Fatal("a parent with a workflow-less child must be rejected at schedule time")
	} else if !strings.Contains(err.Error(), "has no workflow set") {
		t.Fatalf("subtree rejection must name the offender, got %v", err)
	}
	_ = ttx2.Rollback(ctx)

	// The ARM/resume path must fail the workflow-less child loudly and start
	// no workflow run for it.
	setItemStatus(t, env.pool, parent.ID, domain.WorkItemRunning)
	rec := NewSequenceReconciler(env.pool, slog.Default(), env.startFn())
	if res := rec.Reconcile(ctx, parent.ID); res.Error != nil {
		t.Fatalf("sequence reconcile: %v", res.Error)
	}
	if got := mustGet(t, env.pool, child.ID); got.Status != domain.WorkItemFailed {
		t.Fatalf("workflow-less child status = %q, want failed (armed loudly, not started)", got.Status)
	}
	env.mu.Lock()
	started := len(env.starts)
	env.mu.Unlock()
	if started != 0 {
		t.Fatalf("sequence arm started %d workflow(s) for a workflow-less child, want 0", started)
	}
}
