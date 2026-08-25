// WorktreeReconciler — the control loop that provisions each git-backed
// workflow run its own isolated working tree at arm time
// (architecture-notes/worktree-reconciler.md), and — for concurrent
// step-run dispatch — each parallel-branch step run its own branch
// worktree (architecture-notes/concurrent-step-run-dispatch.md D2).
//
// At run-arm (the pending→running transition in WorkflowReconciler), a run
// that belongs to a git-backed project gets `git worktree add
// <project_dir>/.orchicon-worktrees/<runID> -b <branch> develop`, and the
// resulting path + branch are recorded on the workflow_runs row for
// downstream consumers (execution-cwd wiring, cleanup, non-repo fallback).
// The branch name is deterministic — the work item's kebab-case slug plus a
// short run suffix — so concurrent runs of the same item never collide.
//
// At branch dispatch time, a parallel-branch child step run (a step whose
// depends_on references a `parallel` step) gets its OWN worktree at
// <project_dir>/.orchicon-worktrees/<runID>/<stepRunID> on a sub-branch
// <runBranch>/<stepSlug>-<stepRunSuffix>, so independent branches share no
// filesystem and no .orchicon/ metadata dir. The result is recorded on the
// workflow_step_runs row. The key is composite: "<runID>:<stepRunID>" for
// a branch worktree, "<runID>" for the run worktree.
//
// The loop is k8s-style and idempotent: re-running a pass for a run
// converges to the same state (existing worktrees are detected, never
// re-created). Non-repo projects are detected here and marked 'skipped' so
// the run proceeds in place (the non-repo fallback work item consumes that
// decision). A provisioning error marks the run 'failed' — it never fails
// the run itself; the DAG gate and cwd wiring are the execution-cwd
// companion's job.
package scheduler

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/reconciler"
	"github.com/beardedparrott/orchicon/internal/slug"
	"github.com/beardedparrott/orchicon/internal/workflow"
	"github.com/jackc/pgx/v5"
)

// worktreeDirName is the subdirectory under project_dir where per-run
// working trees live.
const worktreeDirName = ".orchicon-worktrees"

// stagingPrefix names the transient sibling directory used to stage a
// nested branch worktree out of a run container during adoption
// (stagingDirName). These are never run namespaces and are never swept as
// orphans.
const stagingPrefix = "_stage-"

// gitCmdTimeout bounds every git subprocess the reconciler spawns.
const gitCmdTimeout = 30 * time.Second

// maxBranchSlugLen caps the slug portion of a branch name so the full
// "<slug>-<suffix>" stays well under git's 255-byte ref limit even for
// very long work item titles.
const maxBranchSlugLen = 180

// maxStepBranchSlugLen caps the step-ID segment of a branch-step-run
// branch name (<runBranch>/<stepSlug>-<stepRunSuffix>). The run branch can
// already reach ~197 bytes (maxBranchSlugLen + suffix), so the step slug
// stays short to leave room for the separator + step-run suffix.
const maxStepBranchSlugLen = 32

// maxBranchNameLen caps the full branch-step-run branch name so it stays
// well under git's 255-byte ref limit (the effective branch-name cap is
// 243 bytes once "refs/heads/" is counted).
const maxBranchNameLen = 220

// maxWorktreeCandidatePages bounds how many 16-row pages the branch-worktree
// scan walks per tick, so non-branch residue rows (never provisioned, so
// they stay 'pending' forever) cannot starve real branch candidates while
// still bounding the pass's work (mirrors scanBatchSize's budget spirit).
const maxWorktreeCandidatePages = 4

// gitDetectionTTL bounds how long a cached git work-tree detection on the
// project row is trusted before it is re-detected (a directory that becomes
// a git repo is picked up within a TTL even without a project_dir change).
const gitDetectionTTL = 24 * time.Hour

// WorktreeReconciler implements reconciler.Reconciler for the "worktree"
// kind, keyed by workflow-run ID.
type WorktreeReconciler struct {
	pool *db.Pool
	log  *slog.Logger
	// limiter, when set, resolves the non-repo in-place serialization limit
	// for the atomic admission gate (concurrency guards D3). Set via
	// SetDispatchLimiter.
	limiter DispatchLimiter
}

// NewWorktreeReconciler creates a WorktreeReconciler.
func NewWorktreeReconciler(pool *db.Pool, log *slog.Logger) *WorktreeReconciler {
	return &WorktreeReconciler{pool: pool, log: log}
}

// SetDispatchLimiter installs the per-project in-place serialization seam
// (concurrency guards D3). When set, non-repo runs are atomically admitted
// before being marked worktree_status='skipped' (the in-place token), so
// two runs never share the mutable project_dir. Nil keeps today's
// unbounded in-place fallback.
func (r *WorktreeReconciler) SetDispatchLimiter(l DispatchLimiter) {
	r.limiter = l
}

// Kind returns the reconciler kind (used for queue + leadership keying).
func (r *WorktreeReconciler) Kind() string { return "worktree" }

// Reconcile provisions a single run's working tree (key = run id), a
// single branch step run's working tree (key = "<runID>:<stepRunID>"), or
// scans for candidates when the key is empty. Idempotent: re-running a
// pass for a key converges to the same state (docs/03 §1).
func (r *WorktreeReconciler) Reconcile(ctx context.Context, key string) reconciler.Result {
	// v0.1: single dev tenant (mirrors TaskReconciler/WorkflowReconciler).
	tenantID := "tnt_dev"
	if key == "" {
		return r.scan(ctx, tenantID)
	}
	runID, stepRunID, _ := splitWorktreeKey(key)
	if stepRunID != "" {
		if err := r.reconcileStepRunOne(ctx, tenantID, runID, stepRunID); err != nil {
			return reconciler.Result{Error: err}
		}
		return reconciler.Result{}
	}
	if err := r.reconcileOne(ctx, tenantID, runID); err != nil {
		return reconciler.Result{Error: err}
	}
	return reconciler.Result{}
}

// splitWorktreeKey parses a worktree reconciler key into its run and
// optional step-run components. Run and step-run IDs are ULIDs (no
// colons), so the first ':' cleanly separates them. Returns
// (runID, stepRunID, composite) where composite is the parsed pair.
func splitWorktreeKey(key string) (runID, stepRunID, composite string) {
	if idx := strings.IndexByte(key, ':'); idx > 0 {
		return key[:idx], key[idx+1:], key
	}
	return key, "", key
}

// scan lists un-provisioned pending/running runs and provisions each,
// then lists terminal runs with a recorded worktree and prunes each. The
// same two passes run for branch worktrees (parallel-branch child step
// runs). All passes go through the same idempotent per-run entry point
// (reconcileOne dispatches terminal runs to pruneOne; reconcileStepRunOne
// dispatches terminal step runs to pruneStepRunOne). Batch-capped so one
// pass can't monopolize the reconciler goroutine (mirrors
// TaskReconciler's scan).
func (r *WorktreeReconciler) scan(ctx context.Context, tenantID string) reconciler.Result {
	ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return reconciler.Result{Error: err}
	}
	runs, err := db.ListWorktreeCandidates(ctx, ttx.Tx, tenantID, 16)
	ttx.Rollback(ctx)
	if err != nil {
		return reconciler.Result{Error: fmt.Errorf("scan worktree candidates: %w", err)}
	}
	for _, run := range runs {
		if err := r.reconcileOne(ctx, tenantID, run.ID); err != nil {
			r.log.Warn("worktree: provision run failed", "run", run.ID, "error", err)
		}
	}
	// Branch worktrees: provision parallel-branch child step runs that are
	// still pending. The DAG shape decides who is a branch child — filter
	// the (batch-capped, paginated) pending step-run candidates by loading
	// each run's version steps once. The scan walks several pages so
	// non-branch residue rows (never provisioned) cannot starve real
	// branch candidates.
	var afterCreated time.Time
	var afterID string
	for page := 0; page < maxWorktreeCandidatePages; page++ {
		ttx, err = r.pool.BeginTenantTx(ctx, tenantID)
		if err != nil {
			return reconciler.Result{Error: err}
		}
		candidates, err := db.ListWorktreeStepRunCandidates(ctx, ttx.Tx, tenantID, 16, afterCreated, afterID)
		ttx.Rollback(ctx)
		if err != nil {
			return reconciler.Result{Error: fmt.Errorf("scan branch worktree candidates: %w", err)}
		}
		if len(candidates) == 0 {
			break
		}
		for _, sr := range candidates {
			afterCreated, afterID = sr.CreatedAt, sr.ID
			if !r.isParallelBranchChildStepRun(ctx, tenantID, sr.WorkflowRunID, sr.StepID) {
				continue
			}
			if err := r.reconcileStepRunOne(ctx, tenantID, sr.WorkflowRunID, sr.ID); err != nil {
				r.log.Warn("worktree: provision branch worktree failed",
					"run", sr.WorkflowRunID, "step_run", sr.ID, "error", err)
			}
		}
		if len(candidates) < 16 {
			break
		}
	}
	// Prune terminal runs' worktrees.
	ttx, err = r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return reconciler.Result{Error: err}
	}
	terminal, err := db.ListTerminalRunsWithWorktrees(ctx, ttx.Tx, tenantID, 16)
	ttx.Rollback(ctx)
	if err != nil {
		return reconciler.Result{Error: fmt.Errorf("scan terminal worktrees: %w", err)}
	}
	for _, run := range terminal {
		if err := r.reconcileOne(ctx, tenantID, run.ID); err != nil {
			r.log.Warn("worktree: prune run failed", "run", run.ID, "error", err)
		}
	}
	// Prune terminal step runs' branch worktrees.
	ttx, err = r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return reconciler.Result{Error: err}
	}
	terminalStepRuns, err := db.ListTerminalStepRunsWithWorktrees(ctx, ttx.Tx, tenantID, 16)
	ttx.Rollback(ctx)
	if err != nil {
		return reconciler.Result{Error: fmt.Errorf("scan terminal step-run worktrees: %w", err)}
	}
	for _, sr := range terminalStepRuns {
		if err := r.reconcileStepRunOne(ctx, tenantID, sr.WorkflowRunID, sr.ID); err != nil {
			r.log.Warn("worktree: prune branch worktree failed",
				"run", sr.WorkflowRunID, "step_run", sr.ID, "error", err)
		}
	}
	// Sweep orphaned branch refs: a COMPLETED run (or a step run of one)
	// whose worktree was already pruned still records worktree_branch, but the
	// prune pass only ran (and deleted the branch) for 'ready' worktrees at
	// prune time. A branch that survived that moment — e.g. the run completed
	// before the squash-aware merge gate landed, or a parallel/superseded step
	// run was pruned-early while a later iteration carried the merge — is
	// never revisited and leaks as a dead local ref. Reclaim any such branch
	// that is provably merged into the base (success-only: never on a failed /
	// aborted run's branch, which a retry re-attaches to).
	r.sweepOrphanBranches(ctx, tenantID)
	return reconciler.Result{}
}

// isParallelBranchChildStepRun reports whether the step with the given ID
// (in the run's published workflow version) is a parallel-branch child —
// its depends_on references a step whose kind is `parallel`. Only such
// steps get a branch worktree. Loads the run's version steps in a short
// transaction; a missing version or steps means "not a branch child"
// (fail-open: the step simply runs in the run's cwd).
func (r *WorktreeReconciler) isParallelBranchChildStepRun(ctx context.Context, tenantID, runID, stepID string) bool {
	ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return false
	}
	defer ttx.Rollback(ctx)
	run, err := db.GetWorkflowRun(ctx, ttx.Tx, tenantID, runID)
	if err != nil {
		return false
	}
	version, err := db.GetWorkflowVersion(ctx, ttx.Tx, tenantID, run.WorkflowID, run.WorkflowVersion)
	if err != nil {
		return false
	}
	steps, err := workflow.ParseSteps(version.Steps)
	if err != nil {
		return false
	}
	return parallelBranchChildIDs(steps)[stepID]
}

// orphanBranchSweepLimit bounds how many orphaned branch refs the sweep
// reclaims per scan pass, mirroring the other batch-capped scan surfaces.
// Branch deletion is idempotent, so an over-budget scan simply continues on
// the next tick.
const orphanBranchSweepLimit = 32

