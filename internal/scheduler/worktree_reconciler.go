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
			return nil
		}
	case domain.WorktreeSkipped, domain.WorktreeFailed, domain.WorktreePruned:
		// Recorded decisions are respected: a skipped, failed or pruned run
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

	return r.recordReady(ctx, tenantID, runID, path, branch)
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
			return nil
		}
	case domain.WorktreeSkipped, domain.WorktreeFailed, domain.WorktreePruned:
		// Recorded decisions are respected: a skipped, failed or pruned
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

	// AC: on run SUCCESS only, delete the branch the reconciler provably
	// created for this step run. Never on failure/cancellation — a failed
	// run keeps its step branches so a retry re-attaches to them. Gated on
	// the branch being merged into the base, never the current branch, and
	// never main/develop.
	if run.Status == domain.WorkflowRunCompleted {
		if err := r.deleteBranch(ctx, projectDir, sr.WorktreeBranch); err != nil {
			r.log.Warn("worktree: branch worktree branch deletion failed",
				"run", sr.WorkflowRunID, "step_run", sr.ID, "branch", sr.WorktreeBranch, "error", err)
		}
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

	// AC: on SUCCESS only, delete the branch the reconciler provably created
	// for this run. The branch is never deleted on failure/cancellation — a
	// failed run keeps its branch so a retry re-attaches to it (carry-over of
	// partial work). Deletion is gated on the branch being merged into the
	// base, never the current branch, and never main/develop.
	if run.Status == domain.WorkflowRunCompleted {
		if err := r.deleteBranch(ctx, projectDir, run.WorktreeBranch); err != nil {
			r.log.Warn("worktree: branch deletion failed", "run", run.ID, "branch", run.WorktreeBranch, "error", err)
		}
	}

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
	return "_stage-" + child
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
// merged. The reconciler never deletes a branch whose work is not merged.
func (r *WorktreeReconciler) branchMergedIntoBase(ctx context.Context, projectDir, branch string) bool {
	for _, ref := range []string{"develop", "origin/develop"} {
		if _, err := runGit(ctx, projectDir, "merge-base", "--is-ancestor", branch, ref); err == nil {
			return true
		}
	}
	return false
}

// deleteBranch deletes a branch ref after a successful run, gated on the
// branch being provably ours (the deterministic name recorded on the row),
// not protected, not the current branch, and merged into the base. Idempotent
// by construction: a missing branch is success.
func (r *WorktreeReconciler) deleteBranch(ctx context.Context, projectDir, branch string) error {
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
	if !r.branchMergedIntoBase(ctx, projectDir, branch) {
		r.log.Warn("worktree: refusing to delete unmerged branch", "branch", branch)
		return nil
	}
	if _, err := runGit(ctx, projectDir, "branch", "-D", branch); err != nil {
		return fmt.Errorf("git branch -D %s: %w", branch, err)
	}
	r.log.Info("worktree: deleted branch after successful run", "branch", branch)
	return nil
}

// --- row state transitions (each a short optimistic tx) --------------------

// recordReady records the provisioned worktree on the run row. The update
// is guarded by optimistic concurrency and a non-terminal status check so a
// run that terminalized mid-provision is not spuriously touched (the
// worktree still exists for the cleanup companion to reap).
func (r *WorktreeReconciler) recordReady(ctx context.Context, tenantID, runID, path, branch string) error {
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
	// Already provisioned by a concurrent pass — converge, don't fight.
	if run.WorktreeStatus == domain.WorktreeReady {
		return ttx.Commit(ctx)
	}
	if _, err := db.UpdateWorkflowRun(ctx, ttx.Tx, tenantID, runID, run.Version, db.UpdateWorkflowRunFields{
		WorktreeStatus: strPtr(domain.WorktreeReady),
		WorktreePath:   strPtr(path),
		WorktreeBranch: strPtr(branch),
	}); err != nil {
		return fmt.Errorf("record worktree ready: %w", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	r.log.Info("worktree provisioned", "run", runID, "path", path, "branch", branch)
	return nil
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
	// Already provisioned by a concurrent pass — converge, don't fight.
	if sr.WorktreeStatus == domain.WorktreeReady {
		return ttx.Commit(ctx)
	}
	if isTerminalStepRun(sr) {
		return nil
	}
	if _, err := db.UpdateWorkflowStepRun(ctx, ttx.Tx, tenantID, stepRunID, sr.Version, db.UpdateWorkflowStepRunFields{
		WorktreeStatus: strPtr(domain.WorktreeReady),
		WorktreePath:   strPtr(path),
		WorktreeBranch: strPtr(branch),
	}); err != nil {
		return fmt.Errorf("record step worktree ready: %w", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	r.log.Info("worktree: branch worktree provisioned", "step_run", stepRunID, "path", path, "branch", branch)
	return nil
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
