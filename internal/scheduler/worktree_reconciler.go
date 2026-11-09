// WorktreeReconciler — the control loop that provisions each git-backed workflow run its own
// isolated working tree at arm time (architecture-notes/worktree-reconciler.md).
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
	"sync"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	projectpkg "github.com/beardedparrott/orchicon/internal/project"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/reconciler"
	"github.com/beardedparrott/orchicon/internal/slug"
	"github.com/beardedparrott/orchicon/internal/workflow"
	"github.com/jackc/pgx/v5"

	"github.com/beardedparrott/orchicon/internal/procwin"
)

// worktreeDirName is the subdirectory under project_dir where per-run
// working trees live.
const worktreeDirName = ".orchicon-worktrees"

// worktreeNoticeKind marks an error_message that is a NOTICE, not a failure.
const worktreeNoticeKind = "worktree_notice"

// stagingPrefix names the transient sibling directory used to stage a nested branch worktree out of
// a run container during adoption (stagingDirName).
const stagingPrefix = "_stage-"

// scratchBaselinePrefix names the throwaway checkouts a baseline capture materialises. The creator
// (scratchCheckout) and the sweep share this one constant so they cannot drift apart.
const scratchBaselinePrefix = "_verify-baseline-"

// scratchBaselineMaxAge is how old a scratch checkout must be before the sweep calls it leaked. It
// tracks the capture's deadline: a capture is cancelled at baselineCaptureTimeout, so nothing older runs.
const scratchBaselineMaxAge = 2 * baselineCaptureTimeout

// gitCmdTimeout bounds every git subprocess the reconciler spawns.
const gitCmdTimeout = 30 * time.Second

// maxBranchSlugLen caps the slug portion of a branch name so the full "<slug>-<suffix>" stays well
// under git's 255-byte ref limit even for very long work item titles.
const maxBranchSlugLen = 180

// maxStepBranchSlugLen caps the step-ID segment of a branch-step-run branch name
// (<runBranch>/<stepSlug>-<stepRunSuffix>).
const maxStepBranchSlugLen = 32

// maxBranchNameLen caps the full branch-step-run branch name so it stays well under git's 255-byte
// ref limit (the effective branch-name cap is 243 bytes once "refs/heads/" is counted).
const maxBranchNameLen = 220

// maxWorktreeCandidatePages bounds how many 16-row pages the branch-worktree scan walks per tick,
// so non-branch residue rows (never provisioned.
const maxWorktreeCandidatePages = 4

// gitDetectionTTL bounds how long a cached git work-tree detection on the project row is trusted
// before it is re-detected (a directory that becomes a git repo is picked up within a TTL even.
const gitDetectionTTL = 24 * time.Hour

// slowPassInterval gates the orphan-sweep slow pass of scan(): the fast pass (pending run +
// branch-candidate provisioning AND terminal run/step-run prunes — all page-bounded) runs every
// tick.
const slowPassInterval = 60 * time.Second

// slowPassBudget bounds the wall-clock git-subprocess cost of one slow pass.
const slowPassBudget = 15 * time.Second

// orphanBranchSweepLimit bounds how many orphaned branch refs the sweep reclaims per scan pass,
// mirroring the other batch-capped scan surfaces.
const orphanBranchSweepLimit = 32

// WorktreeReconciler implements reconciler.Reconciler for the "worktree"
// kind, keyed by workflow-run ID.
type WorktreeReconciler struct {
	pool *db.Pool
	log  *slog.Logger
	// limiter, when set, resolves the non-repo in-place serialization limit for the atomic admission
	// gate (concurrency guards D3).
	limiter DispatchLimiter
	// lastSlowPass records when the sweep slow pass (orphan sweeps) last ran.
	lastSlowPass time.Time
	// integrateMu serializes LOCAL INTEGRATION (local_integrate.go).
	integrateMu sync.Mutex
}

// NewWorktreeReconciler creates a WorktreeReconciler.
func NewWorktreeReconciler(pool *db.Pool, log *slog.Logger) *WorktreeReconciler {
	return &WorktreeReconciler{pool: pool, log: log}
}

// SetDispatchLimiter installs the per-project in-place serialization seam (concurrency guards D3).
func (r *WorktreeReconciler) SetDispatchLimiter(l DispatchLimiter) {
	r.limiter = l
}

// Kind returns the reconciler kind (used for queue + leadership keying).
func (r *WorktreeReconciler) Kind() string { return "worktree" }

// Reconcile provisions a single run's working tree (key = run id), a single branch step run's
// working tree (key = "<runID>:<stepRunID>"), or scans for candidates when the key is empty.
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

// splitWorktreeKey parses a worktree reconciler key into its run and optional step-run components.
func splitWorktreeKey(key string) (runID, stepRunID, composite string) {
	if idx := strings.IndexByte(key, ':'); idx > 0 {
		return key[:idx], key[idx+1:], key
	}
	return key, "", key
}

// scan lists un-provisioned pending/running runs and provisions each, then lists terminal runs with
// a recorded worktree and prunes each.
func (r *WorktreeReconciler) scan(ctx context.Context, tenantID string) reconciler.Result {
	scanStart := time.Now()
	provisions, res := r.scanFast(ctx, tenantID)
	if res.Error != nil {
		return res
	}
	fastElapsed := time.Since(scanStart)
	if !r.slowPassDue() {
		r.logScanTick(provisions, fastElapsed.Milliseconds(), 0, true)
		return reconciler.Result{}
	}
	slowStart := time.Now()
	res = r.scanSlow(ctx, tenantID)
	r.lastSlowPass = time.Now()
	r.logScanTick(provisions, fastElapsed.Milliseconds(), time.Since(slowStart).Milliseconds(), false)
	return res
}

// logScanTick emits the scan summary at a level that reflects whether the tick actually DID
// anything.
func (r *WorktreeReconciler) logScanTick(provisions int, fastMS, slowMS int64, slowSkipped bool) {
	level := slog.LevelDebug
	if provisions > 0 {
		level = slog.LevelInfo
	}
	r.log.Log(context.Background(), level, "worktree: scan",
		"fast_ms", fastMS,
		"slow_ms", slowMS,
		"slow_skipped", slowSkipped,
		"provisions", provisions)
}

// scanFast provisions pending runs and pending parallel-branch child step runs, then prunes
// terminal runs and terminal step runs.
func (r *WorktreeReconciler) scanFast(ctx context.Context, tenantID string) (int, reconciler.Result) {
	provisions := 0
	ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return 0, reconciler.Result{Error: err}
	}
	runs, err := db.ListWorktreeCandidates(ctx, ttx.Tx, tenantID, 16)
	ttx.Rollback(ctx)
	if err != nil {
		return 0, reconciler.Result{Error: fmt.Errorf("scan worktree candidates: %w", err)}
	}
	for _, run := range runs {
		provisions++
		if err := r.reconcileOne(ctx, tenantID, run.ID); err != nil {
			r.log.Warn("worktree: provision run failed", "run", run.ID, "error", err)
		}
	}
	// Branch worktrees: provision parallel-branch child step runs that are still pending.
	var afterCreated time.Time
	var afterID string
	for page := 0; page < maxWorktreeCandidatePages; page++ {
		ttx, err = r.pool.BeginTenantTx(ctx, tenantID)
		if err != nil {
			return provisions, reconciler.Result{Error: err}
		}
		candidates, err := db.ListWorktreeStepRunCandidates(ctx, ttx.Tx, tenantID, 16, afterCreated, afterID)
		ttx.Rollback(ctx)
		if err != nil {
			return provisions, reconciler.Result{Error: fmt.Errorf("scan branch worktree candidates: %w", err)}
		}
		if len(candidates) == 0 {
			break
		}
		for _, sr := range candidates {
			afterCreated, afterID = sr.CreatedAt, sr.ID
			if !r.isParallelBranchChildStepRun(ctx, tenantID, sr.WorkflowRunID, sr.StepID) {
				continue
			}
			provisions++
			if err := r.reconcileStepRunOne(ctx, tenantID, sr.WorkflowRunID, sr.ID); err != nil {
				r.log.Warn("worktree: provision branch worktree failed",
					"run", sr.WorkflowRunID, "step_run", sr.ID, "error", err)
			}
		}
		if len(candidates) < 16 {
			break
		}
	}
	// Prune terminal runs' worktrees (page-bounded lifecycle reap — stays in
	// the fast pass so consecutive scans always converge terminal rows).
	ttx, err = r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return provisions, reconciler.Result{Error: err}
	}
	terminal, err := db.ListTerminalRunsWithWorktrees(ctx, ttx.Tx, tenantID, 16)
	ttx.Rollback(ctx)
	if err != nil {
		return provisions, reconciler.Result{Error: fmt.Errorf("scan terminal worktrees: %w", err)}
	}
	for _, run := range terminal {
		provisions++
		if err := r.reconcileOne(ctx, tenantID, run.ID); err != nil {
			r.log.Warn("worktree: prune run failed", "run", run.ID, "error", err)
		}
	}
	// Prune terminal step runs' branch worktrees (same: bounded reap).
	ttx, err = r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return provisions, reconciler.Result{Error: err}
	}
	terminalStepRuns, err := db.ListTerminalStepRunsWithWorktrees(ctx, ttx.Tx, tenantID, 16)
	ttx.Rollback(ctx)
	if err != nil {
		return provisions, reconciler.Result{Error: fmt.Errorf("scan terminal step-run worktrees: %w", err)}
	}
	for _, sr := range terminalStepRuns {
		provisions++
		if err := r.reconcileStepRunOne(ctx, tenantID, sr.WorkflowRunID, sr.ID); err != nil {
			r.log.Warn("worktree: prune branch worktree failed",
				"run", sr.WorkflowRunID, "step_run", sr.ID, "error", err)
		}
	}
	return provisions, reconciler.Result{}
}