// sweepOrphanBranches reclaims dead local branch refs left behind by the
// prune path. It runs at the end of every scan, after the terminal-run and
// terminal-step-run prune passes:
//
//   - Runs/step-runs with worktree_status='ready' are handled by the prune
//     passes (which delete the branch for a COMPLETED run at prune time).
//   - Runs/step-runs whose worktree was ALREADY pruned ('pruned' +
//     recorded branch) are exactly the ones the prune passes skip — but their
//     branch may still exist locally. That class leaks (observed: 30+ local
//     refs, all merged into develop) because nothing revisits a pruned row.
//
// The sweep reuses the SAME proof-deletion gate as pruneOne (deleteBranch →
// branchProvablyMerged → success-only), so it never deletes unmerged work,
// never touches protected/current branches, and only reaps branches whose run
// actually completed. A branch still attached to a live worktree is left to
// the prune pass (never swept while in use).
func (r *WorktreeReconciler) sweepOrphanBranches(ctx context.Context, tenantID string) {
	// Runs whose worktree was pruned but branch still recorded — inclusive
	// window (completed always reclaimable; failed/aborted reclaimable only
	// when the bound work item is terminal non-replayable or absent). The
	// inclusive query LEFT JOINs work_items so non-reclaimable rows never
	// pin the ORDER BY ... LIMIT page.
	ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		r.log.Warn("worktree: orphan sweep begin tx failed", "error", err)
		return
	}
	runs, err := db.ListTerminalRunsWithPrunedBranchesInclusive(ctx, ttx.Tx, tenantID, orphanBranchSweepLimit)
	ttx.Rollback(ctx)
	if err != nil {
		r.log.Warn("worktree: orphan sweep list runs failed", "error", err)
		return
	}
	for _, run := range runs {
		if err := r.sweepOrphanRun(ctx, tenantID, run); err != nil {
			r.log.Warn("worktree: orphan sweep run branch failed",
				"run", run.ID, "branch", run.WorktreeBranch, "error", err)
		}
	}

	// Step runs (parallel-branch children) of reclaimable runs, same class.
	ttx, err = r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		r.log.Warn("worktree: orphan sweep begin tx (step) failed", "error", err)
		return
	}
	steps, err := db.ListTerminalStepRunsWithPrunedBranchesInclusive(ctx, ttx.Tx, tenantID, orphanBranchSweepLimit)
	ttx.Rollback(ctx)
	if err != nil {
		r.log.Warn("worktree: orphan sweep list step runs failed", "error", err)
		return
	}
	for _, sr := range steps {
		if err := r.sweepOrphanStepBranch(ctx, tenantID, sr); err != nil {
			r.log.Warn("worktree: orphan sweep step branch failed",
				"run", sr.WorkflowRunID, "step_run", sr.ID, "branch", sr.WorktreeBranch, "error", err)
		}
	}
}

// sweepOrphanRun attempts to reclaim a pruned-run branch. Two gates:
//
//	Gate A (completed): same proof-deletion gate as pruneOne (deleteBranch →
//	branchProvablyMerged). Never deletes unmerged work.
//
//	Gate B (failed/aborted with terminal work item or no item):
//	work-item-terminal proof — bypasses branchProvablyMerged but still
//	enforces provably-ours / not-protected / not-current / not-attached /
//	exists. Failed with an active work item is retained for retry.
//
// The inclusive query guarantees only reclaimable rows reach here; the
// Go-level reclaimability check is the defense-in-depth policy.
func (r *WorktreeReconciler) sweepOrphanRun(ctx context.Context, tenantID string, run db.WorkflowRunRow) error {
	if run.WorktreeBranch == "" {
		return nil
	}
	projectDir := r.lookupProjectDir(ctx, tenantID, run.ProjectID)
	if projectDir == "" || !r.isInsideWorkTree(ctx, projectDir) {
		return nil // can't verify — never delete on uncertainty
	}
	// A branch still attached to a live worktree is in use — leave it to
	// the prune pass (which deletes it at prune time).
	if attached, err := r.branchAttachedToWorktree(ctx, projectDir, run.WorktreeBranch); err != nil {
		return nil
	} else if attached {
		return nil
	}
	switch run.Status {
	case domain.WorkflowRunCompleted:
		_, prState := db.PrFromRunContext(run.RunContext)
		// Gate A — provably merged.
		if err := r.deleteBranch(ctx, projectDir, run.WorktreeBranch, prState); err != nil {
			return err
		}
		if !r.branchExists(ctx, projectDir, run.WorktreeBranch) {
			return r.clearSweptRunBranch(ctx, tenantID, run)
		}
		return nil
	case domain.WorkflowRunFailed, domain.WorkflowRunAborted:
		if !r.isWorkItemReclaimable(ctx, tenantID, run.WorkItemID) {
			return nil // retry target — keep
		}
		// Gate B — work-item-terminal proof (no merged check).
		if err := r.deleteDeadBranch(ctx, projectDir, run.WorktreeBranch); err != nil {
			return err
		}
		if !r.branchExists(ctx, projectDir, run.WorktreeBranch) {
			return r.clearSweptRunBranch(ctx, tenantID, run)
		}
		return nil
	default:
		return nil
	}
}

// clearSweptRunBranch clears the worktree_branch on a COMPLETED run whose
// branch was provably reclaimed by the orphan sweep. The orphan query selects
// rows by `worktree_status='pruned' AND worktree_branch<>”` (ASC, LIMIT N);
// WITHOUT clearing the branch, a swept row matches the query forever and the
// per-scan page never advances to newer orphans (the observed stuck-backlog:
// 116 swept rows pinned the MODE first-32 slot and starved 16 newer ones).
// Clearing the recorded branch lets the sweep walk forward one page at a time.
func (r *WorktreeReconciler) clearSweptRunBranch(ctx context.Context, tenantID string, run db.WorkflowRunRow) error {
	ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer ttx.Rollback(ctx)
	cur, err := db.GetWorkflowRun(ctx, ttx.Tx, tenantID, run.ID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("get run: %w", err)
	}
	if cur.WorktreeBranch == "" {
		return ttx.Commit(ctx)
	}
	fields := db.UpdateWorkflowRunFields{WorktreeBranch: strPtr("")}
	if _, err := db.UpdateWorkflowRun(ctx, ttx.Tx, tenantID, run.ID, cur.Version, fields); err != nil {
		return fmt.Errorf("clear swept run branch: %w", err)
	}
	r.log.Info("worktree: orphan sweep cleared run branch", "run", run.ID, "branch", run.WorktreeBranch)
	return ttx.Commit(ctx)
}

// sweepOrphanStepBranch is sweepOrphanRun for a parallel-branch child step
// run. Gate A (completed) uses the merged proof; Gate B (failed/aborted
// with terminal work item or no item) uses work-item-terminal proof.
func (r *WorktreeReconciler) sweepOrphanStepBranch(ctx context.Context, tenantID string, sr db.WorkflowStepRunRow) error {
	if sr.WorktreeBranch == "" {
		return nil
	}
	run, err := r.loadRun(ctx, tenantID, sr.WorkflowRunID)
	if err != nil {
		return nil
	}
	projectDir := r.lookupProjectDir(ctx, tenantID, run.ProjectID)
	if projectDir == "" || !r.isInsideWorkTree(ctx, projectDir) {
		return nil
	}
	if attached, err := r.branchAttachedToWorktree(ctx, projectDir, sr.WorktreeBranch); err != nil {
		return nil
	} else if attached {
		return nil
	}
	switch run.Status {
	case domain.WorkflowRunCompleted:
		_, prState := db.PrFromRunContext(run.RunContext)
		if err := r.deleteBranch(ctx, projectDir, sr.WorktreeBranch, prState); err != nil {
			return fmt.Errorf("step branch %s: %w", sr.WorktreeBranch, err)
		}
		if !r.branchExists(ctx, projectDir, sr.WorktreeBranch) {
			return r.clearSweptStepBranch(ctx, tenantID, sr)
		}
		return nil
	case domain.WorkflowRunFailed, domain.WorkflowRunAborted:
		if !r.isWorkItemReclaimable(ctx, tenantID, run.WorkItemID) {
			return nil
		}
		if err := r.deleteDeadBranch(ctx, projectDir, sr.WorktreeBranch); err != nil {
			return fmt.Errorf("step branch %s: %w", sr.WorktreeBranch, err)
		}
		if !r.branchExists(ctx, projectDir, sr.WorktreeBranch) {
			return r.clearSweptStepBranch(ctx, tenantID, sr)
		}
		return nil
	default:
		return nil
	}
}

// clearSweptStepBranch clears the worktree_branch on a completed step run
// whose branch was provably reclaimed by the orphan sweep (mirror of
// clearSweptRunBranch — see its doc for the stuck-backlog rationale).
func (r *WorktreeReconciler) clearSweptStepBranch(ctx context.Context, tenantID string, sr db.WorkflowStepRunRow) error {
	ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer ttx.Rollback(ctx)
	cur, err := db.GetWorkflowStepRun(ctx, ttx.Tx, tenantID, sr.ID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("get step run: %w", err)
	}
	if cur.WorktreeBranch == "" {
		return ttx.Commit(ctx)
	}
	fields := db.UpdateWorkflowStepRunFields{WorktreeBranch: strPtr("")}
	if _, err := db.UpdateWorkflowStepRun(ctx, ttx.Tx, tenantID, sr.ID, cur.Version, fields); err != nil {
		return fmt.Errorf("clear swept step branch: %w", err)
	}
	r.log.Info("worktree: orphan sweep cleared step run", "step_run", sr.ID, "branch", sr.WorktreeBranch)
	return ttx.Commit(ctx)
}

// branchAttachedToWorktree reports whether branch currently has a live
// worktree attached (git worktree list). A branch with a registered worktree
// is in use — never swept, never deleted while its worktree exists.
func (r *WorktreeReconciler) branchAttachedToWorktree(ctx context.Context, projectDir, branch string) (bool, error) {
	wts, err := listWorktrees(ctx, projectDir)
	if err != nil {
		return false, err
	}
	for i := range wts {
		if wts[i].branch == branch {
			return true, nil
		}
	}
	return false, nil
}

