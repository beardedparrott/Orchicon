package scheduler

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/migrate"
	"github.com/beardedparrott/orchicon/internal/workflow"
	"github.com/jackc/pgx/v5"
)

// These tests exercise the worker-backed approval flow against a real
// Postgres. They are skipped unless ORCHICON_TEST_DSN points at a
// disposable database (the migrations + dev workers are applied on every
// run, so the database must be safe to re-seed):
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/scheduler/ -run 'TestApproval.*NoClone|TestApprovalDispatch' -v
//
// They guard the acceptance criteria "no work item creation from
// approvals": a worker-backed approval step must dispatch the approver
// against the run's SHARED ticket and never create a per-step artifact,
// even across a failure/retry cycle.

func approvalTestPool(t *testing.T) *db.Pool {
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

const approvalTestTenant = "tnt_dev"

// approvalTestEnv is the shared fixture for the integration tests: a
// project, a bound ticket work item, a running workflow run, and a
// worker-backed APPROVAL step run. All ids are fresh per test.
type approvalTestEnv struct {
	t          *testing.T
	pool       *db.Pool
	projectID  string
	ticketID   string
	run        db.WorkflowRunRow
	step       workflow.StepWire
	stepRun    db.WorkflowStepRunRow
	reconciler *WorkflowReconciler
}

func newApprovalTestEnv(t *testing.T) *approvalTestEnv {
	t.Helper()
	pool := approvalTestPool(t)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	env := &approvalTestEnv{t: t, pool: pool,
		reconciler: NewWorkflowReconciler(pool, logger, nil, nil, nil, nil)}

	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)

	slug := "approval-test-" + strings.ToLower(db.NewID())
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		Name: "Approval Test", Slug: slug,
		Status: "active", Goals: []byte("[]"),
		ProjectDir: "/tmp/orchicon/" + slug,
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	env.projectID = proj.ID

	ticket, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		ProjectID: proj.ID, Kind: domain.WorkItemKindTask,
		Title:              "The ticket to approve",
		Description:        "work produced by an upstream step",
		AcceptanceCriteria: "done and reviewed",
		Status:             domain.WorkItemRunning,
	})
	if err != nil {
		t.Fatalf("create ticket: %v", err)
	}
	env.ticketID = ticket.ID

	run, err := db.CreateWorkflowRun(ctx, ttx.Tx, db.WorkflowRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		WorkflowID: "wf-approval-test", WorkflowVersion: 1,
		ProjectID: proj.ID, Status: domain.WorkflowRunRunning,
		RunContext: []byte("{}"), WorkItemID: ticket.ID,
	})
	if err != nil {
		t.Fatalf("create workflow run: %v", err)
	}
	env.run = run

	env.step = workflow.StepWire{
		ID:   "step-approval",
		Name: "Approve",
		Kind: domain.StepKindApproval,
		Ref:  "w_se_ai_approver",
		Config: `{"reviewer":"worker","worker_ref":"w_se_ai_approver","max_iterations":3}`,
	}
	sr, err := db.CreateWorkflowStepRun(ctx, ttx.Tx, db.WorkflowStepRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		WorkflowRunID: run.ID, StepID: env.step.ID,
		StepName: env.step.Name, StepKind: env.step.Kind,
		Status: domain.StepRunReady,
	})
	if err != nil {
		t.Fatalf("create step run: %v", err)
	}
	env.stepRun = sr

	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit fixture: %v", err)
	}
	return env
}

func (e *approvalTestEnv) countWorkItems(t *testing.T) int {
	t.Helper()
	ttx, err := e.pool.BeginTenantTx(context.Background(), approvalTestTenant)
	if err != nil {
		t.Fatalf("count: begin tx: %v", err)
	}
	defer ttx.Rollback(context.Background())
	items, err := db.ListWorkItems(context.Background(), ttx.Tx, db.ListWorkItemsFilter{
		TenantID: approvalTestTenant, ProjectID: e.projectID, PageSize: 1000,
	})
	if err != nil {
		t.Fatalf("count work items: %v", err)
	}
	return len(items)
}