// slowPassDue reports whether the orphan-sweep slow pass may run: at most once per
// slowPassInterval.
func (r *WorktreeReconciler) slowPassDue() bool {
	return time.Since(r.lastSlowPass) >= slowPassInterval
}

// sweepBudgetExpired reports whether a slow-pass wall-clock deadline has passed.
func sweepBudgetExpired(deadline time.Time) bool {
	return time.Now().After(deadline)
}

// scanSlow runs the orphan sweeps: orphan-branch, orphan-dir, and skipped-terminal restore.
func (r *WorktreeReconciler) scanSlow(ctx context.Context, tenantID string) reconciler.Result {
	// LOCAL INTEGRATION RUNS FIRST, ON ITS OWN BUDGET.
	r.integrateLocalRuns(ctx, tenantID, time.Now().Add(slowPassBudget))
	deadline := time.Now().Add(slowPassBudget)
	// Sweep orphaned branch refs: a COMPLETED run (or a step run of one) whose worktree was already
	// pruned still records worktree_branch.
	r.sweepOrphanBranches(ctx, tenantID, deadline)
	// ADR 2.3: reclaim stale .orchicon-worktrees/<id> dirs that are no longer valid git worktrees and
	// have no DB row referencing them.
	r.sweepOrphanDirs(ctx, tenantID, deadline)
	r.sweepSkippedTerminalRuns(ctx, tenantID, deadline)
	return reconciler.Result{}
}

