package scheduler

// local_integrate.go — LOCAL INTEGRATION: finishing the job a `git_strategy=local` worker cannot.
//
// THE PROBLEM. A local-strategy project has no remote, so there is no push and no PR. The worker commits to
// its run branch and reports success — and the work then sits on a branch nobody reads. "A commit sitting
// only on a run branch is unfinished." Repeated runs make it worse: each one branches from wherever it
// started, and the integration branch never advances.
//
// WHY THE PLATFORM DOES THIS AND NOT THE PROMPTS, which is the design decision this file embodies. Teaching
// every canned worker to merge was the alternative, and it loses on three counts that are not stylistic:
//
//  1. IT CANNOT REFRESH THE MAIN CHECKOUT. `develop` is checked out in the operator's own working tree, and
//     moving the ref under it leaves that tree's FILES stale relative to its own HEAD — the operator's next
//     `git status` reports the whole tree as reverted. A worker inside a run worktree cannot see that
//     problem, let alone fix it. Only the controller knows about the main checkout.
//  2. IT RACES. Two workers merging into one ref is a lost update. Serializing it needs a lock, and a lock
//     shared across processes is exactly what a single controller already is.
//  3. IT IS N GIT IMPLEMENTATIONS. Divergence, fast-forward, conflict, and the stale-checkout case would be
//     re-derived in every canned prompt, and they would drift.
//
// It is also cheaper than it looks: integration makes the run branch PROVABLY MERGED, so the existing
// prune path (branchProvablyMerged → ancestry) then deletes the branch and marks the worktree pruned
// without any new bookkeeping. This file adds the missing half; it does not duplicate the other one.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
)

// integrationBranch is where every run lands. It matches branchMergedIntoBase and the worker prompts, so
// the three cannot disagree about the name of the integration branch.
const integrationBranch = "develop"

// localIntegrationLimit bounds how many candidate runs one slow pass examines. The work is a couple of git
// subprocesses per run, and the set is self-cleaning (see db.ListCompletedRunsWithBranch), so a small page
// is enough to converge.
const localIntegrationLimit = 16

// integrateLocalRuns advances the integration branch for completed local-strategy runs.
//
// IDEMPOTENT BY CONSTRUCTION: the ancestry check below is the idempotency key, so a run whose branch is
// already an ancestor of the integration branch costs one `git merge-base` and nothing else. No marker, no
// migration, and no possibility of the marker and git disagreeing.
func (r *WorktreeReconciler) integrateLocalRuns(ctx context.Context, tenantID string, deadline time.Time) {
	ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		r.log.Warn("worktree: local integration begin tx failed", "error", err)
		return
	}
	runs, err := db.ListCompletedRunsWithBranch(ctx, ttx.Tx, tenantID, localIntegrationLimit)
	ttx.Rollback(ctx)
	if err != nil {
		r.log.Warn("worktree: local integration list runs failed", "error", err)
		return
	}
	for _, run := range runs {
		if sweepBudgetExpired(deadline) {
			r.log.Info("worktree: local integration budget expired, resuming next slow pass")
			return
		}
		if err := r.integrateLocalRun(ctx, tenantID, run); err != nil {
			// A FAILED INTEGRATION IS NOT A FAILED RUN. The work is committed and safe on its branch; the
			// branch is simply not merged yet, and the next pass retries. Saying so at Warn (naming the
			// run and the branch) is what stops this from being another silent no-op.
			r.log.Warn("worktree: local integration failed — the run's branch is intact and will be retried",
				"run", run.ID, "branch", run.WorktreeBranch, "error", err)
		}
	}
}

