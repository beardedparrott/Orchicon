package scheduler

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/workflow"
)

// conflictLazySteps is the conflict-aware DAG used by the laziness tests:
// merge (DevOps) → gate (loop_decision) with an explicit conflict_chain, and
// a free-floating Integrator task wired ONLY through that chain (no static
// depends_on edge anywhere).
func conflictLazySteps() []workflow.StepWire {
	return []workflow.StepWire{
		{ID: "merge", Name: "DevOps Merge", Kind: "task", Ref: "w_se_devops_engineer", DependsOn: []string{}},
		{ID: "gate", Name: "Merge Gate", Kind: domain.StepKindLoopDecision, DependsOn: []string{"merge"},
			Config: `{"loop_branch":"merge","max_iterations":3,"conflict_value":"conflict","conflict_chain":["integrator"],"exhausted_review":"human"}`},
		{ID: "integrator", Name: "Integrator", Kind: "task", Ref: "w_se_integrator", DependsOn: []string{}},
	}
}

// newConflictLazyRun seeds a running run (bound to a ticket, runtime ready)
// on the conflict-aware workflow with the given per-step seed statuses, so a
// single reconcileRun pass drives the DAG. Returns the run + the reconciler.
func newConflictLazyRun(t *testing.T, pool *db.Pool, seed map[string]db.WorkflowStepRunRow) (db.WorkflowRunRow, *WorkflowReconciler, *instantDispatcher) {
	t.Helper()
	ctx := context.Background()
	proj := createStuckTestProject(t, pool)
	stepsJSON, _ := json.Marshal(conflictLazySteps())
	wfID := seedPublishedWorkflowSteps(t, pool, proj.ID, string(stepsJSON))

	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	ticket, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: approvalTestTenant, ProjectID: proj.ID,
		Kind: domain.WorkItemKindTask, Title: "conflict lazy ticket",
		Description: "the shared input reference", AcceptanceCriteria: "merge lands",
		Status: domain.WorkItemRunning,
	})
	if err != nil {
		t.Fatalf("create ticket: %v", err)
	}
	run, err := db.CreateWorkflowRun(ctx, ttx.Tx, db.WorkflowRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant, WorkflowID: wfID,
		WorkflowVersion: 1, ProjectID: proj.ID, Status: domain.WorkflowRunRunning,
		RunContext: []byte("{}"), WorkItemID: ticket.ID,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	run, err = db.UpdateWorkflowRun(ctx, ttx.Tx, approvalTestTenant, run.ID, run.Version, db.UpdateWorkflowRunFields{
		RuntimeReady: boolPtr(true),
	})
	if err != nil {
		t.Fatalf("set runtime ready: %v", err)
	}
	if _, err := db.UpdateWorkItem(ctx, ttx.Tx, approvalTestTenant, ticket.ID, ticket.Version, db.UpdateWorkItemFields{
		WorkflowRunID: &run.ID,
	}); err != nil {
		t.Fatalf("bind ticket: %v", err)
	}
	for stepID, seedSR := range seed {
		name := stepID
		kind := "task"
		for _, s := range conflictLazySteps() {
			if s.ID == stepID {
				name = s.Name
				kind = s.Kind
			}
		}
		if _, err := db.CreateWorkflowStepRun(ctx, ttx.Tx, db.WorkflowStepRunRow{
			ID: db.NewID(), TenantID: approvalTestTenant, WorkflowRunID: run.ID,
			StepID: stepID, StepName: name, StepKind: kind,
			Status: seedSR.Status, Iteration: seedSR.Iteration, Result: seedSR.Result,
		}); err != nil {
			t.Fatalf("create step run %s: %v", stepID, err)
		}
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit fixture: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	rec := NewWorkflowReconciler(pool, logger, nil, nil, nil, nil)
	disp := &instantDispatcher{}
	rec.taskDispatcher = disp
	return run, rec, disp
}

func getConflictStepRun(t *testing.T, pool *db.Pool, runID, stepID string) db.WorkflowStepRunRow {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	all, err := db.ListWorkflowStepRuns(ctx, ttx.Tx, approvalTestTenant, runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, sr := range all {
		if sr.StepID == stepID && sr.SupersededBy == "" {
			return sr
		}
	}
	t.Fatalf("no active run for step %s", stepID)
	return db.WorkflowStepRunRow{}
}

// TestReconcileHoldsSeededIntegratorOnCleanPass is the AC#1 regression test:
// a statically-seeded PENDING Integrator run (iteration 0 — the shape StartRun
// produced before the laziness fix) must be HELD on a clean pass. The merge
// reports success, the gate accepts, and the Integrator stays PENDING — it
// must never reach READY or dispatch when there is no conflict.
func TestReconcileHoldsSeededIntegratorOnCleanPass(t *testing.T) {
	pool := approvalTestPool(t)
	ctx := context.Background()

	seed := map[string]db.WorkflowStepRunRow{
		"merge":      {Status: domain.StepRunSucceeded, Result: []byte(`{"_decision":"success"}`)},
		"gate":       {Status: domain.StepRunPending},
		"integrator": {Status: domain.StepRunPending, Iteration: 0},
	}
	run, rec, disp := newConflictLazyRun(t, pool, seed)
	if err := rec.reconcileRun(ctx, approvalTestTenant, run.ID); err != nil {
		t.Fatalf("reconcile clean pass: %v", err)
	}

	gate := getConflictStepRun(t, pool, run.ID, "gate")
	if gate.Status != domain.StepRunSucceeded {
		t.Errorf("gate status = %q, want succeeded (clean merge accepted)", gate.Status)
	}
	integrator := getConflictStepRun(t, pool, run.ID, "integrator")
	if integrator.Status != domain.StepRunPending {
		t.Errorf("Integrator status = %q, want pending — a seeded (iteration 0) run must be held, never dispatched on a clean pass", integrator.Status)
	}
	if got := disp.count(); got != 0 {
		t.Errorf("dispatched %d step runs on a clean pass, want 0 (Integrator must not run without a conflict)", got)
	}
}

// TestReconcileDispatchesIntegratorOnConflictReentry verifies the gate only
// holds iteration-0 (seeded) runs: a PENDING Integrator run created by a
// conflict re-entry (iteration 1) IS progressed to READY and dispatched,
// routing the Integrator worker to resolve the conflict.
func TestReconcileDispatchesIntegratorOnConflictReentry(t *testing.T) {
	pool := approvalTestPool(t)
	ctx := context.Background()

	seed := map[string]db.WorkflowStepRunRow{
		"merge":      {Status: domain.StepRunSucceeded, Result: []byte(`{"_decision":"conflict"}`)},
		"gate":       {Status: domain.StepRunRunning},
		"integrator": {Status: domain.StepRunPending, Iteration: 1},
	}
	run, rec, disp := newConflictLazyRun(t, pool, seed)
	if err := rec.reconcileRun(ctx, approvalTestTenant, run.ID); err != nil {
		t.Fatalf("reconcile conflict pass: %v", err)
	}

	integrator := getConflictStepRun(t, pool, run.ID, "integrator")
	if integrator.Status != domain.StepRunRunning {
		t.Errorf("Integrator status = %q, want running (conflict re-entry run must dispatch)", integrator.Status)
	}
	if got := disp.count(); got != 1 {
		t.Errorf("dispatched %d step runs, want exactly 1 (the Integrator)", got)
	}
}
