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
// subprocesses per pending record, and the set is self-cleaning (a settled record leaves the queue), so a
// small page is enough to converge.
const localIntegrationLimit = 16

// localIntegrationPerRunBudget bounds the integration pass run immediately before a new branch is cut.
//
// Tighter than the sweep's budget on purpose: this sits on the PROVISIONING path, and a run must not wait
// behind a backlog of other projects' integrations to get its worktree. Anything it does not reach, the
// periodic sweep still will.
const localIntegrationPerRunBudget = 5 * time.Second

// integrateLocalRuns is the PERIODIC backstop: drain the queue for every project that has pending records.
//
// It used to select CANDIDATE RUNS tenant-wide by ended_at, which is where the loss started: a busy tenant fills
// that page with other projects' runs, the budget expires, and the project that was about to branch never got
// integrated. Selecting PROJECTS WITH PENDING RECORDS instead is the same work with a queue as its index — and
// a project that has nothing pending costs one indexed query, not a page slot.
func (r *WorktreeReconciler) integrateLocalRuns(ctx context.Context, tenantID string, deadline time.Time) {
	ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		r.log.Warn("worktree: local integration begin tx failed", "error", err)
		return
	}
	projects, perr := db.ListProjectsWithPendingLocalIntegration(ctx, ttx.Tx, tenantID, localIntegrationLimit)
	ttx.Rollback(ctx)
	if perr != nil {
		r.log.Warn("worktree: local integration could not read pending projects", "error", perr)
		return
	}
	for _, projectID := range projects {
		if sweepBudgetExpired(deadline) {
			r.log.Info("worktree: local integration budget expired, resuming next slow pass",
				"projects_remaining", len(projects))
			return
		}
		r.drainLocalIntegrationQueue(ctx, tenantID, projectID, deadline)
	}
}

// IntegrateProjectNow is the post-commit entry point: advance THIS project's local integration queue
// immediately, called when one of its runs reaches COMPLETED.
//
// One implementation, three triggers — terminal success (here), before a branch is cut (the gate), and the
// periodic pass (the backstop). Anything else would be the N-implementations problem this file exists to
// avoid, one layer over.
func (r *WorktreeReconciler) IntegrateProjectNow(ctx context.Context, tenantID, projectID string) {
	r.drainLocalIntegrationQueue(ctx, tenantID, projectID, time.Now().Add(localIntegrationPerRunBudget))
}

// drainLocalIntegrationQueue works one project's PENDING records, oldest first, until the set is empty or the
// budget expires.
//
// THE QUEUE IS A TABLE, WHICH IS THE POINT. The obligation to land a run's work is a row (db.local_integrations),
// written when the run reaches terminal success. This function reads that row — it does not re-derive "which
// runs might still need integrating" from an ancestry check plus a run_context stamp plus whenever the last
// sweep happened to run. That inference is exactly what missed: six features succeeded, two never landed, and
// nothing said so.
//
// EACH RECORD IS SETTLED IN ONE OF FOUR WAYS, and every one of them is written back:
//
//	merged      the integration branch now contains it
//	conflicted  a real conflict; nothing changed, a human must resolve it, AND IT KEEPS BLOCKING THE QUEUE
//	skipped     there was provably nothing to land (no branch, not local, already gone)
//	pending     left alone only when the budget expires or a transient git error occurred — retried next pass
//
// Progress is what terminates the loop, not a fixed iteration count: each pass either settles a record or
// stops, and a record that cannot settle stops the queue rather than being skipped over. Skipping past it is
// what produces divergence.
func (r *WorktreeReconciler) drainLocalIntegrationQueue(ctx context.Context, tenantID, projectID string, deadline time.Time) {
	if projectID == "" {
		return
	}
	// ADOPT ANY RUN THAT HAS NO RECORD YET, before working the queue. Without this, a run that completed
	// while the plane was down — or before this table existed — would never enter the queue at all, which is
	// the same silence in a new place.
	r.enqueueRunsMissingRecords(ctx, tenantID, projectID)

	for {
		ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
		if err != nil {
			r.log.Warn("worktree: local integration queue: begin tx", "project", projectID, "error", err)
			return
		}
		// THREE STATUSES ARE DRAINED, and the third is not optional:
		//
		//	pending     a live obligation the platform owes
		//	adopted     recognised history (does not block, but must still land)
		//	conflicted  RETRIED ON EVERY PASS, because a human resolving the conflict in git does not rewrite
		//	            this platform's record — the row still says conflicted. Without re-examination the queue
		//	            would stay blocked forever after a resolution, turning a fixed problem into a permanent
		//	            stall. The re-check is cheap: the ancestry test finds the branch already an ancestor of
		//	            the integration branch and settles the record as merged.
		records, lerr := db.ListLocalIntegrationsByStatus(ctx, ttx.Tx, tenantID, projectID,
			[]string{db.LocalIntegrationPending, db.LocalIntegrationAdopted, db.LocalIntegrationConflicted}, 1)
		ttx.Rollback(ctx)
		if lerr != nil {
			r.log.Warn("worktree: local integration queue: list", "project", projectID, "error", lerr)
			return
		}
		if len(records) == 0 {
			return // the queue is empty — done
		}
		if sweepBudgetExpired(deadline) {
			r.log.Info("worktree: local integration queue budget expired with work still pending",
				"project", projectID, "remaining_oldest_run", records[0].WorkflowRunID)
			return
		}
		settled := r.settleIntegrationRecord(ctx, tenantID, projectID, records[0])
		if !settled {
			// A conflict, a missing branch, or a git error. The record now says so (or stays pending), and
			// the queue STOPS here: order is what keeps the next branch fast-forwardable, and working past an
			// unsettled record would diverge the project exactly as reported.
			//
			// STOPPING IS NOT ABANDONING. The record is re-examined on the next pass — which is how a conflict
			// a human resolved by hand releases the queue — and the run that is waiting is told, on every pass,
			// which record is holding it (see the QUEUED log line in reconcileOne).
			return
		}
	}
}

