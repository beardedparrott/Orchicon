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

// TestConflictReenterCreatesIntegratorChain verifies the conflict routing:
// when the merge step reports a `conflict` decision, the merge gate
// (loop_decision with a conflict_chain) re-enters the chain by creating a
// PENDING Integrator run and marking the gate RUNNING — the Integrator is a
// lazy step (not a static DAG ancestor) so it never ran on the first pass.
func TestConflictReenterCreatesIntegratorChain(t *testing.T) {
	pool := approvalTestPool(t)
	proj := createStuckTestProject(t, pool)

	steps := []workflow.StepWire{
		{ID: "merge", Name: "DevOps Merge", Kind: "task", DependsOn: []string{}},
		{ID: "gate", Name: "Merge Gate", Kind: "loop_decision", DependsOn: []string{"merge"},
			Config: `{"loop_branch":"merge","max_iterations":3,"conflict_value":"conflict","conflict_chain":["integrator"],"exhausted_review":"human"}`},
		{ID: "integrator", Name: "Integrator", Kind: "task", DependsOn: []string{}},
	}
	stepsJSON, _ := json.Marshal(steps)
	wfID := seedPublishedWorkflowSteps(t, pool, proj.ID, string(stepsJSON))

	ctx := context.Background()
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
		RunContext: []byte("{}"),
	})
	if err != nil {
		t.Fatal(err)
	}

	// The merge step succeeded but reports a CONFLICT decision.
	mergeRun, err := db.CreateWorkflowStepRun(ctx, ttx.Tx, db.WorkflowStepRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		WorkflowRunID: run.ID, StepID: "merge",
		StepName: "DevOps Merge", StepKind: "task",
		Status: domain.StepRunSucceeded, Result: []byte(`{"_decision":"conflict"}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	// The gate is READY to be dispatched.
	gateRun, err := db.CreateWorkflowStepRun(ctx, ttx.Tx, db.WorkflowStepRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		WorkflowRunID: run.ID, StepID: "gate",
		StepName: "Merge Gate", StepKind: "loop_decision",
		Status: domain.StepRunReady,
	})
	if err != nil {
		t.Fatal(err)
	}

	runs := map[string]db.WorkflowStepRunRow{
		"merge": mergeRun,
		"gate":  gateRun,
	}
	gateStep := workflow.StepWire{}
	for _, s := range steps {
		if s.ID == "gate" {
			gateStep = s
		}
	}
	var dispatched []dispatchReq
	if err := rc.dispatchStep(ctx, ttx.Tx, approvalTestTenant, run, gateStep, gateRun, runs, steps, &dispatched, nil); err != nil {
		t.Fatalf("dispatchStep: %v", err)
	}

	// The gate must be RUNNING (blocking downstream) after conflict re-entry.
	if got := runs["gate"].Status; got != domain.StepRunRunning {
		t.Errorf("gate status = %q, want running", got)
	}

	// A PENDING Integrator run must have been created for the conflict chain.
	// Read within the same transaction (dispatchStep created it uncommitted).
	all, err := db.ListWorkflowStepRuns(ctx, ttx.Tx, approvalTestTenant, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	integratorFound := false
	for _, sr := range all {
		if sr.StepID == "integrator" {
			integratorFound = true
			if sr.Status != domain.StepRunPending {
				t.Errorf("integrator run status = %q, want pending", sr.Status)
			}
		}
	}
	if !integratorFound {
		t.Error("no Integrator run created by conflict re-entry")
	}
}

// TestConflictBudgetExhaustionEscalates verifies that when the Integrator
// loop exhausts max_iterations (each attempt reports another conflict), the
// gate transitions to approval_pending for human review instead of failing —
// and that the exhausted marker is written to the step result.
func TestConflictBudgetExhaustionEscalates(t *testing.T) {
	pool := approvalTestPool(t)
	proj := createStuckTestProject(t, pool)

	steps := []workflow.StepWire{
		{ID: "merge", Name: "DevOps Merge", Kind: "task", DependsOn: []string{}},
		{ID: "gate", Name: "Merge Gate", Kind: "loop_decision", DependsOn: []string{"merge"},
			Config: `{"loop_branch":"merge","max_iterations":1,"conflict_value":"conflict","conflict_chain":["integrator"],"exhausted_review":"human"}`},
		{ID: "integrator", Name: "Integrator", Kind: "task", DependsOn: []string{}},
	}
	stepsJSON, _ := json.Marshal(steps)
	wfID := seedPublishedWorkflowSteps(t, pool, proj.ID, string(stepsJSON))

	ctx := context.Background()
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
		RunContext: []byte("{}"),
	})
	if err != nil {
		t.Fatal(err)
	}

	mergeRun, err := db.CreateWorkflowStepRun(ctx, ttx.Tx, db.WorkflowStepRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		WorkflowRunID: run.ID, StepID: "merge",
		StepName: "DevOps Merge", StepKind: "task",
		Status: domain.StepRunSucceeded, Result: []byte(`{"_decision":"conflict"}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	// max_iterations=1 already consumed: the Integrator ran once (iteration 1)
	// and reported another conflict. The gate is READY to re-evaluate.
	integratorRun, err := db.CreateWorkflowStepRun(ctx, ttx.Tx, db.WorkflowStepRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		WorkflowRunID: run.ID, StepID: "integrator",
		StepName: "Integrator (loop)", StepKind: "task",
		Status: domain.StepRunSucceeded, Iteration: 1,
		Result: []byte(`{"_decision":"conflict"}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	gateRun, err := db.CreateWorkflowStepRun(ctx, ttx.Tx, db.WorkflowStepRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		WorkflowRunID: run.ID, StepID: "gate",
		StepName: "Merge Gate", StepKind: "loop_decision",
		Status: domain.StepRunReady,
	})
	if err != nil {
		t.Fatal(err)
	}

	runs := map[string]db.WorkflowStepRunRow{
		"merge":      mergeRun,
		"integrator": integratorRun,
		"gate":       gateRun,
	}
	gateStep := workflow.StepWire{}
	for _, s := range steps {
		if s.ID == "gate" {
			gateStep = s
		}
	}
	var dispatched []dispatchReq
	if err := rc.dispatchStep(ctx, ttx.Tx, approvalTestTenant, run, gateStep, gateRun, runs, steps, &dispatched, nil); err != nil {
		t.Fatalf("dispatchStep: %v", err)
	}

	// The gate must be escalated to approval_pending, not failed.
	if got := runs["gate"].Status; got != domain.StepRunApprovalPending {
		t.Errorf("gate status = %q, want approval_pending", got)
	}
	var res struct {
		Exhausted bool   `json:"_exhausted"`
		Decision  string `json:"_decision"`
	}
	_ = json.Unmarshal(runs["gate"].Result, &res)
	if !res.Exhausted {
		t.Errorf("gate result missing _exhausted=true marker")
	}
	if res.Decision != "pending" {
		t.Errorf("gate decision = %q, want pending", res.Decision)
	}
}

// TestConflictIntegratorSuccessProceedsForward verifies that once the
// Integrator resolves the conflict and reports `success` (the merge landed),
// the gate re-evaluates to SUCCEEDED — the Integrator's decision is
// authoritative over the merge step's stale `conflict` signal.
func TestConflictIntegratorSuccessProceedsForward(t *testing.T) {
	pool := approvalTestPool(t)
	proj := createStuckTestProject(t, pool)

	steps := []workflow.StepWire{
		{ID: "merge", Name: "DevOps Merge", Kind: "task", DependsOn: []string{}},
		{ID: "gate", Name: "Merge Gate", Kind: "loop_decision", DependsOn: []string{"merge"},
			Config: `{"loop_branch":"merge","max_iterations":3,"conflict_value":"conflict","conflict_chain":["integrator"],"exhausted_review":"human"}`},
		{ID: "integrator", Name: "Integrator", Kind: "task", DependsOn: []string{}},
	}
	stepsJSON, _ := json.Marshal(steps)
	wfID := seedPublishedWorkflowSteps(t, pool, proj.ID, string(stepsJSON))

	ctx := context.Background()
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
		RunContext: []byte("{}"),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Merge still reports the ORIGINAL conflict (it was never re-run), but
	// the Integrator has since resolved it and reports success.
	mergeRun, err := db.CreateWorkflowStepRun(ctx, ttx.Tx, db.WorkflowStepRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		WorkflowRunID: run.ID, StepID: "merge",
		StepName: "DevOps Merge", StepKind: "task",
		Status: domain.StepRunSucceeded, Result: []byte(`{"_decision":"conflict"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	integratorRun, err := db.CreateWorkflowStepRun(ctx, ttx.Tx, db.WorkflowStepRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		WorkflowRunID: run.ID, StepID: "integrator",
		StepName: "Integrator (loop)", StepKind: "task",
		Status: domain.StepRunSucceeded, Iteration: 1,
		Result: []byte(`{"_decision":"success"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	gateRun, err := db.CreateWorkflowStepRun(ctx, ttx.Tx, db.WorkflowStepRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		WorkflowRunID: run.ID, StepID: "gate",
		StepName: "Merge Gate", StepKind: "loop_decision",
		Status: domain.StepRunReady,
	})
	if err != nil {
		t.Fatal(err)
	}

	runs := map[string]db.WorkflowStepRunRow{
		"merge":      mergeRun,
		"integrator": integratorRun,
		"gate":       gateRun,
	}
	gateStep := workflow.StepWire{}
	for _, s := range steps {
		if s.ID == "gate" {
			gateStep = s
		}
	}
	var dispatched []dispatchReq
	if err := rc.dispatchStep(ctx, ttx.Tx, approvalTestTenant, run, gateStep, gateRun, runs, steps, &dispatched, nil); err != nil {
		t.Fatalf("dispatchStep: %v", err)
	}

	// The gate must SUCCEED (the Integrator resolved the conflict), despite
	// the merge step's stale conflict decision.
	if got := runs["gate"].Status; got != domain.StepRunSucceeded {
		t.Errorf("gate status = %q, want succeeded", got)
	}
}

// TestConflictIntegratorFailureFailsRun verifies the Integrator `failure`
// sub-path: per the Integrator worker's contract ("`failure` fails it"), a
// non-conflict error (auth/network/missing repo) reported by the Integrator
// must FAIL the run directly — it must NOT loop back to the merge. Looping
// back would leave the failed Integrator run un-superseded, so its stale
// `_decision: failure` would keep overriding subsequent merge evaluations
// and re-loop until the merge budget exhausted (the PR-reviewer defect).
func TestConflictIntegratorFailureFailsRun(t *testing.T) {
	pool := approvalTestPool(t)
	proj := createStuckTestProject(t, pool)

	steps := []workflow.StepWire{
		{ID: "merge", Name: "DevOps Merge", Kind: "task", DependsOn: []string{}},
		{ID: "gate", Name: "Merge Gate", Kind: "loop_decision", DependsOn: []string{"merge"},
			Config: `{"loop_branch":"merge","max_iterations":3,"conflict_value":"conflict","conflict_chain":["integrator"],"exhausted_review":"human"}`},
		{ID: "integrator", Name: "Integrator", Kind: "task", DependsOn: []string{}},
	}
	stepsJSON, _ := json.Marshal(steps)
	wfID := seedPublishedWorkflowSteps(t, pool, proj.ID, string(stepsJSON))

	ctx := context.Background()
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
		RunContext: []byte("{}"),
	})
	if err != nil {
		t.Fatal(err)
	}

	// The merge reports the ORIGINAL conflict, and the Integrator that was
	// routed to resolve it reports `failure` (a non-conflict error).
	mergeRun, err := db.CreateWorkflowStepRun(ctx, ttx.Tx, db.WorkflowStepRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		WorkflowRunID: run.ID, StepID: "merge",
		StepName: "DevOps Merge", StepKind: "task",
		Status: domain.StepRunSucceeded, Result: []byte(`{"_decision":"conflict"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	integratorRun, err := db.CreateWorkflowStepRun(ctx, ttx.Tx, db.WorkflowStepRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		WorkflowRunID: run.ID, StepID: "integrator",
		StepName: "Integrator (loop)", StepKind: "task",
		Status: domain.StepRunSucceeded, Iteration: 1,
		Result: []byte(`{"_decision":"failure"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	gateRun, err := db.CreateWorkflowStepRun(ctx, ttx.Tx, db.WorkflowStepRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		WorkflowRunID: run.ID, StepID: "gate",
		StepName: "Merge Gate", StepKind: "loop_decision",
		Status: domain.StepRunReady,
	})
	if err != nil {
		t.Fatal(err)
	}

	runs := map[string]db.WorkflowStepRunRow{
		"merge":      mergeRun,
		"integrator": integratorRun,
		"gate":       gateRun,
	}
	gateStep := workflow.StepWire{}
	for _, s := range steps {
		if s.ID == "gate" {
			gateStep = s
		}
	}
	var dispatched []dispatchReq
	if err := rc.dispatchStep(ctx, ttx.Tx, approvalTestTenant, run, gateStep, gateRun, runs, steps, &dispatched, nil); err != nil {
		t.Fatalf("dispatchStep: %v", err)
	}
	// The gate must be marked FAILED, not left READY/RUNNING for another
	// merge loop-back, and must not have looped back to the merge chain
	// (no new merge step runs created).
	if got := runs["gate"].Status; got != domain.StepRunFailed {
		t.Errorf("gate status = %q, want failed", got)
	}
	all, err := db.ListWorkflowStepRuns(ctx, ttx.Tx, approvalTestTenant, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, sr := range all {
		if sr.StepID == "merge" && sr.SupersededBy == "" && sr.Status == domain.StepRunPending {
			t.Errorf("merge was re-entered after integrator failure; want no new merge run (status=%q)", sr.Status)
		}
	}
}