// reconcileOne provisions a single run's isolated working tree and records
// the result (path + branch) on the run row. It is safe to call
// concurrently — every branch/path decision is a pure function of the run,
// and the final row update uses optimistic concurrency, so concurrent
// passes for the same run converge rather than duplicate.
func (r *WorktreeReconciler) reconcileOne(ctx context.Context, tenantID, runID string) error {
	// Phase 1 — read the run (short transaction, released before git work).
	run, err := r.loadRun(ctx, tenantID, runID)
	if err != nil {
		return err
	}
	// Terminal runs dispatch to the prune path: their worktree (if any) is
	// reaped instead of provisioned. The scan predicate already excludes
	// them from provisioning; the enqueue path must route here too.
	if isTerminalRun(run) {
		return r.pruneOne(ctx, tenantID, run)
	}
	// One-shot runs have an empty project_id until the PROJECT step binds
	// them on first dispatch — nothing to provision until then.
	if run.ProjectID == "" {
		return nil
	}

	switch run.WorktreeStatus {
	case domain.WorktreeReady:
		// Converge: the worktree we recorded must still exist on the
		// recorded branch. If it does, we're done; if it vanished, fall
		// through and re-provision.
		if r.worktreeMatches(ctx, run.ProjectID, run.WorktreePath, run.WorktreeBranch) {
			r.ensureRunWorktreeStepIgnore(ctx, run.WorktreePath)
			return nil
		}
	case domain.WorktreePruned:
		if isTerminalRun(run) {
			return nil
		}
		// non-terminal pruned → re-provision (retry after prune)
	case domain.WorktreeSkipped, domain.WorktreeFailed:
		// Recorded decisions are respected: a skipped or failed run
		// is never re-provisioned by the loop. (A human may reset the row.)
		return nil
	}

	// Phase 2 — resolve the project dir + detect a git work tree (cached on
	// the project row so the loop does not shell out to git on every pass).
	projectDir, gitBacked, found := r.resolveGitBacked(ctx, tenantID, run.ProjectID)
	if !found {
		return r.markSkipped(ctx, tenantID, runID, "project has no project_dir")
	}
	if !gitBacked {
		return r.markSkipped(ctx, tenantID, runID,
			fmt.Sprintf("project dir %s is not a git work tree", projectDir))
	}

	path := filepath.Join(projectDir, worktreeDirName, runID)
	branch := r.branchName(ctx, tenantID, run)

	// Resolve the base ref up front so the container-adopt path (Phase 3,
	// case 2) can create the run worktree without re-entering Phase 4.
	base := r.resolveBaseRef(ctx, projectDir)

	// Phase 3 — converge against what git actually has at the expected path.
	existing, err := r.worktreeAt(ctx, projectDir, path)
	if err != nil {
		return fmt.Errorf("worktree: list worktrees: %w", err)
	}
	if existing != nil {
		if existing.branch == branch {
			return r.recordReady(ctx, tenantID, runID, path, branch)
		}
		// A foreign worktree occupies our path (e.g. a crashed partial
		// create, or the run's slug source changed). Never destroy a
		// registered worktree we can't prove is ours — but when the row
		// still says 'pending' (no other owner) we own the path by
		// construction (it is runID-namespaced), so repair it.
		if run.WorktreeStatus == domain.WorktreePending {
			if rerr := r.removeWorktree(ctx, projectDir, path); rerr != nil {
				return fmt.Errorf("worktree: repair stale worktree at %s: %w", path, rerr)
			}
		} else {
			// Ready-but-gone reconverge: the recorded worktree vanished
			// and our deterministic path is occupied by a foreign
			// worktree — fail closed rather than record a lie.
			return r.markFailed(ctx, tenantID, runID,
				fmt.Sprintf("path %s occupied by a git worktree on branch %s", path, existing.branch))
		}
	} else if occupied, oerr := r.dirOccupied(path); oerr != nil {
		return fmt.Errorf("worktree: inspect %s: %w", path, oerr)
	} else if occupied {
		// The path exists but is not a registered worktree. Three cases:
		//  1. A partial git-worktree artifact of ours (a gitdir file inside
		//     the run-namespaced dir while the row is 'pending') → prune it.
		//  2. A run-namespaced CONTAINER holding only provably-ours nested
		//     branch-worktree artifacts — a parallel-branch step run was
		//     provisioned first, racing the run worktree, so <runID>/ is a
		//     plain dir holding <runID>/<stepRunID> worktrees. Adopt it:
		//     move the nested worktrees out of the way, create the run
		//     worktree into the (now empty) <runID>, then move them back.
		//     The run worktree and the branch worktrees coexist; the
		//     container is never deleted (AC2).
		//  3. Anything else → fail provisioning — never delete arbitrary
		//     user directories (AC3).
		if run.WorktreeStatus == domain.WorktreePending && r.isOrchiconWorktreeArtifact(path) {
			if rerr := r.removeWorktreeArtifact(path); rerr != nil {
				return fmt.Errorf("worktree: prune partial artifact at %s: %w", path, rerr)
			}
		} else if run.WorktreeStatus == domain.WorktreePending {
			isContainer, cerr := r.isOrchiconContainer(ctx, projectDir, path)
			if cerr != nil {
				return fmt.Errorf("worktree: inspect container %s: %w", path, cerr)
			}
			if isContainer {
				if aerr := r.adoptRunContainer(ctx, projectDir, path, branch, base); aerr != nil {
					return fmt.Errorf("worktree: adopt run container at %s: %w", path, aerr)
				}
				return r.recordReady(ctx, tenantID, runID, path, branch)
			}
			return r.markFailed(ctx, tenantID, runID,
				fmt.Sprintf("path %s is occupied by a non-worktree directory", path))
		} else {
			return r.markFailed(ctx, tenantID, runID,
				fmt.Sprintf("path %s is occupied by a non-worktree directory", path))
		}
	}

	// Phase 4 — create or attach against the resolved base ref. When the
	// deterministic branch already exists (a retried run reusing its branch),
	// provisionWorktree ATTACHES to it so the branch's existing commits carry
	// over — never `-b`, which fails on an existing branch.
	if err := r.provisionWorktree(ctx, projectDir, path, branch, base); err != nil {
		if isAlreadyTrackedErr(err) {
			// A concurrent pass won the create race — converge on the
			// worktree git now has at our path.
			if existing, lerr := r.worktreeAt(ctx, projectDir, path); lerr == nil && existing != nil && existing.branch == branch {
				return r.recordReady(ctx, tenantID, runID, path, branch)
			}
		}
		return r.markFailed(ctx, tenantID, runID, fmt.Sprintf("git worktree add: %v", err))
	}

	if err := r.recordReady(ctx, tenantID, runID, path, branch); err != nil {
		return err
	}
	r.ensureRunWorktreeStepIgnore(ctx, path)
	return nil
}

// reconcileStepRunOne provisions a single parallel-branch step run's
// isolated working tree and records the result (path + branch) on the
// workflow_step_runs row. Mirror of reconcileOne for the step-run key: a
// terminal step run (or a step run of a terminal run) dispatches to the
// prune path; non-branch steps are never provisioned here; non-repo
// projects and runs without a bound project are marked 'skipped' so the
// branch child runs in the run's cwd. Idempotent by construction — every
// path/branch decision is a pure function of (runID, stepRunID), and the
// final row update uses optimistic concurrency, so concurrent passes
// converge rather than duplicate.
func (r *WorktreeReconciler) reconcileStepRunOne(ctx context.Context, tenantID, runID, stepRunID string) error {
	// Phase 1 — read the step run + its run (short transaction, released
	// before git work).
	run, sr, err := r.loadStepRun(ctx, tenantID, runID, stepRunID)
	if err != nil {
		return err
	}
	// Terminal step runs (or step runs of terminal runs) dispatch to the
	// prune path: their branch worktree (if any) is reaped instead of
	// provisioned.
	if isTerminalStepRun(sr) || isTerminalRun(run) {
		return r.pruneStepRunOne(ctx, tenantID, sr)
	}
	if run.ProjectID == "" {
		// No project to provision against — the branch child runs in the
		// run's cwd. Mark skipped so the DAG gate stops holding the step.
		return r.markStepSkipped(ctx, tenantID, stepRunID,
			"run has no bound project; branch runs in the run's cwd")
	}

	switch sr.WorktreeStatus {
	case domain.WorktreeReady:
		// Converge: the worktree we recorded must still exist on the
		// recorded branch. If it does, we're done; if it vanished, fall
		// through and re-provision.
		if r.worktreeMatches(ctx, run.ProjectID, sr.WorktreePath, sr.WorktreeBranch) {
			// Ensure parallel-branch worktrees inherit the latest run branch tip.
			// If the step worktree was provisioned before the SSE committed to the
			// run branch, its branch will be behind run.WorktreeBranch. Fast-forward
			// it so QA/PR reviewers see the implementation without manual merges.
			if run.WorktreeStatus == domain.WorktreeReady && run.WorktreeBranch != "" && sr.WorktreeBranch != "" && sr.WorktreeBranch != run.WorktreeBranch {
				projectDir, _, found := r.resolveGitBacked(ctx, tenantID, run.ProjectID)
				if found {
					if r.isAncestor(ctx, projectDir, sr.WorktreeBranch, run.WorktreeBranch) {
						if err := r.fastForwardBranch(ctx, projectDir, sr.WorktreePath, sr.WorktreeBranch, run.WorktreeBranch); err != nil {
							r.log.Warn("worktree: fast-forward step branch to run branch failed", "step_run", stepRunID, "step_branch", sr.WorktreeBranch, "run_branch", run.WorktreeBranch, "error", err)
						}
					}
				}
			}
			return nil
		}
	case domain.WorktreePruned:
		if isTerminalStepRun(sr) || isTerminalRun(run) {
			return nil
		}
		// non-terminal pruned → re-provision (retry after prune)
	case domain.WorktreeSkipped, domain.WorktreeFailed:
		// Recorded decisions are respected: a skipped or failed
		// step run is never re-provisioned by the loop.
		return nil
	}

	// Phase 2 — resolve the project dir + detect a git work tree (cached
	// on the project row).
	projectDir, gitBacked, found := r.resolveGitBacked(ctx, tenantID, run.ProjectID)
	if !found {
		return r.markStepSkipped(ctx, tenantID, stepRunID, "project has no project_dir")
	}
	if !gitBacked {
		return r.markStepSkipped(ctx, tenantID, stepRunID,
			fmt.Sprintf("project dir %s is not a git work tree", projectDir))
	}

	path := filepath.Join(projectDir, worktreeDirName, runID, stepRunID)
	branch := r.branchWorktreeName(ctx, tenantID, run, sr)

	// Phase 3 — converge against what git actually has at the expected
	// path (mirrors reconcileOne's repair/fail-closed logic).
	existing, err := r.worktreeAt(ctx, projectDir, path)
	if err != nil {
		return fmt.Errorf("worktree: list worktrees: %w", err)
	}
	if existing != nil {
		if existing.branch == branch {
			return r.recordStepReady(ctx, tenantID, stepRunID, path, branch)
		}
		// A foreign worktree occupies our path. When the row still says
		// 'pending' (no other owner) we own the path by construction (it
		// is runID/stepRunID-namespaced), so repair it.
		if sr.WorktreeStatus == domain.WorktreePending {
			if rerr := r.removeWorktree(ctx, projectDir, path); rerr != nil {
				return fmt.Errorf("worktree: repair stale worktree at %s: %w", path, rerr)
			}
		} else {
			return r.markStepFailed(ctx, tenantID, stepRunID,
				fmt.Sprintf("path %s occupied by a git worktree on branch %s", path, existing.branch))
		}
	} else if occupied, oerr := r.dirOccupied(path); oerr != nil {
		return fmt.Errorf("worktree: inspect %s: %w", path, oerr)
	} else if occupied {
		if sr.WorktreeStatus == domain.WorktreePending && r.isOrchiconWorktreeArtifact(path) {
			if rerr := r.removeWorktreeArtifact(path); rerr != nil {
				return fmt.Errorf("worktree: prune partial artifact at %s: %w", path, rerr)
			}
		} else {
			return r.markStepFailed(ctx, tenantID, stepRunID,
				fmt.Sprintf("path %s is occupied by a non-worktree directory", path))
		}
	}

	// Phase 4 — resolve the base ref and create. The base is the run's
	// branch when the run worktree exists (preserving lineage); otherwise
	// the repo's default ref.
	base := ""
	if run.WorktreeStatus == domain.WorktreeReady && run.WorktreeBranch != "" {
		base = run.WorktreeBranch
	} else {
		base = r.resolveBaseRef(ctx, projectDir)
	}
	if err := r.provisionWorktree(ctx, projectDir, path, branch, base); err != nil {
		if isAlreadyTrackedErr(err) {
			// A concurrent pass won the create race — converge.
			if existing, lerr := r.worktreeAt(ctx, projectDir, path); lerr == nil && existing != nil && existing.branch == branch {
				return r.recordStepReady(ctx, tenantID, stepRunID, path, branch)
			}
		}
		return r.markStepFailed(ctx, tenantID, stepRunID, fmt.Sprintf("git worktree add: %v", err))
	}

	return r.recordStepReady(ctx, tenantID, stepRunID, path, branch)
}

// branchWorktreeName computes the deterministic, collision-safe branch for
// a branch step run: <runBranch>-<stepSlug>-<stepRunSuffix>. The run
// branch name is a prefix (lineage is preserved via the base ref the
// worktree is created FROM), and the step-run suffix makes the branch
// unique PER STEP RUN — loop iterations re-create step runs with fresh IDs
// for the same step, so a branch keyed only on the step ID would collide
// on the second iteration (git forbids two worktrees on one branch).
// NOTE: the "<runBranch>/<stepID>" sub-branch form from the original design
// is invalid under git's ref layout — a branch `runbranch` exists, so the
// leaf `refs/heads/runbranch` blocks creating `refs/heads/runbranch/x`
// ("cannot lock ref"). The flat hyphenated form has no such conflict. The
// step-ID segment is slugified so user-authored step IDs (which may hold
// git-ref-invalid characters) never break `git worktree add`. When the run
// branch is not yet recorded (run worktree still pending), the
// deterministic run branch name is computed instead.
func (r *WorktreeReconciler) branchWorktreeName(ctx context.Context, tenantID string, run db.WorkflowRunRow, sr db.WorkflowStepRunRow) string {
	runBranch := run.WorktreeBranch
	if runBranch == "" {
		runBranch = branchNameFor(r.slugSource(ctx, tenantID, run), run.ID)
	}
	stepSlug := slug.Slugify(sr.StepID)
	if stepSlug == "" {
		stepSlug = "step"
	}
	// Cap the step slug so the full ref stays well under git's 255-byte
	// ref limit even for a maximal run branch (~197 bytes) + suffix.
	if len(stepSlug) > maxStepBranchSlugLen {
		stepSlug = stepSlug[:maxStepBranchSlugLen]
	}
	name := runBranch + "-" + stepSlug + "-" + runSuffix(sr.ID)
	// Defensive cap: shrink the step slug, then the run-branch prefix, so
	// the full ref stays under git's limit even for a near-maximal run
	// branch. Uniqueness never depends on the slug — the step-run suffix
	// (16 chars of entropy) is what makes the branch collision-safe.
	if len(name) > maxBranchNameLen {
		over := len(name) - maxBranchNameLen
		if over < len(stepSlug) {
			stepSlug = stepSlug[:len(stepSlug)-over]
			name = runBranch + "-" + stepSlug + "-" + runSuffix(sr.ID)
		}
		if len(name) > maxBranchNameLen {
			keep := maxBranchNameLen - (len(stepSlug) + 2 + len(runSuffix(sr.ID)))
			if keep < 1 {
				keep = 1
			}
			name = runBranch[:keep] + "-" + stepSlug + "-" + runSuffix(sr.ID)
		}
	}
	return name
}