// integrateLocalRun advances the integration branch to include one run's branch.
func (r *WorktreeReconciler) integrateLocalRun(ctx context.Context, tenantID string, run db.WorkflowRunRow) error {
	branch := strings.TrimSpace(run.WorktreeBranch)
	if branch == "" || isProtectedBranch(branch) {
		return nil
	}
	// ONLY A LOCAL-STRATEGY PROJECT INTEGRATES HERE. `pr` projects hand off to GitHub and `none` projects
	// have no branch at all; doing this for either would be acting on a decision the operator did not make.
	strategy, err := r.runGitStrategy(ctx, tenantID, run)
	if err != nil {
		return err
	}
	if strategy != "local" {
		return nil
	}
	projectDir := r.lookupProjectDir(ctx, tenantID, run.ProjectID)
	if projectDir == "" || !r.isInsideWorkTree(ctx, projectDir) {
		return nil // not a git-backed project (or a main repo that cannot be resolved)
	}
	if !r.branchExists(ctx, projectDir, branch) {
		return nil // already gone — the branch was merged and pruned, or never created
	}
	// IDEMPOTENCY. Ancestry is the test, so "already integrated" needs no stored flag.
	if r.isAncestorOf(ctx, projectDir, branch, integrationBranch) {
		return nil
	}

	// SERIALIZE. The reconciler's scan is already a single goroutine, but this makes the property explicit
	// and survives any future fan-out: two merges into one ref is a lost update.
	r.integrateMu.Lock()
	defer r.integrateMu.Unlock()

	// Re-check under the lock: a concurrent pass may have integrated it while we waited.
	if r.isAncestorOf(ctx, projectDir, branch, integrationBranch) {
		return nil
	}

	// WHETHER THE MAIN CHECKOUT MAY BE REFRESHED MUST BE DECIDED **BEFORE** THE REF MOVES.
	//
	// After `update-ref` the index is necessarily stale relative to the new HEAD, so `git status` reports a
	// difference for every file the merge brought in — a check made afterwards can never pass. Captured
	// here, while the tree still describes the commit it is actually on, "clean" means what it should:
	// the operator has no uncommitted work to lose.
	readyToRefresh, refreshBlockedBy := r.mainCheckoutRefreshReadiness(ctx, projectDir)

	// The integration branch must exist before anything can land on it. Creating it at the MAIN checkout's
	// HEAD preserves the history that already exists there, rather than adopting the first run's branch as
	// the base.
	if err := r.ensureIntegrationBranch(ctx, projectDir); err != nil {
		return err
	}

	// FAST-FORWARD when the integration branch is an ancestor of the run branch: the common case, and the
	// only one that needs no merge commit.
	if r.isAncestorOf(ctx, projectDir, integrationBranch, branch) {
		sha, err := r.revParse(ctx, projectDir, branch)
		if err != nil {
			return err
		}
		if err := r.setBranchRef(ctx, projectDir, integrationBranch, sha); err != nil {
			return err
		}
		r.log.Info("worktree: local integration fast-forwarded the integration branch",
			"run", run.ID, "branch", branch, "into", integrationBranch, "commit", short(sha))
		return r.refreshMainCheckout(ctx, projectDir, readyToRefresh, refreshBlockedBy)
	}

	// DIVERGED: the integration branch has moved since this run branched. Merge the run branch in, in a
	// SCRATCH WORKTREE, so the operator's checked-out tree is never the scene of a conflict.
	merged, err := r.mergeRunBranchInScratch(ctx, projectDir, branch, run.ID)
	if err != nil {
		return err
	}
	if merged == "" {
		// A CONFLICT. A controller cannot resolve one SEMANTICALLY — it can only pick a side, which is
		// worse than stopping. Both branches are left exactly as they are, nothing is committed, and the
		// operator gets a line naming the run; a human decides.
		r.log.Warn("worktree: local integration hit a CONFLICT — nothing was merged and both branches are "+
			"untouched; resolve it by hand",
			"run", run.ID, "branch", branch, "into", integrationBranch, "repo", projectDir)
		return nil
	}
	r.log.Info("worktree: local integration merged the run branch",
		"run", run.ID, "branch", branch, "into", integrationBranch, "commit", short(merged))
	return r.refreshMainCheckout(ctx, projectDir, readyToRefresh, refreshBlockedBy)
}

// runGitStrategy resolves the run's effective git strategy.
func (r *WorktreeReconciler) runGitStrategy(ctx context.Context, tenantID string, run db.WorkflowRunRow) (string, error) {
	ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return "", err
	}
	defer ttx.Rollback(ctx)
	return db.EffectiveGitStrategy(ctx, ttx.Tx, tenantID, run.WorkflowID, run.ProjectID), nil
}

// ensureIntegrationBranch creates the integration branch at the main checkout's HEAD when it is missing.
func (r *WorktreeReconciler) ensureIntegrationBranch(ctx context.Context, projectDir string) error {
	if _, err := runGit(ctx, projectDir, "rev-parse", "--verify", "refs/heads/"+integrationBranch); err == nil {
		return nil
	}
	head, err := r.revParse(ctx, projectDir, "HEAD")
	if err != nil {
		// A repository with no commits cannot host an integration branch yet, and the run branch is the
		// only history that exists — adopt it so the run's work is not stranded.
		return fmt.Errorf("worktree: cannot bootstrap %s (no HEAD commit): %w", integrationBranch, err)
	}
	if err := r.setBranchRef(ctx, projectDir, integrationBranch, head); err != nil {
		return err
	}
	r.log.Info("worktree: created the integration branch", "branch", integrationBranch, "at", short(head))
	return nil
}

