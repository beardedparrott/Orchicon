package db

// execution_retention.go — keeping the execution HISTORY bounded.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/beardedparrott/orchicon/internal/procwin"
)

// ExecutionRetentionPlan is what a prune WOULD do (or did), computed from the live data.
type ExecutionRetentionPlan struct {
	TenantID string
	Keep     int

	TotalExecutions int64
	KeepExecutions  int64
	DeleteExecutions int64

	// LiveAmongDelete is the number of candidates in a NON-TERMINAL state.
	LiveAmongDelete int64

	// Child rows in the delete set, table by table.
	SessionParts      int64
	Checkpoints       int64
	RecoveryExecs     int64
	RecoveryStepRuns  int64
	StepRunRefs       int64
	UsageRecords      int64
	UsageRecordsKept  int64
	WorktreeDirsOnDisk int64

	// Oldest/Newest of the delete set, so the operator can see the WINDOW being removed rather than just
	// a count.
	OldestDeleted *time.Time
	NewestDeleted *time.Time
	// KeepBoundary is the created_at of the oldest row that SURVIVES — the line the prune cuts on.
	KeepBoundary *time.Time

	// WorktreesRemoved / WorktreeError report the on-disk half of the prune.
	WorktreesRemoved int
	WorktreeError    error

	// worktreePaths is the set the plan found on disk, carried to the prune so the removal and the count
	// describe the same directories.
	worktreePaths []string
}

// keepSetSQL is the KEEP SET: the newest N executions for the tenant by created_at.
const keepSetSQL = `(SELECT id FROM worker_executions
                    WHERE tenant_id = $1
                    ORDER BY created_at DESC, id DESC
                    LIMIT $2)`

// deleteSetSQL is everything NOT in the keep set, for the tenant.
const deleteSetSQL = `(SELECT id FROM worker_executions
                       WHERE tenant_id = $1 AND id NOT IN ` + keepSetSQL + `)`

// candidateStatusesSQL is the TERMINAL set a prune is allowed to remove: an execution that has
// finished.
const candidateStatusesSQL = `status IN ('succeeded','failed','failed_to_start','terminated','completed')`

// PlanExecutionRetention computes what pruning to the newest `keep` executions would remove, without
// changing anything.
func (p *Pool) PlanExecutionRetention(ctx context.Context, tenantID string, keep int) (ExecutionRetentionPlan, error) {
	if keep < 0 {
		return ExecutionRetentionPlan{}, fmt.Errorf("db: plan execution retention: keep must be >= 0 (got %d)", keep)
	}
	plan := ExecutionRetentionPlan{TenantID: tenantID, Keep: keep}

	// The sets, with the terminal-state guard applied to the DELETE side.
	const candidates = `(SELECT id FROM worker_executions
	                     WHERE tenant_id = $1 AND ` + candidateStatusesSQL + `
	                       AND id NOT IN ` + keepSetSQL + `)`

	steps := []struct {
		dest *int64
		q    string
	}{
		{&plan.TotalExecutions, `SELECT count(*) FROM worker_executions WHERE tenant_id = $1`},
		{&plan.KeepExecutions, `SELECT count(*) FROM ` + keepSetSQL + ` k`},
		{&plan.DeleteExecutions, `SELECT count(*) FROM ` + candidates + ` c`},
		{&plan.LiveAmongDelete, `SELECT count(*) FROM worker_executions w
			WHERE w.tenant_id = $1 AND w.id NOT IN ` + keepSetSQL + ` AND NOT (` + candidateStatusesSQL + `)`},
		{&plan.SessionParts, `SELECT count(*) FROM execution_session_parts WHERE tenant_id = $1 AND execution_id IN ` + candidates},
		{&plan.Checkpoints, `SELECT count(*) FROM checkpoints WHERE worker_execution_id IN ` + candidates},
		{&plan.RecoveryExecs, `SELECT count(*) FROM recovery_executions WHERE tenant_id = $1 AND failed_execution_id IN ` + candidates},
		{&plan.RecoveryStepRuns, `SELECT count(*) FROM recovery_step_runs WHERE tenant_id = $1 AND worker_execution_id IN ` + candidates},
		{&plan.StepRunRefs, `SELECT count(*) FROM workflow_step_runs WHERE tenant_id = $1 AND worker_execution_id IN ` + candidates},
		{&plan.UsageRecords, `SELECT count(*) FROM usage_records WHERE tenant_id = $1 AND execution_id IN ` + candidates},
		{&plan.UsageRecordsKept, `SELECT count(*) FROM usage_records WHERE tenant_id = $1 AND execution_id IN ` + keepSetSQL},
	}
	for _, s := range steps {
		// ONLY THE QUERIES WHOSE SQL USES $2 MAY BE GIVEN IT: pgx rejects a parameter the statement does
		// not reference outright ("expected 1 arguments, got 2"), so a blanket (tenantID.
		args := []any{tenantID}
		if strings.Contains(s.q, "$2") {
			args = append(args, keep)
		}
		if err := p.QueryRow(ctx, s.q, args...).Scan(s.dest); err != nil {
			return ExecutionRetentionPlan{}, fmt.Errorf("db: plan execution retention: %w", err)
		}
	}

	_ = p.QueryRow(ctx, `SELECT min(created_at), max(created_at) FROM worker_executions
		WHERE tenant_id = $1 AND id IN `+candidates, tenantID, keep).Scan(&plan.OldestDeleted, &plan.NewestDeleted)
	_ = p.QueryRow(ctx, `SELECT created_at FROM worker_executions
		WHERE tenant_id = $1 ORDER BY created_at DESC, id DESC OFFSET $2 - 1 LIMIT 1`, tenantID, keep).Scan(&plan.KeepBoundary)

	// Worktrees whose DIRECTORY still exists.
	var paths []string
	rows, err := p.Query(ctx, `SELECT worktree_path FROM worker_executions
		WHERE tenant_id = $1 AND id IN `+candidates+` AND worktree_status = 'ready' AND worktree_path <> ''`,
		tenantID, keep)
	if err == nil {
		for rows.Next() {
			var path string
			if rows.Scan(&path) == nil {
				paths = append(paths, path)
			}
		}
		rows.Close()
	}
	plan.WorktreeDirsOnDisk = int64(len(paths))
	plan.worktreePaths = paths

	return plan, nil
}

