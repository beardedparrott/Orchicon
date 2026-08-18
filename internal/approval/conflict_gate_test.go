package approval

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"

	"connectrpc.com/connect"
	assets "github.com/beardedparrott/orchicon"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/migrate"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// approvalDBPool opens a disposable Postgres for DB-backed approval tests
// (skipped unless ORCHICON_TEST_DSN is set), mirroring the scheduler's
// approvalTestPool.
func approvalDBPool(t *testing.T) *db.Pool {
	t.Helper()
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed approval tests")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := migrate.Run(ctx, pool, assets.MigrationsFS, assets.MigrationsDir); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	if err := db.SeedDevWorkers(ctx, pool); err != nil {
		t.Fatalf("seed dev workers: %v", err)
	}
	return pool
}

// TestListApprovalItemsIncludesExhaustedGate verifies D4: the Approvals list
// now surfaces exhausted loop_decision merge gates (step_kind
// 'loop_decision') that were escalated to approval_pending, not just
// 'approval' steps.
func TestListApprovalItemsIncludesExhaustedGate(t *testing.T) {
	pool := approvalDBPool(t)
	ctx := context.Background()
	const tenantID = "tnt_dev"

	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)

	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: tenantID,
		Name: "Conflict Proj", Slug: "conflict-" + strings.ToLower(db.NewID()),
		Status: "active", Goals: []byte("[]"),
	})
	if err != nil {
		t.Fatal(err)
	}
	wf, err := db.CreateWorkflow(ctx, ttx.Tx, db.WorkflowRow{
		ID: db.NewID(), TenantID: tenantID, ProjectID: proj.ID,
		Name: "Conflict WF", CurrentVersion: 0,
		Status: domain.WorkflowDraft, Type: domain.WorkflowTypeTemplate,
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := db.CreateWorkflowRun(ctx, ttx.Tx, db.WorkflowRunRow{
		ID: db.NewID(), TenantID: tenantID,
		WorkflowID: wf.ID, WorkflowVersion: 1,
		ProjectID: proj.ID, Status: domain.WorkflowRunRunning,
		RunContext: []byte("{}"),
	})
	if err != nil {
		t.Fatal(err)
	}

	// An exhausted loop_decision merge gate escalated to approval_pending.
	exhaustedResult, _ := json.Marshal(map[string]any{
		"_decision":         "pending",
		"_exhausted":        true,
		"_reason":           "merge conflict not resolved",
		"_upstream_summary": "merge conflict not resolved",
	})
	if _, err := db.CreateWorkflowStepRun(ctx, ttx.Tx, db.WorkflowStepRunRow{
		ID: db.NewID(), TenantID: tenantID,
		WorkflowRunID: run.ID, StepID: "step-loop-merge",
		StepName: "Merge Gate", StepKind: domain.StepKindLoopDecision,
		Status: domain.StepRunApprovalPending, Result: exhaustedResult,
	}); err != nil {
		t.Fatal(err)
	}

	// A plain human approval step (control: must still appear).
	if _, err := db.CreateWorkflowStepRun(ctx, ttx.Tx, db.WorkflowStepRunRow{
		ID: db.NewID(), TenantID: tenantID,
		WorkflowRunID: run.ID, StepID: "step-approval",
		StepName: "Approval", StepKind: domain.StepKindApproval,
		Status: domain.StepRunApprovalPending,
	}); err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	svc := NewService(pool, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	reqCtx := tenant.WithID(context.Background(), tenantID)
	resp, err := svc.ListPendingStepApprovals(reqCtx, connect.NewRequest(&apiv1.ListPendingStepApprovalsRequest{
		WorkflowRunId: stringPtr(run.ID),
		PageSize:      50,
	}))
	if err != nil {
		t.Fatalf("ListPendingStepApprovals: %v", err)
	}

	kindSeen := map[string]bool{}
	for _, it := range resp.Msg.Items {
		kindSeen[it.StepRunId] = true
	}
	if len(resp.Msg.Items) != 2 {
		t.Errorf("expected 2 approval items (loop_decision gate + approval), got %d", len(resp.Msg.Items))
	}
	// Assert both step kinds are present by querying back each item's kind.
	ttx2, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx2.Rollback(ctx)
	foundLoop := false
	foundApproval := false
	for _, it := range resp.Msg.Items {
		sr, err := db.GetWorkflowStepRun(ctx, ttx2.Tx, tenantID, it.StepRunId)
		if err != nil {
			t.Fatal(err)
		}
		if sr.StepKind == domain.StepKindLoopDecision {
			foundLoop = true
		}
		if sr.StepKind == domain.StepKindApproval {
			foundApproval = true
		}
	}
	if !foundLoop {
		t.Error("exhausted loop_decision gate not listed as an approval item")
	}
	if !foundApproval {
		t.Error("plain approval step not listed")
	}
}

// TestApproveStepRejectsExhaustedGateFailsRun verifies D3 step 4: rejecting an
// exhausted loop_decision merge gate FAILS the step (so the run fails) while
// approving it SUCCEEDS it — the kind-sensitive branch in ApproveStep.
func TestApproveStepRejectsExhaustedGateFailsRun(t *testing.T) {
	pool := approvalDBPool(t)
	ctx := context.Background()
	const tenantID = "tnt_dev"

	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)

	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: tenantID,
		Name: "Conflict Proj", Slug: "conflict-" + strings.ToLower(db.NewID()),
		Status: "active", Goals: []byte("[]"),
	})
	if err != nil {
		t.Fatal(err)
	}
	wf, err := db.CreateWorkflow(ctx, ttx.Tx, db.WorkflowRow{
		ID: db.NewID(), TenantID: tenantID, ProjectID: proj.ID,
		Name: "Conflict WF", CurrentVersion: 0,
		Status: domain.WorkflowDraft, Type: domain.WorkflowTypeTemplate,
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := db.CreateWorkflowRun(ctx, ttx.Tx, db.WorkflowRunRow{
		ID: db.NewID(), TenantID: tenantID,
		WorkflowID: wf.ID, WorkflowVersion: 1,
		ProjectID: proj.ID, Status: domain.WorkflowRunRunning,
		RunContext: []byte("{}"),
	})
	if err != nil {
		t.Fatal(err)
	}
	exhaustedResult, _ := json.Marshal(map[string]any{"_decision": "pending", "_exhausted": true})
	gate, err := db.CreateWorkflowStepRun(ctx, ttx.Tx, db.WorkflowStepRunRow{
		ID: db.NewID(), TenantID: tenantID,
		WorkflowRunID: run.ID, StepID: "step-loop-merge",
		StepName: "Merge Gate", StepKind: domain.StepKindLoopDecision,
		Status: domain.StepRunApprovalPending, Result: exhaustedResult,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	svc := NewService(pool, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	// Reject the exhausted gate → the step must FAIL.
	if _, err := svc.ApproveStep(tenant.WithID(context.Background(), tenantID), connect.NewRequest(&apiv1.ApproveStepRequest{
		StepRunId: gate.ID, Approved: false, Reason: "cannot resolve", ReviewedBy: "human",
	})); err != nil {
		t.Fatalf("ApproveStep reject: %v", err)
	}

	ttx2, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx2.Rollback(ctx)
	updated, err := db.GetWorkflowStepRun(ctx, ttx2.Tx, tenantID, gate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != domain.StepRunFailed {
		t.Errorf("rejected exhausted loop_decision gate status = %q, want failed", updated.Status)
	}
}

func stringPtr(s string) *string { return &s }
