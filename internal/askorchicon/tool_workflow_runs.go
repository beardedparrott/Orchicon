package askorchicon

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

func toolListWorkflowRuns(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		WorkflowID string `json:"workflow_id"`
		Status     string `json:"status"`
	}
	if len(args) > 0 && string(args) != "null" {
		json.Unmarshal(args, &params)
	}
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	runs, err := db.ListWorkflowRuns(ctx, ttx.Tx, db.ListWorkflowRunsFilter{
		TenantID:   tenantID,
		WorkflowID: params.WorkflowID,
		Status:     params.Status,
	})
	if err != nil {
		return nil, err
	}
	if runs == nil {
		return json.RawMessage("[]"), nil
	}
	return json.Marshal(runs)
}

func toolGetWorkflowRun(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	if params.ID == "" {
		return nil, fmt.Errorf("id is required")
	}
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	run, err := db.GetWorkflowRun(ctx, ttx.Tx, tenantID, params.ID)
	if err != nil {
		return nil, err
	}
	return json.Marshal(run)
}

// toolForceProgressWorkflowRun advances a stuck running run past its current
// in-flight step run(s) regardless of their previous status — the Ask Orchicon
// counterpart to the ForceProgressWorkflowRun RPC (a run can wedge "running"
// forever after a reconcile pass errored on a LATER step and rolled back an
// upstream step's terminal mark, e.g. a corrupted worker_versions index hid a
// worker). Marks every active non-terminal step run succeeded with a
// `_forced: true` note, terminates still-running linked executions, and lets
// the reconciler advance the DAG on its next scan.
func toolForceProgressWorkflowRun(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	if params.RunID == "" {
		return nil, fmt.Errorf("run_id is required")
	}
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)

	run, err := db.GetWorkflowRun(ctx, ttx.Tx, tenantID, params.RunID)
	if err != nil {
		return nil, err
	}
	if run.Status == domain.WorkflowRunCompleted || run.Status == domain.WorkflowRunFailed || run.Status == domain.WorkflowRunAborted {
		return nil, fmt.Errorf("run is already terminal (status=%s) — nothing to force", run.Status)
	}
	stepRuns, err := db.ListWorkflowStepRuns(ctx, ttx.Tx, tenantID, params.RunID)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	var forced []string
	for _, sr := range stepRuns {
		if sr.SupersededBy != "" {
			continue
		}
		switch sr.Status {
		case domain.StepRunSucceeded, domain.StepRunFailed, domain.StepRunSkipped, domain.StepRunBlocked:
			continue
		}
		var result map[string]any
		if len(sr.Result) > 0 {
			_ = json.Unmarshal(sr.Result, &result)
		}
		if result == nil {
			result = map[string]any{}
		}
		result["_forced"] = true
		result["_forced_at"] = now.Format(time.RFC3339)
		result["_forced_reason"] = "manual force-progress"
		forcedResult, _ := json.Marshal(result)

		updated, err := db.UpdateWorkflowStepRun(ctx, ttx.Tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
			Status:  &succeededStatus,
			Result:  &forcedResult,
			EndedAt: &now,
		})
		if err != nil {
			return nil, err
		}
		forced = append(forced, sr.ID)
		_ = updated

		if sr.WorkerExecutionID != "" {
			if exec, err := db.GetExecution(ctx, ttx.Tx, tenantID, sr.WorkerExecutionID); err == nil {
				if exec.Status == domain.ExecutionRunning || exec.Status == domain.ExecutionDispatching {
					termStatus := domain.ExecutionTerminated
					termHealth := domain.HealthUnhealthy
					_, _ = db.UpdateExecution(ctx, ttx.Tx, tenantID, exec.ID, exec.Version, db.UpdateExecutionFields{
						Status:       &termStatus,
						HealthState:  &termHealth,
						EndedAt:      &now,
						ErrorMessage: strPtrTool("forced past by ForceProgressWorkflowRun"),
					})
				}
			}
		}
	}
	if len(forced) == 0 {
		return nil, fmt.Errorf("no active step runs to force — the run may already be advancing")
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"run_id":              run.ID,
		"forced_step_run_ids": forced,
	})
}

var succeededStatus = domain.StepRunSucceeded

func strPtrTool(s string) *string { return &s }