// pruneStepRunOne reaps a terminal step run's branch worktree: remove the
// working tree dir, run `git worktree prune` in the main repo, and record
// 'pruned' on the step run row. Idempotent by construction — every step
// treats "already gone" as success. On run SUCCESS the branch ref is also
// deleted (gated on merged-into-base); on failure/cancellation the branch
// survives so a retry re-attaches to it.
func (r *WorktreeReconciler) pruneStepRunOne(ctx context.Context, tenantID string, sr db.WorkflowStepRunRow) error {
	if sr.WorktreeStatus != domain.WorktreeReady || sr.WorktreePath == "" {
		return nil
	}
	path := sr.WorktreePath

	run, err := r.loadRun(ctx, tenantID, sr.WorkflowRunID)
	if err != nil {
		return err
	}
	projectDir := r.lookupProjectDir(ctx, tenantID, run.ProjectID)
	if projectDir == "" || !r.isInsideWorkTree(ctx, projectDir) {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return r.markStepPruned(ctx, tenantID, sr.ID, "worktree already gone; main repo unresolvable")
		}
		r.log.Warn("worktree: cannot prune branch worktree; main repo unresolvable",
			"run", sr.WorkflowRunID, "step_run", sr.ID, "path", path)
		return nil
	}

	existing, err := r.worktreeAt(ctx, projectDir, path)
	if err != nil {
		return fmt.Errorf("worktree: list worktrees: %w", err)
	}
	if existing != nil {
		if err := r.removeWorktree(ctx, projectDir, path); err != nil {
			return fmt.Errorf("worktree: remove %s: %w", path, err)
		}
	} else if occupied, oerr := r.dirOccupied(path); oerr != nil {
		return fmt.Errorf("worktree: inspect %s: %w", path, oerr)
	} else if occupied {
		if r.isOrchiconWorktreeArtifact(path) {
			if rerr := r.removeWorktreeArtifact(path); rerr != nil {
				return fmt.Errorf("worktree: remove artifact at %s: %w", path, rerr)
			}
		} else {
			r.log.Warn("worktree: refusing to remove non-worktree directory",
				"run", sr.WorkflowRunID, "step_run", sr.ID, "path", path)
		}
	}

	if _, err := runGit(ctx, projectDir, "worktree", "prune"); err != nil {
		r.log.Warn("worktree: git worktree prune failed",
			"run", sr.WorkflowRunID, "step_run", sr.ID, "error", err)
	}

	// Branch deletion — Gate A (completed: merged) or Gate B (failed/aborted with terminal item).
	if run.Status == domain.WorkflowRunCompleted {
		_, prState := db.PrFromRunContext(run.RunContext)
		if err := r.deleteBranch(ctx, projectDir, sr.WorktreeBranch, prState); err != nil {
			r.log.Warn("worktree: branch worktree branch deletion failed",
				"run", sr.WorkflowRunID, "step_run", sr.ID, "branch", sr.WorktreeBranch, "error", err)
		}
	} else if run.Status == domain.WorkflowRunFailed || run.Status == domain.WorkflowRunAborted {
		if r.isWorkItemReclaimable(ctx, tenantID, run.WorkItemID) {
			if err := r.deleteDeadBranch(ctx, projectDir, sr.WorktreeBranch); err != nil {
				r.log.Warn("worktree: dead step branch deletion failed",
					"run", sr.WorkflowRunID, "step_run", sr.ID, "branch", sr.WorktreeBranch, "error", err)
			}
		}
	}

	// Reap a now-empty run-namespaced container left behind once its
	// registered/nested worktrees are all gone (orphan sweep). This must run
	// after the step worktree above is removed so an empty <runID>/ is seen.
	r.sweepOrphanContainers(ctx, projectDir)

	return r.markStepPruned(ctx, tenantID, sr.ID, "")
}

// loadStepRun reads a step run and its run outside a transaction (released
// before git work).
func (r *WorktreeReconciler) loadStepRun(ctx context.Context, tenantID, runID, stepRunID string) (db.WorkflowRunRow, db.WorkflowStepRunRow, error) {
	ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return db.WorkflowRunRow{}, db.WorkflowStepRunRow{}, fmt.Errorf("begin tx: %w", err)
	}
	defer ttx.Rollback(ctx)
	run, err := db.GetWorkflowRun(ctx, ttx.Tx, tenantID, runID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return db.WorkflowRunRow{}, db.WorkflowStepRunRow{}, nil
		}
		return db.WorkflowRunRow{}, db.WorkflowStepRunRow{}, fmt.Errorf("get run: %w", err)
	}
	sr, err := db.GetWorkflowStepRun(ctx, ttx.Tx, tenantID, stepRunID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return db.WorkflowRunRow{}, db.WorkflowStepRunRow{}, nil
		}
		return db.WorkflowRunRow{}, db.WorkflowStepRunRow{}, fmt.Errorf("get step run: %w", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return db.WorkflowRunRow{}, db.WorkflowStepRunRow{}, fmt.Errorf("commit: %w", err)
	}
	return run, sr, nil
}

// isTerminalStepRun reports whether a step run can never progress again —
// the terminal states after which its branch worktree (if any) is reaped.
// Superseded step runs keep their terminal status, so a superseded run is
// covered by its own status.
func isTerminalStepRun(sr db.WorkflowStepRunRow) bool {
	switch sr.Status {
	case domain.StepRunSucceeded, domain.StepRunFailed, domain.StepRunSkipped, domain.StepRunBlocked:
		return true
	}
	return false
}

// pruneOne reaps a terminal run's worktree: remove the working tree dir,
// run `git worktree prune` in the main repo, and record the result
// ('pruned') on the run row. Idempotent by construction — every step
// treats "already gone" as success, so reconcile retries converge:
//
//   - Non-'ready' runs (pending/skipped/failed/pruned) have nothing to
//     reap → no-op.
//   - A recorded worktree that no longer exists on disk (already removed,
//     or the repo plane is gone) is treated as already pruned → success.
//   - On run SUCCESS the branch is deleted (gated on merged-into-base, never
//     the current branch, never main/develop). On failure/cancellation the
//     branch survives so a retry re-attaches to it (carry-over of partial
//     work).
func (r *WorktreeReconciler) pruneOne(ctx context.Context, tenantID string, run db.WorkflowRunRow) error {
	// Idempotent guard: only reap a worktree this reconciler provably
	// provisioned and recorded 'ready'. A terminal run in any other
	// worktree state has nothing to prune (already pruned, or never
	// provisioned). This is the retry-safety core — a terminal run whose
	// worktree was already removed still carries worktree_status='ready',
	// so the missing-worktree handling below reads as success.
	if run.WorktreeStatus != domain.WorktreeReady || run.WorktreePath == "" {
		return nil
	}
	path := run.WorktreePath

	// Resolve the main repo. If it is unresolvable (project_dir missing or
	// not a git work tree), never delete anything we can't verify: when the
	// recorded worktree is already gone, converge on 'pruned' (idempotent
	// success); when it still exists, leave the row for a later pass.
	//
	// NOTE: this prunes a run whose row records worktree_status='ready' with
	// a path — a real worktree was provisioned. We therefore detect LIVE here
	// (not via the cached project detection) so a stale cache value of
	// "non-git" can never leave a provisioned worktree un-reaped: the
	// contradiction guard overrides the cache in the prune path.
	projectDir := r.lookupProjectDir(ctx, tenantID, run.ProjectID)
	if projectDir == "" || !r.isInsideWorkTree(ctx, projectDir) {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return r.markPruned(ctx, tenantID, run.ID, "worktree already gone; main repo unresolvable")
		}
		r.log.Warn("worktree: cannot prune; main repo unresolvable", "run", run.ID, "path", path)
		return nil
	}

	// Registered worktree at the recorded path → remove it (--force: a
	// failed run may leave dirty files). git validates the removal; the
	// branch ref survives (AC4).
	existing, err := r.worktreeAt(ctx, projectDir, path)
	if err != nil {
		return fmt.Errorf("worktree: list worktrees: %w", err)
	}
	if existing != nil {
		if err := r.removeWorktree(ctx, projectDir, path); err != nil {
			return fmt.Errorf("worktree: remove %s: %w", path, err)
		}
	} else if occupied, oerr := r.dirOccupied(path); oerr != nil {
		return fmt.Errorf("worktree: inspect %s: %w", path, oerr)
	} else if occupied {
		// Not registered but the directory exists: only remove it when we
		// can prove it is one of our worktree artifacts; never delete
		// arbitrary user data.
		if r.isOrchiconWorktreeArtifact(path) {
			if rerr := r.removeWorktreeArtifact(path); rerr != nil {
				return fmt.Errorf("worktree: remove artifact at %s: %w", path, rerr)
			}
		} else {
			r.log.Warn("worktree: refusing to remove non-worktree directory",
				"run", run.ID, "path", path)
		}
	}
	// else: not registered and no directory → already pruned; fall through.

	// AC1: run `git worktree prune` in the main repo even when nothing was
	// removed, to sweep stale admin data left by interrupted removes.
	if _, err := runGit(ctx, projectDir, "worktree", "prune"); err != nil {
		// The working tree removal above already succeeded; the prune step
		// is best-effort housekeeping, so warn rather than wedge the run.
		r.log.Warn("worktree: git worktree prune failed", "run", run.ID, "error", err)
	}

	// Branch deletion — two gates:
	// Gate A (completed): provably merged (squash-aware).
	// Gate B (failed/aborted with terminal work item or no item): work-item-terminal proof.
	if run.Status == domain.WorkflowRunCompleted {
		_, prState := db.PrFromRunContext(run.RunContext)
		if err := r.deleteBranch(ctx, projectDir, run.WorktreeBranch, prState); err != nil {
			r.log.Warn("worktree: branch deletion failed", "run", run.ID, "branch", run.WorktreeBranch, "error", err)
		}
	} else if run.Status == domain.WorkflowRunFailed || run.Status == domain.WorkflowRunAborted {
		if r.isWorkItemReclaimable(ctx, tenantID, run.WorkItemID) {
			if err := r.deleteDeadBranch(ctx, projectDir, run.WorktreeBranch); err != nil {
				r.log.Warn("worktree: dead branch deletion failed", "run", run.ID, "branch", run.WorktreeBranch, "error", err)
			}
		}
	}

	// Reap a now-empty run-namespaced container left behind once its
	// registered/nested worktrees are all gone (orphan sweep; also reaps the
	// ones past manual cleanups left). Runs after the registered run worktree
	// removal above.
	r.sweepOrphanContainers(ctx, projectDir)

	return r.markPruned(ctx, tenantID, run.ID, "")
}

// loadRun reads a run outside a transaction (released before git work).
func (r *WorktreeReconciler) loadRun(ctx context.Context, tenantID, runID string) (db.WorkflowRunRow, error) {
	ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return db.WorkflowRunRow{}, fmt.Errorf("begin tx: %w", err)
	}
	defer ttx.Rollback(ctx)
	run, err := db.GetWorkflowRun(ctx, ttx.Tx, tenantID, runID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return db.WorkflowRunRow{}, nil
		}
		return db.WorkflowRunRow{}, fmt.Errorf("get run: %w", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return db.WorkflowRunRow{}, fmt.Errorf("commit: %w", err)
	}
	return run, nil
}

func isTerminalRun(run db.WorkflowRunRow) bool {
	switch run.Status {
	case domain.WorkflowRunPending, domain.WorkflowRunRunning, domain.WorkflowRunPaused:
		return false
	}
	return true
}

// lookupProjectDir resolves a project's directory. Returns "" if the
// project is missing.
func (r *WorktreeReconciler) lookupProjectDir(ctx context.Context, tenantID, projectID string) string {
	ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		r.log.Warn("worktree: lookup project dir: begin tx", "error", err)
		return ""
	}
	defer ttx.Rollback(ctx)
	proj, err := db.GetProject(ctx, ttx.Tx, tenantID, projectID)
	if err != nil {
		return ""
	}
	_ = ttx.Commit(ctx)
	return proj.ProjectDir
}

