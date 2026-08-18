// WorktreeReconciler — the control loop that provisions each git-backed
// workflow run its own isolated working tree at arm time
// (architecture-notes/worktree-reconciler.md).
//
// At run-arm (the pending→running transition in WorkflowReconciler), a run
// that belongs to a git-backed project gets `git worktree add
// <project_dir>/.orchicon-worktrees/<runID> -b <branch> develop`, and the
// resulting path + branch are recorded on the workflow_runs row for
// downstream consumers (execution-cwd wiring, cleanup, non-repo fallback).
// The branch name is deterministic — the work item's kebab-case slug plus a
// short run suffix — so concurrent runs of the same item never collide.
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

// gitDetectionTTL bounds how long a cached git work-tree detection on the
// project row is trusted before it is re-detected (a directory that becomes
// a git repo is picked up within a TTL even without a project_dir change).
const gitDetectionTTL = 24 * time.Hour

// WorktreeReconciler implements reconciler.Reconciler for the "worktree"
// kind, keyed by workflow-run ID.
type WorktreeReconciler struct {
	pool *db.Pool
	log  *slog.Logger
}

// NewWorktreeReconciler creates a WorktreeReconciler.
func NewWorktreeReconciler(pool *db.Pool, log *slog.Logger) *WorktreeReconciler {
	return &WorktreeReconciler{pool: pool, log: log}
}

// Kind returns the reconciler kind (used for queue + leadership keying).
func (r *WorktreeReconciler) Kind() string { return "worktree" }

// Reconcile provisions a single run's working tree (key = run id), or
// scans for candidates when the key is empty. Idempotent: re-running a
// pass for a run converges to the same state (docs/03 §1).
func (r *WorktreeReconciler) Reconcile(ctx context.Context, key string) reconciler.Result {
	// v0.1: single dev tenant (mirrors TaskReconciler/WorkflowReconciler).
	tenantID := "tnt_dev"
	if key == "" {
		return r.scan(ctx, tenantID)
	}
	if err := r.reconcileOne(ctx, tenantID, key); err != nil {
		return reconciler.Result{Error: err}
	}
	return reconciler.Result{}
}

// scan lists un-provisioned pending/running runs and provisions each,
// then lists terminal runs with a recorded worktree and prunes each.
// Both passes go through the same idempotent per-run entry point
// (reconcileOne dispatches terminal runs to pruneOne). Batch-capped so
// one pass can't monopolize the reconciler goroutine (mirrors
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
	return reconciler.Result{}
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
		// The path exists but is not a registered worktree. If it is
		// clearly a partial git-worktree artifact of ours (a gitdir file
		// inside the run-namespaced dir while the row is 'pending'), prune
		// it; otherwise fail provisioning — never delete arbitrary user
		// directories.
		if run.WorktreeStatus == domain.WorktreePending && r.isOrchiconWorktreeArtifact(path) {
			if rerr := r.removeWorktreeArtifact(path); rerr != nil {
				return fmt.Errorf("worktree: prune partial artifact at %s: %w", path, rerr)
			}
		} else {
			return r.markFailed(ctx, tenantID, runID,
				fmt.Sprintf("path %s is occupied by a non-worktree directory", path))
		}
	}

	// Phase 4 — resolve the base ref and create.
	base := r.resolveBaseRef(ctx, projectDir)
	if err := r.addWorktree(ctx, projectDir, path, branch, base); err != nil {
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

// pruneOne reaps a terminal run's worktree: remove the working tree dir,
// run `git worktree prune` in the main repo, and record the result
// ('pruned') on the run row. Idempotent by construction — every step
// treats "already gone" as success, so reconcile retries converge:
//
//   - Non-'ready' runs (pending/skipped/failed/pruned) have nothing to
//     reap → no-op.
//   - A recorded worktree that no longer exists on disk (already removed,
//     or the repo plane is gone) is treated as already pruned → success.
//   - The branch is NEVER deleted: `git worktree remove` and
//     `git worktree prune` leave refs intact — branch deletion after merge
//     stays with the DevOps merge step.
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
	if _, uerr := db.UpdateProjectGitDetection(ctx, ttx.Tx, tenantID, projectID, proj.Version, isWT); uerr != nil {
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

// markPruned records that a terminal run's worktree has been reaped. The
// existing `mark` refuses terminal runs by design, so pruning needs its own
// writer. In one optimistic transaction it sets worktree_status='pruned'
// and clears worktree_path; worktree_branch is KEPT so the DevOps merge
// step knows which branch the run used (branch deletion stays there — the
// control plane never deletes a branch).
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