// enqueueRunsMissingRecords writes the obligation for every completed branch-bearing run of this project that
// does not have one yet.
func (r *WorktreeReconciler) enqueueRunsMissingRecords(ctx context.Context, tenantID, projectID string) {
	ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return
	}
	defer ttx.Rollback(ctx)
	runs, err := db.ListRunsNeedingLocalIntegration(ctx, ttx.Tx, tenantID, projectID, localIntegrationLimit)
	if err != nil || len(runs) == 0 {
		return
	}
	for _, run := range runs {
		if strings.TrimSpace(run.WorktreeBranch) == "" {
			continue
		}
		execID := db.ExecutionIDForRun(ctx, ttx.Tx, tenantID, run.ID)
		base := ""
		if b, berr := r.revParse(ctx, r.lookupProjectDir(ctx, tenantID, run.ProjectID), "merge-base"); berr == nil {
			_ = b // merge-base against the integration branch is resolved at settle time, not here
		}
		adopted, eerr := db.EnqueueLocalIntegration(ctx, ttx.Tx, tenantID, projectID, run.ID, execID,
			run.WorktreeBranch, base)
		if eerr == nil && adopted.Status == db.LocalIntegrationPending {
			// ADOPTED, NOT PENDING. The obligation is real and the drain will work it, but it must not hold the
			// queue: this is history the platform recognised after the fact, and stalling new runs behind an
			// unbounded archaeology set is how a project stops accepting work entirely.
			if serr := db.SetLocalIntegrationStatus(ctx, ttx.Tx, tenantID, adopted.ID, db.LocalIntegrationAdopted,
				"", "adopted: a completed run with no integration record", ""); serr != nil {
				r.log.Warn("worktree: could not mark an adopted obligation", "run", run.ID, "error", serr)
			}
		}
		if eerr != nil {
			r.log.Warn("worktree: could not record an integration obligation",
				"run", run.ID, "branch", run.WorktreeBranch, "error", eerr)
		}
	}
	if err := ttx.Commit(ctx); err != nil {
		r.log.Warn("worktree: could not commit integration obligations", "project", projectID, "error", err)
	}
}