// resolveGitBacked loads a project and decides whether its project_dir is a
// git work tree, using the cached detection on the project row to avoid
// shelling out to git on every reconcile pass. Returns
// (projectDir, isGitWorkTree, found); found is false when the project or its
// project_dir is missing.
//
// Cache semantics: git_detected_at == nil (undetermined) or older than
// gitDetectionTTL → re-detect live and write the result back (best-effort,
// version-guarded — a lost-write race converges on the next pass). A fresh
// cache hit returns the cached value with NO git subprocess.
func (r *WorktreeReconciler) resolveGitBacked(ctx context.Context, tenantID, projectID string) (string, bool, bool) {
	ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		r.log.Warn("worktree: resolve git backed: begin tx", "error", err)
		return "", false, false
	}
	defer ttx.Rollback(ctx)
	proj, err := db.GetProject(ctx, ttx.Tx, tenantID, projectID)
	if err != nil {
		return "", false, false
	}
	if proj.ProjectDir == "" {
		return "", false, false
	}
	dir := proj.ProjectDir
	// A fresh cache entry is authoritative — no git subprocess.
	if proj.GitDetectedAt != nil && time.Since(*proj.GitDetectedAt) < gitDetectionTTL {
		return dir, proj.GitWorkTree, true
	}
	// Cache empty or stale: detect once and write back (best-effort).
	isWT := r.isInsideWorkTree(ctx, dir)
	repoSlug := ""
	if isWT {
		repoSlug = r.detectRepoSlug(ctx, dir)
	}
	if _, uerr := db.UpdateProjectGitDetection(ctx, ttx.Tx, tenantID, projectID, proj.Version, isWT, repoSlug); uerr != nil {
		r.log.Warn("worktree: cache git detection failed (best-effort)", "project", projectID, "error", uerr)
	} else {
		_ = ttx.Commit(ctx)
	}
	return dir, isWT, true
}

// branchName computes the deterministic, collision-safe branch for a run:
// <kebab-slug>-<runSuffix>. The slug source is the bound work item's
// title, else the workflow name, else the project name.
func (r *WorktreeReconciler) branchName(ctx context.Context, tenantID string, run db.WorkflowRunRow) string {
	return branchNameFor(r.slugSource(ctx, tenantID, run), run.ID)
}

// branchNameFor is the pure branch-name constructor: <kebab-slug>-<suffix>.
// The slug portion is capped so the full name stays well under git's
// 255-byte ref limit; the suffix is the run ID's entropy tail (runSuffix),
// so two runs of the same work item always get distinct branches.
func branchNameFor(slugSource, runID string) string {
	s := slug.Slugify(slugSource)
	if s == "" {
		s = "run"
	}
	if len(s) > maxBranchSlugLen {
		s = s[:maxBranchSlugLen]
	}
	return s + "-" + runSuffix(runID)
}

// runSuffix returns the collision-safe entropy portion of a run ID. Run
// IDs are ULIDs from db.NewID(): chars 0..9 are the 48-bit ms timestamp,
// chars 10..25 are the 80-bit entropy (further monotonic under the
// Monotonic source). Taking the timestamp prefix would collide for any two
// runs armed within 256 ms, so the suffix is the entropy tail. Defensive
// for non-ULID ids: fall back to the whole id lowercased.
func runSuffix(runID string) string {
	if len(runID) >= 26 {
		return strings.ToLower(runID[10:])
	}
	return strings.ToLower(runID)
}

// slugSource picks the most descriptive deterministic name for a run's
// branch: the bound work item's title, else the workflow name, else the
// project name. Resolved in a single short transaction.
func (r *WorktreeReconciler) slugSource(ctx context.Context, tenantID string, run db.WorkflowRunRow) string {
	ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return "run"
	}
	defer ttx.Rollback(ctx)
	if run.WorkItemID != "" {
		if wi, gerr := db.GetWorkItem(ctx, ttx.Tx, tenantID, run.WorkItemID); gerr == nil && wi.Title != "" {
			_ = ttx.Commit(ctx)
			return wi.Title
		}
	}
	if run.WorkflowID != "" {
		if wf, gerr := db.GetWorkflow(ctx, ttx.Tx, tenantID, run.WorkflowID); gerr == nil && wf.Name != "" {
			_ = ttx.Commit(ctx)
			return wf.Name
		}
	}
	if run.ProjectID != "" {
		if p, gerr := db.GetProject(ctx, ttx.Tx, tenantID, run.ProjectID); gerr == nil && p.Name != "" {
			_ = ttx.Commit(ctx)
			return p.Name
		}
	}
	return "run"
}

// --- git helpers ----------------------------------------------------------

// runGit runs a git command in dir with argv-only arguments and a bounded
// timeout. It returns the combined output. An exec/exit error is returned
// alongside so callers can distinguish "no git" from a git failure.
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return "", fmt.Errorf("git not available on control-plane host: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, gitCmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), err
	}
	return string(out), nil
}

// isInsideWorkTree reports whether projectDir is inside a git work tree
// (`git rev-parse --is-inside-work-tree` exits 0 and prints "true"). A
// non-zero exit or "false" output means not a git repo / not a work tree —
// the non-repo skip signal. A missing git binary is a hard failure so the
// caller records 'failed', not 'skipped'.
func (r *WorktreeReconciler) isInsideWorkTree(ctx context.Context, projectDir string) bool {
	// Stat the dir first: a nonexistent project_dir is a non-repo, not an
	// error (the project may point at a path not present in this plane).
	if fi, err := os.Stat(projectDir); err != nil || !fi.IsDir() {
		return false
	}
	out, err := runGit(ctx, projectDir, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) == "true"
}

// detectRepoSlug reads the git remote origin of projectDir and returns it as
// "owner/repo" (e.g. "beardedparrott/Orchicon"). Best-effort: an empty string
// is returned when there is no origin or it cannot be parsed, so the project
// falls back to a provider-free PR link only when we actually know the origin.
// The PR surface relies on this slug to synthesize a deterministic
// `pull/new/{branch}` link without calling a provider.
func (r *WorktreeReconciler) detectRepoSlug(ctx context.Context, projectDir string) string {
	out, err := runGit(ctx, projectDir, "remote", "get-url", "origin")
	if err != nil {
		return ""
	}
	return parseRepoSlug(strings.TrimSpace(out))
}

// parseRepoSlug extracts the "owner/repo" portion of a git remote URL,
// handling the common HTTPS and SSH forms:
//
//	https://github.com/owner/repo.git   → owner/repo
//	git@github.com:owner/repo.git       → owner/repo
//	ssh://git@github.com/owner/repo.git → owner/repo
//	git://github.com/owner/repo         → owner/repo
//
// Returns "" when the URL does not look like an owner/repo pair. The repo
// suffix (".git") is stripped.
func parseRepoSlug(remote string) string {
	s := strings.TrimSpace(remote)
	if s == "" {
		return ""
	}
	// Strip a trailing .git (case-insensitive).
	if len(s) > 4 && strings.EqualFold(s[len(s)-4:], ".git") {
		s = s[:len(s)-4]
	}
	var ownerRepo string
	if i := strings.Index(s, "://"); i >= 0 {
		// URL form: scheme://host/owner/repo — take the last two path
		// segments (host is ignored).
		after := s[i+3:]
		parts := strings.SplitN(after, "/", 3)
		if len(parts) < 3 {
			return ""
		}
		ownerRepo = parts[1] + "/" + parts[2]
	} else if c := strings.Index(s, ":"); c >= 0 {
		// SSH scp-like form: user@host:owner/repo — take after the colon.
		ownerRepo = s[c+1:]
	} else {
		return ""
	}
	parts := strings.Split(ownerRepo, "/")
	if len(parts) != 2 {
		return ""
	}
	if parts[0] == "" || parts[1] == "" {
		return ""
	}
	return parts[0] + "/" + parts[1]
}

// worktreeInfo is the subset of `git worktree list --porcelain` we need.
type worktreeInfo struct {
	path   string
	branch string // "" for a detached worktree
}

// listWorktrees parses `git worktree list --porcelain` for the repo at
// projectDir. Entries are separated by blank lines; each starts with
// `worktree <path>` and carries `branch refs/heads/<name>` when attached.
func listWorktrees(ctx context.Context, projectDir string) ([]worktreeInfo, error) {
	out, err := runGit(ctx, projectDir, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var wts []worktreeInfo
	var cur *worktreeInfo
	flush := func() {
		if cur != nil {
			wts = append(wts, *cur)
			cur = nil
		}
	}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, "worktree ") {
			flush()
			cur = &worktreeInfo{path: strings.TrimPrefix(line, "worktree ")}
			continue
		}
		if cur != nil && strings.HasPrefix(line, "branch refs/heads/") {
			cur.branch = strings.TrimPrefix(line, "branch refs/heads/")
		}
	}
	flush()
	return wts, nil
}

// worktreeAt returns the registered worktree at path, or nil.
func (r *WorktreeReconciler) worktreeAt(ctx context.Context, projectDir, path string) (*worktreeInfo, error) {
	wts, err := listWorktrees(ctx, projectDir)
	if err != nil {
		return nil, err
	}
	path = filepath.Clean(path)
	for i := range wts {
		if filepath.Clean(wts[i].path) == path {
			return &wts[i], nil
		}
	}
	return nil, nil
}

// worktreeMatches reports whether a registered worktree exists at path on
// branch (the idempotency check for a 'ready' row).
func (r *WorktreeReconciler) worktreeMatches(ctx context.Context, projectID, path, branch string) bool {
	ttx, err := r.pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		return false
	}
	proj, err := db.GetProject(ctx, ttx.Tx, "tnt_dev", projectID)
	_ = ttx.Rollback(ctx)
	if err != nil || proj.ProjectDir == "" {
		return false
	}
	wt, err := r.worktreeAt(ctx, proj.ProjectDir, path)
	if err != nil {
		return false
	}
	return wt != nil && wt.branch == branch
}

// dirOccupied reports whether path exists at all (registered worktree or
// not) — the precondition for the never-destroy-user-data guard.
func (r *WorktreeReconciler) dirOccupied(path string) (bool, error) {
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return fi.IsDir(), nil
}

// isOrchiconWorktreeArtifact reports whether path is a partial
// git-worktree artifact created by this reconciler: a directory inside our
// run-namespaced .orchicon-worktrees dir carrying a `.git` file that points
// into the parent repo's gitdir. Only such a provably-ours directory may be
// removed; a plain user directory never is.
func (r *WorktreeReconciler) isOrchiconWorktreeArtifact(path string) bool {
	gitFile := filepath.Join(path, ".git")
	b, err := os.ReadFile(gitFile)
	if err != nil {
		return false
	}
	// Worktree gitdir files look like: `gitdir: /abs/repo/.git/worktrees/<n>`
	line := strings.TrimSpace(string(b))
	if !strings.HasPrefix(line, "gitdir: ") {
		return false
	}
	return strings.Contains(line, ".git"+string(filepath.Separator)+"worktrees")
}

// isOrchiconContainer reports whether path is a plain directory whose
// immediate children are all provably ours — a run-namespaced CONTAINER
// holding only nested branch-worktree artifacts (a parallel-branch step run
// was provisioned first, racing the run worktree, so <runID>/ exists as a
// plain dir holding <runID>/<stepRunID> worktrees). This is the "ours"
// extension of the never-delete-user-data predicate for the run-container
// case: a genuinely foreign directory (any child not provably ours) fails
// the predicate and stays fail-closed. An empty run-namespaced container is
// ours vacuously.
func (r *WorktreeReconciler) isOrchiconContainer(ctx context.Context, projectDir, path string) (bool, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	if !fi.IsDir() {
		return false, nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return false, err
	}
	// An empty container holds no foreign content — ours by construction
	// (it is runID-namespaced and the row is pending).
	if len(entries) == 0 {
		return true, nil
	}
	wts, err := listWorktrees(ctx, projectDir)
	if err != nil {
		return false, err
	}
	registered := make(map[string]bool, len(wts))
	for _, wt := range wts {
		registered[filepath.Clean(wt.path)] = true
	}
	for _, e := range entries {
		if !r.childIsOrchiconOwned(path, e, registered) {
			return false, nil
		}
	}
	return true, nil
}

