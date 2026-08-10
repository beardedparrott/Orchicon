package scheduler

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/workflow"
)

// Tests for runs that would otherwise wedge permanently in "running" and
// leak their per-workflow runtime container (the 30s adopt sweep treats
// any running run as active and keeps the container alive):
//
//   - a run on a workflow whose published version has an EMPTY step DAG
//     (observed: 18 prod runs from empty `seq-wf-*` workflows),
//   - a task step whose work item or execution row was hard-deleted
//     (observed: dev runs stuck on step-pr-reviewer with a deleted item).
//
// All three paths must terminalize the run so the container is reaped
// instead of sitting "running" forever.

// seedPublishedWorkflowSteps creates a published workflow template whose
// current version carries the given steps JSON. Returns the workflow id.
func seedPublishedWorkflowSteps(t *testing.T, pool *db.Pool, projID string, stepsJSON string) string {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	wf, err := db.CreateWorkflow(ctx, ttx.Tx, db.WorkflowRow{
		ID: db.NewID(), TenantID: approvalTestTenant, ProjectID: projID,
		Name: "stuck-wf-" + strings.ToLower(db.NewID())[:8],
		CurrentVersion: 0, Status: domain.WorkflowDraft, Type: domain.WorkflowTypeTemplate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateWorkflowVersion(ctx, ttx.Tx, db.WorkflowVersionRow{
		ID: db.NewID(), TenantID: approvalTestTenant, WorkflowID: wf.ID,
		Version: 1, Status: domain.WorkflowVersionDraft, Steps: []byte(stepsJSON),
		Inputs: []byte("{}"), Outputs: []byte("{}"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.PublishWorkflowVersion(ctx, ttx.Tx, approvalTestTenant, wf.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpdateWorkflowCurrentVersion(ctx, ttx.Tx, approvalTestTenant, wf.ID, wf.Version, 1); err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return wf.ID
}

// createStuckTestProject creates a throwaway active project.
func createStuckTestProject(t *testing.T, pool *db.Pool) db.ProjectRow {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	slug := "stuck-test-" + strings.ToLower(db.NewID())
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		Name: "Stuck Test", Slug: slug,
		Status: domain.ProjectActive, Goals: []byte("[]"),
		ProjectDir: "/tmp/orchicon/" + slug,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return proj
}

// TestReconcileRunFailsEmptyStepDag verifies a run on a workflow whose
// published version has NO steps is failed at start by the reconciler
// instead of sitting "running" forever (the container leak).
func TestReconcileRunFailsEmptyStepDag(t *testing.T) {
	pool := approvalTestPool(t)
	proj := createStuckTestProject(t, pool)
	wfID := seedPublishedWorkflowSteps(t, pool, proj.ID, "[]")
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	rc := NewWorkflowReconciler(pool, logger, nil, nil, nil, nil)

	for _, initial := range []string{domain.WorkflowRunPending, domain.WorkflowRunRunning} {
		t.Run("initial="+initial, func(t *testing.T) {
			ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
			if err != nil {
				t.Fatal(err)
			}
			defer ttx.Rollback(ctx)
			run, err := db.CreateWorkflowRun(ctx, ttx.Tx, db.WorkflowRunRow{
				ID: db.NewID(), TenantID: approvalTestTenant,
				WorkflowID: wfID, WorkflowVersion: 1,
				ProjectID: proj.ID, Status: initial,
				RunContext: []byte("{}"),
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := ttx.Commit(ctx); err != nil {
				t.Fatal(err)
			}

			if err := rc.reconcileRun(ctx, approvalTestTenant, run.ID); err != nil {
				t.Fatalf("reconcileRun: %v", err)
			}

			ttx2, err := pool.BeginTenantTx(ctx, approvalTestTenant)
			if err != nil {
				t.Fatal(err)
			}
			defer ttx2.Rollback(ctx)
			got, err := db.GetWorkflowRun(ctx, ttx2.Tx, approvalTestTenant, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != domain.WorkflowRunFailed {
				t.Fatalf("run status = %q, want %q", got.Status, domain.WorkflowRunFailed)
			}
			if got.EndedAt == nil {
				t.Fatalf("run ended_at not set — the run would stay 'running' and leak its container")
			}
		})
	}
}

// TestReconcileRunFailsMissingVersion verifies a run whose workflow
// version row is gone (deleted workflow / raw-seeded run) is failed at
// start rather than erroring every pass forever.
func TestReconcileRunFailsMissingVersion(t *testing.T) {
	pool := approvalTestPool(t)
	proj := createStuckTestProject(t, pool)
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
		WorkflowID: "no-such-workflow", WorkflowVersion: 99,
		ProjectID: proj.ID, Status: domain.WorkflowRunRunning,
		RunContext: []byte("{}"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	if err := rc.reconcileRun(ctx, approvalTestTenant, run.ID); err != nil {
		t.Fatalf("reconcileRun: %v", err)
	}

	ttx2, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx2.Rollback(ctx)
	got, err := db.GetWorkflowRun(ctx, ttx2.Tx, approvalTestTenant, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.WorkflowRunFailed {
		t.Fatalf("run status = %q, want %q", got.Status, domain.WorkflowRunFailed)
	}
	if got.EndedAt == nil {
		t.Fatalf("run ended_at not set — the run would stay 'running' forever")
	}
}

// TestPollTaskStepFailsOnDeletedWorkItem verifies a running task step
// whose referenced work item was hard-deleted is failed terminal by
// pollTaskStep instead of being waited on forever (the run then
// terminalizes and its container is reaped).
func TestPollTaskStepFailsOnDeletedWorkItem(t *testing.T) {
	pool := approvalTestPool(t)
	proj := createStuckTestProject(t, pool)
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
		WorkflowID: "wf-empty-steps", WorkflowVersion: 1,
		ProjectID: proj.ID, Status: domain.WorkflowRunRunning,
		RunContext: []byte("{}"),
	})
	if err != nil {
		t.Fatal(err)
	}
	step := workflow.StepWire{
		ID: "step-task", Name: "Task", Kind: domain.StepKindTask,
		Ref: "w_se_devops_engineer", Config: `{}`,
	}
	stepResult, _ := json.Marshal(map[string]string{
		"_work_item_id": db.NewID(), // deleted work item — never created
	})
	sr, err := db.CreateWorkflowStepRun(ctx, ttx.Tx, db.WorkflowStepRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		WorkflowRunID: run.ID, StepID: step.ID, StepName: step.Name,
		StepKind: step.Kind, Status: domain.StepRunRunning,
		Result: stepResult, StartedAt: timePtr(time.Now().Add(-time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	ptx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ptx.Rollback(ctx)
	terminal, failed, err := rc.pollTaskStep(ctx, ptx.Tx, approvalTestTenant, run, sr, step.Config,
		map[string]db.WorkflowStepRunRow{step.ID: sr}, nil)
	if err != nil {
		t.Fatalf("pollTaskStep: %v", err)
	}
	if !terminal || !failed {
		t.Fatalf("pollTaskStep = (terminal=%v, failed=%v), want (true, true) — a deleted work item must fail the step, not wait forever", terminal, failed)
	}
}

// TestPollTaskStepRecoversOnDeletedExecution verifies a running task step
// whose worker execution row was hard-deleted falls through to the
// recovery block (retry) instead of being waited on forever.
func TestPollTaskStepRecoversOnDeletedExecution(t *testing.T) {
	pool := approvalTestPool(t)
	proj := createStuckTestProject(t, pool)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	rc := NewWorkflowReconciler(pool, logger, nil, nil, nil, nil)

	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)

	item, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		ProjectID: proj.ID, Kind: domain.WorkItemKindTask,
		Title: "shared ticket", Status: domain.WorkItemRunning,
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := db.CreateWorkflowRun(ctx, ttx.Tx, db.WorkflowRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		WorkflowID: "wf-empty-steps", WorkflowVersion: 1,
		ProjectID: proj.ID, Status: domain.WorkflowRunRunning,
		RunContext: []byte("{}"), WorkItemID: item.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	step := workflow.StepWire{
		ID: "step-task", Name: "Task", Kind: domain.StepKindTask,
		Ref: "w_se_devops_engineer", Config: `{}`,
	}
	stepResult, _ := json.Marshal(map[string]string{"_work_item_id": item.ID})
	sr, err := db.CreateWorkflowStepRun(ctx, ttx.Tx, db.WorkflowStepRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		WorkflowRunID: run.ID, StepID: step.ID, StepName: step.Name,
		StepKind: step.Kind, Status: domain.StepRunRunning,
		Result:            stepResult,
		WorkerExecutionID: db.NewID(), // execution row never exists
		StartedAt:         timePtr(time.Now().Add(-time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	ptx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ptx.Rollback(ctx)
	runByID := map[string]db.WorkflowStepRunRow{step.ID: sr}
	var recoveryTriggers []recoveryTriggerReq
	terminal, failed, err := rc.pollTaskStep(ctx, ptx.Tx, approvalTestTenant, run, sr, step.Config, runByID, &recoveryTriggers)
	if err != nil {
		t.Fatalf("pollTaskStep: %v", err)
	}
	if terminal || failed {
		t.Fatalf("pollTaskStep = (terminal=%v, failed=%v), want (false, false) — a lost execution must be retried, not failed on the first pass", terminal, failed)
	}
	got := runByID[step.ID]
	if got.Status != domain.StepRunRecovering {
		t.Fatalf("step status = %q, want %q (recovery retry initiated)", got.Status, domain.StepRunRecovering)
	}
	if got.Attempt != 1 {
		t.Fatalf("step attempt = %d, want 1", got.Attempt)
	}
}

func timePtr(t time.Time) *time.Time { return &t }
