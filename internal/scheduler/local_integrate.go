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

// defaultIntegrationBranch is the HISTORICAL convention: where every run used to land unconditionally.
//
// It is now only the PREFERRED candidate, never an assumption. A trunk-based repo (`main` only), a release
// scheme (`release/x`) or an environment split (`dev`/`staging`) has an integration branch this name does not
// match — and the old code did not merely get those wrong, it CREATED `develop` at HEAD and merged there,
// silently forking a repository's history into a branch the operator never asked for. Inventing a branch is
// worse than refusing.
const defaultIntegrationBranch = "develop"

// integrationTarget resolves the branch runs land on.
//
// ORDER:
//  1. `develop` IF IT EXISTS — byte-identical to the previous behaviour for every repository that relies on
//     the convention, and the name the worker prompts and branchMergedIntoBase use.
//  2. OTHERWISE THE REPOSITORY'S OWN DEFAULT, via resolveBaseRefWithProvenance (origin/HEAD symref, then
//     `remote show origin`, then the configured candidates, then HEAD). Using that chain is what makes
//     INTEGRATION agree with PROVISIONING instead of contradicting it: the base a run branch was cut from is
//     the branch it should land on.
//  3. OTHERWISE REFUSE. A missing integration branch is reported, never created.
func (r *WorktreeReconciler) integrationTarget(ctx context.Context, projectDir string) (string, error) {
	if r.branchExists(ctx, projectDir, defaultIntegrationBranch) {
		return defaultIntegrationBranch, nil
	}
	ref, how, err := r.resolveBaseRefWithProvenance(ctx, projectDir)
	if err != nil || strings.TrimSpace(ref) == "" {
		return "", fmt.Errorf("no integration branch: %q does not exist and the repository's default could not "+
			"be resolved (%v) — refusing to create one", defaultIntegrationBranch, err)
	}
	name := strings.TrimPrefix(ref, "origin/")
	if name == "" || name == "HEAD" {
		return "", fmt.Errorf("no integration branch: %q does not exist and the resolved default %q is not a "+
			"usable branch name", defaultIntegrationBranch, ref)
	}
	if !r.branchExists(ctx, projectDir, name) {
		return "", fmt.Errorf("no integration branch: %q does not exist and the resolved default %q (from %s) "+
			"has no LOCAL branch — refusing to create one", defaultIntegrationBranch, name, how)
	}
	r.log.Info("worktree: integration target resolved from the repository's default",
		"branch", name, "source", how, "repo", projectDir)
	return name, nil
}

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
		// Not this path's job — and it never will be, so stamp it rather than re-reading the project every
		// pass for the rest of the instance's life.
		r.stampIntegrated(ctx, tenantID, run.ID, "not a local-strategy project")
		return nil
	}
	projectDir := r.lookupProjectDir(ctx, tenantID, run.ProjectID)
	if projectDir == "" || !r.isInsideWorkTree(ctx, projectDir) {
		// Not a git-backed project (or a main repo that cannot be resolved). Nothing can land, and it will
		// not resolve itself by being retried, so stamp with the reason instead of rescanning forever.
		r.stampIntegrated(ctx, tenantID, run.ID, "project dir is not a git work tree")
		return nil
	}
	if !r.branchExists(ctx, projectDir, branch) {
		// The branch is gone: already merged and deleted by the orphan sweep, or never created (a run whose
		// provisioning failed). Either way there is nothing to integrate and nothing to wait for. SAID OUT
		// LOUD, because "the branch this run recorded does not exist" is a finding an operator wants, not a
		// silent no-op — the reported run 1 recorded a branch that was never created at all.
		r.log.Warn("worktree: local integration found NO branch for a completed run — nothing to integrate",
			"run", run.ID, "branch", branch, "repo", projectDir,
			"hint", "the branch was already merged and swept, or the run never created it")
		r.stampIntegrated(ctx, tenantID, run.ID, "recorded branch does not exist")
		return nil
	}
	// RESOLVE THE TARGET BEFORE ANY MUTATION, and REFUSE rather than invent one.
	target, terr := r.integrationTarget(ctx, projectDir)
	if terr != nil {
		// NOT STAMPED: an unresolvable integration branch is a repository/configuration fact that CAN change
		// without this platform acting — the operator creates the branch, or fixes origin/HEAD — so the next
		// pass must try again. Logged on every pass for the same reason: silence is what hid this before.
		r.log.Warn("worktree: local integration cannot run — the repository has no usable integration branch",
			"run", run.ID, "branch", branch, "repo", projectDir, "error", terr)
		return nil
	}

	// IDEMPOTENCY. Ancestry is the test, so "already integrated" needs no stored flag.
	if r.isAncestorOf(ctx, projectDir, branch, target) {
		r.stampIntegrated(ctx, tenantID, run.ID, "already an ancestor of "+target)
		return nil
	}

	// SERIALIZE. The reconciler's scan is already a single goroutine, but this makes the property explicit
	// and survives any future fan-out: two merges into one ref is a lost update.
	r.integrateMu.Lock()
	defer r.integrateMu.Unlock()

	// Re-check under the lock: a concurrent pass may have integrated it while we waited.
	if r.isAncestorOf(ctx, projectDir, branch, target) {
		return nil
	}

	// WHETHER THE MAIN CHECKOUT MAY BE REFRESHED MUST BE DECIDED **BEFORE** THE REF MOVES.
	//
	// After `update-ref` the index is necessarily stale relative to the new HEAD, so `git status` reports a
	// difference for every file the merge brought in — a check made afterwards can never pass. Captured
	// here, while the tree still describes the commit it is actually on, "clean" means what it should:
	// the operator has no uncommitted work to lose.
	readyToRefresh, refreshBlockedBy := r.mainCheckoutRefreshReadiness(ctx, projectDir, target)

	// FAST-FORWARD when the integration branch is an ancestor of the run branch: the common case, and the
	// only one that needs no merge commit.
	if r.isAncestorOf(ctx, projectDir, target, branch) {
		sha, err := r.revParse(ctx, projectDir, branch)
		if err != nil {
			return err
		}
		if err := r.setBranchRef(ctx, projectDir, target, sha); err != nil {
			return err
		}
		r.log.Info("worktree: local integration fast-forwarded the integration branch",
			"run", run.ID, "branch", branch, "into", target, "commit", short(sha))
		r.stampIntegrated(ctx, tenantID, run.ID, "fast-forwarded to "+short(sha))
		return r.refreshMainCheckout(ctx, projectDir, target, readyToRefresh, refreshBlockedBy)
	}

	// DIVERGED: the integration branch has moved since this run branched. Merge the run branch in, in a
	// SCRATCH WORKTREE, so the operator's checked-out tree is never the scene of a conflict.
	merged, err := r.mergeRunBranchInScratch(ctx, projectDir, branch, run.ID, target)
	if err != nil {
		return err
	}
	if merged == "" {
		// A CONFLICT. A controller cannot resolve one SEMANTICALLY — it can only pick a side, which is
		// worse than stopping. Both branches are left exactly as they are, nothing is committed, and the
		// operator gets a line naming the run; a human decides.
		// NOT STAMPED, deliberately: a conflict is the one outcome that CAN change without this platform
		// doing anything, because a human resolving it makes the next pass succeed. Stamping here would
		// strand the run permanently.
		r.log.Warn("worktree: local integration hit a CONFLICT — nothing was merged and both branches are "+
			"untouched; resolve it by hand, and the next pass will integrate it",
			"run", run.ID, "branch", branch, "into", target, "repo", projectDir)
		return nil
	}
	r.log.Info("worktree: local integration merged the run branch",
		"run", run.ID, "branch", branch, "into", target, "commit", short(merged))
	r.stampIntegrated(ctx, tenantID, run.ID, "merged as "+short(merged))
	return r.refreshMainCheckout(ctx, projectDir, target, readyToRefresh, refreshBlockedBy)
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