// childIsOrchiconOwned reports whether an immediate child of a run
// container is provably ours: a registered git worktree, a gitdir worktree
// artifact, or a lone `.git` gitdir file. Only such children may be staged
// by adoptRunContainer; a foreign child (any other file/dir) is never
// touched.
func (r *WorktreeReconciler) childIsOrchiconOwned(parent string, e os.DirEntry, registered map[string]bool) bool {
	child := filepath.Join(parent, e.Name())
	if registered[filepath.Clean(child)] {
		return true
	}
	if r.isOrchiconWorktreeArtifact(child) {
		return true
	}
	if e.Name() == ".git" && !e.IsDir() {
		b, err := os.ReadFile(child)
		if err == nil {
			line := strings.TrimSpace(string(b))
			if strings.HasPrefix(line, "gitdir: ") {
				return true
			}
		}
	}
	return false
}

// sweepOrphanContainers removes run-namespaced container directories under
// .orchicon-worktrees/ that are EMPTY and provably ours. After a terminal
// run's registered/nested worktrees are pruned, an empty <runID>/ can be
// left behind (a parallel-branch step run was provisioned first without a
// run worktree, or a past manual cleanup left it) — this reaps it. Never
// touches: a still-registered worktree (a live run/step worktree has a
// checked-out tree, so it is not empty), a non-empty provably-ours container
// (it still holds live nested worktrees), any foreign content (isOrchicon
// Container fails closed), or a staging dir (_stage-*, mid-adoption).
func (r *WorktreeReconciler) sweepOrphanContainers(ctx context.Context, projectDir string) {
	root := filepath.Join(projectDir, worktreeDirName)
	entries, err := os.ReadDir(root)
	if err != nil {
		r.log.Warn("worktree: orphan sweep: cannot list containers", "error", err)
		return
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), stagingPrefix) {
			continue
		}
		child := filepath.Join(root, e.Name())
		ours, oerr := r.isOrchiconContainer(ctx, projectDir, child)
		if oerr != nil || !ours {
			continue // foreign or unresolvable — never touch non-native content
		}
		// A live registered worktree is never empty (its checked-out tree is
		// present); if it somehow is, leave it alone.
		if wt, werr := r.worktreeAt(ctx, projectDir, child); werr != nil || wt != nil {
			continue
		}
		empty, eerr := dirEmpty(child)
		if eerr != nil || !empty {
			continue // still holds nested worktrees/artifacts — not an orphan
		}
		if rerr := os.Remove(child); rerr != nil {
			r.log.Warn("worktree: orphan sweep: remove failed", "path", child, "error", rerr)
			continue
		}
		r.log.Info("worktree: orphan sweep removed empty run container", "path", child)
	}
}

// dirEmpty reports whether path exists and contains no entries.
func dirEmpty(path string) (bool, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return false, err
	}
	return len(entries) == 0, nil
}

// adoptRunContainer repairs the run-worktree race: when a parallel-branch
// step run was provisioned first, <path> (the runID dir) is a plain
// container holding nested branch worktrees at <path>/<child>. git refuses
// `worktree add` into a non-empty directory, so this stages each
// provably-ours nested child out of the way, creates the run worktree into
// the now-empty <path>, then moves the children back to <path>/<child>.
// The container is NEVER deleted (AC2): only `git worktree move` (for
// registered worktrees) or os.Rename of provably-ours artifacts mutates the
// filesystem. On success both the run worktree and every nested branch
// worktree are registered and intact at their recorded paths.
func (r *WorktreeReconciler) adoptRunContainer(ctx context.Context, projectDir, path, branch, base string) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	wts, err := listWorktrees(ctx, projectDir)
	if err != nil {
		return err
	}
	registered := make(map[string]bool, len(wts))
	for _, wt := range wts {
		registered[filepath.Clean(wt.path)] = true
	}

	// 1. Stage each provably-ours child out of the container.
	type staged struct {
		from, to string
		move     bool // true → registered worktree (git worktree move), false → artifact (os.Rename)
	}
	var moves []staged
	for _, e := range entries {
		child := filepath.Join(path, e.Name())
		if !r.childIsOrchiconOwned(path, e, registered) {
			return fmt.Errorf("container %s holds foreign child %s — refusing to adopt", path, e.Name())
		}
		stagePath := filepath.Join(projectDir, worktreeDirName, stagingDirName(e.Name()))
		ok := registered[filepath.Clean(child)]
		if ok {
			if _, gerr := runGit(ctx, projectDir, "worktree", "move", child, stagePath); gerr != nil {
				return fmt.Errorf("stage nested worktree %s: %w", child, gerr)
			}
		} else if rerr := os.Rename(child, stagePath); rerr != nil {
			return fmt.Errorf("stage nested artifact %s: %w", child, rerr)
		}
		moves = append(moves, staged{from: child, to: stagePath, move: ok})
	}

	// 2. <path> is now empty — create the run worktree into it.
	if err := r.addWorktree(ctx, projectDir, path, branch, base); err != nil {
		// Best-effort rollback: move already-staged children back so a
		// later pass can retry cleanly.
		for _, m := range moves {
			if m.move {
				_, _ = runGit(ctx, projectDir, "worktree", "move", m.to, m.from)
			} else {
				_ = os.Rename(m.to, m.from)
			}
		}
		return err
	}

	// 3. Move the nested children back into the run worktree.
	for _, m := range moves {
		if m.move {
			if _, gerr := runGit(ctx, projectDir, "worktree", "move", m.to, m.from); gerr != nil {
				return fmt.Errorf("restore nested worktree %s: %w", m.from, gerr)
			}
		} else if rerr := os.Rename(m.to, m.from); rerr != nil {
			return fmt.Errorf("restore nested artifact %s: %w", m.from, rerr)
		}
	}
	return nil
}

// stagingDirName builds the transient sibling directory name used to stage
// a nested branch worktree out of a run container during adoption. The
// child name (a step-run ULID, or a gitdir artifact name) is unique within
// the container, and the "_stage-" prefix keeps it out of the run
// namespace so git's worktree list never mistakes it for a real worktree.
func stagingDirName(child string) string {
	return stagingPrefix + child
}

// removeWorktreeArtifact removes a provably-ours partial worktree directory
// (gitdir file pointing into the repo's worktrees metadata). No user data.
func (r *WorktreeReconciler) removeWorktreeArtifact(path string) error {
	return os.RemoveAll(path)
}

// removeWorktree runs `git worktree remove --force` for a registered
// worktree that must be repaired (row still pending — we own the path).
func (r *WorktreeReconciler) removeWorktree(ctx context.Context, projectDir, path string) error {
	if _, err := runGit(ctx, projectDir, "worktree", "remove", "--force", path); err != nil {
		return err
	}
	return nil
}

// resolveBaseRef picks the deterministic base for the new branch: local
// `develop`, else `origin/develop`, else the remote default branch, else
// the project dir's currently checked-out branch. It never hard-fails over
// a missing base — the ultimate fallback is "" which makes `git worktree
// add` start the branch at HEAD.
func (r *WorktreeReconciler) resolveBaseRef(ctx context.Context, projectDir string) string {
	for _, ref := range []string{"develop", "origin/develop"} {
		if _, err := runGit(ctx, projectDir, "rev-parse", "--verify", "--quiet", ref); err == nil {
			return ref
		}
	}
	if out, err := runGit(ctx, projectDir, "symbolic-ref", "refs/remotes/origin/HEAD"); err == nil {
		ref := strings.TrimSpace(out)
		ref = strings.TrimPrefix(ref, "refs/remotes/")
		if ref != "" {
			return ref
		}
	}
	if out, err := runGit(ctx, projectDir, "symbolic-ref", "--short", "HEAD"); err == nil {
		if ref := strings.TrimSpace(out); ref != "" {
			return ref
		}
	}
	return ""
}

// branchExists reports whether a local branch ref exists in the repo.
func (r *WorktreeReconciler) branchExists(ctx context.Context, projectDir, branch string) bool {
	if branch == "" {
		return false
	}
	if _, err := runGit(ctx, projectDir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err != nil {
		return false
	}
	return true
}

// provisionWorktree creates or attaches a worktree at path on branch. When
// the branch already exists in git (a retried run reusing its branch), it
// ATTACHES with `git worktree add <path> <branch>` so the branch's existing
// commits carry over — never `-b`, which fails on an existing branch. When
// the branch does not exist, it CREATES it from base with
// `git worktree add <path> -b <branch> <base>`.
func (r *WorktreeReconciler) provisionWorktree(ctx context.Context, projectDir, path, branch, base string) error {
	if r.branchExists(ctx, projectDir, branch) {
		if _, err := runGit(ctx, projectDir, "worktree", "add", path, branch); err != nil {
			return err
		}
		return nil
	}
	return r.addWorktree(ctx, projectDir, path, branch, base)
}

// addWorktree runs `git worktree add <path> -b <branch> [<base>]` with
// argv-only arguments (never a shell string). base may be "" to start the
// branch at HEAD.
func (r *WorktreeReconciler) addWorktree(ctx context.Context, projectDir, path, branch, base string) error {
	args := []string{"worktree", "add", path, "-b", branch}
	if base != "" {
		args = append(args, base)
	}
	if _, err := runGit(ctx, projectDir, args...); err != nil {
		return err
	}
	return nil
}

// isAlreadyTrackedErr reports whether a worktree-add failure is the
// "already exists / already tracked" race another pass could have won.
func isAlreadyTrackedErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "already exists") || strings.Contains(msg, "already tracked")
}

// isProtectedBranch reports whether a branch must never be deleted by the
// reconciler: the integration/release branches and any remote-tracking ref.
func isProtectedBranch(branch string) bool {
	switch branch {
	case "", "main", "develop", "master":
		return true
	}
	return strings.HasPrefix(branch, "origin/")
}

// currentBranch returns the repo's currently checked-out branch ("" if
// detached or unresolvable).
func (r *WorktreeReconciler) currentBranch(ctx context.Context, projectDir string) string {
	out, err := runGit(ctx, projectDir, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// branchMergedIntoBase reports whether branch's tip is an ancestor of the
// integration base (develop / origin/develop) — i.e. its commits are already
// merged. The DevOps worker merges via `gh pr merge`, which advances the
// REMOTE develop but not the local refs; the base is therefore fetched before
// the ancestor check, or the gate always reads the stale pre-merge state and
// a successfully-merged branch is never deleted (the leak this feature exists
// to fix). A failed fetch falls through to the local refs — conservative:
// never delete on uncertainty. The reconciler never deletes unmerged work.
func (r *WorktreeReconciler) branchMergedIntoBase(ctx context.Context, projectDir, branch string) bool {
	if _, err := runGit(ctx, projectDir, "fetch", "origin", "develop"); err != nil {
		r.log.Warn("worktree: fetch origin develop failed; merge gate uses local refs",
			"branch", branch, "error", err)
	}
	for _, ref := range []string{"develop", "origin/develop"} {
		if _, err := runGit(ctx, projectDir, "merge-base", "--is-ancestor", branch, ref); err == nil {
			return true
		}
	}
	return false
}

// isWorkItemReclaimable reports whether a failed/aborted run's branch is
// reclaimable: the run has no bound work item, the work item no longer
// exists, or the work item is terminal non-replayable (succeeded/skipped/
// cancelled/archived). A failed run with an active work item is a retry
// target and must be retained.
func (r *WorktreeReconciler) isWorkItemReclaimable(ctx context.Context, tenantID, workItemID string) bool {
	if workItemID == "" {
		return true
	}
	ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		r.log.Warn("worktree: isWorkItemReclaimable begin tx failed", "error", err)
		return false
	}
	defer ttx.Rollback(ctx)
	wi, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, workItemID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return true
		}
		r.log.Warn("worktree: isWorkItemReclaimable get work item failed", "work_item", workItemID, "error", err)
		return false
	}
	_ = ttx.Commit(ctx)
	return domain.WorkItemIsTerminalNonReplayable(wi.Status)
}

