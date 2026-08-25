package scheduler

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/audit"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/workflow"
)

// recordingRecoveryTrigger is a RecoveryTrigger stub that records the
// deferred post-commit triggers reconcileRun fires instead of invoking the
// real recovery engine. It lets us assert that a failure-then-loop_decision
// pass actually staged a trigger (the AC#9 "not rolled back" property).
type recordingRecoveryTrigger struct {
	calls []recoveryTriggerCall
}

type recoveryTriggerCall struct {
	tenantID, taskID, failedExecID, stepRunID, reason string
}

func (r *recordingRecoveryTrigger) TriggerOnFailure(ctx context.Context, tenantID, taskID, failedExecID, stepRunID, triggerReason string, _ *audit.Entry) error {
	r.calls = append(r.calls, recoveryTriggerCall{
		tenantID: tenantID, taskID: taskID, failedExecID: failedExecID, stepRunID: stepRunID, reason: triggerReason,
	})
	return nil
}

// countStepRuns returns how many workflow_step_runs rows exist for a given
// step id in a run (so we can assert no iteration flood).
func countStepRuns(t *testing.T, pool *db.Pool, tenantID, runID, stepID string) int {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	var n int
	if err := ttx.QueryRow(ctx,
		`SELECT count(*) FROM workflow_step_runs WHERE tenant_id=$1 AND workflow_run_id=$2 AND step_id=$3`,
		tenantID, runID, stepID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// getStepRunResult fetches the Result jsonb of the first step run for a step.
func getStepRunResult(t *testing.T, pool *db.Pool, tenantID, runID, stepID string) []byte {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	var res []byte
	if err := ttx.QueryRow(ctx,
		`SELECT result FROM workflow_step_runs WHERE tenant_id=$1 AND workflow_run_id=$2 AND step_id=$3 ORDER BY created_at, id LIMIT 1`,
		tenantID, runID, stepID).Scan(&res); err != nil {
		t.Fatal(err)
	}
	return res
}

// getActiveStepRunStatus fetches the status of the single non-superseded
// active step run for a step id.
func getActiveStepRunStatus(t *testing.T, pool *db.Pool, tenantID, runID, stepID string) string {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	var status string
	if err := ttx.QueryRow(ctx,
		`SELECT status FROM workflow_step_runs WHERE tenant_id=$1 AND workflow_run_id=$2 AND step_id=$3 AND (superseded_by IS NULL OR superseded_by='') ORDER BY created_at, id LIMIT 1`,
		tenantID, runID, stepID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

// newReconcileRun seeds a project, workflow steps, a running work item, and
// one workflow run, then returns the pool, reconciler, run, and ticket. The
// provided seedStepRuns callback creates the per-step step runs inside the
// same transaction as the run.
func newReconcileRun(t *testing.T, trigger *recordingRecoveryTrigger, steps []workflow.StepWire, seedStepRuns func(t *testing.T, ttx *db.TenantTx, run db.WorkflowRunRow, ticket db.WorkItemRow)) (*db.Pool, *WorkflowReconciler, db.WorkflowRunRow, db.WorkItemRow) {
	t.Helper()
	pool := approvalTestPool(t)
	proj := createStuckTestProject(t, pool)
	stepsJSON, _ := json.Marshal(steps)
	wfID := seedPublishedWorkflowSteps(t, pool, proj.ID, string(stepsJSON))

	ctx := context.Background()
	ttxSeed, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := db.CreateWorkItem(ctx, ttxSeed.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		ProjectID: proj.ID, Kind: domain.WorkItemKindTask,
		Title: "the ticket", Description: "t", AcceptanceCriteria: "t",
		Status: domain.WorkItemRunning,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ttxSeed.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	run, err := db.CreateWorkflowRun(ctx, ttx.Tx, db.WorkflowRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		WorkflowID: wfID, WorkflowVersion: 1,
		ProjectID: proj.ID, Status: domain.WorkflowRunRunning,
		RunContext: []byte("{}"), WorkItemID: ticket.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	// CreateWorkflowRun does not persist runtime_ready (DB-default false), so
	// the mid-pass runtime-gate flip (workflow_reconciler.go:455) would bump
	// run.Version without refreshing `run`, leaving the later terminal-fail
	// update (line 1154) on a stale version → "db: not found". Headless tests
	// are always-serve-ready, so set the flag and move one version ahead.
	run, err = db.UpdateWorkflowRun(ctx, ttx.Tx, approvalTestTenant, run.ID, run.Version, db.UpdateWorkflowRunFields{
		RuntimeReady: boolPtr(true),
	})
	if err != nil {
		t.Fatal(err)
	}
	seedStepRuns(t, ttx, run, ticket)
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	rc := NewWorkflowReconciler(pool, logger, nil, nil, trigger, nil)
	return pool, rc, run, ticket
}

// createRunningFailedStepRun seeds a running task step run whose linked
// execution is terminal-failed (the poll-failure recovery path) with a
// summarize_restart recovery config. Returns the failed execution id.
func createRunningFailedStepRun(t *testing.T, ttx *db.TenantTx, run db.WorkflowRunRow, ticket db.WorkItemRow, stepID string) string {
	t.Helper()
	ctx := context.Background()
	execID := db.NewID()
	if _, err := db.CreateExecution(ctx, ttx.Tx, db.ExecutionRow{
		ID: execID, TenantID: approvalTestTenant, ProjectID: run.ProjectID,
		TaskID: ticket.ID, WorkerID: "w_se_devops_engineer", WorkerVersion: 1,
		Status:        domain.ExecutionFailed,
		WorkflowRunID: run.ID, WorkflowStepID: stepID,
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	resB, _ := json.Marshal(map[string]any{
		"_work_item_id":   ticket.ID,
		"_worker_id":      "w_se_devops_engineer",
		"_worker_version": "1",
	})
	if _, err := db.CreateWorkflowStepRun(ctx, ttx.Tx, db.WorkflowStepRunRow{
		ID:                db.NewID(),
		TenantID:          approvalTestTenant,
		WorkflowRunID:     run.ID,
		StepID:            stepID,
		StepName:          "Task Step",
		StepKind:          domain.StepKindTask,
		Status:            domain.StepRunRunning,
		WorkerExecutionID: execID,
		Result:            resB,
		StartedAt:         &now,
	}); err != nil {
		t.Fatal(err)
	}
	return execID
}

// createPendingStepRun seeds a single pending step run of the given kind.
func createPendingStepRun(t *testing.T, ttx *db.TenantTx, run db.WorkflowRunRow, stepID, name, kind, config string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := db.CreateWorkflowStepRun(ctx, ttx.Tx, db.WorkflowStepRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant, WorkflowRunID: run.ID,
		StepID: stepID, StepName: name, StepKind: kind,
		Status: domain.StepRunPending, Result: []byte("{}"), StartedAt: &now,
	}); err != nil {
		t.Fatal(err)
	}
}

// TestFailureThenLoopDecisionRecoveryTriggerRace is AC#9 (a): a running
// summarize_restart step whose execution fails must transition to
// `recovering` with `_failed_execution_id` persisted, and stage a
// post-commit `step_recovery` trigger within one reconcile pass — even when
// a loop_decision sits downstream — and must NOT flood a loop iteration.
func TestFailureThenLoopDecisionRecoveryTriggerRace(t *testing.T) {
	trigger := &recordingRecoveryTrigger{}
	steps := []workflow.StepWire{
		{ID: "st", Name: "SDLC Dev", Kind: "task", DependsOn: []string{},
			Config: `{"recovery":{"strategy":"summarize_restart"}}`, Ref: "w_se_devops_engineer"},
		{ID: "loop", Name: "Loop Decision", Kind: "loop_decision", DependsOn: []string{"st"},
			Config: `{"loop_branch":"st","success_branch":"qa","max_iterations":6,"max_reask":2}`},
		{ID: "qa", Name: "QA", Kind: "task", DependsOn: []string{"loop"}, Ref: "w_se_qa_engineer"},
	}

	pool, rc, run, _ := newReconcileRun(t, trigger, steps, func(t *testing.T, ttx *db.TenantTx, run db.WorkflowRunRow, ticket db.WorkItemRow) {
		createRunningFailedStepRun(t, ttx, run, ticket, "st")
		createPendingStepRun(t, ttx, run, "loop", "Loop Decision", domain.StepKindLoopDecision, "")
		createPendingStepRun(t, ttx, run, "qa", "QA", domain.StepKindTask, "")
	})

	if err := rc.reconcileRun(context.Background(), approvalTestTenant, run.ID); err != nil {
		t.Fatalf("reconcileRun: %v", err)
	}

	if got := getActiveStepRunStatus(t, pool, approvalTestTenant, run.ID, "st"); got != domain.StepRunRecovering {
		t.Fatalf("step not recovering after failure poll, got %q", got)
	}
	var res map[string]any
	if err := json.Unmarshal(getStepRunResult(t, pool, approvalTestTenant, run.ID, "st"), &res); err != nil {
		t.Fatal(err)
	}
	if res["_failed_execution_id"] == nil || res["_failed_execution_id"] == "" {
		t.Fatalf("_failed_execution_id not persisted in recovering result: %v", res)
	}
	if res["_recovering_since"] == nil || res["_recovering_since"] == "" {
		t.Fatalf("_recovering_since not stamped in recovering result: %v", res)
	}

	if len(trigger.calls) != 1 {
		t.Fatalf("expected exactly 1 staged recovery trigger, got %d", len(trigger.calls))
	}
	if trigger.calls[0].reason != "step_recovery" {
		t.Fatalf("expected trigger reason step_recovery, got %q", trigger.calls[0].reason)
	}
	if trigger.calls[0].failedExecID == "" {
		t.Fatalf("trigger missing failed_exec_id")
	}
	// The downstream loop_decision must not have spawned an iteration flood.
	if got := countStepRuns(t, pool, approvalTestTenant, run.ID, "loop"); got != 1 {
		t.Fatalf("loop_decision spawned %d iterations, want exactly 1 (no flood)", got)
	}
}

// TestMaxDAGPassesCommitsRecoveryNotRollback is AC#9 (b): at maxDAGPasses
// the reconciler must COMMIT the pass's accumulated progress (the
// `recovering` transition + staged recovery trigger) rather than silently
// roll it back with a DAG-pass-limit error.
func TestMaxDAGPassesCommitsRecoveryNotRollback(t *testing.T) {
	t.Setenv("ORCHICON_MAX_DAG_PASSES", "1")
	trigger := &recordingRecoveryTrigger{}
	steps := []workflow.StepWire{
		{ID: "sel", Name: "Select", Kind: domain.StepKindParallel, DependsOn: []string{}},
		{ID: "st", Name: "SDLC Dev", Kind: "task", DependsOn: []string{"sel"},
			Config: `{"recovery":{"strategy":"summarize_restart"}}`, Ref: "w_se_devops_engineer"},
		{ID: "qa", Name: "QA", Kind: "task", DependsOn: []string{"st"}, Ref: "w_se_qa_engineer"},
	}

	pool, rc, run, _ := newReconcileRun(t, trigger, steps, func(t *testing.T, ttx *db.TenantTx, run db.WorkflowRunRow, ticket db.WorkItemRow) {
		createPendingStepRun(t, ttx, run, "sel", "Select", domain.StepKindParallel, "")
		createRunningFailedStepRun(t, ttx, run, ticket, "st")
		createPendingStepRun(t, ttx, run, "qa", "QA", domain.StepKindTask, "")
	})

	// A maxDAGPasses abort must NOT return an error that rolls back the
	// recovering write — the whole acceptance criterion is that reconcileRun
	// cleanly commits the pass's progress.
	if err := rc.reconcileRun(context.Background(), approvalTestTenant, run.ID); err != nil {
		t.Fatalf("reconcileRun must not error on DAG pass limit (that would roll back recovery writes), got: %v", err)
	}

	if got := getActiveStepRunStatus(t, pool, approvalTestTenant, run.ID, "st"); got != domain.StepRunRecovering {
		t.Fatalf("recovering transition was rolled back by the pass-limit break, step status = %q", got)
	}
	if got := getActiveStepRunStatus(t, pool, approvalTestTenant, run.ID, "sel"); got != domain.StepRunSucceeded {
		t.Fatalf("parallel dispatch progress was rolled back, sel status = %q", got)
	}
	if len(trigger.calls) != 1 || trigger.calls[0].reason != "step_recovery" {
		t.Fatalf("recovery trigger was rolled back after pass limit, calls = %+v", trigger.calls)
	}
}

// TestLoopDecisionUpstreamFailedNoIterationFlood is AC#9 (c): an
// upstream-failed loop_decision creates at most one recovering/loop
// iteration per failed-upstream reconcile scan and no cycles across scans.
func TestLoopDecisionUpstreamFailedNoIterationFlood(t *testing.T) {
	trigger := &recordingRecoveryTrigger{}
	steps := []workflow.StepWire{
		{ID: "review", Name: "PR Reviewer", Kind: "task", DependsOn: []string{}, Ref: "w_se_devops_engineer"},
		{ID: "loop", Name: "Loop Decision", Kind: "loop_decision", DependsOn: []string{"review"},
			Config: `{"loop_branch":"review","success_branch":"qa","max_iterations":6,"max_reask":2}`},
		{ID: "qa", Name: "QA", Kind: "task", DependsOn: []string{"loop"}, Ref: "w_se_qa_engineer"},
	}

	pool, rc, run, _ := newReconcileRun(t, trigger, steps, func(t *testing.T, ttx *db.TenantTx, run db.WorkflowRunRow, ticket db.WorkItemRow) {
		ctx := context.Background()
		now := time.Now().UTC()
		failedExecID := db.NewID()
		if _, err := db.CreateExecution(ctx, ttx.Tx, db.ExecutionRow{
			ID: failedExecID, TenantID: approvalTestTenant, ProjectID: run.ProjectID,
			TaskID: ticket.ID, WorkerID: "w_se_devops_engineer", WorkerVersion: 1,
			Status:        domain.ExecutionFailed,
			WorkflowRunID: run.ID, WorkflowStepID: "review",
		}); err != nil {
			t.Fatal(err)
		}
		// Upstream PR reviewer terminal-failed with a bound ticket: this is
		// what drives the loop_decision "upstream failed" branch. It must
		// carry a WorkerExecutionID + _work_item_id so the branch can bind
		// the recovery trigger to the failed execution.
		resB, _ := json.Marshal(map[string]any{
			"_work_item_id": ticket.ID, "_worker_id": "w_se_devops_engineer", "_worker_version": "1",
		})
		if _, err := db.CreateWorkflowStepRun(ctx, ttx.Tx, db.WorkflowStepRunRow{
			ID: db.NewID(), TenantID: approvalTestTenant, WorkflowRunID: run.ID,
			StepID: "review", StepName: "PR Reviewer", StepKind: domain.StepKindTask,
			Status: domain.StepRunFailed, WorkerExecutionID: failedExecID,
			Result: resB, StartedAt: &now,
		}); err != nil {
			t.Fatal(err)
		}
		createPendingStepRun(t, ttx, run, "loop", "Loop Decision", domain.StepKindLoopDecision, "")
		createPendingStepRun(t, ttx, run, "qa", "QA", domain.StepKindTask, "")
	})

	ctx := context.Background()
	if err := rc.reconcileRun(ctx, approvalTestTenant, run.ID); err != nil {
		t.Fatalf("reconcileRun #1: %v", err)
	}
	// Exactly one recovering/loop iteration per failed-upstream scan: the
	// original iter0 is superseded by a single new iteration → 2 rows total.
	if got := countStepRuns(t, pool, approvalTestTenant, run.ID, "loop"); got != 2 {
		t.Fatalf("first scan created %d loop iterations, want 2 (original + one iteration)", got)
	}
	if len(trigger.calls) != 1 {
		t.Fatalf("expected 1 staged loop_decision trigger, got %d", len(trigger.calls))
	}
	if trigger.calls[0].reason != "loop_decision:upstream_failed" {
		t.Fatalf("expected loop_decision:upstream_failed reason, got %q", trigger.calls[0].reason)
	}
	if trigger.calls[0].failedExecID == "" {
		t.Fatalf("loop_decision trigger missing failed_exec_id")
	}

	// Re-scan: no new iteration must be spawned despite the still-failed
	// upstream (the create-once scan guard + Iteration>0 hold).
	if err := rc.reconcileRun(ctx, approvalTestTenant, run.ID); err != nil {
		t.Fatalf("reconcileRun #2: %v", err)
	}
	if got := countStepRuns(t, pool, approvalTestTenant, run.ID, "loop"); got != 2 {
		t.Fatalf("second scan spawned %d loop iterations, want still 2 (no flood/cycle)", got)
	}
	if len(trigger.calls) != 1 {
		t.Fatalf("second scan re-fired a trigger, calls = %d, want 1", len(trigger.calls))
	}
}