// EnqueueRunForIntegration records the obligation for ONE run at terminal success. This is the live path; the
// backfill above catches everything that arrives another way.
//
// The RECORD IS WRITTEN BEFORE THE WORK IS ATTEMPTED, and that ordering is the whole design: an obligation
// that exists and has not been met is a pending row an operator can see, while an obligation that was never
// written is indistinguishable from one that was never owed.
func (r *WorktreeReconciler) EnqueueRunForIntegration(ctx context.Context, tenantID, projectID, runID string) {
	if projectID == "" || runID == "" {
		return
	}
	ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		r.log.Warn("worktree: enqueue integration: begin tx", "run", runID, "error", err)
		return
	}
	defer ttx.Rollback(ctx)
	run, err := db.GetWorkflowRun(ctx, ttx.Tx, tenantID, runID)
	if err != nil {
		r.log.Warn("worktree: enqueue integration: read run", "run", runID, "error", err)
		return
	}
	branch := strings.TrimSpace(run.WorktreeBranch)
	if branch == "" {
		return // no branch, no obligation — and no row to explain away later
	}
	// A NON-LOCAL project owes nothing here; record that as `skipped` so the absence of a pending row is
	// evidence rather than a question.
	strategy := r.effectiveGitStrategy(ctx, tenantID, run)
	rec, eerr := db.EnqueueLocalIntegration(ctx, ttx.Tx, tenantID, projectID, runID,
		db.ExecutionIDForRun(ctx, ttx.Tx, tenantID, runID), branch, "")
	if eerr != nil {
		r.log.Warn("worktree: enqueue integration: write", "run", runID, "error", eerr)
		return
	}
	if strategy != "local" && rec.Status == db.LocalIntegrationPending {
		if serr := db.SetLocalIntegrationStatus(ctx, ttx.Tx, tenantID, rec.ID, db.LocalIntegrationSkipped,
			"", "git_strategy="+strategy+": nothing to land locally", ""); serr != nil {
			r.log.Warn("worktree: could not mark a non-local obligation skipped", "run", runID, "error", serr)
		}
	}
	if cerr := ttx.Commit(ctx); cerr != nil {
		r.log.Warn("worktree: enqueue integration: commit", "run", runID, "error", cerr)
	}
}