// deleteDeadBranch deletes a branch for a dead (failed/aborted) run whose
// work item is terminal. It bypasses branchProvablyMerged but still
// enforces every other safety gate: provably ours (recorded name),
// not protected, not current/HEAD, not attached to a live worktree, and
// exists. Logs with reason "work-item-terminal" so it is auditable
// separately from the merged proof.
func (r *WorktreeReconciler) deleteDeadBranch(ctx context.Context, projectDir, branch string) error {
	if branch == "" || isProtectedBranch(branch) {
		return nil
	}
	if cur := r.currentBranch(ctx, projectDir); cur != "" && cur == branch {
		r.log.Warn("worktree: refusing to delete the current branch", "branch", branch)
		return nil
	}
	if !r.branchExists(ctx, projectDir, branch) {
		return nil
	}
	if attached, err := r.branchAttachedToWorktree(ctx, projectDir, branch); err != nil {
		return err
	} else if attached {
		r.log.Warn("worktree: refusing to delete branch still attached to a worktree", "branch", branch)
		return nil
	}
	if _, err := runGit(ctx, projectDir, "branch", "-D", branch); err != nil {
		return fmt.Errorf("git branch -D %s: %w", branch, err)
	}
	r.log.Info("worktree: deleted dead branch", "branch", branch, "reason", "work-item-terminal")
	return nil
}

// deleteBranch deletes a branch ref after a successful run, gated on the
// branch being provably ours (the deterministic name recorded on the row),
// not protected, not the current branch, and provably landed in the base.
// prState is the run's authoritative PR state from run_context ("" when not
// recorded); it feeds the squash-aware proof gate (branchProvablyMerged).
// Idempotent by construction: a missing branch is success.
func (r *WorktreeReconciler) deleteBranch(ctx context.Context, projectDir, branch, prState string) error {
	if branch == "" || isProtectedBranch(branch) {
		return nil
	}
	if cur := r.currentBranch(ctx, projectDir); cur != "" && cur == branch {
		r.log.Warn("worktree: refusing to delete the current branch", "branch", branch)
		return nil
	}
	if !r.branchExists(ctx, projectDir, branch) {
		return nil // already gone — idempotent
	}
	merged, reason := r.branchProvablyMerged(ctx, projectDir, branch, prState)
	if !merged {
		r.log.Warn("worktree: refusing to delete branch (not provably merged)",
			"branch", branch, "reason", reason)
		return nil
	}
	if _, err := runGit(ctx, projectDir, "branch", "-D", branch); err != nil {
		return fmt.Errorf("git branch -D %s: %w", branch, err)
	}
	r.log.Info("worktree: deleted branch after successful run", "branch", branch, "reason", reason)
	return nil
}

// branchProvablyMerged reports whether a branch's tip provably landed in the
// integration base, so a squash-merged PR (whose branch tip is never an
// ancestor of the base) is still reclaimed on terminal success. This is a
// tri-state proof, not a single ancestry boolean: uncertainty never deletes.
//
//   - P1 (ancestry): the branch tip is an ancestor of develop/origin/develop.
//   - P2 (authoritative PR merged): the run row records pr_state == "merged"
//     (worker-authored after a successful `gh pr merge --squash`) AND the
//     branch tip is not newer than the base tip (nothing was committed after
//     the merge). Preferred.
//   - P3 (remote branch gone, fallback): only when the run row records no
//     pr_state, the remote PR branch ref is provably gone (fail-closed — a
//     network/lookup failure keeps the branch) AND the branch tip is not
//     newer than the base tip (no proof the branch's unique commits were
//     landed).
//
// reason names the proof that applied (or "uncertain"), and is logged so a
// human can audit which gate authorized a prune.
func (r *WorktreeReconciler) branchProvablyMerged(ctx context.Context, projectDir, branch, prState string) (bool, string) {
	// P1 — fast path: the branch tip is an ancestor of the base. The fetch
	// raises the remote base ref before the check, so a stale local develop
	// never hides a merged branch.
	if r.branchMergedIntoBase(ctx, projectDir, branch) {
		return true, "ancestry"
	}
	// P2 — authoritative PR merged (the squash path): pr_state is worker-
	// written only after a successful merge, so it is the strongest signal.
	if prState == "merged" {
		if r.branchTipNotNewerThanBase(ctx, projectDir, branch) {
			return true, "pr_state=merged"
		}
		r.log.Warn("worktree: pr_state=merged but branch tip newer than base; keeping",
			"branch", branch)
		return false, "pr_state=merged-but-tip-newer"
	}
	// P3 — fallback when the run row carries no pr_state: a branch whose
	// remote ref is gone is treated as merged, but only on a successful
	// probe AND when its tip is not newer than the base (no proof its unique
	// commits landed). A failed probe is uncertain (keep) — never delete on
	// a lookup failure or on unproven commits.
	if prState == "" {
		r.log.Info("worktree: no pr_state; probing remote branch ref", "branch", branch)
		if r.remoteBranchRefGone(ctx, projectDir, branch) && r.branchTipNotNewerThanBase(ctx, projectDir, branch) {
			return true, "remote-branch-gone"
		}
		return false, "uncertain"
	}
	return false, "uncertain"
}

// branchTipNotNewerThanBase reports whether branch's tip committer date is
// not newer than the base tip's (origin/develop preferred, else local
// develop). A squash merge lands a NEW base commit dated after the last
// branch commit, so a provably-squashed branch has tip <= base. A branch
// tip strictly NEWER than the base carries commits that were never merged
// (uploaded after the merge) — treat as uncertain and keep. Any read
// failure returns false so a branch is never deleted on uncertainty.
func (r *WorktreeReconciler) branchTipNotNewerThanBase(ctx context.Context, projectDir, branch string) bool {
	branchDate := r.commitDate(ctx, projectDir, branch)
	if branchDate == 0 {
		return false
	}
	var baseDate int64
	if _, err := runGit(ctx, projectDir, "rev-parse", "--verify", "--quiet", "origin/develop"); err == nil {
		baseDate = r.commitDate(ctx, projectDir, "origin/develop")
	} else {
		baseDate = r.commitDate(ctx, projectDir, "develop")
	}
	if baseDate == 0 {
		return false
	}
	return branchDate <= baseDate
}

