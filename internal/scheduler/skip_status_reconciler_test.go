package scheduler

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/workflow"
)

// Skip-status interplay tests (D3 producer): the WorkflowReconciler's
// run-completion path distinguishes a run that completes with a skipped
// step from a fully-succeeded run. A bound work item whose active step
// run is StepRunSkipped must terminalize as WorkItemSkipped (never
// conflated with succeeded), and a skipped sequence child must still
// advance its parent's chain — skipped/terminal-success never blocks
// dependents.

// TestRunCompletionWithSkippedStepMarksWorkItemSkipped verifies the
// skipped producer: a bound run whose every active step is terminal-
// success with at least one skipped completes the bound work item as
// "skipped" (not "succeeded").
func TestRunCompletionWithSkippedStepMarksWorkItemSkipped(t *testing.T) {
	pool := approvalTestPool(t)
	ctx := context.Background()

	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: approvalTestTenant, Name: "skip-producer",
		Slug: "skip-producer-" + strings.ToLower(db.NewID()), Status: domain.ProjectActive,
		Goals: []byte("[]"), ProjectDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	steps := []workflow.StepWire{{ID: "step-1", Name: "Do work", Kind: domain.StepKindTask, DependsOn: []string{}}}
	stepsJSON, _ := json.Marshal(steps)
	wfID := seedPublishedWorkflowSteps(t, pool, proj.ID, string(stepsJSON))
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	item := createWorkItem(t, pool, proj.ID, domain.WorkItemKindTask, "Bound Leaf", nil, &wfID)

	ttx2, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	run, err := db.CreateWorkflowRun(ctx, ttx2.Tx, db.WorkflowRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		WorkflowID: wfID, WorkflowVersion: 1,
		ProjectID: proj.ID, Status: domain.WorkflowRunRunning,
		RunContext: []byte("{}"), WorkItemID: item.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Flip the runtime-serve readiness gate so the reconciler completes the
	// run in this pass (same pattern as the recurring completion test).
	if _, err := db.UpdateWorkflowRun(ctx, ttx2.Tx, approvalTestTenant, run.ID, run.Version, db.UpdateWorkflowRunFields{
		RuntimeReady: boolPtr(true),
	}); err != nil {
		t.Fatal(err)
	}
	// The run's single active step is SKIPPED (not succeeded).
	if _, err := db.CreateWorkflowStepRun(ctx, ttx2.Tx, db.WorkflowStepRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		WorkflowRunID: run.ID, StepID: "step-1",
		StepName: "Do work", StepKind: domain.StepKindTask,
		Status: domain.StepRunSkipped,
	}); err != nil {
		t.Fatal(err)
	}
	running := domain.WorkItemRunning
	if _, err := db.UpdateWorkItem(ctx, ttx2.Tx, approvalTestTenant, item.ID, item.Version,
		db.UpdateWorkItemFields{Status: &running, WorkflowRunID: &run.ID}); err != nil {
		t.Fatal(err)
	}
	if err := ttx2.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	wfRec := NewWorkflowReconciler(pool, slog.New(slog.NewTextHandler(os.Stderr, nil)), nil, nil, nil, nil)
	if res := wfRec.Reconcile(ctx, run.ID); res.Error != nil {
		t.Fatalf("workflow reconciler: %v", res.Error)
	}

	final := mustGet(t, pool, item.ID)
	if final.Status != domain.WorkItemSkipped {
		t.Errorf("item status = %q, want %q (run completed with a skipped step)", final.Status, domain.WorkItemSkipped)
	}
}