// settleIntegrationRecord attempts ONE record and writes back what happened. Reports whether the record is
// settled (so the queue may advance); `false` means the queue must stop here.
func (r *WorktreeReconciler) settleIntegrationRecord(ctx context.Context, tenantID, projectID string, rec db.LocalIntegrationRow) bool {
	projectDir := r.lookupProjectDir(ctx, tenantID, projectID)
	if projectDir == "" || !r.isInsideWorkTree(ctx, projectDir) {
		r.finishIntegrationRecord(ctx, tenantID, rec.ID, db.LocalIntegrationSkipped, "", "project dir is not a git work tree", "")
		return true
	}
	// THE BRANCH IS THE EVIDENCE, and this is where the old code's silence lived: a record whose branch no
	// longer exists has nothing to land. It is `skipped`, not `pending` — leaving it pending would block the
	// project's queue forever on a ref that will never come back.
	if !r.branchExists(ctx, projectDir, rec.Branch) {
		r.log.Warn("worktree: integration record has NO branch — nothing to land",
			"run", rec.WorkflowRunID, "branch", rec.Branch, "repo", projectDir,
			"hint", "already merged and swept, or the run never created it")
		r.finishIntegrationRecord(ctx, tenantID, rec.ID, db.LocalIntegrationSkipped, "",
			"recorded branch does not exist", "")
		return true
	}
	target, terr := r.integrationTarget(ctx, projectDir)
	if terr != nil {
		// A REPOSITORY WITH NO USABLE INTEGRATION BRANCH IS A CONFIGURATION FACT, NOT PENDING WORK, and it must
		// not hold the queue. There is nothing to compare the branch against, so waiting cannot help and would
		// stall the project forever — which is worse than the silence this table replaces, because a stalled
		// project looks like a hang rather than a lost merge.
		//
		// So it is recorded as SKIPPED with the reason on the record: visible, countable, and non-blocking. The
		// work stays on its branch and the run page's integration notice reports it as not merged, so nothing is
		// hidden — the operator is told the repository needs an integration branch, which is the actionable fact.
		r.finishIntegrationRecord(ctx, tenantID, rec.ID, db.LocalIntegrationSkipped, "", terr.Error(), "")
		r.log.Warn("worktree: integration cannot run — the repository has no usable integration branch; the "+
			"branch is kept and the run reports NOT MERGED",
			"run", rec.WorkflowRunID, "branch", rec.Branch, "repo", projectDir, "error", terr)
		return true // settle it: a missing branch must not stall the queue
	}

	if r.isAncestorOf(ctx, projectDir, rec.Branch, target) {
		// ALREADY LANDED — the ancestry check VERIFIES the record rather than substituting for it.
		sha, _ := r.revParse(ctx, projectDir, target)
		r.finishIntegrationRecord(ctx, tenantID, rec.ID, db.LocalIntegrationMerged, sha,
			"already an ancestor of "+target, target)
		return true
	}

	r.integrateMu.Lock()
	defer r.integrateMu.Unlock()
	// Re-check under the lock: another pass may have landed it while we waited.
	if r.isAncestorOf(ctx, projectDir, rec.Branch, target) {
		sha, _ := r.revParse(ctx, projectDir, target)
		r.finishIntegrationRecord(ctx, tenantID, rec.ID, db.LocalIntegrationMerged, sha,
			"already an ancestor of "+target, target)
		return true
	}

	readyToRefresh, refreshBlockedBy := r.mainCheckoutRefreshReadiness(ctx, projectDir, target)

	if r.isAncestorOf(ctx, projectDir, target, rec.Branch) {
		sha, err := r.revParse(ctx, projectDir, rec.Branch)
		if err != nil {
			r.finishIntegrationRecord(ctx, tenantID, rec.ID, db.LocalIntegrationPending, "", err.Error(), target)
			return false
		}
		if err := r.setBranchRef(ctx, projectDir, target, sha,
			fmt.Sprintf("orchicon: local integration fast-forward of %s to run %s", target, rec.WorkflowRunID)); err != nil {
			r.finishIntegrationRecord(ctx, tenantID, rec.ID, db.LocalIntegrationPending, "", err.Error(), target)
			return false
		}
		r.log.Info("worktree: local integration fast-forwarded the integration branch",
			"run", rec.WorkflowRunID, "branch", rec.Branch, "into", target, "commit", short(sha))
		r.finishIntegrationRecord(ctx, tenantID, rec.ID, db.LocalIntegrationMerged, sha, "", target)
		if err := r.refreshMainCheckout(ctx, projectDir, target, readyToRefresh, refreshBlockedBy); err != nil {
			r.log.Warn("worktree: could not refresh the main checkout after integration",
				"run", rec.WorkflowRunID, "error", err)
		}
		return true
	}

	merged, err := r.mergeRunBranchInScratch(ctx, projectDir, rec.Branch, rec.WorkflowRunID, target)
	if err != nil {
		r.finishIntegrationRecord(ctx, tenantID, rec.ID, db.LocalIntegrationPending, "", err.Error(), target)
		return false
	}
	if merged == "" {
		// A CONFLICT. Recorded as such — visible, and it KEEPS BLOCKING, because branching past a conflict is
		// what makes the NEXT merge un-mergeable too. The old code logged a warning and retried forever, which
		// looked like a stall and told nobody why.
		r.log.Warn("worktree: local integration hit a CONFLICT — nothing was merged, both branches are untouched, "+
			"and the project's queue is HELD until it is resolved by hand",
			"run", rec.WorkflowRunID, "branch", rec.Branch, "into", target, "repo", projectDir)
		r.finishIntegrationRecord(ctx, tenantID, rec.ID, db.LocalIntegrationConflicted, "",
			fmt.Sprintf("merge conflict between %s and %s; resolve by hand", rec.Branch, target), target)
		return false
	}
	r.log.Info("worktree: local integration merged the run branch",
		"run", rec.WorkflowRunID, "branch", rec.Branch, "into", target, "commit", short(merged))
	r.finishIntegrationRecord(ctx, tenantID, rec.ID, db.LocalIntegrationMerged, merged, "", target)
	if err := r.refreshMainCheckout(ctx, projectDir, target, readyToRefresh, refreshBlockedBy); err != nil {
		r.log.Warn("worktree: could not refresh the main checkout after integration",
			"run", rec.WorkflowRunID, "error", err)
	}
	return true
}