// isParallelBranchChildStepRun reports whether the step with the given ID (in the run's published
// workflow version) is a parallel-branch child.
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
func (r *WorktreeReconciler) sweepOrphanBranches(ctx context.Context, tenantID string, deadline time.Time) {
	// Runs whose worktree was pruned but branch still recorded — inclusive window (completed always
	// reclaimable; failed/aborted reclaimable only when the bound work item is terminal non-replayable.
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
		if sweepBudgetExpired(deadline) {
			r.log.Info("worktree: orphan sweep budget expired, resuming next slow pass", "phase", "runs")
			return
		}
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
		if sweepBudgetExpired(deadline) {
			r.log.Info("worktree: orphan sweep budget expired, resuming next slow pass", "phase", "step_runs")
			return
		}
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

// clearSweptRunBranch clears the worktree_branch on a COMPLETED run whose branch was provably
// reclaimed by the orphan sweep.
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

// sweepOrphanStepBranch is sweepOrphanRun for a parallel-branch child step run.
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

// clearSweptStepBranch clears the worktree_branch on a completed step run whose branch was provably
// reclaimed by the orphan sweep (mirror of clearSweptRunBranch.
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

// branchAttachedToWorktree reports whether branch currently has a live worktree attached (git
// worktree list).
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

// reconcileOne provisions a single run's isolated working tree and records the result (path +
// branch) on the run row.
func (r *WorktreeReconciler) reconcileOne(ctx context.Context, tenantID, runID string) error {
	// Dispatch-latency observability: one structured line per provision attempt with the durable
	// outcome, so a future slow-dispatch report maps to exactly one slow provision vs scan-queued.
	start := time.Now()
	stages := newStageTimer()
	defer func() { r.logProvisionOutcome(ctx, tenantID, runID, start, stages) }()
	// Phase 1 — read the run (short transaction, released before git work).
	run, err := r.loadRun(ctx, tenantID, runID)
	if err != nil {
		return err
	}
	// Terminal runs dispatch to the prune path: their worktree (if any) is reaped instead of
	// provisioned.
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
		// Converge: the worktree we recorded must still exist on the recorded branch.
		if r.worktreeMatches(ctx, run.ProjectID, run.WorktreePath, run.WorktreeBranch) {
			r.ensureRunWorktreeStepIgnore(ctx, run.WorktreePath)
			return nil
		}
	case domain.WorktreePruned:
		// Re-provisioning happens ONLY through an explicit retry, which resets worktree_status to pending
		// (RetryFailedWorkflowRun flips pruned → pending while preserving the branch for re-attach).
		return nil
	case domain.WorktreeSkipped, domain.WorktreeFailed:
		// Recorded decisions are respected: a skipped or failed run
		// is never re-provisioned by the loop. (A human may reset the row.)
		return nil
	}

	// THE STRATEGY IS RESOLVED BEFORE THE REPOSITORY CHECK, because it decides what a MISSING repository means:
	// a problem to fix (local/pr) or simply how this project works (none).
	strategy := r.effectiveGitStrategy(ctx, tenantID, run)

	// Phase 2 — resolve the project dir + detect a git work tree (cached on
	// the project row so the loop does not shell out to git on every pass).
	projectDir, gitBacked, found := r.resolveGitBacked(ctx, tenantID, run.ProjectID)
	if !found {
		return r.markSkippedNotice(ctx, tenantID, runID, "project has no project_dir",
			"this project has no directory, so the run works with no isolated tree")
	}
	if !gitBacked {
		// A GIT-TYPE STRATEGY WITH NO REPOSITORY IS INITIALISED, NOT REFUSED.
		if db.RequiresRepository(strategy) {
			rep := projectpkg.EnsureGitState(ctx, projectDir, strategy)
			// "NO ERROR" IS NOT "REPAIRED".
			repaired := rep.Needed && rep.Err == nil && !rep.GitMissing && r.isRepoRoot(ctx, projectDir)
			if repaired {
				// The cached detection is stale by construction — we just changed the fact it caches.
				r.refreshGitDetection(ctx, tenantID, run.ProjectID, projectDir)
				gitBacked = true
				// A NOTICE, NOT AN ERROR: nothing failed, and the text says what happened and what the platform did
				// about it.
				notice := "This project had no git repository, so this run could not have cut a branch. The " +
					"platform initialised one on the project directory (" + strings.Join(rep.Done, "; ") + ") " +
					"and then provisioned the run's worktree as usual. Nothing was lost and nothing failed."
				r.noteRunNotice(ctx, tenantID, runID, "worktree_git_initialised", notice)
				r.log.Warn("worktree: initialised a missing git repository so the run can branch",
					"run", runID, "project_dir", projectDir, "strategy", strategy, "done", rep.Done)
			} else {
				// No git binary, an unwritable directory, or a directory inside another repository. Still NOT a
				// failure: the run proceeds in place and the notice says why.
				reason := rep.Summary()
				if reason == "" {
					reason = "a git repository could not be initialised there"
				}
				return r.markSkippedNotice(ctx, tenantID, runID,
					fmt.Sprintf("project dir %s has no usable git repository: %s", projectDir, reason),
					"This project is set to "+strategy+", which expects a git repository, and one could not be "+
						"created here. The run proceeds in place with no branch and no integration — nothing is "+
						"lost, but this run's work will not appear on the integration branch.")
			}
		} else {
			// `none`: no repository is required by design. Informational, never a warning.
			return r.markSkippedNotice(ctx, tenantID, runID,
				fmt.Sprintf("project dir %s is not a git work tree", projectDir),
				"This project is ephemeral (git_strategy=none), so the run works in place in the project "+
					"directory with no branch — exactly as configured.")
		}
	}

	stages.mark("project")

	// REPAIR THE REGISTRY BEFORE CREATING INTO IT.
	r.malformedWorktreeRegistrations(ctx, projectDir)

	path := filepath.Join(projectDir, worktreeDirName, runID)
	// For a `none` (ephemeral) run, NO named branch is created — the worktree is detached HEAD, so "no
	// branch retained" is structural (a detached worktree creates no ref that can leak locally.
	branch := ""
	if strategy != "none" {
		branch = r.branchName(ctx, tenantID, run)
	}

	// INTEGRATE BEFORE THE BASE IS RESOLVED, AND HOLD THE BRANCH UNTIL IT IS SAFE TO CUT.
	//
	// THIS IS THE PATH THAT CUTS THE RUN'S OWN BRANCH. The equivalent step in reconcileStepRunOne (added for
	// the same reason) covers a BRANCH CHILD, which is not what a worker commits to — so the guarantee was
	// documented and absent exactly where it mattered, and the run branch kept being cut from a stale tip.
	// Measured on the reported run set: news started 18:45:06 and location integrated 18:45:40; theme started
	// 19:01:24 and favourites integrated 19:02:21. Both misses branched 30-60s early, both were then divergent,
	// and a divergent branch is not a small inefficiency — the merge needs a real conflict, so one missed
	// integration makes every following run in the project un-mergeable cleanly.
	//
	// TWO STEPS, and the second is the real fix:
	//   1. INTEGRATE this project's backlog now, project-scoped (a tenant-wide page can be filled by other
	//      projects and leave this one untouched).
	//   2. If anything is STILL un-integrated, WAIT. Waiting is what makes the guarantee independent of
	//      timing: a run cannot branch over work that has not landed, whether the cause is a slow sweep, a
	//      just-completed predecessor, or a conflict a human must resolve. It is a queue, not a failure.
	if strategy == "local" {
		// DRAIN THE QUEUE FIRST — this project's pending obligations, oldest first. A tenant-wide page can be
		// filled entirely by other projects' older runs, which is why the drain is project-scoped.
		r.drainLocalIntegrationQueue(ctx, tenantID, run.ProjectID, time.Now().Add(localIntegrationPerRunBudget))
		// THEN ASK.
		if blocking := r.BlockingIntegrationCount(ctx, tenantID, run.ProjectID); blocking > 0 {
			ahead, recs := r.QueuePosition(ctx, tenantID, run.ProjectID, runID)
			// NOT A FAILURE, and deliberately no status change: the step run stays pending and the next pass
			// retries.
			r.log.Info("worktree: this run's branch is QUEUED behind work that has not landed yet",
				"run", runID, "project", run.ProjectID, "blocking_records", blocking, "ahead_of_it", ahead,
				"oldest_blocking_run", oldestBlockingRun(recs))
			return nil
		}
	}

	// Resolve the base ref up front so the container-adopt path (Phase 3,
	// case 2) can create the run worktree without re-entering Phase 4.
	base := r.resolveBaseRef(ctx, projectDir)
	stages.mark("base")

	// Phase 3 — converge against what git actually has at the expected path.
	existing, err := r.worktreeAt(ctx, projectDir, path)
	if err != nil {
		return fmt.Errorf("worktree: list worktrees: %w", err)
	}
	stages.mark("list")
	if existing != nil {
		if existing.branch == branch {
			// THE REGISTRATION IS NOT THE DIRECTORY.
			if fi, serr := os.Stat(path); serr == nil && fi.IsDir() {
				return r.recordReady(ctx, tenantID, runID, path, branch)
			}
			r.log.Warn("worktree: git lists a worktree whose directory is missing — pruning and recreating",
				"run", runID, "path", path, "branch", branch)
			if rerr := r.removeWorktree(ctx, projectDir, path); rerr != nil {
				return fmt.Errorf("worktree: prune stale registration at %s: %w", path, rerr)
			}
			existing = nil
		}
	}
	if existing != nil {
		// A foreign worktree occupies our path (e.g. a crashed partial create, or the run's slug source
		// changed).
		if run.WorktreeStatus == domain.WorktreePending {
			if rerr := r.removeWorktree(ctx, projectDir, path); rerr != nil {
				return fmt.Errorf("worktree: repair stale worktree at %s: %w", path, rerr)
			}
		} else {
			// Ready-but-gone reconverge: the recorded worktree vanished and our deterministic path is occupied
			// by a foreign worktree — fail closed rather than record a lie.
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
		// NOT GATED ON `pending`. The reclaim below used to require it, so a RETRIED run — which arrives here
		// with worktree_status=failed and a path — fell straight through to "occupied by a non-worktree
		// directory" and refused. That is the reported "why was it occupied": the directory was this run's OWN
		// empty leftover, created before the branch was cut by the attempt that failed, and the retry
		// classified it as foreign.
		//
		// Reclaiming is safe on the same terms it always was — the directory is EMPTY, or holds only a gitdir
		// file proving it is a partial worktree of ours — so requiring a particular status bought nothing and
		// cost a whole class of retry.
		// A WORKTREE THE CONTAINER CREATED IS OURS, and the host must reclaim it even though it holds content.
		//
		// REPORTED, four runs running: "path … is occupied by a non-worktree directory" on a directory that was
		// a COMPLETE worktree (.git, src/, package.json). The chain is worth stating because two of the three
		// links are ours:
		//
		//  1. The container's worker may create the worktree itself (the arbitration gap — bug 2, still open).
		//  2. #906 then rewrites that worktree's `.git` to the GUEST form so the container's git can follow it.
		//     The file is BIND-MOUNTED, so the rewrite is visible to the host too: the host now reads
		//     `/mnt/c/.../.git/worktrees/<run>`, a path it cannot resolve.
		//  3. This branch's ownership test compared with `filepath.Separator`, so on Windows the forward-slash
		//     pointer never matched, the directory was judged foreign, and provisioning refused.
		//
		// Reclaiming is safe because the WORK IS ON THE BRANCH, not in the working tree: a worktree is a
		// checkout of a commit, and the commits this one carried are in the repository's own .git. Nothing is
		// lost by removing the directory and cutting a fresh worktree from the same branch.
		//
		// Genuinely foreign content — a directory with no worktree pointer, or one naming somebody else's
		// repository — is STILL refused below. That distinction is what keeps this from deleting user data.
		reclaimForeign := r.isForeignWorktreeOfOurs(path, projectDir)
		if reclaimForeign {
			r.log.Warn("worktree: reclaiming a worktree this platform created but cannot resolve "+
				"(its .git names a container path) — the branch and its commits are unaffected",
				"run", runID, "path", path, "pointer", r.worktreePointerTarget(path))
		}
		if reclaimForeign || r.isReclaimableRunDir(path) {
			// EMPTY, or nothing but a gitdir file: a leftover of ours.
			r.log.Info("worktree: reclaiming a leftover run directory before provisioning",
				"run", runID, "path", path, "status", run.WorktreeStatus)
			if rerr := r.removeWorktreeArtifact(path); rerr != nil {
				return fmt.Errorf("worktree: reclaim leftover at %s: %w", path, rerr)
			}
			// THE REGISTRATION OUTLIVES THE DIRECTORY.
			if _, perr := runGit(ctx, projectDir, "worktree", "prune"); perr != nil {
				r.log.Warn("worktree: prune after reclaiming a leftover", "run", runID, "error", perr)
			}
		} else {
			// A RUN-NAMESPACED CONTAINER holding only provably-ours nested branch-worktree artifacts — a
			// parallel-branch step run was provisioned first, racing the run worktree. Adopt it.
			isContainer, cerr := r.isOrchiconContainer(ctx, projectDir, path)
			if cerr != nil {
				return fmt.Errorf("worktree: inspect container %s: %w", path, cerr)
			}
			if isContainer {
				if aerr := r.adoptRunContainer(ctx, projectDir, path, branch, base, strategy == "none"); aerr != nil {
					return fmt.Errorf("worktree: adopt run container at %s: %w", path, aerr)
				}
				return r.recordReady(ctx, tenantID, runID, path, branch)
			}
			return r.markFailed(ctx, tenantID, runID,
				fmt.Sprintf("path %s is occupied by a non-worktree directory", path))
		}
	}

	// Phase 4 — create or attach against the resolved base ref.
	var perr error
	if strategy == "none" {
		perr = r.provisionDetachedWorktree(ctx, projectDir, path, base)
	} else {
		perr = r.provisionWorktree(ctx, projectDir, path, branch, base)
	}
	if perr != nil {
		if isAlreadyTrackedErr(perr) {
			// A concurrent pass won the create race — converge on the
			// worktree git now has at our path.
			if existing, lerr := r.worktreeAt(ctx, projectDir, path); lerr == nil && existing != nil && existing.branch == branch {
				return r.recordReady(ctx, tenantID, runID, path, branch)
			}
		}
		// WHAT GIT ACTUALLY SAID, plus every input the attempt used.
		reason := worktreeProvisionError(perr, strategy, branch, base, projectDir)
		// AND LEAVE NOTHING BEHIND. A failed add can leave a stray branch and a half-built directory; the
		// next attempt then meets an "already exists" the failure record does not explain.
		if leftovers := r.cleanupFailedProvision(ctx, projectDir, path, branch, strategy != "none"); len(leftovers) > 0 {
			r.log.Warn("worktree: failure cleanup left artifacts", "run", runID, "path", path, "branch", branch,
				"leftovers", leftovers)
			reason += " (cleanup could not remove: " + strings.Join(leftovers, ", ") + ")"
		}
		return r.markFailed(ctx, tenantID, runID, reason)
	}

	if err := r.recordReady(ctx, tenantID, runID, path, branch); err != nil {
		return err
	}
	r.ensureRunWorktreeStepIgnore(ctx, path)
	return nil
}

// reconcileStepRunOne provisions a single parallel-branch step run's isolated working tree and
// records the result (path + branch) on the workflow_step_runs row.
func (r *WorktreeReconciler) reconcileStepRunOne(ctx context.Context, tenantID, runID, stepRunID string) error {
	// Phase 1 — read the step run + its run (short transaction, released
	// before git work).
	run, sr, err := r.loadStepRun(ctx, tenantID, runID, stepRunID)
	if err != nil {
		return err
	}
	// Terminal step runs (or step runs of terminal runs) dispatch to the prune path: their branch
	// worktree (if any) is reaped instead of provisioned.
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
		// Converge: the worktree we recorded must still exist on the recorded branch.
		if r.worktreeMatches(ctx, run.ProjectID, sr.WorktreePath, sr.WorktreeBranch) {
			// Ensure parallel-branch worktrees inherit the latest run branch tip.
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
		// Re-provisioning happens ONLY through an explicit retry, which resets worktree_status to pending
		// while preserving the branch for re-attach — never by flipping statuses alone.
		return nil
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
	// `none` (ephemeral): the branch child worktree is detached HEAD too — no named branch, so a
	// parallel-branch child can never create a ref that leaks.
	strategy := r.effectiveGitStrategy(ctx, tenantID, run)
	branch := ""
	if strategy != "none" {
		branch = r.branchWorktreeName(ctx, tenantID, run, sr)
	}

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
		// A foreign worktree occupies our path.
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

	// Phase 4 — resolve the base ref and create.
	base := ""
	if run.WorktreeStatus == domain.WorktreeReady && run.WorktreeBranch != "" {
		base = run.WorktreeBranch
	} else {
		// INTEGRATE FIRST, THEN BRANCH.
		r.integrateLocalRuns(ctx, tenantID, time.Now().Add(localIntegrationPerRunBudget))

		// REFUSE AN UNUSABLE BASE RATHER THAN PROVISIONING AN EMPTY WORKTREE.
		resolved, ok, reason := r.resolveBaseRefForProvisioning(ctx, projectDir)
		if !ok {
			return r.markFailed(ctx, tenantID, runID,
				"worktree provisioning refused: "+reason)
		}
		base = resolved
	}
	var perr error
	if strategy == "none" {
		perr = r.provisionDetachedWorktree(ctx, projectDir, path, base)
	} else {
		perr = r.provisionWorktree(ctx, projectDir, path, branch, base)
	}
	if perr != nil {
		if isAlreadyTrackedErr(perr) {
			// A concurrent pass won the create race — converge.
			if existing, lerr := r.worktreeAt(ctx, projectDir, path); lerr == nil && existing != nil && existing.branch == branch {
				return r.recordStepReady(ctx, tenantID, stepRunID, path, branch)
			}
		}
		reason := worktreeProvisionError(perr, strategy, branch, base, projectDir)
		if leftovers := r.cleanupFailedProvision(ctx, projectDir, path, branch, strategy != "none"); len(leftovers) > 0 {
			r.log.Warn("worktree: failure cleanup left artifacts", "step_run", stepRunID, "path", path, "branch", branch,
				"leftovers", leftovers)
			reason += " (cleanup could not remove: " + strings.Join(leftovers, ", ") + ")"
		}
		return r.markStepFailed(ctx, tenantID, stepRunID, reason)
	}

	return r.recordStepReady(ctx, tenantID, stepRunID, path, branch)
}

// branchWorktreeName computes the deterministic, collision-safe branch for a branch step run:
// <runBranch>-<stepSlug>-<stepRunSuffix>.
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
	// Defensive cap: shrink the step slug, then the run-branch prefix, so the full ref stays under
	// git's limit even for a near-maximal run branch.
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

// pruneStepRunOne reaps a terminal step run's branch worktree: remove the working tree dir, run
// `git worktree prune` in the main repo, and record 'pruned' on the step run row.
func (r *WorktreeReconciler) pruneStepRunOne(ctx context.Context, tenantID string, sr db.WorkflowStepRunRow) error {
	// 'failed' as well as 'ready', for the same reason as pruneOne: a provisioning failure can leave a
	// directory behind, and skipping it here is what let the debris outlive every sweep.
	if (sr.WorktreeStatus != domain.WorktreeReady && sr.WorktreeStatus != domain.WorktreeFailed) || sr.WorktreePath == "" {
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
		ours, perr := r.pruneWorktreeLeftover(ctx, projectDir, path)
		if perr != nil {
			return fmt.Errorf("worktree: inspect %s: %w", path, perr)
		}
		if ours {
			if rerr := r.removeWorktreeArtifact(path); rerr != nil {
				return fmt.Errorf("worktree: remove artifact at %s: %w", path, rerr)
			}
			r.log.Info("worktree: removed a leftover directory at the recorded worktree path",
				"run", sr.WorkflowRunID, "step_run", sr.ID, "path", path)
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

	// Reap a now-empty run-namespaced container left behind once its registered/nested worktrees are
	// all gone (orphan sweep).
	r.sweepOrphanContainers(ctx, projectDir)

	// ADR 2.3: verify physical removal before DB transition.
	if occupied, _ := r.dirOccupied(path); occupied {
		r.log.Warn("worktree: step prune verification failed — path still exists", "run", sr.WorkflowRunID, "step_run", sr.ID, "path", path)
		return nil
	}

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

// isTerminalStepRun reports whether a step run can never progress again — the terminal states after
// which its branch worktree (if any) is reaped.
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
	// REAP A 'ready' WORKTREE — and also a 'failed' one, which is the case this guard used to skip.
	if (run.WorktreeStatus != domain.WorktreeReady && run.WorktreeStatus != domain.WorktreeFailed) || run.WorktreePath == "" {
		return nil
	}
	path := run.WorktreePath

	// Resolve the main repo.
	projectDir := r.lookupProjectDir(ctx, tenantID, run.ProjectID)
	if projectDir == "" || !r.isInsideWorkTree(ctx, projectDir) {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return r.markPruned(ctx, tenantID, run.ID, "worktree already gone; main repo unresolvable")
		}
		r.log.Warn("worktree: cannot prune; main repo unresolvable", "run", run.ID, "path", path)
		return nil
	}

	// Registered worktree at the recorded path → remove it (--force: a failed run may leave dirty
	// files).
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
		// Not registered but the directory exists: only remove it when we can prove it is one of our
		// worktree artifacts; never delete arbitrary user data.
		ours, perr := r.pruneWorktreeLeftover(ctx, projectDir, path)
		if perr != nil {
			return fmt.Errorf("worktree: inspect %s: %w", path, perr)
		}
		if ours {
			if rerr := r.removeWorktreeArtifact(path); rerr != nil {
				return fmt.Errorf("worktree: remove artifact at %s: %w", path, rerr)
			}
			r.log.Info("worktree: removed a leftover directory at the recorded worktree path",
				"run", run.ID, "path", path)
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

	// Branch deletion — two gates: Gate A (completed): provably merged (squash-aware).
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

	// Reap a now-empty run-namespaced container left behind once its registered/nested worktrees are
	// all gone (orphan sweep; also reaps the ones past manual cleanups left).
	r.sweepOrphanContainers(ctx, projectDir)

	// ADR 2.3: verify physical removal before DB state transition.
	if occupied, _ := r.dirOccupied(path); occupied {
		r.log.Warn("worktree: prune verification failed — path still exists", "run", run.ID, "path", path)
		return nil
	}
	if run.Status == domain.WorkflowRunCompleted && run.WorktreeBranch != "" && r.branchExists(ctx, projectDir, run.WorktreeBranch) {
		// Branch should have been deleted (provably merged).
		r.log.Info("worktree: branch still exists after prune (orphan sweep will reclaim)", "run", run.ID, "branch", run.WorktreeBranch)
	}

	return r.markPruned(ctx, tenantID, run.ID, "")
}

// logProvisionOutcome emits one structured log line per reconcileOne attempt with the run's
// post-commit worktree_status.
func (r *WorktreeReconciler) logProvisionOutcome(ctx context.Context, tenantID, runID string, start time.Time, stages *stageTimer) {
	status := "unknown"
	if cur, err := r.loadRun(ctx, tenantID, runID); err == nil {
		status = cur.WorktreeStatus
		if status == "" {
			status = "missing"
		}
	}
	// THE STAGE BREAKDOWN IS THE POINT.
	r.log.Info("worktree: provision", "run", runID,
		"duration_ms", time.Since(start).Milliseconds(),
		"stages", stages.String(),
		"worktree_status", status)
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

// resolveGitBacked loads a project and decides whether its project_dir is a git work tree, using
// the cached detection on the project row to avoid shelling out to git on every reconcile pass.
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

// branchName computes the deterministic, collision-safe branch for a run: <kebab-slug>-<runSuffix>.
func (r *WorktreeReconciler) branchName(ctx context.Context, tenantID string, run db.WorkflowRunRow) string {
	return branchNameFor(r.slugSource(ctx, tenantID, run), run.ID)
}

// effectiveGitStrategy resolves the run's effective git strategy (workflow > project > "local") in
// a short read-only transaction.
func (r *WorktreeReconciler) effectiveGitStrategy(ctx context.Context, tenantID string, run db.WorkflowRunRow) string {
	ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return db.DefaultGitStrategy
	}
	defer ttx.Rollback(ctx)
	return db.EffectiveGitStrategy(ctx, ttx.Tx, tenantID, run.WorkflowID, run.ProjectID)
}

// branchNameFor is the pure branch-name constructor: <kebab-slug>-<suffix>.
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

// runSuffix returns the collision-safe entropy portion of a run ID.
func runSuffix(runID string) string {
	if len(runID) >= 26 {
		return strings.ToLower(runID[10:])
	}
	return strings.ToLower(runID)
}

// slugSource picks the most descriptive deterministic name for a run's branch: the bound work
// item's title, else the workflow name, else the project name.
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

// runGit runs a git command in dir with argv-only arguments and a bounded timeout.

// stageTimer accumulates how long each PHASE of a provision took.
type stageTimer struct {
	last  time.Time
	marks []stageMark
}

type stageMark struct {
	name string
	d    time.Duration
}

func newStageTimer() *stageTimer { return &stageTimer{last: time.Now()} }

// mark closes the current stage and names it. Consecutive marks attribute the time BETWEEN them, so the
// caller marks phase boundaries rather than measuring each phase individually.
func (s *stageTimer) mark(name string) {
	if s == nil {
		return
	}
	now := time.Now()
	s.marks = append(s.marks, stageMark{name: name, d: now.Sub(s.last)})
	s.last = now
}

// String renders the marks for a log field: "resolve=12ms base=1ms add=65200ms".
func (s *stageTimer) String() string {
	if s == nil || len(s.marks) == 0 {
		return ""
	}
	var b strings.Builder
	for i, m := range s.marks {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(m.name)
		b.WriteByte('=')
		b.WriteString(m.d.Round(time.Millisecond).String())
	}
	return b.String()
}

// hasOriginRemote reports whether this repository has an `origin` remote configured at all.
//
// IT EXISTS TO STOP ASKING THE IMPOSSIBLE. Every remote-touching operation in this file — `remote show
// origin`, `fetch origin develop`, `ls-remote origin`, `push origin --delete` — is asked of a repository
// that may have no remote at all, and on such a repository each one fails with
//
//	fatal: 'origin' does not appear to be a git repository
//
// MEASURED on a real Windows host: that message repeated through every run's provision and prune, and
// `fetch origin develop` plus the remote-branch-gone probe were both attempted per prune. Each fails in
// milliseconds when origin is absent, so this is not by itself the 65-second provision — but it is pure
// noise in the log an operator reads while diagnosing one, and it hides the failures that matter.
//
// `remote get-url` is the cheapest question that answers it: local, no network, no output parsing. It is a
// per-call check rather than a cached one on purpose — it costs single-digit milliseconds and a cache is
// one more thing that can go stale when a remote is added mid-session.
func (r *WorktreeReconciler) hasOriginRemote(ctx context.Context, projectDir string) bool {
	_, err := runGit(ctx, projectDir, "remote", "get-url", "origin")
	return err == nil
}
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return "", fmt.Errorf("git not available on control-plane host: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, gitCmdTimeout)
	defer cancel()
	cmd := procwin.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	// CombinedOutput is the point: git writes the sentence that names a fatal failure — `fatal: invalid
	// reference`, `fatal: not a git repository`, an auth prompt rejected — to stderr.
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), gitError(dir, args, string(out), err)
	}
	return string(out), nil
}

// isRepoRoot reports whether projectDir is ITSELF a git repository root, as opposed to a directory
// that merely sits inside someone else's repository.
func (r *WorktreeReconciler) isRepoRoot(ctx context.Context, dir string) bool {
	top, err := runGit(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return false
	}
	top = strings.TrimSpace(top)
	if top == "" {
		return false
	}
	// Symlinks and trailing separators differ across platforms; compare resolved paths when possible.
	if resolved, rerr := filepath.EvalSymlinks(top); rerr == nil {
		top = resolved
	}
	if resolved, rerr := filepath.EvalSymlinks(dir); rerr == nil {
		dir = resolved
	}
	return filepath.Clean(top) == filepath.Clean(dir)
}

// isInsideWorkTree reports whether projectDir is inside a git work tree (`git rev-parse
// --is-inside-work-tree` exits 0 and prints "true").
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

// detectRepoSlug reads the git remote origin of projectDir and returns it as "owner/repo" (e.g.
// "beardedparrott/Orchicon").
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

// listWorktrees parses `git worktree list --porcelain` for the repo at projectDir.
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

// isOrchiconWorktreeArtifact reports whether path is a partial git-worktree artifact created by
// this reconciler: a directory inside our run-namespaced .orchicon-worktrees dir carrying a `.git`.
func (r *WorktreeReconciler) isOrchiconWorktreeArtifact(path string) bool {
	return r.worktreePointerTarget(path) != ""
}

// worktreePointerTarget returns the gitdir a worktree's `.git` file names, when that gitdir lives
// in a repository's worktrees metadata — the shape of EVERY git worktree.
func (r *WorktreeReconciler) worktreePointerTarget(path string) string {
	gitFile := filepath.Join(path, ".git")
	st, err := os.Stat(gitFile)
	if err != nil || st.IsDir() {
		return "" // absent, or a real .git DIRECTORY (a repository, not a worktree)
	}
	b, err := os.ReadFile(gitFile)
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(b))
	if !strings.HasPrefix(line, "gitdir: ") {
		return ""
	}
	target := strings.TrimSpace(strings.TrimPrefix(line, "gitdir: "))
	if target == "" {
		return ""
	}
	folded := strings.ReplaceAll(target, "\\", "/")
	if !strings.Contains(folded, ".git/worktrees") {
		return ""
	}
	return target
}

// isForeignWorktreeOfOurs reports whether path holds a worktree of OURS that the host cannot
// validate — a pointer naming the repository's own worktrees metadata.
func (r *WorktreeReconciler) isForeignWorktreeOfOurs(path, projectDir string) bool {
	target := r.worktreePointerTarget(path)
	if target == "" {
		return false
	}
	folded := strings.ReplaceAll(target, "\\", "/")
	hostRepo := strings.ReplaceAll(filepath.ToSlash(filepath.Clean(projectDir)), "\\", "/")
	hostRepo = strings.TrimRight(hostRepo, "/")
	// THE HOST FORM: an exact prefix, which is the strongest evidence available.
	if hostRepo != "" && strings.HasPrefix(folded, hostRepo+"/.git/worktrees/") {
		return true
	}
	// THE CONTAINER FORM: the host repo path is not a prefix of `/mnt/c/...`, so an exact prefix cannot
	// work.
	base := strings.TrimRight(filepath.ToSlash(path), "/")
	leaf := base[strings.LastIndex(base, "/")+1:]
	projectBase := strings.TrimRight(filepath.ToSlash(filepath.Clean(projectDir)), "/")
	projectBase = projectBase[strings.LastIndex(projectBase, "/")+1:]
	if leaf == "" || projectBase == "" || leaf == "." || leaf == "/" {
		return false
	}
	return strings.HasSuffix(folded, "/"+projectBase+"/.git/worktrees/"+leaf)
}

// pruneWorktreeLeftover reports whether the directory at path is provably ours to delete during a
// PRUNE.
func (r *WorktreeReconciler) pruneWorktreeLeftover(ctx context.Context, projectDir, path string) (bool, error) {
	if r.isOrchiconWorktreeArtifact(path) {
		return true, nil
	}
	return r.isOrchiconContainer(ctx, projectDir, path)
}

// isOrchiconContainer reports whether path is a plain directory whose immediate children are all
// provably ours — a run-namespaced CONTAINER holding only nested branch-worktree artifacts (a.
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

// childIsOrchiconOwned reports whether an immediate child of a run container is provably ours: a
// registered git worktree, a gitdir worktree artifact, or a lone `.git` gitdir file.
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

// sweepOrphanContainers removes run-namespaced container directories under .orchicon-worktrees/
// that are EMPTY and provably ours.
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

// The container is NEVER deleted (AC2): only `git worktree move` (for registered worktrees) or
// os.Rename of provably-ours artifacts mutates the filesystem.
func (r *WorktreeReconciler) adoptRunContainer(ctx context.Context, projectDir, path, branch, base string, detached bool) error {
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

	// 2.
	var createErr error
	if detached {
		createErr = r.provisionDetachedWorktree(ctx, projectDir, path, base)
	} else {
		createErr = r.addWorktree(ctx, projectDir, path, branch, base)
	}
	if createErr != nil {
		// Best-effort rollback: move already-staged children back so a
		// later pass can retry cleanly.
		for _, m := range moves {
			if m.move {
				_, _ = runGit(ctx, projectDir, "worktree", "move", m.to, m.from)
			} else {
				_ = os.Rename(m.to, m.from)
			}
		}
		return createErr
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

// stagingDirName builds the transient sibling directory name used to stage a nested branch worktree
// out of a run container during adoption.
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
	// NEVER DESTROY UNCOMMITTED WORK.
	if dirty, derr := r.worktreeIsDirty(ctx, path); derr != nil {
		r.log.Warn("worktree: could not tell whether this worktree has uncommitted work — leaving it in place",
			"path", path, "error", derr)
		return nil
	} else if dirty {
		r.log.Warn("worktree: NOT removing a worktree that has UNCOMMITTED changes — it holds work that exists "+
			"nowhere else. Commit or discard it, then the sweep will reclaim it",
			"path", path, "remedy", "cd "+path+" && git status")
		return nil
	}
	if _, err := runGit(ctx, projectDir, "worktree", "remove", "--force", path); err != nil {
		return err
	}
	return nil
}

// worktreeIsDirty reports whether the worktree at path holds modified, staged, or untracked files.
func (r *WorktreeReconciler) worktreeIsDirty(ctx context.Context, path string) (bool, error) {
	out, err := runGit(ctx, path, "status", "--porcelain")
	if err != nil {
		return true, err
	}
	return strings.TrimSpace(out) != "", nil
}

// cleanupFailedProvision removes what a FAILED `git worktree add` may have left behind.
func (r *WorktreeReconciler) cleanupFailedProvision(ctx context.Context, projectDir, path, branch string, named bool) []string {
	var leftovers []string
	if path != "" {
		// Registered worktree first (git must drop its metadata), then any directory git left behind.
		if _, err := runGit(ctx, projectDir, "worktree", "remove", "--force", path); err != nil {
			if rmErr := r.removeWorktreeArtifact(path); rmErr != nil {
				leftovers = append(leftovers, "worktree directory "+path+" ("+rmErr.Error()+")")
			}
		}
		if _, err := runGit(ctx, projectDir, "worktree", "prune"); err != nil {
			leftovers = append(leftovers, "worktree prune")
		}
	}
	// ONLY A BRANCH THIS ATTEMPT CREATED, and never a protected one. A detached (`none`) run creates no
	// branch at all, and a branch that already existed belongs to an earlier run.
	if named && branch != "" && !isProtectedBranch(branch) {
		if _, err := runGit(ctx, projectDir, "branch", "-D", branch); err != nil {
			// `-D` on a branch that was never created fails here; that is the common case and is not a
			// leftover worth reporting.
			if exists := r.branchExists(ctx, projectDir, branch); exists {
				leftovers = append(leftovers, "branch "+branch)
			}
		}
	}
	return leftovers
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

// provisionWorktree creates or attaches a worktree at path on branch.
func (r *WorktreeReconciler) provisionWorktree(ctx context.Context, projectDir, path, branch, base string) error {
	// BOTH ARMS GO THROUGH addWorktreeOrFreeStale, and that is not a detail: this function's ATTACH arm
	// used to call git directly, so it bypassed the stale-registration repair entirely.
	if r.branchExists(ctx, projectDir, branch) {
		return r.addWorktreeOrFreeStale(ctx, projectDir, []string{"worktree", "add", path, branch}, branch, path)
	}
	return r.addWorktree(ctx, projectDir, path, branch, base)
}

// provisionDetachedWorktree creates a detached-HEAD worktree at path from base (`git worktree add
// --detach <path> <base>`).
func (r *WorktreeReconciler) provisionDetachedWorktree(ctx context.Context, projectDir, path, base string) error {
	args := []string{"worktree", "add", "--detach", path}
	if base != "" {
		args = append(args, base)
	}
	if _, err := runGit(ctx, projectDir, args...); err != nil {
		return err
	}
	return nil
}

// addWorktree runs `git worktree add <path> -b <branch> [<base>]` with argv-only arguments (never a
// shell string).
func (r *WorktreeReconciler) addWorktree(ctx context.Context, projectDir, path, branch, base string) error {
	args := []string{"worktree", "add", path, "-b", branch}
	if base != "" {
		args = append(args, base)
	}
	return r.addWorktreeOrFreeStale(ctx, projectDir, args, branch, path)
}

// A BRANCH ALREADY CLAIMED BY A REGISTRATION THE HOST CANNOT SATISFY.
func (r *WorktreeReconciler) addWorktreeOrFreeStale(ctx context.Context, projectDir string, args []string, branch, path string) error {
	//
	// (`<repo>/.git/worktrees/<n>/gitdir`) and finding this branch claimed there. When that recorded path
	// cannot be resolved — a container path, a deleted directory, a worktree whose admin data outlived it —
	// the registration is stale and `git worktree add` will refuse forever.
	//
	// PRUNE AND RETRY ONCE. `git worktree prune` removes registrations whose worktree is gone or whose
	// back-pointer does not resolve; measured against a registry corrupted this exact way, it clears the
	// claim and the next add succeeds. It is TARGETED — only this specific refusal triggers it, so a genuine
	// concurrently-checked-out branch is not disturbed by an unrelated prune.
	_, err := runGit(ctx, projectDir, args...)
	if err == nil {
		return nil
	}
	if !strings.Contains(err.Error(), "is already used by worktree") {
		return err
	}
	r.log.Warn("worktree: the branch is claimed by a stale registration — pruning and retrying once",
		"branch", branch, "path", path, "error", err)
	if _, perr := runGit(ctx, projectDir, "worktree", "prune"); perr != nil {
		r.log.Warn("worktree: prune before retry failed", "error", perr)
		return err // report the ORIGINAL refusal: it is the actionable one
	}
	if _, rerr := runGit(ctx, projectDir, args...); rerr != nil {
		return rerr
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

// branchMergedIntoBase reports whether branch's tip is an ancestor of the integration base (develop
// / origin/develop) — i.e. its commits are already merged.
func (r *WorktreeReconciler) branchMergedIntoBase(ctx context.Context, projectDir, branch string) bool {
	// NO ORIGIN, NO FETCH — and no warning about it.
	if r.hasOriginRemote(ctx, projectDir) {
		if _, err := runGit(ctx, projectDir, "fetch", "origin", "develop"); err != nil {
			r.log.Warn("worktree: fetch origin develop failed; merge gate uses local refs",
				"branch", branch, "error", err)
		}
	}
	for _, ref := range []string{"develop", "origin/develop"} {
		if _, err := runGit(ctx, projectDir, "merge-base", "--is-ancestor", branch, ref); err == nil {
			return true
		}
	}
	return false
}

// isWorkItemReclaimable reports whether a failed/aborted run's branch is reclaimable: the run has
// no bound work item, the work item no longer exists.
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

// deleteDeadBranch deletes a branch for a dead (failed/aborted) run whose work item is terminal.
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

// deleteBranch deletes a branch ref after a successful run, gated on the branch being provably ours
// (the deterministic name recorded on the row), not protected, not the current branch.
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
	// L3 — remote half of the prune: when a remote ref exists, delete it FIRST, fail-closed.
	if exists, determined := r.branchRemoteExists(ctx, projectDir, branch); determined && exists {
		if _, err := runGit(ctx, projectDir, "push", "origin", "--delete", branch); err != nil {
			return fmt.Errorf("git push origin --delete %s: %w", branch, err)
		}
		r.log.Info("worktree: deleted remote branch after successful run", "branch", branch)
	} else if !determined {
		r.log.Info("worktree: remote branch probe inconclusive; skipping remote delete (local delete proceeds)", "branch", branch)
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
	// P1 — fast path: the branch tip is an ancestor of the base.
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
	// P3 — fallback when the run row carries no pr_state: a branch whose remote ref is gone is treated
	// as merged, but only on a successful probe AND when its tip is not newer than the base (no proof.
	if prState == "" {
		r.log.Info("worktree: no pr_state; probing remote branch ref", "branch", branch)
		if r.remoteBranchRefGone(ctx, projectDir, branch) && r.branchTipNotNewerThanBase(ctx, projectDir, branch) {
			return true, "remote-branch-gone"
		}
		return false, "uncertain"
	}
	return false, "uncertain"
}

// branchTipNotNewerThanBase reports whether branch's tip committer date is not newer than the base
// tip's (origin/develop preferred, else local develop).
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

// remoteBranchRefGone reports whether the remote PR branch ref is gone, used as the P3 fallback
// ONLY when the run row records no pr_state.
func (r *WorktreeReconciler) remoteBranchRefGone(ctx context.Context, projectDir, branch string) bool {
	// A REPOSITORY WITH NO ORIGIN HAS NO REMOTE BRANCH TO BE GONE, and asking git to prune a remote
	// that does not exist logged a WARN per prune.
	if !r.hasOriginRemote(ctx, projectDir) {
		return false
	}
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

// branchRemoteExists reports whether a remote ref refs/heads/<branch>
// exists on origin. It returns (exists, determined):
//   - determined=false means the probe could NOT confirm (origin absent /
//     network down) — the caller must SKIP the remote `--delete` but still
//     allow a provably-merged LOCAL delete (the Design Approver's
//     refinement: fail-closed for deletion, but never deadlock local
//     cleanup on a missing origin).
//   - determined=true means the probe spoke to origin and exists is
//     authoritative.
//
// It is the L3 gate that decides whether `git push origin --delete` is even
// attempted; it never deletes on doubt.
func (r *WorktreeReconciler) branchRemoteExists(ctx context.Context, projectDir, branch string) (bool, bool) {
	if branch == "" {
		return false, true
	}
	// THE SAME ANSWER AS A FAILED PROBE, WITHOUT THE FAILED PROBE.
	if !r.hasOriginRemote(ctx, projectDir) {
		return false, false
	}
	out, err := runGit(ctx, projectDir, "ls-remote", "--heads", "origin", branch)
	if err != nil {
		// origin absent / network down — cannot confirm. Skip the remote
		// delete (do not block a provably-merged local delete).
		r.log.Warn("worktree: remote branch probe failed; skipping remote delete",
			"branch", branch, "error", err)
		return false, false
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == "refs/heads/"+branch {
			return true, true
		}
	}
	return false, true
}

// isAncestor reports whether branch a is an ancestor of branch b (a..b fast-forwardable).
func (r *WorktreeReconciler) isAncestor(ctx context.Context, projectDir, a, b string) bool {
	_, err := runGit(ctx, projectDir, "merge-base", "--is-ancestor", a, b)
	return err == nil
}

// ensureRunWorktreeStepIgnore creates a .gitignore at the run worktree root that ignores nested step worktrees.
func (r *WorktreeReconciler) ensureRunWorktreeStepIgnore(ctx context.Context, runWorktreePath string) {
	// Create a .gitignore at the run worktree root that ignores nested step worktrees.
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

// fastForwardBranch fast-forwards stepBranch to runBranch tip.
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

// recordReady records the provisioned worktree on the run row.
func (r *WorktreeReconciler) recordReady(ctx context.Context, tenantID, runID, path, branch string) error {
	// A WORKTREE THAT IS NOT THERE IS NOT READY.
	if fi, err := os.Stat(path); err != nil || !fi.IsDir() {
		return fmt.Errorf("worktree: refusing to record %s as ready — it is not a directory on disk (%v). "+
			"git may still list it; the registration is stale and provisioning must recreate it", path, err)
	}
	// Retry on optimistic-lock version conflicts (ErrNotFound from WHERE version=...).
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

// markSkipped records that the project is not git-backed so the run proceeds in place (the non-repo
// fallback consumes this decision).
func (r *WorktreeReconciler) markSkippedNotice(ctx context.Context, tenantID, runID, reason, note string) error {
	r.log.Info("worktree: run skipped (works in place)", "run", runID, "reason", reason, "notice", note)
	return r.mark(ctx, tenantID, runID, domain.WorktreeSkipped, "", "", reason+"\n\n"+note)
}

// noteRunNotice records an operator-facing note on the run without touching its status.
func (r *WorktreeReconciler) noteRunNotice(ctx context.Context, tenantID, runID, kind, note string) {
	ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return
	}
	defer ttx.Rollback(ctx)
	run, err := db.GetWorkflowRun(ctx, ttx.Tx, tenantID, runID)
	if err != nil {
		return
	}
	merged, ok := mergeRunContext(run.RunContext, map[string]any{
		"worktree_notice":      note,
		"worktree_notice_kind": kind,
	})
	if !ok {
		return
	}
	if _, uerr := db.UpdateWorkflowRun(ctx, ttx.Tx, tenantID, runID, run.Version,
		db.UpdateWorkflowRunFields{RunContext: &merged}); uerr != nil {
		r.log.Warn("worktree: could not record the run notice", "run", runID, "error", uerr)
		return
	}
	if cerr := ttx.Commit(ctx); cerr != nil {
		r.log.Warn("worktree: could not commit the run notice", "run", runID, "error", cerr)
	}
}

// refreshGitDetection re-detects whether projectDir is a git work tree and WRITES the result back,
// bypassing the cached value.
func (r *WorktreeReconciler) refreshGitDetection(ctx context.Context, tenantID, projectID, projectDir string) bool {
	isWT := r.isInsideWorkTree(ctx, projectDir)
	slug := ""
	if isWT {
		slug = r.detectRepoSlug(ctx, projectDir)
	}
	ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return isWT // detection still answers; only the cache write is lost
	}
	defer ttx.Rollback(ctx)
	proj, err := db.GetProject(ctx, ttx.Tx, tenantID, projectID)
	if err != nil {
		return isWT
	}
	if _, uerr := db.UpdateProjectGitDetection(ctx, ttx.Tx, tenantID, projectID, proj.Version, isWT, slug); uerr != nil {
		r.log.Warn("worktree: could not refresh the cached git detection after init", "project", projectID, "error", uerr)
		return isWT
	}
	if cerr := ttx.Commit(ctx); cerr != nil {
		r.log.Warn("worktree: could not commit the git detection refresh", "project", projectID, "error", cerr)
	}
	return isWT
}

func (r *WorktreeReconciler) markSkipped(ctx context.Context, tenantID, runID, reason string) error {
	r.log.Info("worktree: run skipped (non-repo project)", "run", runID, "reason", reason)
	return r.mark(ctx, tenantID, runID, domain.WorktreeSkipped, "", "", reason)
}

// markFailed records a provisioning error on the run.
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
	// D3 (concurrency guards): marking a run 'skipped' is admitting it into the IN-PLACE fallback — its
	// executions will run in the shared project_dir.
	if status == domain.WorktreeSkipped {
		// ADR 2.2 pre-dispatch clean check: never dispatch in-place against a dirty git checkout.
		if run.ProjectID != "" {
			if dir := r.lookupProjectDir(ctx, tenantID, run.ProjectID); dir != "" && r.isInsideWorkTree(ctx, dir) && r.isDirtyWorkTree(ctx, dir) {
				r.log.Warn("worktree: refusing in-place dispatch — checkout dirty", "run", runID, "project_dir", dir)
				// Mark failed instead of skipped so the run does not proceed in place.
				fields := db.UpdateWorkflowRunFields{WorktreeStatus: strPtr(domain.WorktreeFailed)}
				reasonDirty := "checkout dirty — refusing in-place dispatch; clean the working tree and retry"
				if merged, ok := mergeRunContext(run.RunContext, map[string]any{"worktree_error": reasonDirty}); ok {
					fields.RunContext = &merged
				}
				if _, err := db.UpdateWorkflowRun(ctx, ttx.Tx, tenantID, runID, run.Version, fields); err != nil {
					return fmt.Errorf("mark worktree failed (dirty): %w", err)
				}
				return ttx.Commit(ctx)
			}
		}
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
		// THE REASON IS A FIRST-CLASS OUTCOME, not just a run_context key.
		if ctx2, ok := mergeRunContext(run.RunContext, map[string]any{"worktree_error": reason}); ok {
			fields.RunContext = &ctx2
		}
		fields.ErrorMessage = strPtr(reason)
		// The REWRITE is what makes this actionable: the raw git line names a ref, not a remedy. The platform
		// knows the strategy and the project dir, so it can say what to DO.
		if friendly := preflightRemedy(status, reason); friendly != "" {
			fields.ErrorMessage = strPtr(friendly + "\n\nDetail: " + reason)
		}
		if status == domain.WorktreeSkipped {
			// A SKIP IS A NOTICE. Status is untouched (skipped still dispatches in place); only the CLASS of the
			// message changes, so the UI stops painting a supported outcome as a failure.
			fields.ErrorKind = strPtr(worktreeNoticeKind)
		} else {
			fields.ErrorKind = strPtr(preflightErrorKind(status, reason))
		}
	}
	// CLEAN UP AN EMPTY LEFTOVER ON FAILURE, so a retry does not inherit it. ONLY EMPTY.
	if status == domain.WorktreeFailed && path != "" && r.isReclaimableRunDir(path) {
		if rerr := os.RemoveAll(path); rerr != nil {
			r.log.Warn("worktree: could not clear an empty leftover after a failed provision", "run", runID, "path", path, "error", rerr)
		} else {
			r.log.Info("worktree: cleared an empty leftover after a failed provision", "run", runID, "path", path)
		}
	}
	if _, err := db.UpdateWorkflowRun(ctx, ttx.Tx, tenantID, runID, run.Version, fields); err != nil {
		return fmt.Errorf("mark worktree %s: %w", status, err)
	}
	return ttx.Commit(ctx)
}

// admitInPlace is the D3 atomic admission for a run entering the in-place (non-repo) fallback.
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

// markPruned records that a terminal run's worktree has been reaped.
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

// recordStepReady records the provisioned branch worktree on the step run row.
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

// markStepSkipped records that no branch worktree will be provisioned for this step run (non-repo
// project, or run has no bound project) so the DAG gate stops holding the branch child at ready.
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

// markStep applies a terminal worktree_status to a step run, folding a reason into the step run's
// result (best-effort — the status is the source of truth).
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
	// D3 (concurrency guards): a branch child's 'skipped' means "run in the run's cwd" — which, for a
	// non-repo project, is the shared project_dir.
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

// stepInPlaceAdmitted reports whether a branch child may be released into the in-place fallback
// ('skipped'): the child executes in its RUN's cwd.
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

// markStepPruned records that a terminal step run's branch worktree has been reaped.
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

// isDirtyWorkTree reports whether the git work tree at projectDir has uncommitted changes
// (modified/deleted) or untracked non-ignored files.
func (r *WorktreeReconciler) isDirtyWorkTree(ctx context.Context, projectDir string) bool {
	out, err := runGit(ctx, projectDir, "status", "--porcelain")
	if err != nil {
		// If git status fails, assume dirty to fail-closed (never dispatch
		// in-place on uncertainty).
		return true
	}
	return strings.TrimSpace(out) != ""
}

// restoreWorkTree restores a git checkout to a clean state: `git reset --hard HEAD` plus `git clean
// -fd` (not -fdx — preserve ignored build artifacts.
func (r *WorktreeReconciler) restoreWorkTree(ctx context.Context, projectDir string) error {
	// PRESERVE BEFORE DESTROYING, OR REFUSE.
	if out, err := runGit(ctx, projectDir, "stash", "push", "-u", "-m", stashMessage); err != nil {
		return fmt.Errorf("refusing to reset %s: could not preserve the working tree first "+
			"(git stash push -u): %w — %s", projectDir, err, strings.TrimSpace(out))
	} else {
		r.log.Info("worktree: preserved the working tree in a stash before restoring the checkout",
			"dir", projectDir, "stash", strings.TrimSpace(out))
	}
	if _, err := runGit(ctx, projectDir, "reset", "--hard", "HEAD"); err != nil {
		return fmt.Errorf("git reset --hard HEAD: %w", err)
	}
	if _, err := runGit(ctx, projectDir, "clean", "-fd"); err != nil {
		return fmt.Errorf("git clean -fd: %w", err)
	}
	out, err := runGit(ctx, projectDir, "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("checkout still dirty after restore")
	}
	if rest := strings.TrimSpace(stripLiveWorktreeResidue(ctx, projectDir, out)); rest != "" {
		return fmt.Errorf("checkout still dirty after restore")
	}
	return nil
}

// stashMessage names the stash entry restoreWorkTree creates before it resets a checkout.
const stashMessage = "orchicon: working tree preserved before the post-run checkout restore (safe to `git stash pop`)"

// stripLiveWorktreeResidue removes the `??
func stripLiveWorktreeResidue(ctx context.Context, projectDir, porcelain string) string {
	var keep []string
	for _, line := range strings.Split(porcelain, "\n") {
		f := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "??"))
		if f == worktreeDirName+"/" || f == worktreeDirName {
			if hasLiveWorktreeUnder(ctx, projectDir) {
				continue
			}
		}
		keep = append(keep, line)
	}
	return strings.Join(keep, "\n")
}

// hasLiveWorktreeUnder reports whether <projectDir>/.orchicon-worktrees/
// contains at least one directory that is still a registered git worktree.
func hasLiveWorktreeUnder(ctx context.Context, projectDir string) bool {
	out, err := runGit(ctx, projectDir, "worktree", "list", "--porcelain")
	if err != nil || strings.TrimSpace(out) == "" {
		return false
	}
	prefix := filepath.Join(projectDir, worktreeDirName) + string(os.PathSeparator)
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "worktree ") {
			if p := strings.TrimSpace(strings.TrimPrefix(line, "worktree ")); strings.HasPrefix(p, prefix) {
				return true
			}
		}
	}
	return false
}

// sweepOrphanDirs removes stale .orchicon-worktrees/<id> directories that are not valid git
// worktrees and have no DB row referencing them.
func (r *WorktreeReconciler) sweepOrphanDirs(ctx context.Context, tenantID string, deadline time.Time) {
	// Collect all projects with a project_dir so we sweep each repo once.
	ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return
	}
	// Use raw query to list projects with project_dir set — avoid loading all
	// project rows via ListProjects and filter in Go.
	rows, err := ttx.Tx.Query(ctx, `SELECT project_dir FROM projects WHERE tenant_id = $1 AND project_dir <> ''`, tenantID)
	if err != nil {
		ttx.Rollback(ctx)
		return
	}
	var dirs []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err == nil && d != "" {
			dirs = append(dirs, d)
		}
	}
	rows.Close()
	ttx.Rollback(ctx)
	seen := make(map[string]bool)
	for _, dir := range dirs {
		if sweepBudgetExpired(deadline) {
			r.log.Info("worktree: orphan dir sweep budget expired, resuming next slow pass")
			return
		}
		if seen[dir] {
			continue
		}
		seen[dir] = true
		if !r.isInsideWorkTree(ctx, dir) {
			continue
		}
		root := filepath.Join(dir, worktreeDirName)
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		// Build set of valid worktree paths from git worktree list.
		wts, _ := listWorktrees(ctx, dir)
		validPaths := make(map[string]bool, len(wts))
		for _, wt := range wts {
			validPaths[filepath.Clean(wt.path)] = true
		}
		// Also collect DB-referenced paths (run-level ready worktrees) to avoid
		// deleting a just-provisioned path that git hasn't registered yet.
		ttx2, err := r.pool.BeginTenantTx(ctx, tenantID)
		if err == nil {
			if readyRuns, err := r.listReadyRunPaths(ctx, ttx2.Tx, tenantID, dir); err == nil {
				for _, rp := range readyRuns {
					validPaths[filepath.Clean(rp)] = true
				}
			}
			ttx2.Rollback(ctx)
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), stagingPrefix) {
				continue
			}
			child := filepath.Join(root, e.Name())
			// BEFORE the registered-worktree skip: a leaked scratch checkout IS a
			// registered worktree, so that check skips exactly what needs reclaiming.
			if strings.HasPrefix(e.Name(), scratchBaselinePrefix) {
				r.reclaimStaleScratchDir(ctx, dir, child)
				continue
			}
			if validPaths[filepath.Clean(child)] {
				continue
			}
			// A run-namespaced path whose DB row is not ready (pruned/terminal) and not a registered worktree
			// is an orphan.
			if wt, _ := r.worktreeAt(ctx, dir, child); wt != nil {
				continue
			}
			// If it's an orchicon container with live children, skip.
			if fi, err := os.Stat(child); err == nil && fi.IsDir() {
				if isContainer, _ := r.isOrchiconContainer(ctx, dir, child); isContainer {
					if occupied, _ := r.dirOccupied(child); occupied {
						// Check if container has any orchicon-owned child that is
						// still registered — keep it.
						entries2, _ := os.ReadDir(child)
						hasLive := false
						for _, ce := range entries2 {
							nested := filepath.Join(child, ce.Name())
							if validPaths[filepath.Clean(nested)] {
								hasLive = true
								break
							}
							if wt2, _ := r.worktreeAt(ctx, dir, nested); wt2 != nil {
								hasLive = true
								break
							}
						}
						if hasLive {
							continue
						}
					}
				}
			}
			// Orphan dir — remove if it's a worktree artifact or empty, else leave (never delete arbitrary user
			// data).
			if r.isOrchiconWorktreeArtifact(child) {
				if err := r.removeWorktreeArtifact(child); err == nil {
					r.log.Info("worktree: orphan dir sweep removed artifact", "path", child)
				}
				continue
			}
			if empty, _ := dirEmpty(child); empty {
				if err := os.Remove(child); err == nil {
					r.log.Info("worktree: orphan dir sweep removed empty dir", "path", child)
				}
				continue
			}
			// For any other orphan directory, try to remove via git worktree prune
			// housekeeping; otherwise leave it for manual inspection.
			r.log.Warn("worktree: orphan dir sweep found stale dir (not artifact, not empty)", "path", child)
		}
	}
}

