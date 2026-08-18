package scheduler

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
)

// These tests verify the execution-row worktree acceptance criteria
// against a real Postgres (skipped unless ORCHICON_TEST_DSN is set — see
// approval_no_clone_test.go for the DSN contract):
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/scheduler/ -run 'TestWorktreeExecutionRow' -v
//
// They guard the regression found in QA: the dispatcher never persisted
// the run's worktree state onto the execution row, so the execution
// detail view's worktree tiles always rendered nothing even for a
// provisioned git-backed run.

// TestWorktreeExecutionRowCarriesRunState dispatches a step of a run
// whose worktree was provisioned to 'ready' and verifies the created
// execution row records the same worktree status/path/branch as the run.
func TestWorktreeExecutionRowCarriesRunState(t *testing.T) {
	env := newWorktreeTestEnv(t)
	ctx := context.Background()

	// Provision the run's worktree (git-backed project).
	if res := env.rec.Reconcile(ctx, env.run.ID); res.Error != nil {
		t.Fatalf("reconcile worktree: %v", res.Error)
	}
	run := env.getRun(t)
	if run.WorktreeStatus != domain.WorktreeReady {
		t.Fatalf("worktree_status = %q, want ready", run.WorktreeStatus)
	}

	// The step run carries the worker pin + work item ref the TaskReconciler
	// needs to dispatch (reconciler.go workerVersionForStepRun).
	result, _ := json.Marshal(map[string]any{
		"_work_item_id":    env.itemID,
		"_worker_id":       "w_se_devops_engineer",
		"_worker_version":  1,
		"_prompt":          "test prompt",
		"_decision":        "",
	})
	stx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	sr, err := db.CreateWorkflowStepRun(ctx, stx.Tx, db.WorkflowStepRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		WorkflowRunID: run.ID, StepID: "step-task", StepName: "Task",
		StepKind: domain.StepKindTask, Status: domain.StepRunReady,
		Result: result,
	})
	if err != nil {
		t.Fatalf("create step run: %v", err)
	}
	if err := stx.Commit(ctx); err != nil {
		t.Fatalf("commit step run: %v", err)
	}

	// A ready adapter of the seeded runtime kind so selectAdapter succeeds.
	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	now := time.Now().UTC()
	if _, err := db.CreateAdapter(ctx, ttx.Tx, db.AdapterRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		Kind: "opencode", Version: "test", Endpoint: "localhost:0",
		Capabilities: []byte("{}"), Status: "ready",
		MaxConcurrentExecutions: 8, LastHeartbeatAt: &now,
	}); err != nil {
		t.Fatalf("create adapter: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit adapter: %v", err)
	}

	bridge := &manifestCaptureBridge{}
	rec := NewTaskReconciler(env.pool, slog.Default(), bridge)
	if err := rec.reconcileOne(ctx, env.itemID, sr.ID); err != nil {
		t.Fatalf("reconcileOne: %v", err)
	}

	// The step run is linked to the created execution — read the execution
	// back and assert its worktree columns match the run's.
	linked := getStepRunForStep(t, env, run.ID, "step-task")
	if linked.WorkerExecutionID == "" {
		t.Fatalf("step run was not linked to an execution")
	}
	exec := getExecution(t, env, linked.WorkerExecutionID)
	if exec.WorktreeStatus == nil || *exec.WorktreeStatus != domain.WorktreeReady {
		t.Errorf("execution worktree_status = %v, want %q", exec.WorktreeStatus, domain.WorktreeReady)
	}
	if exec.WorktreePath == nil || *exec.WorktreePath != env.expectedPath() {
		t.Errorf("execution worktree_path = %v, want %q", exec.WorktreePath, env.expectedPath())
	}
	if exec.WorktreeBranch == nil || *exec.WorktreeBranch != env.expectedBranch() {
		t.Errorf("execution worktree_branch = %v, want %q", exec.WorktreeBranch, env.expectedBranch())
	}
}

// TestWorktreeExecutionRowSkippedRun records the neutral in-place state
// for a non-repo run: the execution carries worktree_status=skipped and
// no path/branch, so the UI renders "Runs in place" without branch info.
func TestWorktreeExecutionRowSkippedRun(t *testing.T) {
	env := newWorktreeTestEnv(t)
	ctx := context.Background()

	// Simulate the worktree reconciler's recorded non-repo decision: the
	// run's worktree_status is 'skipped' with no path/branch.
	run := env.getRun(t)
	utx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if _, err := db.UpdateWorkflowRun(ctx, utx.Tx, approvalTestTenant, run.ID, run.Version, db.UpdateWorkflowRunFields{
		WorktreeStatus: strPtr(domain.WorktreeSkipped),
	}); err != nil {
		t.Fatalf("mark run skipped: %v", err)
	}
	if err := utx.Commit(ctx); err != nil {
		t.Fatalf("commit run skipped: %v", err)
	}

	result, _ := json.Marshal(map[string]any{
		"_work_item_id":   env.itemID,
		"_worker_id":      "w_se_devops_engineer",
		"_worker_version": 1,
		"_prompt":         "test prompt",
	})
	stx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	sr, err := db.CreateWorkflowStepRun(ctx, stx.Tx, db.WorkflowStepRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		WorkflowRunID: run.ID, StepID: "step-task", StepName: "Task",
		StepKind: domain.StepKindTask, Status: domain.StepRunReady,
		Result: result,
	})
	if err != nil {
		t.Fatalf("create step run: %v", err)
	}
	if err := stx.Commit(ctx); err != nil {
		t.Fatalf("commit step run: %v", err)
	}

	rec := NewTaskReconciler(env.pool, slog.Default(), &manifestCaptureBridge{})
	if err := rec.reconcileOne(ctx, env.itemID, sr.ID); err != nil {
		t.Fatalf("reconcileOne: %v", err)
	}

	linked := getStepRunForStep(t, env, run.ID, "step-task")
	if linked.WorkerExecutionID == "" {
		t.Fatalf("step run was not linked to an execution")
	}
	exec := getExecution(t, env, linked.WorkerExecutionID)
	if exec.WorktreeStatus == nil || *exec.WorktreeStatus != domain.WorktreeSkipped {
		t.Errorf("execution worktree_status = %v, want %q", exec.WorktreeStatus, domain.WorktreeSkipped)
	}
	if exec.WorktreePath != nil || exec.WorktreeBranch != nil {
		t.Errorf("skipped run must record no path/branch, got path=%v branch=%v", exec.WorktreePath, exec.WorktreeBranch)
	}
}

func getStepRunForStep(t *testing.T, env *worktreeTestEnv, runID, stepID string) db.WorkflowStepRunRow {
	t.Helper()
	ttx, err := env.pool.BeginTenantTx(context.Background(), approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(context.Background())
	sr, err := db.GetWorkflowStepRunByStep(context.Background(), ttx.Tx, approvalTestTenant, runID, stepID)
	if err != nil {
		t.Fatalf("get step run: %v", err)
	}
	return sr
}

func getExecution(t *testing.T, env *worktreeTestEnv, id string) db.ExecutionRow {
	t.Helper()
	ttx, err := env.pool.BeginTenantTx(context.Background(), approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(context.Background())
	exec, err := db.GetExecution(context.Background(), ttx.Tx, approvalTestTenant, id)
	if err != nil {
		t.Fatalf("get execution: %v", err)
	}
	return exec
}