// finishIntegrationRecord writes an outcome back to the record. Best-effort and logged: a failure here must
// not crash a pass, but it MUST be visible, because the record is the only thing that knows what happened.
func (r *WorktreeReconciler) finishIntegrationRecord(ctx context.Context, tenantID, id, status, mergeCommit, lastError, integrationBranch string) {
	ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		r.log.Warn("worktree: integration record: begin tx", "record", id, "error", err)
		return
	}
	defer ttx.Rollback(ctx)
	if err := db.SetLocalIntegrationStatus(ctx, ttx.Tx, tenantID, id, status, mergeCommit, lastError, integrationBranch); err != nil {
		r.log.Warn("worktree: could not write the integration outcome", "record", id, "status", status, "error", err)
		return
	}
	if err := ttx.Commit(ctx); err != nil {
		r.log.Warn("worktree: integration record: commit", "record", id, "error", err)
	}
}

// BlockingIntegrationCount reports how many of this project's records are still holding the queue.
//
// THIS IS THE GATE'S QUESTION, and it is a COUNT rather than a judgement: a run about to cut a branch must not
// do so while an earlier run's work is unlanded or conflicted. That converts a silent loss into an ordered
// queue, which is what these runs already are.
func (r *WorktreeReconciler) BlockingIntegrationCount(ctx context.Context, tenantID, projectID string) int {
	if projectID == "" {
		return 0
	}
	ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return 0 // cannot verify — do not block on a broken read
	}
	defer ttx.Rollback(ctx)
	n, cerr := db.CountBlockingLocalIntegrations(ctx, ttx.Tx, tenantID, projectID)
	if cerr != nil {
		r.log.Warn("worktree: could not count blocking integrations — proceeding", "project", projectID, "error", cerr)
		return 0
	}
	return n
}

// QueuePosition returns a run's place in the queue plus the records ahead of it, for the wait notice. Empty
// when the run is not blocked.
func (r *WorktreeReconciler) QueuePosition(ctx context.Context, tenantID, projectID, runID string) (ahead int, blocking []db.LocalIntegrationRow) {
	if projectID == "" {
		return 0, nil
	}
	ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return 0, nil
	}
	defer ttx.Rollback(ctx)
	recs, lerr := db.ListLocalIntegrationsByStatus(ctx, ttx.Tx, tenantID, projectID,
		[]string{db.LocalIntegrationPending, db.LocalIntegrationConflicted}, localIntegrationLimit)
	if lerr != nil {
		return 0, nil
	}
	for _, rec := range recs {
		if rec.WorkflowRunID == runID {
			continue // the asking run's own record does not block it
		}
		blocking = append(blocking, rec)
	}
	return len(blocking), blocking
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
	if err := r.setBranchRef(ctx, projectDir, target, tip,
		fmt.Sprintf("orchicon: local integration merge of run %s into %s", runID, target)); err != nil {
		return "", err
	}
	return tip, nil
}

// mainCheckoutRefreshReadiness reports whether the operator's working tree may safely be brought up to
// date after the integration branch moves, and — when it may not — WHY, so the log line can name it.
//
// MUST BE CALLED BEFORE THE REF MOVES (see settleIntegrationRecord): once the ref advances, the index is stale by
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
func (r *WorktreeReconciler) setBranchRef(ctx context.Context, projectDir, branch, sha, why string) error {
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
	// THE REFLOG MESSAGE IS THE ATTRIBUTION, and its absence cost five rounds of investigation.
	//
	// MEASURED, from the operator's own repository: `git reflog show develop` returned
	//
	//	066279b (develop) develop@{0}:
	//	cb3af04           develop@{1}:
	//	69845f3           develop@{2}:
	//	f421a7c           develop@{3}:
	//	558d52e (HEAD -> master) develop@{4}:
	//
	// Five moves, every message EMPTY — because `git update-ref` without `-m` writes a blank entry. That is the
	// exact output of this function, and it made the moves UNATTRIBUTABLE: nothing in the repository could say
	// whether a human, a worker, or the platform moved the branch, or which run did it. The investigation had to
	// reconstruct the cause from mount tables and path mappers because the ref log said nothing.
	//
	// `-m` costs one argument and makes every future move self-describing.
	if _, err := runGit(ctx, projectDir, "update-ref", "-m", why, "refs/heads/"+branch, sha); err != nil {
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

// oldestBlockingRun names the earliest record holding a project's queue, for the log line. Empty when there
// is none, which the caller has already established.
func oldestBlockingRun(recs []db.LocalIntegrationRow) string {
	if len(recs) == 0 {
		return ""
	}
	return recs[0].WorkflowRunID
}