// TestApprovalDispatchCreatesNoWorkItems is the core acceptance test: a
// worker-backed approval step must dispatch the approver against the
// run's shared ticket without creating any work item, and the step run —
// the approval record — must carry the composite prompt, the approver
// worker pin, the upstream context, and the pending decision.
func TestApprovalDispatchCreatesNoWorkItems(t *testing.T) {
	env := newApprovalTestEnv(t)
	ctx := context.Background()

	before := env.countWorkItems(t)
	if before != 1 {
		t.Fatalf("fixture should contain exactly the ticket, got %d work items", before)
	}

	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)

	runs := map[string]db.WorkflowStepRunRow{env.step.ID: env.stepRun}
	var dispatched []dispatchReq
	err = env.reconciler.dispatchStep(ctx, ttx.Tx, approvalTestTenant, env.run,
		env.step, env.stepRun, runs, []workflow.StepWire{env.step}, &dispatched, nil)
	if err != nil {
		t.Fatalf("dispatchStep: %v", err)
	}

	// 1. No work item was created.
	if after := len(env.mustList(t, ttx.Tx)); after != before {
		t.Fatalf("worker-backed approval created a work item: before=%d after=%d", before, after)
	}

	// 2. The inline dispatch targets the shared ticket + this step run.
	if len(dispatched) != 1 {
		t.Fatalf("expected exactly one dispatch, got %d", len(dispatched))
	}
	if dispatched[0].taskID != env.ticketID || dispatched[0].stepRunID != env.stepRun.ID {
		t.Fatalf("dispatch = %+v, want {taskID:%s stepRunID:%s}", dispatched[0], env.ticketID, env.stepRun.ID)
	}

	// 3. The step run (the approval record) carries the full context.
	updated := runs[env.step.ID]
	var res struct {
		WorkItemID    string   `json:"_work_item_id"`
		Prompt        string   `json:"_prompt"`
		WorkerID      string   `json:"_worker_id"`
		WorkerVersion int      `json:"_worker_version"`
		Decision      string   `json:"_decision"`
		UpstreamFiles []string `json:"_upstream_files"`
	}
	if err := json.Unmarshal(updated.Result, &res); err != nil {
		t.Fatalf("step run result: %v", err)
	}
	if res.WorkItemID != env.ticketID {
		t.Errorf("_work_item_id = %q, want ticket %q", res.WorkItemID, env.ticketID)
	}
	if res.Prompt == "" {
		t.Errorf("_prompt is empty — the approver would run without context")
	}
	if res.WorkerID != "w_se_ai_approver" {
		t.Errorf("_worker_id = %q, want w_se_ai_approver", res.WorkerID)
	}
	if res.WorkerVersion <= 0 {
		t.Errorf("_worker_version = %d, want the resolved published version", res.WorkerVersion)
	}
	if res.Decision != "pending" {
		t.Errorf("_decision = %q, want pending", res.Decision)
	}
	if updated.Status != domain.StepRunRunning {
		t.Errorf("step run status = %q, want running", updated.Status)
	}

	// 4. The shared ticket stays untouched (no assigned worker, no
	//    prompt_context, still running for the whole run).
	ticket, err := db.GetWorkItem(ctx, ttx.Tx, approvalTestTenant, env.ticketID)
	if err != nil {
		t.Fatalf("get ticket: %v", err)
	}
	if ticket.Status != domain.WorkItemRunning {
		t.Errorf("ticket status mutated: %q", ticket.Status)
	}
	if len(ticket.AssignedWorkerRef) != 0 {
		t.Errorf("ticket assigned_worker_ref mutated: %s", ticket.AssignedWorkerRef)
	}
	if len(ticket.PromptContext) != 0 {
		t.Errorf("ticket prompt_context mutated: %s", ticket.PromptContext)
	}
}