// reclaimStaleScratchDir removes a baseline capture's scratch checkout once no capture can still be
// running in it. Without this they survive forever: the sweep treats a REGISTERED worktree as valid.
func (r *WorktreeReconciler) reclaimStaleScratchDir(ctx context.Context, projectDir, path string) {
	fi, err := os.Stat(path)
	if err != nil {
		return
	}
	if time.Since(fi.ModTime()) < scratchBaselineMaxAge {
		return // young enough to belong to a capture that is still measuring
	}
	if _, err := runGit(ctx, projectDir, "worktree", "remove", "--force", path); err != nil {
		// Not registered (or git refuses the path): drop the tree and let prune clear the metadata.
		if rerr := os.RemoveAll(path); rerr != nil {
			r.log.Warn("worktree: leaked baseline scratch checkout could not be removed",
				"path", path, "error", rerr.Error())
			return
		}
	}
	_, _ = runGit(ctx, projectDir, "worktree", "prune")
	r.log.Info("worktree: reclaimed leaked baseline scratch checkout", "path", path)
}

// listReadyRunPaths returns worktree_path for runs in ready state whose project_dir matches the
// given dir (used to protect a just-provisioned path from the orphan dir sweep).
func (r *WorktreeReconciler) listReadyRunPaths(ctx context.Context, tx pgx.Tx, tenantID, projectDir string) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT worktree_path FROM workflow_runs WHERE tenant_id = $1 AND worktree_path <> '' AND worktree_status = 'ready'`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err == nil {
			// Only consider paths under this project_dir's worktree root.
			if strings.HasPrefix(p, projectDir+string(filepath.Separator)) {
				out = append(out, p)
			}
		}
	}
	return out, rows.Err()
}

// sweepSkippedTerminalRuns restores the shared checkout for any terminal skipped run (in-place
// fallback).
func (r *WorktreeReconciler) sweepSkippedTerminalRuns(ctx context.Context, tenantID string, deadline time.Time) {
	ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return
	}
	rows, err := ttx.Tx.Query(ctx, `SELECT id, project_id, worktree_status, status FROM workflow_runs WHERE tenant_id = $1 AND worktree_status = 'skipped' AND status IN ('completed','failed','aborted')`, tenantID)
	if err != nil {
		ttx.Rollback(ctx)
		return
	}
	type rec struct{ id, projectID string }
	var recs []rec
	for rows.Next() {
		var id, projectID, ws, st string
		if err := rows.Scan(&id, &projectID, &ws, &st); err == nil {
			recs = append(recs, rec{id, projectID})
		}
	}
	rows.Close()
	ttx.Rollback(ctx)
	for _, rec := range recs {
		if sweepBudgetExpired(deadline) {
			r.log.Info("worktree: skipped-terminal restore budget expired, resuming next slow pass")
			return
		}
		dir := r.lookupProjectDir(ctx, tenantID, rec.projectID)
		if dir == "" || !r.isInsideWorkTree(ctx, dir) {
			continue
		}
		if !r.isDirtyWorkTree(ctx, dir) {
			continue
		}
		r.log.Warn("worktree: restoring dirty checkout after skipped terminal run", "run", rec.id, "dir", dir)
		if err := r.restoreWorkTree(ctx, dir); err != nil {
			r.log.Warn("worktree: restore after skipped run failed", "run", rec.id, "error", err)
		} else {
			r.log.Info("worktree: restored checkout after skipped run", "run", rec.id)
		}
	}
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

// preflightErrorKind classifies a worktree-preflight failure for the UI, which groups and phrases on it rather
// than parsing prose. Deliberately a SMALL vocabulary with a catch-all: a new kind must not need a migration.
func preflightErrorKind(status, reason string) string {
	if status != domain.WorktreeFailed {
		return ""
	}
	switch {
	case strings.Contains(reason, "invalid reference") || strings.Contains(reason, "not a valid object name"):
		// The reported case: HEAD names a branch that does not exist, so there is nothing to branch from.
		return "worktree_no_base_ref"
	case strings.Contains(reason, "EMPTY tree"):
		return "worktree_empty_base"
	case strings.Contains(reason, "worktree"):
		return "worktree_preflight"
	}
	return "worktree_preflight"
}

// preflightRemedy turns a raw preflight failure into operator language, when the platform can be
// specific.
func preflightRemedy(status, reason string) string {
	if status != domain.WorktreeFailed {
		return ""
	}
	switch {
	case strings.Contains(reason, "invalid reference") || strings.Contains(reason, "not a valid object name"):
		return "This repository has no valid HEAD to branch from: git reports that the ref HEAD points at does " +
			"not exist. Create the branch it points at (or check one out), and re-run. The detail below names " +
			"the branch, the base ref and the directory the attempt used."
	case strings.Contains(reason, "EMPTY tree"):
		return "The branch this run would be cut from has no files, so a worktree from it would be empty. " +
			"Point the project at a branch with content (or commit something to it) and re-run."
	}
	return ""
}

// isReclaimableRunDir reports whether the directory at path is one this platform may delete during
// PROVISIONING: it is EMPTY.
func (r *WorktreeReconciler) isReclaimableRunDir(path string) bool {
	entries, err := os.ReadDir(path)
	if err != nil {
		return false
	}
	if len(entries) == 0 {
		return true
	}
	// Only a `.git` file, which is what a half-created worktree leaves.
	if len(entries) == 1 && entries[0].Name() == ".git" && !entries[0].IsDir() {
		return true
	}
	return false
}