// PruneExecutionHistory removes every execution older than the newest `keep` for the tenant, with
// its children, and returns the plan describing exactly what it removed.
func (p *Pool) PruneExecutionHistory(ctx context.Context, tenantID string, keep int, batch int) (ExecutionRetentionPlan, error) {
	plan, err := p.PlanExecutionRetention(ctx, tenantID, keep)
	if err != nil {
		return ExecutionRetentionPlan{}, err
	}
	if plan.LiveAmongDelete > 0 {
		return plan, fmt.Errorf("db: prune execution history: refusing to prune %d execution(s) that are still "+
			"live — this pass removes HISTORY, not running work (cancel or wait for them first)",
			plan.LiveAmongDelete)
	}
	if plan.DeleteExecutions == 0 {
		return plan, nil
	}
	if batch <= 0 {
		batch = 500
	}

	const candidates = `(SELECT id FROM worker_executions
	                     WHERE tenant_id = $1 AND ` + candidateStatusesSQL + `
	                       AND id NOT IN ` + keepSetSQL + `)`

	// LEAF-FIRST, and every step is idempotent, so a re-run after a partial prune simply finds less.
	childSteps := []struct {
		what string
		q    string
	}{
		{"execution session parts", `DELETE FROM execution_session_parts
			WHERE tenant_id = $1 AND execution_id NOT IN ` + keepSetSQL},
		{"checkpoints", `DELETE FROM checkpoints
			WHERE worker_execution_id <> ''
			  AND worker_execution_id NOT IN ` + keepSetSQL},
		{"recovery step runs", `DELETE FROM recovery_step_runs
			WHERE tenant_id = $1 AND worker_execution_id <> ''
			  AND worker_execution_id NOT IN ` + keepSetSQL},
		// THE RECOVERY ITSELF, not only the step runs.
		{"continuation plans", `DELETE FROM continuation_plans
			WHERE recovery_id IN (SELECT id FROM recovery_executions
			                      WHERE tenant_id = $1 AND failed_execution_id <> ''
			                        AND failed_execution_id NOT IN ` + keepSetSQL + `)`},
		{"recovery executions", `DELETE FROM recovery_executions
			WHERE tenant_id = $1 AND failed_execution_id <> ''
			  AND failed_execution_id NOT IN ` + keepSetSQL},
		// THE STEP-RUN REFERENCE IS CLEARED, NOT THE STEP RUN.
		{"workflow step run references", `UPDATE workflow_step_runs SET worker_execution_id = ''
			WHERE tenant_id = $1 AND worker_execution_id <> ''
			  AND worker_execution_id NOT IN ` + keepSetSQL},
		// USAGE IS DE-LINKED, NOT DELETED — deliberately.
		{"usage record links", `UPDATE usage_records SET execution_id = ''
			WHERE tenant_id = $1 AND execution_id <> ''
			  AND execution_id NOT IN ` + keepSetSQL},
	}
	for _, s := range childSteps {
		if _, err := p.Exec(ctx, s.q, tenantID, keep); err != nil {
			return plan, fmt.Errorf("db: prune execution history (%s): %w", s.what, err)
		}
	}

	// WORKTREES ON DISK, before the rows that record them go: after the delete nothing can find them.
	removedTrees, treeErr := RemoveWorktreePaths(ctx, plan.worktreePaths)

	// THE EXECUTIONS, in batches, so no single statement holds the table for the whole prune.
	for {
		tag, err := p.Exec(ctx, `DELETE FROM worker_executions WHERE id IN (
			SELECT id FROM worker_executions
			WHERE tenant_id = $1 AND `+candidateStatusesSQL+`
			  AND id NOT IN `+keepSetSQL+`
			LIMIT $3)`, tenantID, keep, batch)
		if err != nil {
			return plan, fmt.Errorf("db: prune execution history (executions): %w", err)
		}
		if tag.RowsAffected() == 0 {
			break
		}
	}
	plan.WorktreesRemoved = removedTrees
	plan.WorktreeError = treeErr
	return plan, nil
}

// IT ONLY EVER REMOVES A PATH GIVEN TO IT — a path read out of the very row being deleted — and it
// treats a failure as a REPORTABLE OUTCOME rather than an error.
func RemoveWorktreePaths(ctx context.Context, paths []string) (int, error) {
	removed := 0
	var firstErr error
	for _, path := range paths {
		if path == "" {
			continue
		}
		if fi, err := os.Stat(path); err != nil || !fi.IsDir() {
			// Already gone: nothing to remove, and nothing to report.
			continue
		}
		// The project dir is the worktree's PARENT (.orchicon-worktrees/<id>), which is the directory git
		// must be run from for `worktree remove` to resolve the worktree.
		parent := filepath.Dir(path)
		cmd := procwin.CommandContext(ctx, "git", "-C", parent, "worktree", "remove", "--force", path)
		if err := cmd.Run(); err != nil {
			if rerr := os.RemoveAll(path); rerr != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("remove worktree %s: %w", path, rerr)
				}
				continue
			}
		}
		removed++
	}
	return removed, firstErr
}