// mergeRunBranchInScratch merges branch into the integration branch inside a throwaway worktree, returning
// the resulting commit — or "" when the merge conflicted (aborted, nothing changed).
//
// A SCRATCH WORKTREE, not the main checkout, and not the run's worktree: the main checkout is the
// operator's (a conflict there is a mess they did not ask for) and the run worktree belongs to the run
// (merging into it would rewrite the very branch we are integrating).
func (r *WorktreeReconciler) mergeRunBranchInScratch(ctx context.Context, projectDir, branch, runID, target string) (string, error) {
	base, err := r.revParse(ctx, projectDir, target)
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

	msg := fmt.Sprintf("merge %s into %s (orchicon local integration, run %s)", branch, target, runID)
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
	if err := r.setBranchRef(ctx, projectDir, target, tip); err != nil {
		return "", err
	}
	return tip, nil
}

// mainCheckoutRefreshReadiness reports whether the operator's working tree may safely be brought up to
// date after the integration branch moves, and — when it may not — WHY, so the log line can name it.
//
// MUST BE CALLED BEFORE THE REF MOVES (see integrateLocalRun): once the ref advances, the index is stale by
// construction and every file the integration brought in looks like a local modification.
func (r *WorktreeReconciler) mainCheckoutRefreshReadiness(ctx context.Context, projectDir, target string) (bool, string) {
	if cur := r.currentBranch(ctx, projectDir); cur != target {
		// The operator is not on the branch that moved, so its working tree is unaffected: nothing to do,
		// and nothing to warn about.
		return false, ""
	}
	status, err := runGit(ctx, projectDir, "status", "--porcelain")
	if err != nil {
		return false, "git status could not be read"
	}
	if stale, why := staleCheckoutOnly(status); stale {
		// THE TREE LAGS ITS OWN HEAD, WHICH IS NOT UNCOMMITTED WORK. Reported: "develop's ref is 066279b, but
		// C:\projects\weatherapp contains only .git, .orchicon, .orchicon-worktrees — no src/, no index.html".
		// A tree left behind by an earlier ref move reports every file HEAD has as DELETED, which the old check
		// read as "uncommitted changes" and therefore refused to fix — so the staleness was permanent.
		//
		// Refreshing is safe here precisely because there is nothing to lose: every dirty entry is a file git
		// already has at HEAD, absent from the working tree only because the tree is older. The moment there is
		// a MODIFICATION or an UNTRACKED file — where real work lives — this does not apply and the tree is
		// left alone. (`why` is carried for the log line.)
		r.log.Info("worktree: the main checkout lags its own HEAD — refreshing is safe, nothing is uncommitted",
			"repo", projectDir, "shape", why)
		return true, ""
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
func (r *WorktreeReconciler) refreshMainCheckout(ctx context.Context, projectDir, target string, ready bool, blockedBy string) error {
	if blockedBy != "" {
		r.log.Warn("worktree: the integration branch advanced but the main checkout was left alone — it is "+
			"now stale relative to its own HEAD",
			"repo", projectDir, "branch", target, "reason", blockedBy,
			"remedy", "commit or stash, then run: git reset --hard "+target)
		return nil
	}
	if !ready {
		return nil // the operator is not on the integration branch
	}
	if _, err := runGit(ctx, projectDir, "reset", "--hard", target); err != nil {
		r.log.Warn("worktree: could not refresh the main checkout after integration",
			"repo", projectDir, "branch", target, "error", err)
		return nil
	}
	r.log.Info("worktree: refreshed the main checkout to the integration branch", "repo", projectDir, "branch", target)
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
	// A BRANCH MAY ONLY MOVE FORWARD. Callers already check ancestry, but the invariant belongs HERE: this is
	// the only function in the platform that moves a branch ref without a working tree, and "never lose
	// commits" should not depend on every future caller remembering to guard it.
	//
	// The shape this prevents is specific and was reported: a run provisioned at an EMPTY ROOT commit while
	// the integration branch was already ahead of it. Moving the branch to that tip would rewind it — the
	// integration branch would lose every commit and its tree would come back empty, while every run's
	// branch still held the work. `git update-ref` does that happily; it has no notion of direction.
	if current, err := r.revParse(ctx, projectDir, "refs/heads/"+branch); err == nil && current != "" {
		if current != sha && !r.isAncestorOf(ctx, projectDir, current, sha) {
			return fmt.Errorf("worktree: refusing to move %s from %s to %s — the target %s is not a descendant "+
				"of the current tip, so this would REWIND the branch and lose commits",
				branch, short(current), short(sha), short(sha))
		}
	}
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

// integrationStampKey is the run_context key recording that local integration has dealt with a run.
const integrationStampKey = "local_integrated_at"

// stampIntegrated records that this run needs no further integration attention, so the candidate query stops
// returning it. Best-effort: a failed stamp costs a re-examination next pass, which is a no-op — never the
// other way round, so a failure here is logged and nothing else.
func (r *WorktreeReconciler) stampIntegrated(ctx context.Context, tenantID, runID, why string) {
	ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		r.log.Warn("worktree: local integration stamp: begin tx", "run", runID, "error", err)
		return
	}
	defer ttx.Rollback(ctx)
	run, err := db.GetWorkflowRun(ctx, ttx.Tx, tenantID, runID)
	if err != nil {
		r.log.Warn("worktree: local integration stamp: read run", "run", runID, "error", err)
		return
	}
	merged, ok := db.MergeRunContext(run.RunContext, map[string]any{
		integrationStampKey:     time.Now().UTC().Format(time.RFC3339),
		"local_integrated_note": why,
	})
	if !ok {
		return
	}
	if _, err := db.UpdateWorkflowRun(ctx, ttx.Tx, tenantID, runID, run.Version,
		db.UpdateWorkflowRunFields{RunContext: &merged}); err != nil {
		r.log.Warn("worktree: local integration stamp: write run", "run", runID, "error", err)
		return
	}
	if err := ttx.Commit(ctx); err != nil {
		r.log.Warn("worktree: local integration stamp: commit", "run", runID, "error", err)
	}
}

// staleCheckoutOnly reports whether a `git status --porcelain` result consists ONLY of tracked files MISSING
// from the working tree — the exact shape a ref that moved under its own checkout produces.
//
// WHAT IT REFUSES, and why the line is drawn there:
//
//   - `??` UNTRACKED — user data that exists nowhere else. Never touched.
//   - ` M` / `MM` MODIFIED — a real edit, even if it happens to resemble a checkout shape. Never touched.
//   - `A ` / `R ` / staged changes — an index that disagrees with HEAD on purpose. Never touched.
//
// WHAT IT ACCEPTS: every line is ` D` (deleted in the working tree) or `D ` on a path that HEAD still has. That
// is unrecoverable-work-free by construction: the content is in git, and `git reset --hard` restores it.
//
// AN EMPTY STATUS IS NOT "STALE" — it is clean, and the caller handles it.
func staleCheckoutOnly(status string) (bool, string) {
	lines := strings.Split(strings.TrimRight(status, "\n"), "\n")
	if len(lines) == 0 || strings.TrimSpace(status) == "" {
		return false, ""
	}
	deleted := 0
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if len(line) < 3 {
			return false, ""
		}
		x, y := line[0], line[1]
		switch {
		case x == '?' && y == '?':
			return false, "the working tree has untracked files"
		case x == ' ' && y == 'D':
			// Deleted in the working tree, present at HEAD: the stale shape.
			deleted++
		default:
			return false, "the working tree has staged or modified changes"
		}
	}
	if deleted == 0 {
		return false, ""
	}
	return true, "tracked files missing from the working tree"
}
