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
	"github.com/beardedparrott/orchicon/internal/workflow"
)

// Regression test for the wedge that stalled a prod run (Task 2) at the
// PR-reviewer → loop_decision junction:
//
// A loop_decision step can have a SUPERSEDED iteration and an ACTIVE
// iteration created in the SAME transaction (the upstream-failed branch
// creates a new pending iteration and supersedes the current one in one
// pass) — so both rows share `created_at`. reconcileRun built runByID as
// "last row in created_at order wins", and with ORDER BY created_at ASC
// (no tiebreaker) the SUPERSEDED succeeded run could shadow the ACTIVE
// pending run. Then:
//   - the ready-phase substituted the active pending iteration with the
//     superseded succeeded one → skipped, never marked ready,
//   - depsSatisfied for downstream steps saw the superseded row → returned
//     false → QA/approval/merge never dispatched,
//   - the run sat "running" forever (terminal check hid the pending row).
//
// Fix: ListWorkflowStepRuns orders by created_at, id and runByID prefers
// non-superseded rows. This test seeds the exact tie (identical created_at
// on superseded + active loop iterations) and asserts the reconciler
// progresses the ACTIVE iteration to dispatched/terminal instead of wedging.
func TestReconcileRunProgressesLoopDecisionActiveIteration(t *testing.T) {
	pool := approvalTestPool(t)
	proj := createStuckTestProject(t, pool)

	steps := []workflow.StepWire{
		{ID: "review", Name: "PR Reviewer", Kind: "task", DependsOn: []string{}},
		{ID: "loop", Name: "Loop Decision", Kind: "loop_decision", DependsOn: []string{"review"},
			Config: `{"loop_branch":"review","success_branch":"qa","max_iterations":6,"max_reask":2}`},
		{ID: "qa", Name: "QA", Kind: "task", DependsOn: []string{"loop"}, Ref: "w_se_qa_engineer"},
		{ID: "approve", Name: "Approve", Kind: "approval", DependsOn: []string{"qa"},
			Config: `{"reviewer":"worker","worker_ref":"w_se_code_approver","max_iterations":3}`},
		{ID: "merge", Name: "Merge", Kind: "task", DependsOn: []string{"approve"}, Ref: "w_se_qa_engineer"},
	}
	stepsJSON, _ := json.Marshal(steps)
	wfID := seedPublishedWorkflowSteps(t, pool, proj.ID, string(stepsJSON))

	ctx := context.Background()

	// The downstream QA task step needs a bound work item to dispatch
	// against (dispatchStep resolves the work item from run.WorkItemID
	// when there is no WORK_ITEM marker upstream). Without it QA fails
	// "no upstream work_item" and the run is marked failed — which is
	// independent of the same-created_at tie this test exercises. Give
	// the run a ticket so the DAG can progress through the loop.
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
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	rc := NewWorkflowReconciler(pool, logger, nil, nil, nil, nil)

	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)

	run, err := db.CreateWorkflowRun(ctx, ttx.Tx, db.WorkflowRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		WorkflowID: wfID, WorkflowVersion: 1,
		ProjectID: proj.ID, Status: domain.WorkflowRunRunning,
		RunContext: []byte("{}"), WorkItemID: ticket.ID,
	})
	if err != nil {
		t.Fatal(err)
	}

	started := time.Now().UTC().Add(-time.Minute)
	// The exact same timestamp for the superseded and active loop iterations
	// reproduces the "last row wins" tie.
	tie := time.Now().UTC()
	newStepRun := func(stepID, name, kind, status string, iter int, supersededBy string, result []byte) string {
		id := db.NewID()
		if _, err := db.CreateWorkflowStepRun(ctx, ttx.Tx, db.WorkflowStepRunRow{
			ID: id, TenantID: approvalTestTenant, WorkflowRunID: run.ID,
			StepID: stepID, StepName: name, StepKind: kind,
			Status: status, Iteration: iter, SupersededBy: supersededBy,
			Result: result, StartedAt: &started,
		}); err != nil {
			t.Fatal(err)
		}
		// Force the exact tie: same created_at on the superseded + active
		// loop iterations (the DB default now() would differ by µs).
		if stepID == "loop" {
			if _, err := ttx.Exec(ctx,
				`UPDATE workflow_step_runs SET created_at = $1 WHERE id = $2`, tie, id); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}

	supersededLoop := newStepRun("loop", "Loop Decision", "loop_decision", domain.StepRunSucceeded, 2, "", []byte("{}"))
	// Superseded loop iteration 3 — same created_at as the active one below.
	supersededLoop3 := newStepRun("loop", "Loop Decision", "loop_decision", domain.StepRunSucceeded, 3, "", []byte("{}"))
	activeLoop := newStepRun("loop", "Loop Decision", "loop_decision", domain.StepRunPending, 4, "", []byte("{}"))

	// Chain the supersession forward: iter2 → iter3 → active (only the
	// active is non-superseded), mirroring the prod run's loop history.
	if _, err := ttx.Exec(ctx,
		`UPDATE workflow_step_runs SET superseded_by = $1 WHERE id = $2`,
		supersededLoop3, supersededLoop); err != nil {
		t.Fatal(err)
	}
	if _, err := ttx.Exec(ctx,
		`UPDATE workflow_step_runs SET superseded_by = $1 WHERE id = $2`,
		activeLoop, supersededLoop3); err != nil {
		t.Fatal(err)
	}

	// Upstream reviewer succeeded with a success decision (the DAG should
	// now proceed forward through the loop to QA).
	newStepRun("review", "PR Reviewer", "task", domain.StepRunSucceeded, 0, "", []byte(`{"_decision":"success"}`))
	// Downstream steps pending.
	newStepRun("qa", "QA", "task", domain.StepRunPending, 0, "", []byte("{}"))
	newStepRun("approve", "Approve", "approval", domain.StepRunPending, 0, "", []byte("{}"))
	newStepRun("merge", "Merge", "task", domain.StepRunPending, 0, "", []byte("{}"))

	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// Reconcile: the active pending loop iteration must be progressed (its
	// upstream succeeded), NOT wedged by the superseded same-created_at row.
	if err := rc.reconcileRun(ctx, approvalTestTenant, run.ID); err != nil {
		t.Fatalf("reconcileRun: %v", err)
	}

	// The active loop iteration must no longer be pending (it should have
	// dispatched and re-evaluated — the loop_decision with an upstream
	// "success" decision proceeds forward). Fetch its state.
	ttx2, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx2.Rollback(ctx)
	var activeStatus string
	if err := ttx2.QueryRow(ctx,
		`SELECT status FROM workflow_step_runs WHERE id = $1 AND tenant_id = $2`,
		activeLoop, approvalTestTenant).Scan(&activeStatus); err != nil {
		t.Fatalf("fetch active loop iteration: %v", err)
	}
	if activeStatus == domain.StepRunPending {
		t.Fatalf("active loop iteration still pending after reconcile — the superseded same-created_at run shadowed it (the wedge)")
	}
}
