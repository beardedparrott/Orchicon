package workflow_test

import (
	"context"
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

// DB-backed integration test for RetryFailedWorkflowRun (skipped unless
// ORCHICON_TEST_DSN points at a disposable database, same convention as
// internal/workitem/validate_test.go):
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/workflow/ -run TestRetryFailedWorkflowRun -v
const retryTestTenant = "tnt_retry_failed"

func retryTestPool(t *testing.T) *db.Pool {
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

// retrySeedRun builds a failed workflow run with a bound work item (also
// failed) and four active step runs: failed / skipped / blocked / succeeded.
func retrySeedRun(t *testing.T, ctx context.Context, pool *db.Pool) (string, map[string]string) {
	t.Helper()
	ttx, err := pool.BeginTenantTx(ctx, retryTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)

	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: retryTestTenant,
		Name: "Retry Test", Slug: "retry-test-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		Status: "active", Goals: []byte("[]"),
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	wi, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: retryTestTenant, ProjectID: proj.ID,
		Kind: domain.WorkItemKindTask, Title: "Retry ticket",
		Status: domain.WorkItemFailed,
	})
	if err != nil {
		t.Fatalf("create work item: %v", err)
	}
	wf, err := db.CreateWorkflow(ctx, ttx.Tx, db.WorkflowRow{
		ID: db.NewID(), TenantID: retryTestTenant, ProjectID: proj.ID,
		Name: "Retry Workflow", CurrentVersion: 1, Status: "published", Type: "one_shot",
	})
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	ended := time.Now().UTC()
	run, err := db.CreateWorkflowRun(ctx, ttx.Tx, db.WorkflowRunRow{
		ID: db.NewID(), TenantID: retryTestTenant, WorkflowID: wf.ID,
		WorkflowVersion: 1, ProjectID: proj.ID, Status: domain.WorkflowRunFailed,
		WorkItemID: wi.ID, RunContext: []byte("{}"), EndedAt: &ended,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	newStep := func(name, status string, attempt int, execID string) string {
		stepID := name + "-" + db.NewID()
		s, err := db.CreateWorkflowStepRun(ctx, ttx.Tx, db.WorkflowStepRunRow{
			ID: db.NewID(), TenantID: retryTestTenant, WorkflowRunID: run.ID,
			StepID: stepID, StepName: name, StepKind: "TASK",
			Status: status, Attempt: attempt,
			Result:            []byte(`{"_summary":"stale result"}`),
			WorkerExecutionID: execID,
			StartedAt:         &ended, EndedAt: &ended,
		})
		if err != nil {
			t.Fatalf("create step run %s: %v", name, err)
		}
		return s.ID
	}

	ids := map[string]string{
		"failed":    newStep("failedStep", domain.StepRunFailed, 3, db.NewID()),
		"skipped":   newStep("skippedStep", domain.StepRunSkipped, 0, ""),
		"blocked":   newStep("blockedStep", domain.StepRunBlocked, 0, ""),
		"succeeded": newStep("succeededStep", domain.StepRunSucceeded, 0, db.NewID()),
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
	return run.ID, ids
}

func retryFetchStep(t *testing.T, ctx context.Context, pool *db.Pool, stepRunID string) db.WorkflowStepRunRow {
	t.Helper()
	ttx, err := pool.BeginTenantTx(ctx, retryTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	sr, err := db.GetWorkflowStepRun(ctx, ttx.Tx, retryTestTenant, stepRunID)
	if err != nil {
		t.Fatalf("fetch step run %s: %v", stepRunID, err)
	}
	return sr
}

// TestRetryFailedWorkflowRunDB verifies the full retry semantics: run →
// pending with ended_at cleared, failed/skipped/blocked step runs → pending
// (result/attempt/execution/ended_at cleared), succeeded steps untouched,
// and the bound work item flipped back to running.
func TestRetryFailedWorkflowRunDB(t *testing.T) {
	pool := retryTestPool(t)
	ctx := tenant.WithID(context.Background(), retryTestTenant)
	runID, ids := retrySeedRun(t, ctx, pool)

	svc := workflow.New(pool, slog.Default(), nil)
	resp, err := svc.RetryFailedWorkflowRun(ctx, connect.NewRequest(&apiv1.RetryFailedWorkflowRunRequest{RunId: runID}))
	if err != nil {
		t.Fatalf("retry failed run: %v", err)
	}
	if resp.Msg.Run.Status != apiv1.WorkflowRunStatus_WORKFLOW_RUN_STATUS_PENDING {
		t.Errorf("run status = %q, want pending", resp.Msg.Run.Status)
	}
	if len(resp.Msg.ResetStepRunIds) != 3 {
		t.Errorf("reset %d step runs, want 3", len(resp.Msg.ResetStepRunIds))
	}

	// Run: pending, ended_at cleared.
	var runStatus string
	var runEnded *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT status, ended_at FROM workflow_runs WHERE id = $1 AND tenant_id = $2`, runID, retryTestTenant,
	).Scan(&runStatus, &runEnded); err != nil {
		t.Fatalf("fetch run: %v", err)
	}
	if runStatus != domain.WorkflowRunPending {
		t.Errorf("run status in db = %q, want pending", runStatus)
	}
	if runEnded != nil {
		t.Errorf("run ended_at should be cleared, got %v", runEnded)
	}

	// Failed/skipped/blocked step runs: pending with cleared fields.
	for _, name := range []string{"failed", "skipped", "blocked"} {
		sr := retryFetchStep(t, ctx, pool, ids[name])
		if sr.Status != domain.StepRunPending {
			t.Errorf("%s status = %q, want pending", name, sr.Status)
		}
		if sr.Attempt != 0 {
			t.Errorf("%s attempt = %d, want 0", name, sr.Attempt)
		}
		if sr.WorkerExecutionID != "" {
			t.Errorf("%s worker_execution_id should be cleared, got %q", name, sr.WorkerExecutionID)
		}
		if string(sr.Result) != "{}" {
			t.Errorf("%s result should be reset to {}, got %q", name, string(sr.Result))
		}
		if sr.EndedAt != nil {
			t.Errorf("%s ended_at should be cleared, got %v", name, sr.EndedAt)
		}
	}

	// Succeeded step run: untouched.
	sr := retryFetchStep(t, ctx, pool, ids["succeeded"])
	if sr.Status != domain.StepRunSucceeded || sr.Attempt != 0 || sr.WorkerExecutionID == "" {
		t.Errorf("succeeded step should be untouched, got status=%q attempt=%d exec=%q", sr.Status, sr.Attempt, sr.WorkerExecutionID)
	}

	// Bound work item: back to running.
	var wiStatus string
	if err := pool.QueryRow(ctx,
		`SELECT wi.status FROM work_items wi
		   JOIN workflow_runs r ON r.work_item_id = wi.id
		  WHERE r.id = $1 AND wi.tenant_id = $2`, runID, retryTestTenant,
	).Scan(&wiStatus); err != nil {
		t.Fatalf("fetch work item: %v", err)
	}
	if wiStatus != domain.WorkItemRunning {
		t.Errorf("work item status = %q, want running", wiStatus)
	}
}

// TestRetryFailedWorkflowRunRejectsNonFailedDB verifies the guard: only a
// failed run can be retried.
func TestRetryFailedWorkflowRunRejectsNonFailedDB(t *testing.T) {
	pool := retryTestPool(t)
	ctx := tenant.WithID(context.Background(), retryTestTenant)
	runID, _ := retrySeedRun(t, ctx, pool)

	// Flip the run to running (the retry must refuse it).
	ttx, err := pool.BeginTenantTx(ctx, retryTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	run, err := db.GetWorkflowRun(ctx, ttx.Tx, retryTestTenant, runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if _, err := db.UpdateWorkflowRun(ctx, ttx.Tx, retryTestTenant, runID, run.Version, db.UpdateWorkflowRunFields{
		Status: strPtr("running"),
	}); err != nil {
		t.Fatalf("flip run to running: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	svc := workflow.New(pool, slog.Default(), nil)
	_, err = svc.RetryFailedWorkflowRun(ctx, connect.NewRequest(&apiv1.RetryFailedWorkflowRunRequest{RunId: runID}))
	if err == nil {
		t.Fatalf("expected FailedPrecondition for a non-failed run")
	}
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("error code = %v, want failed_precondition", connect.CodeOf(err))
	}
}

func strPtr(s string) *string { return &s }
