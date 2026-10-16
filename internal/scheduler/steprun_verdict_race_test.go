package scheduler

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
)

// stepRunVerdictRaceTest — a terminal step run must never be recorded WITHOUT
// the verdict its execution produced.
//
// THE DEFECT (measured, not theorised). A step run's `_decision` is written by
// the TaskReconciler (propagateStepRunResults, on the execution's completion)
// while the WORKFLOW reconciler marks the step run terminal from the
// execution's STATUS, in its own pass. Two reconcilers, two independent scan
// loops, so there is a window in which the step run is `succeeded` and its
// Result still carries only the dispatch-time keys.
//
// A loop_decision reading it in that window finds no verdict, falls through to
// the missing-decision branch, and RE-ASKS the reviewer for a decision the
// reviewer had already given. On run 01M522422MB39XNM0R6819JGXM (SDLC
// non-human) every loop_decision iteration was created 47-108ms BEFORE the
// reviewer step run received its `_decision` (loop1 10:32:33.873 vs rev1
// 10:32:33.981; loop3 10:47:41.073 vs rev2 10:47:41.120). All three re-asks
// fired on verdicts that already existed, consumed max_reask (default 3), and
// FAILED the run — while every reviewer execution had itself succeeded and
// signed off with `ORCHICON WORKER SUMMARY: success`.
//
// These tests pin the two halves of the fix:
//  1. the helper folds the execution's verdict into the terminal mark, and
//  2. an ALREADY-WRITTEN verdict is never clobbered (the merge is additive, so
//     the two writers cannot disagree).
func TestStepRunResultWithVerdictCarriesTheExecutionDecision(t *testing.T) {
	pool, _, run, ticket := newReconcileRun(t, nil, nil, func(t *testing.T, ttx *db.TenantTx, run db.WorkflowRunRow, ticket db.WorkItemRow) {})
	ctx := context.Background()

	const stepID = "step-reviewer"
	execID := db.NewID()
	// A step run whose Result carries ONLY dispatch-time keys — exactly the
	// state the race leaves behind — whose execution succeeded with a signed-off
	// summary.
	dispatchOnly, _ := json.Marshal(map[string]any{
		"_work_item_id":   ticket.ID,
		"_worker_id":      "w_se_pr_reviewer",
		"_worker_version": 1,
	})

	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	exec, err := db.CreateExecution(ctx, ttx.Tx, db.ExecutionRow{
		ID: execID, TenantID: approvalTestTenant, ProjectID: run.ProjectID,
		TaskID: ticket.ID, WorkerID: "w_se_pr_reviewer", WorkerName: "PR Reviewer", WorkerVersion: 1,
		Status:        domain.ExecutionSucceeded,
		WorkflowRunID: run.ID, WorkflowStepID: stepID,
	})
	if err != nil {
		t.Fatal(err)
	}
	// CreateExecution does NOT persist `output` (db/execution.go:55-86 -- the
	// column is absent from both the INSERT and its RETURNING); production
	// writes it through UpdateExecution when the execution completes. Mirror
	// that, or this fixture would prove nothing about the real path.
	out := "All gates green.\n\nORCHICON WORKER SUMMARY: success — reviewed the change.\n"
	if _, err := db.UpdateExecution(ctx, ttx.Tx, approvalTestTenant, exec.ID, exec.Version,
		db.UpdateExecutionFields{Output: &out}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	srID := db.NewID()
	sr, err := db.CreateWorkflowStepRun(ctx, ttx.Tx, db.WorkflowStepRunRow{
		ID: srID, TenantID: approvalTestTenant, WorkflowRunID: run.ID,
		StepID: stepID, StepName: "PR Reviewer", StepKind: domain.StepKindTask,
		Status: domain.StepRunRunning, WorkerExecutionID: execID,
		Result: dispatchOnly, StartedAt: &now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ttx.Commit(ctx) != nil {
		t.Fatal("commit")
	}

	rc := NewWorkflowReconciler(pool, slog.New(slog.NewTextHandler(os.Stderr, nil)), nil, nil, nil, nil)

	// 1. The helper folds the verdict in — this is the half that closes the race.
	ttx2, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx2.Rollback(ctx)
	merged, err := rc.stepRunResultWithVerdict(ctx, ttx2.Tx, approvalTestTenant, sr)
	if err != nil {
		t.Fatalf("stepRunResultWithVerdict: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(merged, &got); err != nil {
		t.Fatalf("decode merged result: %v", err)
	}
	if got["_decision"] != "success" {
		t.Errorf("_decision = %v, want \"success\" — a terminal step run must not be recorded without its execution's verdict; without this the loop re-asks a reviewer that already answered", got["_decision"])
	}
	if got["_summary"] == nil || got["_summary"] == "" {
		t.Error("_summary was not carried — the loop's failure narrative and the run view read it")
	}
	// The dispatch-time keys must survive: the merge is additive, not a replace.
	if got["_work_item_id"] != ticket.ID {
		t.Errorf("_work_item_id = %v, want %q (the merge must not drop the dispatch keys)", got["_work_item_id"], ticket.ID)
	}
}

// TestStepRunResultWithVerdictDoesNotClobberAnExistingDecision guards the OTHER
// writer. The TaskReconciler may already have written the verdict (it won the
// race); the helper must leave it untouched rather than re-deriving it, so the
// two paths can never disagree about what the reviewer said.
func TestStepRunResultWithVerdictDoesNotClobberAnExistingDecision(t *testing.T) {
	pool, _, run, ticket := newReconcileRun(t, nil, nil, func(t *testing.T, ttx *db.TenantTx, run db.WorkflowRunRow, ticket db.WorkItemRow) {})
	ctx := context.Background()

	const stepID = "step-reviewer"
	execID := db.NewID()
	// The step run ALREADY carries a decision; the execution's stdout says the
	// opposite. The recorded verdict must win.
	already, _ := json.Marshal(map[string]any{
		"_work_item_id": ticket.ID,
		"_decision":     "failure",
		"_summary":      "the recorded verdict",
	})

	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	exec, err := db.CreateExecution(ctx, ttx.Tx, db.ExecutionRow{
		ID: execID, TenantID: approvalTestTenant, ProjectID: run.ProjectID,
		TaskID: ticket.ID, WorkerID: "w_se_pr_reviewer", WorkerVersion: 1,
		Status:        domain.ExecutionSucceeded,
		WorkflowRunID: run.ID, WorkflowStepID: stepID,
	})
	if err != nil {
		t.Fatal(err)
	}
	out := "ORCHICON WORKER SUMMARY: success — a LATER, contradictory marker.\n"
	if _, err := db.UpdateExecution(ctx, ttx.Tx, approvalTestTenant, exec.ID, exec.Version,
		db.UpdateExecutionFields{Output: &out}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	srID := db.NewID()
	sr, err := db.CreateWorkflowStepRun(ctx, ttx.Tx, db.WorkflowStepRunRow{
		ID: srID, TenantID: approvalTestTenant, WorkflowRunID: run.ID,
		StepID: stepID, StepName: "PR Reviewer", StepKind: domain.StepKindTask,
		Status: domain.StepRunRunning, WorkerExecutionID: execID,
		Result: already, StartedAt: &now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ttx.Commit(ctx) != nil {
		t.Fatal("commit")
	}

	rc := NewWorkflowReconciler(pool, slog.New(slog.NewTextHandler(os.Stderr, nil)), nil, nil, nil, nil)
	ttx2, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx2.Rollback(ctx)
	merged, err := rc.stepRunResultWithVerdict(ctx, ttx2.Tx, approvalTestTenant, sr)
	if err != nil {
		t.Fatalf("stepRunResultWithVerdict: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(merged, &got); err != nil {
		t.Fatalf("decode merged result: %v", err)
	}
	if got["_decision"] != "failure" {
		t.Errorf("_decision = %v, want the RECORDED \"failure\" — an existing verdict must never be overwritten by a re-derivation", got["_decision"])
	}
	if got["_summary"] != "the recorded verdict" {
		t.Errorf("_summary = %v, want the recorded summary", got["_summary"])
	}
}

// TestStepRunResultWithVerdictHandlesNoExecution: a step run with no execution
// link (a dispatch that never produced one) must be a no-op, not an error — the
// terminal mark still has to land so the step does not wedge.
func TestStepRunResultWithVerdictHandlesNoExecution(t *testing.T) {
	pool, _, run, ticket := newReconcileRun(t, nil, nil, func(t *testing.T, ttx *db.TenantTx, run db.WorkflowRunRow, ticket db.WorkItemRow) {})
	ctx := context.Background()

	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	srID := db.NewID()
	sr, err := db.CreateWorkflowStepRun(ctx, ttx.Tx, db.WorkflowStepRunRow{
		ID: srID, TenantID: approvalTestTenant, WorkflowRunID: run.ID,
		StepID: "step-noexec", StepName: "Task Step", StepKind: domain.StepKindTask,
		Status: domain.StepRunRunning, StartedAt: &now,
		Result: []byte(`{"_work_item_id":"` + ticket.ID + `"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if ttx.Commit(ctx) != nil {
		t.Fatal("commit")
	}

	rc := NewWorkflowReconciler(pool, slog.New(slog.NewTextHandler(os.Stderr, nil)), nil, nil, nil, nil)
	ttx2, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx2.Rollback(ctx)
	merged, err := rc.stepRunResultWithVerdict(ctx, ttx2.Tx, approvalTestTenant, sr)
	if err != nil {
		t.Fatalf("a step run with no execution must not error: %v", err)
	}
	if merged != nil {
		t.Errorf("merged = %s, want nil (nothing to merge, caller keeps the existing Result)", merged)
	}
}