// mergeRunBranchInScratch merges branch into the integration branch inside a throwaway worktree, returning
// the resulting commit — or "" when the merge conflicted (aborted, nothing changed).
//
// A SCRATCH WORKTREE, not the main checkout, and not the run's worktree: the main checkout is the
// operator's (a conflict there is a mess they did not ask for) and the run worktree belongs to the run
// (merging into it would rewrite the very branch we are integrating).
func (r *WorktreeReconciler) mergeRunBranchInScratch(ctx context.Context, projectDir, branch, runID string) (string, error) {
	base, err := r.revParse(ctx, projectDir, integrationBranch)
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp("", "orchicon-integrate-")
	if err != nil {
		return "", fmt.Errorf("worktree: scratch dir: %w", err)
	}
	defer os.RemoveAll(dir)
	scratch := filepath.Join(dir, "merge")

	// --detach at the integration branch: git refuses to check out a branch that another worktree holds,
	// and the operator's checkout almost certainly holds this one.
	if _, err := runGit(ctx, projectDir, "worktree", "add", "--detach", scratch, base); err != nil {
		return "", fmt.Errorf("worktree: scratch worktree at %s: %w", base, err)
	}
	defer func() {
		if _, derr := runGit(ctx, projectDir, "worktree", "remove", "--force", scratch); derr != nil {
			r.log.Warn("worktree: could not remove the integration scratch worktree", "path", scratch, "error", derr)
		}
	}()

	msg := fmt.Sprintf("merge %s into %s (orchicon local integration, run %s)", branch, integrationBranch, runID)
	if _, merr := runGit(ctx, scratch, "merge", "--no-ff", "-m", msg, branch); merr != nil {
		// ABORT so the scratch worktree holds no half-applied merge, then report "conflict" rather than an
		// error: the branches are untouched and retrying cannot help until a human intervenes.
		if _, aerr := runGit(ctx, scratch, "merge", "--abort"); aerr != nil {
			r.log.Warn("worktree: local integration could not abort a conflicted merge", "path", scratch, "error", aerr)
		}
		return "", nil
	}
	tip, err := r.revParse(ctx, scratch, "HEAD")
	if err != nil {
		return "", err
	}
	if err := r.setBranchRef(ctx, projectDir, integrationBranch, tip); err != nil {
		return "", err
	}
	return tip, nil
}

// mainCheckoutRefreshReadiness reports whether the operator's working tree may safely be brought up to
// date after the integration branch moves, and — when it may not — WHY, so the log line can name it.
//
// MUST BE CALLED BEFORE THE REF MOVES (see integrateLocalRun): once the ref advances, the index is stale by
// construction and every file the integration brought in looks like a local modification.
func (r *WorktreeReconciler) mainCheckoutRefreshReadiness(ctx context.Context, projectDir string) (bool, string) {
	if cur := r.currentBranch(ctx, projectDir); cur != integrationBranch {
		// The operator is not on the branch that moved, so its working tree is unaffected: nothing to do,
		// and nothing to warn about.
		return false, ""
	}
	status, err := runGit(ctx, projectDir, "status", "--porcelain")
	if err != nil {
		return false, "git status could not be read"
	}
	if strings.TrimSpace(status) != "" {
		return false, "the operator's main checkout has UNCOMMITTED changes"
	}
	return true, ""
}

// refreshMainCheckout makes the operator's working tree match the integration branch after its ref moved.
//
// THIS IS THE HAZARD THE PROMPT APPROACH COULD NOT ADDRESS. `develop` is checked out in the main worktree,
// so advancing the ref leaves that tree's FILES at the old commit while its HEAD says otherwise — the
// operator's next `git status` reports every file as reverted. Only the controller knows about this
// checkout; a worker inside a run worktree cannot see the problem, let alone fix it.
//
// Uncommitted work is NEVER touched. Destroying it to tidy up a ref is not a trade worth making, so a tree
// that was dirty before the integration is left exactly as it was and the operator is told, with the
// command that resolves it.
func (r *WorktreeReconciler) refreshMainCheckout(ctx context.Context, projectDir string, ready bool, blockedBy string) error {
	if blockedBy != "" {
		r.log.Warn("worktree: the integration branch advanced but the main checkout was left alone — it is "+
			"now stale relative to its own HEAD",
			"repo", projectDir, "branch", integrationBranch, "reason", blockedBy,
			"remedy", "commit or stash, then run: git reset --hard "+integrationBranch)
		return nil
	}
	if !ready {
		return nil // the operator is not on the integration branch
	}
	if _, err := runGit(ctx, projectDir, "reset", "--hard", integrationBranch); err != nil {
		r.log.Warn("worktree: could not refresh the main checkout after integration",
			"repo", projectDir, "branch", integrationBranch, "error", err)
		return nil
	}
	r.log.Info("worktree: refreshed the main checkout to the integration branch", "repo", projectDir)
	return nil
}

// isAncestorOf reports whether ancestor is reachable from descendant.
func (r *WorktreeReconciler) isAncestorOf(ctx context.Context, projectDir, ancestor, descendant string) bool {
	_, err := runGit(ctx, projectDir, "merge-base", "--is-ancestor", ancestor, descendant)
	return err == nil
}

// setBranchRef moves a local branch ref WITHOUT touching any working tree, which is what makes it safe to
// do while the main checkout has that branch checked out. Callers have already established the ancestry
// relationship, so this cannot rewind a branch.
func (r *WorktreeReconciler) setBranchRef(ctx context.Context, projectDir, branch, sha string) error {
	if _, err := runGit(ctx, projectDir, "update-ref", "refs/heads/"+branch, sha); err != nil {
		return fmt.Errorf("worktree: update %s to %s: %w", branch, short(sha), err)
	}
	return nil
}

func (r *WorktreeReconciler) revParse(ctx context.Context, dir, ref string) (string, error) {
	out, err := runGit(ctx, dir, "rev-parse", ref)
	if err != nil {
		return "", fmt.Errorf("worktree: rev-parse %s: %w", ref, err)
	}
	return strings.TrimSpace(out), nil
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
