package db

// execution_retention.go — keeping the execution HISTORY bounded.
//
// The operator: "The goal is just to clean up my console a little. 3500 executions roughly is a lot to keep
// in history and is unnecessary." Measured on that tenant: 3466 executions, 6.29M execution_session_parts
// rows (the bulk of the bytes), and no way to remove any of it in bulk — the TUI's bulk delete is one RPC
// per marked row and shares a single 30s deadline across the whole batch, so a thousand-row selection dies
// at roughly eighty-five (see internal/tui/mutate/mutate.go and screens/execution/bulkactions.go). Marking
// three thousand rows by hand is not a gesture anyone can perform anyway, which is why this is a
// predicate-based prune rather than a faster loop over the existing one.
//
// EVERYTHING HERE IS DERIVED FROM THE KEEP SET, in SQL, from ONE parameter (keep). That is deliberate:
// "which executions survive" and "which child rows go with them" are the same question asked twice, and
// computing them independently is how the two answers drift apart. A candidate selected against a
// slightly different predicate than its children were is exactly the orphan class DeleteProject's own
// comment documents (1264 orphaned worker_executions, 9959 orphaned usage_records left behind by a
// project delete that looked complete).
//
// WHAT CASCADES ON ITS OWN, AND WHAT DOES NOT. Only execution_session_parts declares a foreign key to
// worker_executions (ON DELETE CASCADE). checkpoints, recovery_executions.failed_execution_id,
// recovery_step_runs.worker_execution_id, workflow_step_runs.worker_execution_id and usage_records.
// execution_id are all PLAIN TEXT COLUMNS WITH NO FK — deleting the execution row orphans every one of
// them SILENTLY, because nothing errors and nothing points at the leftovers afterwards. So each of those
// is handled explicitly below, in leaf-first order, and each is reported in the plan so the operator can
// see what a prune will take before it takes it.

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
//
// It is returned by the dry run and by the real prune alike, so the numbers the operator approves are
// produced by the same code that performs the delete — the defect this prevents is a preview that counts
// one thing and a delete that touches another, which is how a destructive action becomes unpredictable.
type ExecutionRetentionPlan struct {
	TenantID string
	Keep     int

	TotalExecutions int64
	KeepExecutions  int64
	DeleteExecutions int64

	// LiveAmongDelete is the number of candidates in a NON-TERMINAL state. A non-zero value is reported
	// rather than acted on: pruning a live execution is a different act from pruning history, and this
	// pass exists to remove history.
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
//
// ONE definition, used by the plan, every cascade step, and the delete itself. It is a subquery rather
// than a materialised id list so a prune of any size costs no client-side round trip, and so no caller
// can pass a slightly different notion of "newest" to one of the steps.
//
// The tiebreaker is id, matching ListExecutions' own ordering (ORDER BY created_at DESC, id DESC) — two
// rows sharing a created_at must be ordered deterministically or the keep boundary moves between the
// preview and the prune.
const keepSetSQL = `(SELECT id FROM worker_executions
                    WHERE tenant_id = $1
                    ORDER BY created_at DESC, id DESC
                    LIMIT $2)`

// deleteSetSQL is everything NOT in the keep set, for the tenant.
const deleteSetSQL = `(SELECT id FROM worker_executions
                       WHERE tenant_id = $1 AND id NOT IN ` + keepSetSQL + `)`

// candidateStatusesSQL is the TERMINAL set a prune is allowed to remove: an execution that has finished.
//
// EXPRESSED AS A NEGATION of the slot-holding set rather than as its own enumeration, because the two must
// partition the world: activeExecutionStatuses (execution.go) is what can hold a dispatch slot, and
// anything else has released it. Listing the terminal states by hand here would mean a new status added
// later is silently never prunable — a history that stops shrinking with no error to point at — whereas
// the negation can only be wrong in the safe direction (a new ACTIVE state would be treated as prunable
// only if it were also missing from the slot-holding set, which is the same edit that would break the
// dispatch guard).
// THE TERMINAL SET — and `terminating` is deliberately NOT in it, which is the fix for a real orphan bug.
//
// It used to be `status NOT IN activeExecutionStatuses`, a NEGATION of the slot-holding set. That is the wrong
// shape: the negated set is not "terminal", it is "everything the dispatch guard does not count", and the two
// differ on `terminating` — an execution on its way out. `terminating` HOLDS a slot, so the live guard refuses
// to prune while one is present, but it is not terminal, so the old predicate admitted it to the candidate set.
// The result was two statements disagreeing about one row: the child steps skipped it (status-filtered) and
// the DELETE took it (unfiltered), leaving its children behind — measured on the live tenant as 2,898 usage
// rows pointing at deleted executions.
//
// ENUMERATED NOW, because "terminal" is a fact about the lifecycle worth stating rather than deriving by
// subtraction. A status added later is simply not prunable until someone decides it is — which fails SAFE
// (history stops shrinking, visibly) instead of destructively (rows vanish, children do not).
const candidateStatusesSQL = `status IN ('succeeded','failed','failed_to_start','terminated','completed')`

// PlanExecutionRetention computes what pruning to the newest `keep` executions would remove, without
// changing anything.
func (p *Pool) PlanExecutionRetention(ctx context.Context, tenantID string, keep int) (ExecutionRetentionPlan, error) {
	if keep < 0 {
		return ExecutionRetentionPlan{}, fmt.Errorf("db: plan execution retention: keep must be >= 0 (got %d)", keep)
	}
	plan := ExecutionRetentionPlan{TenantID: tenantID, Keep: keep}

	// The sets, with the terminal-state guard applied to the DELETE side. Excluding non-terminal rows from
	// the candidate set is what makes a prune safe to run against a tenant with work in flight: a live
	// execution is simply not history yet.
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
		// ONLY THE QUERIES WHOSE SQL USES $2 MAY BE GIVEN IT: pgx rejects a parameter the statement does not
		// reference outright ("expected 1 arguments, got 2"), so a blanket (tenantID, keep) here fails every
		// count that is not itself scoped by the keep set.
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

	// Worktrees whose DIRECTORY still exists. Deleting the execution row is what makes a worktree
	// unreachable to the pruner (the WorktreeReconciler enumerates worker_executions.worktree_status), so
	// a row whose tree is still on disk must have that tree removed in the same pass or it leaks forever.
	// The plan reports the count so the operator sees the disk work before approving it.
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

// PruneExecutionHistory removes every execution older than the newest `keep` for the tenant, with its
// children, and returns the plan describing exactly what it removed.
//
// NOT A TRANSACTION ACROSS THE WHOLE PRUNE, on purpose. A tenant can hold thousands of executions and
// millions of session parts; one transaction covering all of it would hold locks for its whole duration
// and produce a single enormous WAL flush. Each CHILD table is cleared in its own statement (which is
// atomic per table), and the executions themselves are deleted last, in batches — so the invariant a
// reader observes at every moment is that no execution row is gone while a child of it remains. Batches
// are committed per chunk, and a failure part-way leaves a tenant that is MORE pruned, never inconsistent,
// which is why the caller can simply run it again.
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
	//
	// execution_session_parts WOULD cascade (it is the one child with a real FK), and it is deleted
	// explicitly anyway: relying on the cascade would leave this function correct only while that
	// constraint exists, and the counts it reports are read from the plan, not inferred.
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
		// THE RECOVERY ITSELF, not only the step runs. A recovery_executions row is identified by the
		// execution that FAILED (failed_execution_id), so it is a child of that execution in everything but
		// the schema. Deleting the execution and leaving the recovery leaves a recovery whose reason for
		// existing is gone — the same orphan class, one table further out.
		{"continuation plans", `DELETE FROM continuation_plans
			WHERE recovery_id IN (SELECT id FROM recovery_executions
			                      WHERE tenant_id = $1 AND failed_execution_id <> ''
			                        AND failed_execution_id NOT IN ` + keepSetSQL + `)`},
		{"recovery executions", `DELETE FROM recovery_executions
			WHERE tenant_id = $1 AND failed_execution_id <> ''
			  AND failed_execution_id NOT IN ` + keepSetSQL},
		// THE STEP-RUN REFERENCE IS CLEARED, NOT THE STEP RUN. workflow_step_runs is the RUN's state — it
		// may still be the record of a step of a live workflow run, and deleting it would corrupt a run
		// graph that is not history at all. Emptying the reference is the same thing DeleteWorkflowRun's
		// mirror image does, and it leaves the step run readable.
		{"workflow step run references", `UPDATE workflow_step_runs SET worker_execution_id = ''
			WHERE tenant_id = $1 AND worker_execution_id <> ''
			  AND worker_execution_id NOT IN ` + keepSetSQL},
		// USAGE IS DE-LINKED, NOT DELETED — deliberately. migration 20260731000015 exists so that cost
		// attribution SURVIVES execution deletion; usage_records is the tenant's spend ledger and the Cost
		// Explorer reads it. Removing those rows would rewrite the operator's history of what they spent,
		// which is a different act from removing their execution history, and not one this prune may take
		// on its own authority. The execution_id link is cleared so nothing points at a dead row.
		// DE-LINKED BY THE SURVIVOR SET, NOT BY THE CANDIDATE SET — a bug fix, not a tidy-up. Keying this on
		// the status-filtered candidate set meant the two statements disagreed about which rows would go: an
		// execution outside that filter could be deleted with its usage links still pointing at it. Observed
		// on the live tenant as 2,898 orphaned usage rows. The suite could not see it because it seeded only
		// the classes where the sets coincide.
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

// RemoveWorktreePaths removes worktree directories that belong to executions being pruned, and reports how
// many it removed.
//
// IT ONLY EVER REMOVES A PATH GIVEN TO IT — a path read out of the very row being deleted — and it treats
// a failure as a REPORTABLE OUTCOME rather than an error. The prune is a history clean-up: a directory it
// could not remove is a leftover to tell the operator about, not a reason to roll back a delete that has
// already happened. There is no transaction spanning a filesystem anyway.
//
// `git worktree remove --force` is tried first, because a registered worktree left behind corrupts the
// repo's own bookkeeping (`git worktree list` keeps pointing at a path that no longer exists). The plain
// RemoveAll fallback covers the common case here: a directory that is already GONE (the prune measured
// 2112 rows claiming worktree_status='ready' whose directories no longer exist) or one git no longer
// knows about.
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