// commitDate returns the unix committer timestamp of the tip of ref, or 0
// when the ref cannot be resolved (never delete on a failed read).
func (r *WorktreeReconciler) commitDate(ctx context.Context, projectDir, ref string) int64 {
	out, err := runGit(ctx, projectDir, "log", "-1", "--format=%ct", ref)
	if err != nil {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// remoteBranchRefGone reports whether the remote PR branch ref is gone,
// used as the P3 fallback ONLY when the run row records no pr_state. It
// prunes stale remote-tracking refs, then treats the branch as gone when
// origin/<branch> no longer resolves. Fail-closed: a fetch/lookup failure
// (no origin, no network) returns false so a branch is never deleted on an
// unsuccessful probe — a weaker signal than pr_state, so it must never
// delete on doubt.
func (r *WorktreeReconciler) remoteBranchRefGone(ctx context.Context, projectDir, branch string) bool {
	if _, err := runGit(ctx, projectDir, "fetch", "--prune", "origin"); err != nil {
		r.log.Warn("worktree: remote-branch-gone probe fetch failed; keeping branch",
			"branch", branch, "error", err)
		return false
	}
	if _, err := runGit(ctx, projectDir, "rev-parse", "--verify", "--quiet", "origin/"+branch); err != nil {
		return true
	}
	return false
}

// isAncestor reports whether branch a is an ancestor of branch b (a..b fast-forwardable).
func (r *WorktreeReconciler) isAncestor(ctx context.Context, projectDir, a, b string) bool {
	_, err := runGit(ctx, projectDir, "merge-base", "--is-ancestor", a, b)
	return err == nil
}

// ensureRunWorktreeStepIgnore creates a .gitignore at the run worktree root that ignores nested step worktrees.
func (r *WorktreeReconciler) ensureRunWorktreeStepIgnore(ctx context.Context, runWorktreePath string) {
	// Create a .gitignore at the run worktree root that ignores nested step worktrees.
	// Step worktrees are at <runID>/<stepRunID> where stepRunID is a ULID (01...).
	// Without this, `git status` in the run worktree shows step dirs as untracked,
	// and `git add .` would include sibling step files. The ignore is best-effort.
	ignorePath := runWorktreePath + "/.gitignore"
	content := "# Orchicon: ignore nested step worktrees (parallel-branch children)\n01*/\n"
	if data, err := os.ReadFile(ignorePath); err == nil {
		if strings.Contains(string(data), "01*/") {
			return
		}
		content = string(data) + "\n" + content
	}
	_ = os.WriteFile(ignorePath, []byte(content), 0644)
}

// fastForwardBranch fast-forwards stepBranch to runBranch tip. The step worktree
// is checked out on stepBranch, so we first try `git -C <stepPath> merge --ff-only <runBranch>`.
// If that fails (e.g. worktree not at path), fall back to `git branch -f <stepBranch> <runBranch>`.
func (r *WorktreeReconciler) fastForwardBranch(ctx context.Context, projectDir, stepPath, stepBranch, runBranch string) error {
	if stepPath != "" {
		if _, err := runGit(ctx, stepPath, "merge", "--ff-only", runBranch); err == nil {
			r.log.Info("worktree: fast-forwarded step branch to run branch", "step_branch", stepBranch, "run_branch", runBranch)
			return nil
		}
	}
	if _, err := runGit(ctx, projectDir, "branch", "-f", stepBranch, runBranch); err != nil {
		return err
	}
	r.log.Info("worktree: fast-forwarded step branch via branch -f", "step_branch", stepBranch, "run_branch", runBranch)
	return nil
}

// --- row state transitions (each a short optimistic tx) --------------------

// recordReady records the provisioned worktree on the run row. The update
// is guarded by optimistic concurrency and a non-terminal status check so a
// run that terminalized mid-provision is not spuriously touched (the
// worktree still exists for the cleanup companion to reap).
func (r *WorktreeReconciler) recordReady(ctx context.Context, tenantID, runID, path, branch string) error {
	// Retry on optimistic-lock version conflicts (ErrNotFound from WHERE version=...).
	// Two parallel step completions racing on the same run can both read v3,
	// one wins v3->v4, the other gets 0 rows. Re-read and retry instead of
	// failing the workflow (mirrors ControlSequence STOP fix).
	for attempt := 0; attempt < 3; attempt++ {
		ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
		if err != nil {
			return fmt.Errorf("begin tx: %w", err)
		}
		run, err := db.GetWorkflowRun(ctx, ttx.Tx, tenantID, runID)
		if err != nil {
			ttx.Rollback(ctx)
			if errors.Is(err, db.ErrNotFound) {
				return nil
			}
			return fmt.Errorf("get run: %w", err)
		}
		// Already provisioned by a concurrent pass — converge, don't fight.
		if run.WorktreeStatus == domain.WorktreeReady {
			if err := ttx.Commit(ctx); err != nil {
				return fmt.Errorf("commit: %w", err)
			}
			return nil
		}
		_, err = db.UpdateWorkflowRun(ctx, ttx.Tx, tenantID, runID, run.Version, db.UpdateWorkflowRunFields{
			WorktreeStatus: strPtr(domain.WorktreeReady),
			WorktreePath:   strPtr(path),
			WorktreeBranch: strPtr(branch),
		})
		if err != nil {
			ttx.Rollback(ctx)
			if errors.Is(err, db.ErrNotFound) && attempt < 2 {
				continue
			}
			return fmt.Errorf("record worktree ready: %w", err)
		}
		if err := ttx.Commit(ctx); err != nil {
			return fmt.Errorf("commit: %w", err)
		}
		r.log.Info("worktree provisioned", "run", runID, "path", path, "branch", branch)
		return nil
	}
	return fmt.Errorf("record worktree ready: version conflict after retries")
}

// markSkipped records that the project is not git-backed so the run
// proceeds in place (the non-repo fallback consumes this decision).
func (r *WorktreeReconciler) markSkipped(ctx context.Context, tenantID, runID, reason string) error {
	r.log.Info("worktree: run skipped (non-repo project)", "run", runID, "reason", reason)
	return r.mark(ctx, tenantID, runID, domain.WorktreeSkipped, "", "", reason)
}

// markFailed records a provisioning error on the run. The run itself is
// never failed — it proceeds in the main tree (today's behavior); the
// execution-cwd companion owns the gate.
func (r *WorktreeReconciler) markFailed(ctx context.Context, tenantID, runID, reason string) error {
	r.log.Warn("worktree: provisioning failed", "run", runID, "reason", reason)
	return r.mark(ctx, tenantID, runID, domain.WorktreeFailed, "", "", reason)
}

// mark applies a terminal worktree_status with a reason folded into the
// run's run_context (best-effort — the status is the source of truth).
func (r *WorktreeReconciler) mark(ctx context.Context, tenantID, runID, status, path, branch, reason string) error {
	ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer ttx.Rollback(ctx)
	run, err := db.GetWorkflowRun(ctx, ttx.Tx, tenantID, runID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("get run: %w", err)
	}
	if isTerminalRun(run) {
		return nil
	}
	// D3 (concurrency guards): marking a run 'skipped' is admitting it into
	// the IN-PLACE fallback — its executions will run in the shared
	// project_dir. Admit ATOMICALLY (project-row lock + count within this
	// tx) so two runs of a non-repo project never share the mutable
	// working tree. A denied admission leaves the run 'pending'; the next
	// scan pass re-admits it once a slot frees. Only 'skipped' transitions
	// consult the gate — 'ready'/'failed' carry no in-place token.
	if status == domain.WorktreeSkipped {
		admitted, aerr := r.admitInPlace(ctx, ttx.Tx, tenantID, run)
		if aerr != nil {
			return fmt.Errorf("admit in-place run: %w", aerr)
		}
		if !admitted {
			r.log.Info("worktree: in-place run deferred (project at in-place concurrency limit)",
				"run", runID, "project", run.ProjectID)
			return ttx.Commit(ctx)
		}
	}
	fields := db.UpdateWorkflowRunFields{WorktreeStatus: strPtr(status)}
	if path != "" {
		fields.WorktreePath = strPtr(path)
	}
	if branch != "" {
		fields.WorktreeBranch = strPtr(branch)
	}
	if reason != "" {
		if ctx2, ok := mergeRunContext(run.RunContext, map[string]any{"worktree_error": reason}); ok {
			fields.RunContext = &ctx2
		}
	}
	if _, err := db.UpdateWorkflowRun(ctx, ttx.Tx, tenantID, runID, run.Version, fields); err != nil {
		return fmt.Errorf("mark worktree %s: %w", status, err)
	}
	return ttx.Commit(ctx)
}

// admitInPlace is the D3 atomic admission for a run entering the in-place
// (non-repo) fallback. It resolves the project's in-place serialization
// limit and consults the project-row-locked counter — all within the
// caller's transaction, so the mark that follows commits atomically with
// the reservation. Runs without a bound project have no shared project_dir
// to serialize, so they always admit (fail-open). A nil limiter (never
// wired) keeps today's unbounded fallback.
func (r *WorktreeReconciler) admitInPlace(ctx context.Context, tx pgx.Tx, tenantID string, run db.WorkflowRunRow) (bool, error) {
	if r.limiter == nil || run.ProjectID == "" {
		return true, nil
	}
	limit, err := r.limiter.InPlaceLimit(ctx, tx, tenantID, run.ProjectID)
	if err != nil {
		return false, err
	}
	return db.AdmitInPlaceRun(ctx, tx, tenantID, run.ProjectID, limit)
}

// markPruned records that a terminal run's worktree has been reaped. The
// existing `mark` refuses terminal runs by design, so pruning needs its own
// writer. In one optimistic transaction it sets worktree_status='pruned'
// and clears worktree_path; worktree_branch is KEPT so a retry can
// re-attach to the same branch (and so the DevOps merge step knows which
// branch the run used). Branch deletion on success is handled by
// pruneOne's deleteBranch call, not here.
func (r *WorktreeReconciler) markPruned(ctx context.Context, tenantID, runID, reason string) error {
	ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer ttx.Rollback(ctx)
	run, err := db.GetWorkflowRun(ctx, ttx.Tx, tenantID, runID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("get run: %w", err)
	}
	// Already pruned by a concurrent pass — converge, don't fight.
	if run.WorktreeStatus == domain.WorktreePruned {
		return ttx.Commit(ctx)
	}
	fields := db.UpdateWorkflowRunFields{
		WorktreeStatus: strPtr(domain.WorktreePruned),
		WorktreePath:   strPtr(""),
	}
	if reason != "" {
		if merged, ok := mergeRunContext(run.RunContext, map[string]any{"worktree_pruned": reason}); ok {
			fields.RunContext = &merged
		}
	}
	if _, err := db.UpdateWorkflowRun(ctx, ttx.Tx, tenantID, runID, run.Version, fields); err != nil {
		return fmt.Errorf("mark worktree pruned: %w", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	r.log.Info("worktree pruned", "run", runID, "branch", run.WorktreeBranch)
	return nil
}

// --- step-run row state transitions (each a short optimistic tx) -----------

// recordStepReady records the provisioned branch worktree on the step run
// row. Optimistic concurrency + a non-terminal guard so a step run that
// terminalized mid-provision is not spuriously touched (the worktree still
// exists for the prune pass to reap).
func (r *WorktreeReconciler) recordStepReady(ctx context.Context, tenantID, stepRunID, path, branch string) error {
	for attempt := 0; attempt < 3; attempt++ {
		ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
		if err != nil {
			return fmt.Errorf("begin tx: %w", err)
		}
		sr, err := db.GetWorkflowStepRun(ctx, ttx.Tx, tenantID, stepRunID)
		if err != nil {
			ttx.Rollback(ctx)
			if errors.Is(err, db.ErrNotFound) {
				return nil
			}
			return fmt.Errorf("get step run: %w", err)
		}
		// Already provisioned by a concurrent pass — converge, don't fight.
		if sr.WorktreeStatus == domain.WorktreeReady {
			if err := ttx.Commit(ctx); err != nil {
				return fmt.Errorf("commit: %w", err)
			}
			return nil
		}
		if isTerminalStepRun(sr) {
			ttx.Rollback(ctx)
			return nil
		}
		_, err = db.UpdateWorkflowStepRun(ctx, ttx.Tx, tenantID, stepRunID, sr.Version, db.UpdateWorkflowStepRunFields{
			WorktreeStatus: strPtr(domain.WorktreeReady),
			WorktreePath:   strPtr(path),
			WorktreeBranch: strPtr(branch),
		})
		if err != nil {
			ttx.Rollback(ctx)
			if errors.Is(err, db.ErrNotFound) && attempt < 2 {
				continue
			}
			return fmt.Errorf("record step worktree ready: %w", err)
		}
		if err := ttx.Commit(ctx); err != nil {
			return fmt.Errorf("commit: %w", err)
		}
		r.log.Info("worktree: branch worktree provisioned", "step_run", stepRunID, "path", path, "branch", branch)
		return nil
	}
	return fmt.Errorf("record step worktree ready: version conflict after retries")
}

// markStepSkipped records that no branch worktree will be provisioned for
// this step run (non-repo project, or run has no bound project) so the
// DAG gate stops holding the branch child at ready.
func (r *WorktreeReconciler) markStepSkipped(ctx context.Context, tenantID, stepRunID, reason string) error {
	r.log.Info("worktree: branch skipped (non-repo project)", "step_run", stepRunID, "reason", reason)
	return r.markStep(ctx, tenantID, stepRunID, domain.WorktreeSkipped, "", "", reason)
}

// markStepFailed records a branch-worktree provisioning error on the step
// run. The DAG gate fails the branch step on this mark.
func (r *WorktreeReconciler) markStepFailed(ctx context.Context, tenantID, stepRunID, reason string) error {
	r.log.Warn("worktree: branch provisioning failed", "step_run", stepRunID, "reason", reason)
	return r.markStep(ctx, tenantID, stepRunID, domain.WorktreeFailed, "", "", reason)
}

// markStep applies a terminal worktree_status to a step run, folding a
// reason into the step run's result (best-effort — the status is the
// source of truth). Refuses to touch a terminal step run.
func (r *WorktreeReconciler) markStep(ctx context.Context, tenantID, stepRunID, status, path, branch, reason string) error {
	ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer ttx.Rollback(ctx)
	sr, err := db.GetWorkflowStepRun(ctx, ttx.Tx, tenantID, stepRunID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("get step run: %w", err)
	}
	if isTerminalStepRun(sr) {
		return nil
	}
	// D3 (concurrency guards): a branch child's 'skipped' means "run in the
	// run's cwd" — which, for a non-repo project, is the shared project_dir.
	// Never release a branch child into the in-place cwd before its RUN
	// holds the in-place slot: that would let a branch child race an
	// admitted run's directory while the run row is still 'pending'. Runs
	// without a bound project have no shared directory (fail-open).
	if status == domain.WorktreeSkipped {
		admitted, aerr := r.stepInPlaceAdmitted(ctx, ttx.Tx, tenantID, sr)
		if aerr != nil {
			return fmt.Errorf("admit in-place step run: %w", aerr)
		}
		if !admitted {
			r.log.Info("worktree: branch step run deferred (run not yet admitted to in-place)",
				"step_run", stepRunID, "run", sr.WorkflowRunID)
			return ttx.Commit(ctx)
		}
	}
	fields := db.UpdateWorkflowStepRunFields{WorktreeStatus: strPtr(status)}
	if path != "" {
		fields.WorktreePath = strPtr(path)
	}
	if branch != "" {
		fields.WorktreeBranch = strPtr(branch)
	}
	if reason != "" {
		if merged, ok := mergeStepRunContext(sr.Result, map[string]any{"worktree_error": reason}); ok {
			fields.Result = &merged
		}
	}
	if _, err := db.UpdateWorkflowStepRun(ctx, ttx.Tx, tenantID, stepRunID, sr.Version, fields); err != nil {
		return fmt.Errorf("mark step worktree %s: %w", status, err)
	}
	return ttx.Commit(ctx)
}

// stepInPlaceAdmitted reports whether a branch child may be released into
// the in-place fallback ('skipped'): the child executes in its RUN's cwd,
// so the run must already hold the in-place slot (worktree_status
// 'skipped') or be fail-open ('failed' — a provisioning error still runs in
// place today). A still-'pending' run means the run-level admission gate
// hasn't admitted it yet — hold the child until the next pass, when the
// run-level reconcileOne marks the run 'skipped' (or not) and this step
// re-evaluates. Runs with no bound project have no shared directory.
func (r *WorktreeReconciler) stepInPlaceAdmitted(ctx context.Context, tx pgx.Tx, tenantID string, sr db.WorkflowStepRunRow) (bool, error) {
	run, err := db.GetWorkflowRun(ctx, tx, tenantID, sr.WorkflowRunID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("get run for step in-place admission: %w", err)
	}
	if run.ProjectID == "" {
		return true, nil
	}
	switch run.WorktreeStatus {
	case domain.WorktreeSkipped, domain.WorktreeFailed:
		return true, nil
	}
	return false, nil
}

// markStepPruned records that a terminal step run's branch worktree has
// been reaped. In one optimistic transaction it sets worktree_status
// 'pruned' and clears worktree_path; worktree_branch is KEPT so a retry can
// re-attach to the same branch (and so the DevOps merge step knows which
// branch the branch step used). Branch deletion on success is handled by
// pruneStepRunOne's deleteBranch call, not here.
func (r *WorktreeReconciler) markStepPruned(ctx context.Context, tenantID, stepRunID, reason string) error {
	ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer ttx.Rollback(ctx)
	sr, err := db.GetWorkflowStepRun(ctx, ttx.Tx, tenantID, stepRunID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("get step run: %w", err)
	}
	// Already pruned by a concurrent pass — converge, don't fight.
	if sr.WorktreeStatus == domain.WorktreePruned {
		return ttx.Commit(ctx)
	}
	fields := db.UpdateWorkflowStepRunFields{
		WorktreeStatus: strPtr(domain.WorktreePruned),
		WorktreePath:   strPtr(""),
	}
	if reason != "" {
		if merged, ok := mergeStepRunContext(sr.Result, map[string]any{"worktree_pruned": reason}); ok {
			fields.Result = &merged
		}
	}
	if _, err := db.UpdateWorkflowStepRun(ctx, ttx.Tx, tenantID, stepRunID, sr.Version, fields); err != nil {
		return fmt.Errorf("mark step worktree pruned: %w", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	r.log.Info("worktree: branch worktree pruned", "step_run", stepRunID, "branch", sr.WorktreeBranch)
	return nil
}

// mergeStepRunContext folds a worktree_error into a step run's result
// jsonb (best-effort, additive).
func mergeStepRunContext(existing []byte, add map[string]any) ([]byte, bool) {
	out := map[string]any{}
	if len(existing) > 0 {
		if err := json.Unmarshal(existing, &out); err != nil {
			out = map[string]any{}
		}
	}
	for k, v := range add {
		out[k] = v
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, false
	}
	return b, true
}

// mergeRunContext folds a worktree_error into the run's run_context jsonb
// (best-effort, additive).
func mergeRunContext(existing []byte, add map[string]any) ([]byte, bool) {
	out := map[string]any{}
	if len(existing) > 0 {
		if err := json.Unmarshal(existing, &out); err != nil {
			out = map[string]any{}
		}
	}
	for k, v := range add {
		out[k] = v
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, false
	}
	return b, true
}
