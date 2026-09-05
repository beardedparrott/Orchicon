package recovery

// Integration tests for the recovery-seed keys published at resume time:
// the recovery engine must write _recovery_execution_id + _recovery_worker_id
// + _recovery_worker_version + _recovery_adapter beside _recovery_summary so
// the scheduler's dispatch path can gate the .orchicon/worker.recovery file
// on the SAME worker being re-dispatched and fail fast when the step's
// current version moved off the dead execution's version/adapter
// (model_ref is per-version: same worker ID, different version may mean a
// different adapter).
// Both paths are covered: the step-run path (keys land on the step run's
// result) and the standalone path (keys land on the work item's Results).
// Skipped unless ORCHICON_TEST_DSN points at a disposable database.
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/recovery/ -run TestRecoverySeedKeys -v

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

type seedKeysFixture struct {
	pool      *db.Pool
	taskID    string
	execID    string
	stepRunID string
	runID     string
	projectID string
}

func seedKeysSetup(t *testing.T, workflowBound bool) *seedKeysFixture {
	t.Helper()
	pool := auditPool(t)
	ctx := context.Background()
	const tenant = "tnt_dev"

	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: tenant, Name: "seed-keys", Slug: "seed-keys", Status: "active",
		Goals: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	task, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: tenant, ProjectID: proj.ID, Title: "seed keys task",
		Status: domain.WorkItemAssigned, Kind: domain.WorkItemKindTask,
	})
	if err != nil {
		t.Fatalf("create work item: %v", err)
	}

	f := &seedKeysFixture{pool: pool, taskID: task.ID, projectID: proj.ID}

	if workflowBound {
		run, err := db.CreateWorkflowRun(ctx, ttx.Tx, db.WorkflowRunRow{
			ID: db.NewID(), TenantID: tenant, WorkflowID: db.NewID(), WorkflowVersion: 1,
			ProjectID: proj.ID, Status: domain.WorkflowRunRunning, WorkItemID: task.ID,
			RunContext: []byte("{}"),
		})
		if err != nil {
			t.Fatalf("create workflow run: %v", err)
		}
		sr, err := db.CreateWorkflowStepRun(ctx, ttx.Tx, db.WorkflowStepRunRow{
			ID: db.NewID(), TenantID: tenant, WorkflowRunID: run.ID, StepID: "step-test",
			StepName: "Test Step", StepKind: domain.StepKindTask, Status: domain.StepRunFailed,
		})
		if err != nil {
			t.Fatalf("create step run: %v", err)
		}
		f.runID = run.ID
		f.stepRunID = sr.ID
	}

	now := time.Now().UTC()
	exec, err := db.CreateExecution(ctx, ttx.Tx, db.ExecutionRow{
		ID:             db.NewID(),
		TenantID:       tenant,
		ProjectID:      proj.ID,
		TaskID:         task.ID,
		WorkerID:       "w-failed",
		WorkerVersion:  1,
		Status:         domain.ExecutionFailed,
		HealthState:    domain.HealthUnhealthy,
		StartedAt:      &now,
		EndedAt:        &now,
		WorkflowRunID:  f.runID,
		WorkflowStepID: "step-test",
	})
	if err != nil {
		t.Fatalf("create execution: %v", err)
	}
	f.execID = exec.ID

	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	t.Cleanup(func() {
		ct := context.Background()
		dttx, err := pool.BeginTenantTx(ct, tenant)
		if err == nil {
			_, _ = dttx.Exec(ct, `DELETE FROM execution_session_parts WHERE execution_id = $1`, f.execID)
			_, _ = dttx.Exec(ct, `DELETE FROM recovery_step_runs WHERE tenant_id = $1 AND recovery_id IN (SELECT id FROM recovery_executions WHERE task_id = $2)`, tenant, f.taskID)
			_, _ = dttx.Exec(ct, `DELETE FROM recovery_executions WHERE task_id = $1`, f.taskID)
			_, _ = dttx.Exec(ct, `DELETE FROM worker_executions WHERE id = $1`, f.execID)
			if f.stepRunID != "" {
				_, _ = dttx.Exec(ct, `DELETE FROM workflow_step_runs WHERE id = $1`, f.stepRunID)
			}
			if f.runID != "" {
				_, _ = dttx.Exec(ct, `DELETE FROM workflow_runs WHERE id = $1`, f.runID)
			}
			_, _ = dttx.Exec(ct, `DELETE FROM work_items WHERE id = $1`, f.taskID)
			_, _ = dttx.Exec(ct, `DELETE FROM projects WHERE id = $1`, f.projectID)
			_ = dttx.Commit(ct)
		}
	})
	return f
}