// TestApprovalRetryDoesNotCloneTicket verifies the failure/retry path
// stays artifact-free: when the approver execution fails, the retry
// strategy re-dispatches the SAME step run against the SAME ticket (no
// fresh work item) and increments the attempt counter.
func TestApprovalRetryDoesNotCloneTicket(t *testing.T) {
	env := newApprovalTestEnv(t)
	ctx := context.Background()
	before := env.countWorkItems(t)

	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)

	// Link a FAILED approver execution to the step run (the adapter
	// completed the run with a failure; pollTaskStep will retry).
	now := time.Now().UTC()
	exec, err := db.CreateExecution(ctx, ttx.Tx, db.ExecutionRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		ProjectID: env.projectID, TaskID: env.ticketID,
		WorkerID: "w_se_ai_approver", WorkerVersion: 1,
		Status: domain.ExecutionFailed, HealthState: domain.HealthTerminating,
		StartedAt: &now, EndedAt: &now,
		WorkflowRunID: env.run.ID, WorkflowStepID: env.step.ID,
		ErrorMessage: "the model refused",
	})
	if err != nil {
		t.Fatalf("create failed execution: %v", err)
	}
	runningResult, _ := json.Marshal(map[string]any{"_work_item_id": env.ticketID, "_decision": "pending"})
	runningSR, err := db.UpdateWorkflowStepRun(ctx, ttx.Tx, approvalTestTenant,
		env.stepRun.ID, env.stepRun.Version, db.UpdateWorkflowStepRunFields{
			Status:            strPtr(domain.StepRunRunning),
			Result:            &runningResult,
			WorkerExecutionID: &exec.ID,
		})
	if err != nil {
		t.Fatalf("mark step run running: %v", err)
	}

	runByID := map[string]db.WorkflowStepRunRow{env.step.ID: runningSR}
	var triggers []recoveryTriggerReq
	terminal, failed, err := env.reconciler.pollTaskStep(ctx, ttx.Tx, approvalTestTenant,
		env.run, runningSR, "", runByID, &triggers)
	if err != nil {
		t.Fatalf("pollTaskStep: %v", err)
	}
	if terminal || failed {
		t.Fatalf("pollTaskStep prematurely terminal (terminal=%v failed=%v) with attempts remaining", terminal, failed)
	}

	// No work item was cloned for the retry.
	if after := len(env.mustList(t, ttx.Tx)); after != before {
		t.Fatalf("approval retry cloned a work item: before=%d after=%d", before, after)
	}

	// The step run is recovering with attempt incremented and the SAME
	// ticket still recorded (the record was not replaced by a clone id).
	recovering := runByID[env.step.ID]
	if recovering.Status != domain.StepRunRecovering {
		t.Errorf("step run status = %q, want recovering", recovering.Status)
	}
	if recovering.Attempt != 1 {
		t.Errorf("attempt = %d, want 1", recovering.Attempt)
	}
	var res struct {
		WorkItemID string `json:"_work_item_id"`
	}
	_ = json.Unmarshal(recovering.Result, &res)
	if res.WorkItemID != env.ticketID {
		t.Errorf("retry _work_item_id = %q, want ticket %q", res.WorkItemID, env.ticketID)
	}

	// Re-dispatching the recovering step must succeed against the same
	// ticket, clear the stale execution link, and queue exactly one
	// dispatch — again without creating any work item.
	var dispatched []dispatchReq
	err = env.reconciler.dispatchStep(ctx, ttx.Tx, approvalTestTenant, env.run,
		env.step, recovering, runByID, []workflow.StepWire{env.step}, &dispatched, nil)
	if err != nil {
		t.Fatalf("re-dispatch recovering approval step: %v", err)
	}
	if len(dispatched) != 1 || dispatched[0].taskID != env.ticketID {
		t.Fatalf("re-dispatch = %+v, want a single dispatch on ticket %s", dispatched, env.ticketID)
	}
	redone := runByID[env.step.ID]
	if redone.Status != domain.StepRunRunning {
		t.Errorf("re-dispatched step status = %q, want running", redone.Status)
	}
	if redone.WorkerExecutionID != "" {
		t.Errorf("re-dispatched step still links the failed execution %q", redone.WorkerExecutionID)
	}
	if after := len(env.mustList(t, ttx.Tx)); after != before {
		t.Fatalf("approval re-dispatch created a work item: before=%d after=%d", before, after)
	}
}

// TestApprovalDispatchFailsWithoutTicket verifies the documented
// constraint: a worker-backed approval step on a one-shot run with no
// bound ticket and no upstream WORK_ITEM marker fails the step with a
// clear message instead of silently creating an artifact.
func TestApprovalDispatchFailsWithoutTicket(t *testing.T) {
	pool := approvalTestPool(t)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	rc := NewWorkflowReconciler(pool, logger, nil, nil, nil, nil)

	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)

	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		Name: "One Shot", Slug: "one-shot-" + db.NewID()[:8],
		Status: "active", Goals: []byte("[]"),
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	run, err := db.CreateWorkflowRun(ctx, ttx.Tx, db.WorkflowRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		WorkflowID: "wf-one-shot", WorkflowVersion: 1,
		ProjectID: proj.ID, Status: domain.WorkflowRunRunning,
		RunContext: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create one-shot run: %v", err)
	}
	step := workflow.StepWire{
		ID: "step-approval", Name: "Approve", Kind: domain.StepKindApproval,
		Ref:    "w_se_ai_approver",
		Config: `{"reviewer":"worker"}`,
	}
	sr, err := db.CreateWorkflowStepRun(ctx, ttx.Tx, db.WorkflowStepRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		WorkflowRunID: run.ID, StepID: step.ID,
		StepName: step.Name, StepKind: step.Kind,
		Status: domain.StepRunReady,
	})
	if err != nil {
		t.Fatalf("create step run: %v", err)
	}

	runs := map[string]db.WorkflowStepRunRow{step.ID: sr}
	var dispatched []dispatchReq
	if err := rc.dispatchStep(ctx, ttx.Tx, approvalTestTenant, run,
		step, sr, runs, []workflow.StepWire{step}, &dispatched, nil); err != nil {
		t.Fatalf("dispatchStep should fail the step, not error out: %v", err)
	}
	if len(dispatched) != 0 {
		t.Fatalf("expected no dispatch without a ticket, got %+v", dispatched)
	}
	failed := runs[step.ID]
	if failed.Status != domain.StepRunFailed {
		t.Errorf("step status = %q, want failed", failed.Status)
	}
	var res struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(failed.Result, &res)
	if res.Error == "" || len(res.Error) > 200 {
		t.Errorf("expected a clear failure reason, got %q", res.Error)
	}
}

// mustList re-queries work items inside the caller's transaction.
func (e *approvalTestEnv) mustList(t *testing.T, tx pgx.Tx) []db.WorkItemRow {
	t.Helper()
	items, err := db.ListWorkItems(context.Background(), tx, db.ListWorkItemsFilter{
		TenantID: approvalTestTenant, ProjectID: e.projectID, PageSize: 1000,
	})
	if err != nil {
		t.Fatalf("list work items: %v", err)
	}
	return items
}