// TestRunCompletionSkippedChildAdvancesParentChain verifies a skipped
// bound child still advances its parent's sequence chain: the completion
// path marks the child skipped and the sequence notifier arms the next
// strict sibling (skipped/terminal-success never blocks dependents).
func TestRunCompletionSkippedChildAdvancesParentChain(t *testing.T) {
	pool := approvalTestPool(t)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: approvalTestTenant, Name: "skip-advance",
		Slug: "skip-advance-" + strings.ToLower(db.NewID()), Status: domain.ProjectActive,
		Goals: []byte("[]"), ProjectDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	steps := []workflow.StepWire{{ID: "step-1", Name: "Do work", Kind: domain.StepKindTask, DependsOn: []string{}}}
	stepsJSON, _ := json.Marshal(steps)
	wfID := seedPublishedWorkflowSteps(t, pool, proj.ID, string(stepsJSON))
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	env := &sequenceTestEnv{pool: pool, proj: proj}
	parent := createWorkItem(t, pool, proj.ID, domain.WorkItemKindEpic, "Parent", nil, nil)
	c1 := createWorkItem(t, pool, proj.ID, domain.WorkItemKindTask, "C1", &parent.ID, &wfID)
	c2 := createWorkItem(t, pool, proj.ID, domain.WorkItemKindTask, "C2", &parent.ID, &wfID)
	reorder(t, pool, proj.ID, parent.ID, []string{c1.ID, c2.ID})

	// Fire the sequence: C1 arms (stub records the start, no real run).
	if err := StartSequence(ctx, pool, logger, approvalTestTenant, parent.ID, env.startFn()); err != nil {
		t.Fatal(err)
	}
	armedC1 := mustGet(t, pool, c1.ID)
	if armedC1.Status != domain.WorkItemRunning {
		t.Fatalf("C1 status = %q, want running (armed by StartSequence)", armedC1.Status)
	}

	// C1's bound run completes with a skipped step.
	ttx2, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	run, err := db.CreateWorkflowRun(ctx, ttx2.Tx, db.WorkflowRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		WorkflowID: wfID, WorkflowVersion: 1,
		ProjectID: proj.ID, Status: domain.WorkflowRunRunning,
		RunContext: []byte("{}"), WorkItemID: c1.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpdateWorkflowRun(ctx, ttx2.Tx, approvalTestTenant, run.ID, run.Version, db.UpdateWorkflowRunFields{
		RuntimeReady: boolPtr(true),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateWorkflowStepRun(ctx, ttx2.Tx, db.WorkflowStepRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		WorkflowRunID: run.ID, StepID: "step-1",
		StepName: "Do work", StepKind: domain.StepKindTask,
		Status: domain.StepRunSkipped,
	}); err != nil {
		t.Fatal(err)
	}
	running := domain.WorkItemRunning
	if _, err := db.UpdateWorkItem(ctx, ttx2.Tx, approvalTestTenant, c1.ID, armedC1.Version,
		db.UpdateWorkItemFields{Status: &running, WorkflowRunID: &run.ID}); err != nil {
		t.Fatal(err)
	}
	if err := ttx2.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// Reconcile the run; on completion the sequence notifier must advance
	// the parent (C1 skipped → C2 arms).
	rec := NewSequenceReconciler(pool, logger, env.startFn())
	wfRec := NewWorkflowReconciler(pool, logger, nil, nil, nil, nil)
	wfRec.SetSequenceNotifier(func(c context.Context, parentID string) {
		if err := rec.reconcileOne(c, approvalTestTenant, parentID); err != nil {
			t.Fatalf("advance parent chain: %v", err)
		}
	})
	if res := wfRec.Reconcile(ctx, run.ID); res.Error != nil {
		t.Fatalf("workflow reconciler: %v", res.Error)
	}

	if got := mustGet(t, pool, c1.ID); got.Status != domain.WorkItemSkipped {
		t.Errorf("C1 status = %q, want %q (bound child whose run completed with a skipped step)", got.Status, domain.WorkItemSkipped)
	}
	if got := mustGet(t, pool, c2.ID); got.Status != domain.WorkItemRunning {
		t.Errorf("C2 status = %q, want running (chain advanced past the skipped child)", got.Status)
	}
	armed := env.armedWorkItems()
	if len(armed) != 2 || armed[1] != c2.ID {
		t.Errorf("armed work items = %v, want [c1 c2]", armed)
	}
}