// TestRecoverySeedKeysStandalone drives a standalone recovery to resume and
// verifies the work item's Results carry _recovery_execution_id and
// _recovery_worker_id (the keys the dispatch path seeds the recovery file
// from).
func TestRecoverySeedKeysStandalone(t *testing.T) {
	if os.Getenv("ORCHICON_TEST_DSN") == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed recovery seed test")
	}
	f := seedKeysSetup(t, false)
	ctx := context.Background()
	const tenant = "tnt_dev"
	engine := New(f.pool, slog.New(slog.DiscardHandler))
	rc := NewReconciler(engine)

	if err := engine.TriggerOnFailure(ctx, tenant, f.taskID, f.execID, "", "test", nil); err != nil {
		t.Fatalf("trigger recovery: %v", err)
	}
	// Drive the recovery to resume (one Reconcile call runs all 6 steps +
	// the resume branch).
	rc.Reconcile(ctx, recoveryIDFor(t, f.pool, f.taskID))

	ttx, err := f.pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	task, err := db.GetWorkItem(ctx, ttx.Tx, tenant, f.taskID)
	if err != nil {
		t.Fatalf("get work item: %v", err)
	}
	var results map[string]any
	if err := jsonUnmarshal(task.Results, &results); err != nil {
		t.Fatalf("unmarshal results: %v", err)
	}
	if got, ok := results["_recovery_execution_id"].(string); !ok || got != f.execID {
		t.Errorf("_recovery_execution_id = %q, want %q", got, f.execID)
	}
	if got, ok := results["_recovery_worker_id"].(string); !ok || got != "w-failed" {
		t.Errorf("_recovery_worker_id = %q, want w-failed", got)
	}
	if got, ok := results["_recovery_worker_version"].(float64); !ok || got != 1 {
		t.Errorf("_recovery_worker_version = %v, want 1", results["_recovery_worker_version"])
	}
	if _, ok := results["_recovery_adapter"]; !ok {
		t.Errorf("_recovery_adapter missing (still expected: empty when the dead execution carries none)")
	}
	if task.Status != domain.WorkItemReady {
		t.Errorf("task status = %q, want %q (resumed)", task.Status, domain.WorkItemReady)
	}
}

// TestRecoverySeedKeysStepRun drives a workflow-bound recovery to resume and
// verifies the STEP RUN's result carries the seed keys (the workflow
// dispatch path reads them from there).
func TestRecoverySeedKeysStepRun(t *testing.T) {
	if os.Getenv("ORCHICON_TEST_DSN") == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed recovery seed test")
	}
	f := seedKeysSetup(t, true)
	ctx := context.Background()
	const tenant = "tnt_dev"
	engine := New(f.pool, slog.New(slog.DiscardHandler))
	rc := NewReconciler(engine)

	if err := engine.TriggerOnFailure(ctx, tenant, f.taskID, f.execID, f.stepRunID, "test", nil); err != nil {
		t.Fatalf("trigger recovery: %v", err)
	}
	rc.Reconcile(ctx, recoveryIDFor(t, f.pool, f.taskID))

	ttx, err := f.pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	sr, err := db.GetWorkflowStepRunByStep(ctx, ttx.Tx, tenant, f.runID, "step-test")
	if err != nil {
		t.Fatalf("get step run: %v", err)
	}
	var result map[string]any
	if err := jsonUnmarshal(sr.Result, &result); err != nil {
		t.Fatalf("unmarshal step run result: %v", err)
	}
	if got, ok := result["_recovery_execution_id"].(string); !ok || got != f.execID {
		t.Errorf("step run _recovery_execution_id = %q, want %q", got, f.execID)
	}
	if got, ok := result["_recovery_worker_id"].(string); !ok || got != "w-failed" {
		t.Errorf("step run _recovery_worker_id = %q, want w-failed", got)
	}
	if got, ok := result["_recovery_worker_version"].(float64); !ok || got != 1 {
		t.Errorf("step run _recovery_worker_version = %v, want 1", result["_recovery_worker_version"])
	}
	if _, ok := result["_recovery_adapter"]; !ok {
		t.Errorf("step run _recovery_adapter missing (still expected: empty when the dead execution carries none)")
	}
	if _, ok := result["_recovery_summary"]; !ok {
		t.Errorf("step run _recovery_summary missing (still expected)")
	}
}

func recoveryIDFor(t *testing.T, pool *db.Pool, taskID string) string {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	recs, err := db.ListRecoveries(ctx, ttx.Tx, db.ListRecoveriesFilter{TenantID: "tnt_dev", TaskID: taskID})
	if err != nil {
		t.Fatalf("list recoveries: %v", err)
	}
	if len(recs) == 0 {
		t.Fatal("no recovery rows created")
	}
	return recs[0].ID
}

func jsonUnmarshal(b []byte, v any) error {
	if len(b) == 0 {
		b = []byte("{}")
	}
	return json.Unmarshal(b, v)
}
