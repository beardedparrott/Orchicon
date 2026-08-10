package workflow_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strconv"
	"testing"
	"time"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/migrate"
	"github.com/beardedparrott/orchicon/internal/tenant"
	"github.com/beardedparrott/orchicon/internal/workflow"
)

// DB-backed integration test for ForceProgressWorkflowRun (skipped unless
// ORCHICON_TEST_DSN points at a disposable database, same convention as
// retry_failed_test.go):
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/workflow/ -run TestForceProgressWorkflowRun -v
//
// It verifies the fix for a field incident: force-progress used to mark
// EVERY active non-terminal step succeeded — including downstream steps
// still waiting on an unresolved upstream (a DevOps PR step), which skipped
// real work and prematurely "completed" the run. Now it forces only stuck
// steps (in-flight, or pending with DAG deps satisfied) and leaves pending
// steps with unsatisfied deps for the reconciler to dispatch normally.
const forceTestTenant = "tnt_force_progress"

func forceTestPool(t *testing.T) *db.Pool {
	t.Helper()
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed workflow tests")
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
	return pool
}

// forceSeedRun builds a running workflow run with a published version whose
// steps form: review → loop (loop_decision) → qa → approve → merge. The
// review step is succeeded; the loop_decision is PENDING with its dep
// satisfied (it is stuck); qa/approve/merge are PENDING with unsatisfied
// deps (they must NOT be forced). Also seeds one in-flight RUNNING step to
// assert in-flight steps are always forced.
func forceSeedRun(t *testing.T, ctx context.Context, pool *db.Pool) (string, map[string]string) {
	t.Helper()
	ttx, err := pool.BeginTenantTx(ctx, forceTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)

	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: forceTestTenant,
		Name: "Force Test", Slug: "force-test-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		Status: "active", Goals: []byte("[]"),
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	wi, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: forceTestTenant, ProjectID: proj.ID,
		Kind: domain.WorkItemKindTask, Title: "Force ticket",
		Status: domain.WorkItemRunning,
	})
	if err != nil {
		t.Fatalf("create work item: %v", err)
	}
	wf, err := db.CreateWorkflow(ctx, ttx.Tx, db.WorkflowRow{
		ID: db.NewID(), TenantID: forceTestTenant, ProjectID: proj.ID,
		Name: "Force Workflow", CurrentVersion: 1, Status: "published", Type: "one_shot",
	})
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}

	// Published version: the DAG the reconciler/force-progress must honor.
	steps := []workflow.StepWire{
		{ID: "review", Name: "PR Reviewer", Kind: "task", DependsOn: []string{}},
		{ID: "loop", Name: "Loop Decision", Kind: "loop_decision", DependsOn: []string{"review"},
			Config: `{"loop_branch":"review","success_branch":"qa","max_iterations":6}`},
		{ID: "qa", Name: "QA", Kind: "task", DependsOn: []string{"loop"}},
		{ID: "approve", Name: "Approve", Kind: "approval", DependsOn: []string{"qa"}},
		{ID: "merge", Name: "Merge", Kind: "task", DependsOn: []string{"approve"}},
		{ID: "arch", Name: "Architect", Kind: "task", DependsOn: []string{}},
	}
	stepsJSON, _ := json.Marshal(steps)
	ver, err := db.CreateWorkflowVersion(ctx, ttx.Tx, db.WorkflowVersionRow{
		ID: db.NewID(), TenantID: forceTestTenant, WorkflowID: wf.ID,
		Version: 1, Status: "published", Steps: stepsJSON, Inputs: []byte("{}"), Outputs: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create workflow version: %v", err)
	}
	run, err := db.CreateWorkflowRun(ctx, ttx.Tx, db.WorkflowRunRow{
		ID: db.NewID(), TenantID: forceTestTenant, WorkflowID: wf.ID,
		WorkflowVersion: ver.Version, ProjectID: proj.ID, Status: domain.WorkflowRunRunning,
		WorkItemID: wi.ID, RunContext: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	now := time.Now().UTC()
	started := now.Add(-time.Minute)
	newStep := func(stepID, name, kind, status string, execID string, result []byte, iter int, supersededBy string) string {
		id := db.NewID()
		s, err := db.CreateWorkflowStepRun(ctx, ttx.Tx, db.WorkflowStepRunRow{
			ID: id, TenantID: forceTestTenant, WorkflowRunID: run.ID,
			StepID: stepID, StepName: name, StepKind: kind,
			Status: status, Iteration: iter, Result: result,
			WorkerExecutionID: execID, SupersededBy: supersededBy,
			StartedAt: &started,
		})
		if err != nil {
			t.Fatalf("create step run %s: %v", stepID, err)
		}
		return s.ID
	}

	ids := map[string]string{
		"review":     newStep("review", "PR Reviewer", "task", domain.StepRunSucceeded, db.NewID(), []byte(`{"_decision":"success"}`), 0, ""),
		"loop":       newStep("loop", "Loop Decision", "loop_decision", domain.StepRunPending, "", []byte("{}"), 1, ""),
		"qa":         newStep("qa", "QA", "task", domain.StepRunPending, "", []byte("{}"), 0, ""),
		"approve":    newStep("approve", "Approve", "approval", domain.StepRunPending, "", []byte("{}"), 0, ""),
		"merge":      newStep("merge", "Merge", "task", domain.StepRunPending, "", []byte("{}"), 0, ""),
		"arch":       newStep("arch", "Architect", "task", domain.StepRunRunning, db.NewID(), []byte("{}"), 0, ""),
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
	return run.ID, ids
}

// TestForceProgressWorkflowRunStuckOnly verifies force-progress marks ONLY
// the stuck steps succeeded: the in-flight RUNNING step and the PENDING
// loop_decision whose dep is satisfied. Steps still waiting on an
// unsatisfied dep (qa/approve/merge) stay PENDING so the reconciler
// dispatches them — real work is never skipped.
func TestForceProgressWorkflowRunStuckOnly(t *testing.T) {
	pool := forceTestPool(t)
	ctx := tenant.WithID(context.Background(), forceTestTenant)
	runID, ids := forceSeedRun(t, ctx, pool)

	svc := workflow.New(pool, slog.Default(), nil)
	resp, err := svc.ForceProgressWorkflowRun(ctx, connect.NewRequest(&apiv1.ForceProgressWorkflowRunRequest{RunId: runID}))
	if err != nil {
		t.Fatalf("force progress: %v", err)
	}
	forced := map[string]bool{}
	for _, id := range resp.Msg.ForcedStepRunIds {
		forced[id] = true
	}

	// The in-flight running step must be forced.
	if !forced[ids["arch"]] {
		t.Errorf("in-flight running step was not forced: %v", resp.Msg.ForcedStepRunIds)
	}
	// The stuck loop_decision (pending, dep satisfied) must be forced.
	if !forced[ids["loop"]] {
		t.Errorf("stuck loop_decision was not forced: %v", resp.Msg.ForcedStepRunIds)
	}
	// Downstream pending steps with unsatisfied deps must NOT be forced.
	for _, name := range []string{"qa", "approve", "merge"} {
		if forced[ids[name]] {
			t.Errorf("pending %s step with unsatisfied deps was force-marked — real work skipped", name)
		}
	}

	// Verify DB state: arch + loop succeeded, downstream still pending.
	fetch := func(stepRunID string) db.WorkflowStepRunRow {
		ttx, err := pool.BeginTenantTx(ctx, forceTestTenant)
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		defer ttx.Rollback(ctx)
		sr, err := db.GetWorkflowStepRun(ctx, ttx.Tx, forceTestTenant, stepRunID)
		if err != nil {
			t.Fatalf("fetch step %s: %v", stepRunID, err)
		}
		return sr
	}
	if got := fetch(ids["arch"]).Status; got != domain.StepRunSucceeded {
		t.Errorf("arch status = %q, want succeeded", got)
	}
	if got := fetch(ids["loop"]).Status; got != domain.StepRunSucceeded {
		t.Errorf("loop status = %q, want succeeded", got)
	}
	for _, name := range []string{"qa", "approve", "merge"} {
		if got := fetch(ids[name]).Status; got != domain.StepRunPending {
			t.Errorf("%s status = %q, want pending (left for reconciler)", name, got)
		}
	}
}
