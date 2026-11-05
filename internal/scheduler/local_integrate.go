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
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/procwin"
)

// Bounds for the project's declared post-integration check. A hung check must fail verification rather than
// wedge the queue, and a failure's output must fit in a record a person will actually read.
const (
	verifyCommandTimeout = 10 * time.Minute
	verifyOutputLimit    = 1200
)

// shellBinary/shellFlag give the declared command the same shell a person would use on this host.
func shellBinary() string {
	if runtime.GOOS == "windows" {
		return "cmd"
	}
	return "/bin/sh"
}

func shellFlag() string {
	if runtime.GOOS == "windows" {
		return "/C"
	}
	return "-c"
}

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

	// EACH RECORD IS ATTEMPTED AT MOST ONCE PER PASS. Without this a record that settles into a state we retry —
	// `failed` (a declared check that no longer passes) or `conflicted` — is re-read the moment it is written,
	// re-attempted, re-failed, and the pass spins until its budget expires. Measured: a 10-second stall on the
	// first run of the failing-check test, which is a wedge in the integration path, not a slow check.
	attempted := map[string]bool{}
	for {
		ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
		if err != nil {
			r.log.Warn("worktree: local integration queue: begin tx", "project", projectID, "error", err)
			return
		}
		// EVERY UNPROVEN STATUS IS DRAINED, and the set is not optional:
		//
		//	pending     a live obligation the platform owes
		//	adopted     recognised history (does not block, but must still land)
		//	conflicted  RETRIED ON EVERY PASS, because a human resolving the conflict in git does not rewrite
		//	            this platform's record — the row still says conflicted. Without re-examination the queue
		//	            would stay blocked forever after a resolution, turning a fixed problem into a permanent
		//	            stall. The re-check is cheap: the ancestry test finds the branch already an ancestor of
		//	            the integration branch and settles the record as merged.
		records, lerr := db.ListLocalIntegrationsByStatus(ctx, ttx.Tx, tenantID, projectID,
			[]string{
				db.LocalIntegrationPending,
				db.LocalIntegrationAdopted,
				db.LocalIntegrationConflicted,
				// An interrupted attempt (the plane died mid-merge) and a legacy `merged` row (landed, never
				// proven) are both work to finish, not work to skip.
				db.LocalIntegrationIntegrating,
				db.LocalIntegrationMerged,
				db.LocalIntegrationFailed,
			}, 1)
		ttx.Rollback(ctx)
		if lerr != nil {
			r.log.Warn("worktree: local integration queue: list", "project", projectID, "error", lerr)
			return
		}
		if len(records) == 0 {
			return // the queue is empty — done
		}
		if attempted[records[0].ID] {
			// The oldest outstanding record has already been tried in this pass. STOP: retrying it now would
			// spin, and working past it would break the ordering the whole queue exists for.
			return
		}
		attempted[records[0].ID] = true
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
		// ALREADY LANDED — AND STILL NOT TRUSTED. This branch used to record `merged` and return, which made it a
		// shortcut AROUND verification: a record that had FAILED its check came back as a clean pass on the next
		// attempt simply because the branch was an ancestor, and a project whose checkout was empty stayed empty
		// because nothing repaired it. The ancestry test answers "is it in the history"; it says nothing about the
		// tree, which is the whole reason verification exists.
		//
		// So the already-landed path does exactly what a fresh merge does: repair the checkout if it is
		// repairable, then VERIFY. That also makes it the repair path for a project left broken by an earlier
		// build — the next pass materialises the tree and proves it.
		sha, _ := r.revParse(ctx, projectDir, target)
		ready := r.mainCheckoutRefreshReadiness(ctx, projectDir, target)
		if err := r.refreshMainCheckout(ctx, projectDir, target, ready); err != nil {
			r.log.Warn("worktree: could not refresh the main checkout for an already-landed integration",
				"run", rec.WorkflowRunID, "error", err)
		}
		r.completeIntegration(ctx, tenantID, rec, projectDir, target, sha, ready.reason)
		return true
	}

	// MARK THE ATTEMPT BEFORE MAKING IT. If the plane dies mid-merge, the record says `integrating` rather
	// than `pending` — so a restart knows an attempt was interrupted rather than never started, and the row is
	// retried. That is the survivability the operator asked for: state in the table, not inferred from timing.
	r.finishIntegrationRecord(ctx, tenantID, rec.ID, db.LocalIntegrationIntegrating, "", rec.LastError, target)

	r.integrateMu.Lock()
	defer r.integrateMu.Unlock()
	// Re-check under the lock: another pass may have landed it while we waited. Same rule — verify, never
	// assume, and repair the checkout first if it needs it.
	if r.isAncestorOf(ctx, projectDir, rec.Branch, target) {
		sha, _ := r.revParse(ctx, projectDir, target)
		ready := r.mainCheckoutRefreshReadiness(ctx, projectDir, target)
		if err := r.refreshMainCheckout(ctx, projectDir, target, ready); err != nil {
			r.log.Warn("worktree: could not refresh the main checkout after a concurrent integration",
				"run", rec.WorkflowRunID, "error", err)
		}
		r.completeIntegration(ctx, tenantID, rec, projectDir, target, sha, ready.reason)
		return true
	}

	readiness := r.mainCheckoutRefreshReadiness(ctx, projectDir, target)

	var landed string
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
		landed = sha
	} else {
		merged, err := r.mergeRunBranchInScratch(ctx, projectDir, rec.Branch, rec.WorkflowRunID, target)
		if err != nil {
			r.finishIntegrationRecord(ctx, tenantID, rec.ID, db.LocalIntegrationPending, "", err.Error(), target)
			return false
		}
		if merged == "" {
			// A CONFLICT. Recorded as such — visible, and it KEEPS BLOCKING, because branching past a conflict
			// is what makes the NEXT merge un-mergeable too. The old code logged a warning and retried forever,
			// which looked like a stall and told nobody why.
			r.log.Warn("worktree: local integration hit a CONFLICT — nothing was merged, both branches are "+
				"untouched, and the project's queue is HELD until it is resolved by hand",
				"run", rec.WorkflowRunID, "branch", rec.Branch, "into", target, "repo", projectDir)
			r.finishIntegrationRecord(ctx, tenantID, rec.ID, db.LocalIntegrationConflicted, "",
				fmt.Sprintf("merge conflict between %s and %s; resolve by hand", rec.Branch, target), target)
			return false
		}
		r.log.Info("worktree: local integration merged the run branch",
			"run", rec.WorkflowRunID, "branch", rec.Branch, "into", target, "commit", short(merged))
		landed = merged
	}

	if err := r.refreshMainCheckout(ctx, projectDir, target, readiness); err != nil {
		r.log.Warn("worktree: could not refresh the main checkout after integration",
			"run", rec.WorkflowRunID, "error", err)
	}
	r.completeIntegration(ctx, tenantID, rec, projectDir, target, landed, readiness.reason)
	return true
}

// completeIntegration VERIFIES the integration and records the outcome. A merge that cannot be proven is not a
// success, and it must not let the next run start.
//
// THE REQUIREMENT, in the operator's words: "a failing check must NOT be recorded as success and must NOT let
// the next run start. It should mark that integration row failed and surface it — ideally blocking the chain."
// So verification is not advisory: `failed` blocks the queue exactly as `pending` and `conflicted` do.
//
// WHY IT EXISTS AT ALL: six runs integrated correctly — the ref moved six times, the reflog proved it, the file
// counts grew — and the operator's checkout was left at the initial empty commit, so `ls` showed nothing and
// the whole cycle looked like a total failure. The record said `merged`, and it was right about the ref and
// silent about the tree. Verification closes that gap.
func (r *WorktreeReconciler) completeIntegration(ctx context.Context, tenantID string, rec db.LocalIntegrationRow,
	projectDir, target, landed, notRefreshedBecause string) {
	verified, detail := r.verifyIntegration(ctx, projectDir, target, rec.Branch, notRefreshedBecause)
	if verified {
		r.log.Info("worktree: local integration VERIFIED",
			"run", rec.WorkflowRunID, "branch", rec.Branch, "into", target, "commit", short(landed), "detail", detail)
		r.finishIntegrationRecord(ctx, tenantID, rec.ID, db.LocalIntegrationVerified, landed, "", target)
		return
	}
	// NOT A SUCCESS, AND IT BLOCKS. The commit exists and the ref moved — the work is not lost — but the state
	// a person or a later run would build on is not there, so the chain stops here and says why.
	r.log.Warn("worktree: local integration FAILED VERIFICATION — the branch is intact but the result is not "+
		"usable, so the project's queue is HELD",
		"run", rec.WorkflowRunID, "branch", rec.Branch, "into", target, "commit", short(landed), "detail", detail)
	r.finishIntegrationRecord(ctx, tenantID, rec.ID, db.LocalIntegrationFailed, landed, detail, target)
}

// verifyIntegration proves the integration actually produced a usable tree, and reports what it found.
//
// THREE CHECKS, in the order that fails fastest and cheapest:
//
//  1. THE WORK IS REACHABLE — the run branch is an ancestor of the integration branch. This is the merge
//     itself, re-asserted after the fact rather than trusted from the code path that performed it.
//  2. THE TREE IS MATERIALISED — when the main checkout is ON the integration branch, its index must match HEAD
//     and no tracked file may be missing from disk. This is the check that was absent and would have caught the
//     reported failure on its FIRST occurrence instead of after six.
//  3. THE PROJECT'S OWN COMMAND PASSES — opt-in, only when the project declares one (see projectVerifyCommand).
//
// A checkout left alone BECAUSE THE OPERATOR HAS UNCOMMITTED WORK is not a verification failure: the platform
// deliberately does not touch their tree, so demanding it match the branch would fail an integration for the
// operator's own edits. It is reported, not failed.
func (r *WorktreeReconciler) verifyIntegration(ctx context.Context, projectDir, target, branch, notRefreshedBecause string) (bool, string) {
	// 0. Leave a trace if git's registry is malformed. Not a verdict — a registration that looks wrong must be
	// REPORTED, never silently repaired, and never treated as the cause of anything on its own.
	r.malformedWorktreeRegistrations(ctx, projectDir)

	// 1. Reachability. Always checked, always possible.
	if !r.isAncestorOf(ctx, projectDir, branch, target) {
		return false, fmt.Sprintf("verification: %s is NOT an ancestor of %s — the merge did not land", branch, target)
	}
	onTarget := r.currentBranch(ctx, projectDir) == target
	if !onTarget {
		return true, fmt.Sprintf("branch merged into %s; the main checkout is on another branch and was not "+
			"touched, so its tree was not verified", target)
	}
	if notRefreshedBecause != "" {
		return true, fmt.Sprintf("branch merged into %s; the main checkout was left alone (%s), so its tree was "+
			"not verified", target, notRefreshedBecause)
	}
	// 2. The index must agree with HEAD. `git diff --cached --quiet` exits 0 exactly when the index matches,
	// which is the mechanical form of "the ref and the tree describe the same commit".
	if _, err := runGit(ctx, projectDir, "diff", "--cached", "--quiet"); err != nil {
		return false, "verification: the index does not match the integration branch — the checkout is stale " +
			"(the operator's remedy is `git reset --hard " + target + "`)"
	}
	status, serr := runGit(ctx, projectDir, "status", "--porcelain")
	if serr != nil {
		return false, "verification: could not read the working tree state"
	}
	if stale, why := staleCheckoutOnly(status); stale {
		return false, fmt.Sprintf("verification: tracked files are MISSING from the working tree (%s) — the "+
			"integration branch records them but they are not on disk", why)
	}
	if strings.TrimSpace(status) != "" {
		if _, why := staleCheckoutOnly(status); why != "" {
			return false, "verification: the working tree is not clean after integration — " + why
		}
	}
	// 3. The project's own command, when it declares one.
	if cmd := projectVerifyCommand(projectDir); cmd != "" {
		if out, verr := runVerifyCommand(ctx, projectDir, cmd); verr != nil {
			return false, "verification: the project's declared check failed: " + trimForMessage(out, verr)
		}
	}
	return true, "ancestry proven, index matches " + target + ", working tree clean"
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

// mainCheckoutRefreshReadiness reports the state of the operator's checkout BEFORE the ref moves.
//
// MUST BE CALLED BEFORE THE REF MOVES: once the ref advances, the index is stale by construction and every file
// the integration brought in looks like a local modification.
func (r *WorktreeReconciler) mainCheckoutRefreshReadiness(ctx context.Context, projectDir, target string) checkoutReadiness {
	if cur := r.currentBranch(ctx, projectDir); cur != target {
		// The operator is not on the branch that moved, so its working tree is unaffected: nothing to do, and
		// nothing to demand of the verification either.
		return checkoutReadiness{onTarget: false}
	}
	status, err := runGit(ctx, projectDir, "status", "--porcelain")
	if err != nil {
		return checkoutReadiness{onTarget: true, reason: "git status could not be read"}
	}
	if stale, why := staleCheckoutOnly(status); stale {
		// THE TREE LAGS ITS OWN HEAD, WHICH IS NOT UNCOMMITTED WORK. Reported: "develop's ref is 066279b, but
		// C:\projects\weatherapp contains only .git, .orchicon, .orchicon-worktrees — no src/, no index.html".
		// Refreshing is safe here precisely because there is nothing to lose: every dirty entry is a file git
		// already has at HEAD, absent from the tree only because the tree is out of date.
		r.log.Info("worktree: the main checkout is behind its own HEAD — refreshing is safe, nothing is uncommitted",
			"repo", projectDir, "shape", why)
		return checkoutReadiness{onTarget: true, repairable: true}
	}
	if strings.TrimSpace(status) != "" {
		// THREE OUTCOMES, NOT TWO. staleCheckoutOnly answers "repairable?" and "complaint?" separately, and
		// `(false, "")` means the only entries were OUR OWN artifact directories — which is CLEAN, not dirty.
		// Reading "status non-empty" as "dirty" is what made a project containing nothing but
		// `?? .orchicon/` refuse every refresh, and the log then blamed the operator's uncommitted work.
		// Measured in this very test: status was exactly `?? .orchicon/`.
		if _, why := staleCheckoutOnly(status); why != "" {
			return checkoutReadiness{onTarget: true, reason: why, status: status}
		}
	}
	return checkoutReadiness{onTarget: true, repairable: true}
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
func (r *WorktreeReconciler) refreshMainCheckout(ctx context.Context, projectDir, target string, rd checkoutReadiness) error {
	if !rd.onTarget {
		return nil // the operator is on another branch; its tree is not this branch's business
	}
	if rd.reason != "" {
		r.log.Warn("worktree: the integration branch advanced but the main checkout was left alone — it is "+
			"now stale relative to its own HEAD",
			"repo", projectDir, "branch", target, "reason", rd.reason,
			// WHAT the verdict was made from — capped, because the point is to name the entries, not to dump a
			// tree.
			"status", trimForMessage(rd.status, nil),
			"remedy", "commit or stash, then run: git reset --hard "+target)
		return nil
	}
	// `reset --hard` is the repair for BOTH repairable shapes: a tree behind the index, and an index behind
	// HEAD. It is safe here by construction — mainCheckoutRefreshReadiness only reports repairable when every
	// dirty entry is a tracked file git already has, so there is nothing of the operator's to lose.
	if _, err := runGit(ctx, projectDir, "reset", "--hard", target); err != nil {
		return fmt.Errorf("git reset --hard %s: %w", target, err)
	}
	r.log.Info("worktree: refreshed the main checkout to the integration branch", "repo", projectDir, "branch", target)
	return nil
}

// isMalformedWorktreePath reports whether a worktree path carries a HOST path inside it — the shape
// `.../.orchicon-worktrees/<runID>/C:/projects/weatherapp/.orchicon-worktrees/<runID>`.
//
// THE INVARIANT, stated as the thing that is actually wrong: on a POSIX filesystem `C:` is an ordinary DIRECTORY
// NAME, so a Windows path handed to `git worktree add` inside a container becomes a RELATIVE path whose first
// component is a drive designator. A drive-letter segment can therefore only be legitimate at position 0 of a
// Windows path — anywhere else it is the signature of a host path used in the wrong namespace.
//
// Three shapes, all reported:
//   - a drive-letter segment at any position other than the first;
//   - `.orchicon-worktrees` appearing twice (the platform directory joined under itself);
//   - a nested absolute path (`home/`, `tmp/`, or a leading `/`) after the platform directory.
//
// A parallel-branch CHILD worktree (`<id>/<stepRunID>`) is legitimate and stays that way: an id is an opaque
// token, so a second segment alone is not malformed.
func isMalformedWorktreePath(p string) bool {
	folded := strings.ReplaceAll(p, "\\", "/")
	segments := strings.Split(folded, "/")
	worktreesDirs := 0
	for i, seg := range segments {
		if isDriveLetterName(strings.TrimSpace(seg)) {
			if i > 0 {
				return true // a drive designator that is not the path's root
			}
			continue
		}
		if strings.TrimSpace(seg) == worktreeDirName {
			worktreesDirs++
		}
	}
	if worktreesDirs > 1 {
		return true // the platform directory joined under itself
	}
	i := strings.LastIndex(folded, worktreeDirName+"/")
	if i < 0 {
		return false
	}
	rest := strings.TrimPrefix(folded[i+len(worktreeDirName)+1:], "/")
	if rest == "" {
		return false
	}
	parts := strings.Split(rest, "/")
	for _, extra := range parts[1:] { // parts[0] is the id; anything after it must be an id too
		if extra == "" {
			continue
		}
		if strings.Contains(extra, ":") || extra == "home" || extra == "tmp" || strings.HasPrefix(extra, ".") {
			return true
		}
	}
	return false
}

// malformedWorktreeRegistrations DETECTS AND REPAIRS registrations whose path has another absolute path appended
// under it — the shape `.../.orchicon-worktrees/<runID>/C:/projects/weatherapp/.orchicon-worktrees/<runID>`.
//
// THE PRODUCER IS KNOWN NOW, and it is worth stating because "no trace was left" cost two cycles. REPRODUCED
// against real git: `git worktree add C:/projects/weatherapp/.orchicon-worktrees/<id>` executed somewhere that
// does NOT have C: as a drive — i.e. INSIDE THE RUNTIME CONTAINER — treats the host path as a RELATIVE one:
//
//	$ git worktree add 'C:/projects/weatherapp/.orchicon-worktrees/01M6TEST'
//	$ cat .git/worktrees/01M6TEST/gitdir
//	/mnt/c/projects/weatherapp/C:/projects/weatherapp/.orchicon-worktrees/01M6TEST/.git
//	$ git worktree list
//	worktree /mnt/c/projects/weatherapp/C:/projects/weatherapp/.orchicon-worktrees/01M6TEST
//
// The admin dir is named `<runID>` (the basename, which is why it looks legitimate), the recorded gitdir is a
// GUEST path the host cannot resolve — hence "prunable" — and the checkout it names does not exist. So a
// containerised process (a worker running `git worktree add` with the path it was told the project lives at)
// registers a worktree at a path that exists only in its own namespace.
//
// TWO THINGS IT COSTS, beyond a confusing `git worktree list`:
//   1. the stray tree (`<project>/C:/...`) is an UNTRACKED entry, and `C:` is not one of ours — so after the
//      readiness fix it would block the operator's checkout refresh again, as `?? C:/`;
//   2. a registration naming a path makes `git worktree add` refuse if a later run computes the same path.
//
// SO THIS REPAIRS RATHER THAN ONLY REPORTS. It prunes the stale registrations and removes the stray trees, but
// ONLY when both are provably ours: the recorded path must not exist on disk (git already calls such an entry
// prunable), and every file inside a stray tree must be a worktree pointer naming THIS repository. Anything else
// is logged and left alone — a wrong deletion is worse than a confusing listing.
func (r *WorktreeReconciler) malformedWorktreeRegistrations(ctx context.Context, projectDir string) {
	wts, err := listWorktrees(ctx, projectDir)
	if err != nil {
		return
	}
	var malformed []string
	for _, wt := range wts {
		if isMalformedWorktreePath(wt.path) {
			malformed = append(malformed, wt.path)
		}
	}
	if len(malformed) == 0 {
		return
	}
	for _, p := range malformed {
		r.log.Warn("worktree: MALFORMED worktree registration — a host path (C:/...) was used inside the "+
			"container, where C: is a relative directory name",
			"repo", projectDir, "path", p)
	}
	// THE FILESYSTEM HALF GOES FIRST, and the order is load-bearing twice over: `git worktree prune` drops an
	// entry only when the directory it names is MISSING, so a stray tree that still exists pins its own
	// registration; and the existence test below is precisely "is this entry still pinned".
	r.removeStrayContainerTrees(ctx, projectDir)

	// Now decide per entry: pinned by a directory that still exists, or genuinely reclaimable.
	repaired := 0
	survivors := 0
	for _, p := range malformed {
		if _, serr := os.Stat(p); serr == nil {
			survivors++
			r.log.Warn("worktree: a malformed registration names a path that STILL EXISTS — git will not prune "+
				"it and this needs a human. Do not delete it blindly: it is a worktree, at a wrong path",
				"repo", projectDir, "path", p)
			continue
		}
		repaired++
	}
	if repaired > 0 {
		if _, perr := runGit(ctx, projectDir, "worktree", "prune"); perr != nil {
			r.log.Warn("worktree: prune after malformed registrations failed", "repo", projectDir, "error", perr)
		}
		// REPORT WHAT SURVIVED, not what was attempted: a second list is the only honest evidence the registry
		// is clean.
		if after, aerr := listWorktrees(ctx, projectDir); aerr == nil {
			left := 0
			for _, wt := range after {
				if isMalformedWorktreePath(wt.path) {
					left++
				}
			}
			if left > 0 {
				r.log.Warn("worktree: malformed registrations REMAIN after repair", "repo", projectDir, "remaining", left)
			} else {
				r.log.Info("worktree: repaired malformed worktree registrations",
					"repo", projectDir, "repaired", repaired, "still_pinned", survivors)
			}
		}
	}
}

// removeStrayContainerTrees deletes a drive-letter directory at the project root that holds nothing but OUR
// worktree pointers — the on-disk half of a host path used inside a container.
//
// `C:` under the project root is never legitimate content of a git project, and its presence breaks two things:
// it is an untracked entry that blocks the operator's checkout refresh, and it is the tree the malformed
// registration named. Every file inside it is verified to be a worktree pointer into THIS repository before
// anything is deleted, so a directory some other tool created — or one holding real files — is left alone.
func (r *WorktreeReconciler) removeStrayContainerTrees(ctx context.Context, projectDir string) {
	entries, err := os.ReadDir(projectDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !isDriveLetterName(e.Name()) {
			continue
		}
		root := filepath.Join(projectDir, e.Name())
		ours, checked := strayTreeIsProvablyOurWorktrees(projectDir, root)
		if !ours {
			r.log.Warn("worktree: a drive-letter directory exists at the project root but does NOT hold only "+
				"this repository's worktree pointers — leaving it alone. Tell the worker to use the relative "+
				"worktree path inside the container",
				"repo", projectDir, "path", root, "files_checked", checked)
			continue
		}
		if rerr := os.RemoveAll(root); rerr != nil {
			r.log.Warn("worktree: could not remove a stray container-path tree", "path", root, "error", rerr)
			continue
		}
		r.log.Warn("worktree: removed a stray container-path tree left by `git worktree add C:/...` inside the "+
			"container (a host path used where C: is a relative directory)",
			"repo", projectDir, "path", root)
	}
}

// isDriveLetterName reports whether a directory name is a bare drive designator ("C:", "d:").
//
// That is what a Windows path's first component looks like when a POSIX filesystem treats it as a name, and it is
// never valid content of a git project at the root.
func isDriveLetterName(name string) bool {
	if len(name) != 2 || name[1] != ':' {
		return false
	}
	c := name[0]
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

// strayTreeIsProvablyOurWorktrees reports whether root holds nothing but checked-out worktrees OF THIS
// REPOSITORY — which is what a stray container-path tree is, once the mechanism is understood.
//
// "ONLY WORKTREE POINTERS" WAS THE FIRST PREDICATE AND IT WAS WRONG: `git worktree add` CHECKS OUT the branch, so
// the stray tree contains a `.git` pointer AND every file of the branch (measured: `.git` + `README.md`). A rule
// that demands "only `.git` files" therefore classified our own mess as foreign and never repaired it.
//
// The safe rule is COVERAGE: find every directory inside root whose `.git` file points into this repository's
// `.git/worktrees`, then require that EVERY file in the tree lies under one of those directories. So:
//
//	our stray tree        → each file belongs to a worktree of ours  → reclaimed
//	a user's `C:` folder  → files covered by no worktree of ours     → left alone
//	an empty directory    → no pointer at all                        → left alone
//
// It returns whether the tree is provably ours and how many files were covered, so the log can say what it saw
// rather than asserting a verdict.
func strayTreeIsProvablyOurWorktrees(repo, root string) (bool, int) {
	owned := map[string]bool{} // directories holding a `.git` pointer into our own worktree registry
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != ".git" {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		pointer := strings.ReplaceAll(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(b)), "gitdir: ")), "\\", "/")
		if strings.Contains(pointer, "/.git/worktrees/") || strings.HasPrefix(pointer, ".git/worktrees/") {
			owned[filepath.Dir(path)] = true
		}
		return nil
	})
	if len(owned) == 0 {
		return false, 0 // no worktree of ours anywhere — not our mess
	}
	covered := 0
	proven := true
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			proven = false
			return fs.SkipAll
		}
		if d.IsDir() {
			return nil
		}
		for dir := range owned {
			if path == dir || strings.HasPrefix(path, dir+string(os.PathSeparator)) {
				covered++
				return nil
			}
		}
		// A file belonging to no worktree of ours is somebody's content, and the caller would delete it.
		proven = false
		return fs.SkipAll
	})
	if !proven {
		return false, covered
	}
	return true, covered
}

// projectVerifyCommand returns the project's declared post-integration check, or "" when it declares none.
//
// OPT-IN BY A FILE IN THE PROJECT, deliberately: running a command the platform invents would be worse than not
// running one, and a per-project setting would need a schema and a wire change for something most projects will
// not set. Two lines in `.orchicon/verify` is a declaration the operator can see, version, and delete:
//
//	<the shell command, run with the project dir as its cwd>
//
// Anything else — absent file, empty file, a comment — means "no check", and verification then rests on the two
// mechanical checks that need no cooperation from the project.
func projectVerifyCommand(projectDir string) string {
	b, err := os.ReadFile(filepath.Join(projectDir, ".orchicon", "verify"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		return line
	}
	return ""
}

// runVerifyCommand runs the project's declared check with a bounded budget and returns its combined output.
//
// BOUNDED, because this runs on the integration path and a hung check would wedge the queue exactly as a
// missing merge did. A timeout is a FAILURE of the check, not a pass.
func runVerifyCommand(ctx context.Context, projectDir, command string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, verifyCommandTimeout)
	defer cancel()
	// procwin.CommandContext, NOT exec.CommandContext: a bare exec here spawns a VISIBLE console window on
	// Windows for a check the operator never asked to watch. (Caught by the platform's own guard test — the
	// first version of this function failed it.)
	cmd := procwin.CommandContext(cctx, shellBinary(), shellFlag(), command)
	cmd.Dir = projectDir
	out, err := cmd.CombinedOutput()
	if cctx.Err() != nil {
		return string(out), fmt.Errorf("timed out after %s", verifyCommandTimeout)
	}
	return string(out), err
}

// trimForMessage keeps a failing check's output to something a record can carry: the tail, where the failure
// is, rather than the head, where the banner is.
//
// err MAY BE NIL — this is also used to cap a diagnostic string that has no error attached. It dereferenced the
// error unconditionally at first and panicked inside the integration path, which is the worst place to learn that.
func trimForMessage(out string, err error) string {
	tail := truncateTail(strings.TrimSpace(out), verifyOutputLimit)
	if err == nil {
		return tail
	}
	if tail == "" {
		return err.Error()
	}
	return err.Error() + ": " + tail
}

// truncateTail keeps the END of s, where a failure's cause usually is, capped at limit bytes.
func truncateTail(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return "…" + s[len(s)-limit:]
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

// platformArtifactPaths are directories the PLATFORM creates inside a project directory. The operator never
// authored them, so their presence must never read as "the operator has uncommitted work".
//
// THIS WAS THE CAUSE OF A SIX-RUN FAILURE, and it is worth stating plainly because the message it produced
// blamed the operator. `.orchicon-worktrees/` holds the per-run worktrees; `.orchicon/` holds the transcript,
// offload and memory files. A worker prompt has claimed for a long time that `.orchicon-worktrees/` "is already
// gitignored" — nothing ever writes that entry into a project, so on a project without it `git status` reports
// `?? .orchicon-worktrees/`, the readiness check read that as the operator's untracked files, REFUSED to
// refresh, and logged "the operator's main checkout has UNCOMMITTED changes" about our own artifact. Six
// integrations then moved develop six times while the checkout stayed at the initial empty commit, and the
// operator saw an empty directory and concluded the whole run had failed.
var platformArtifactPaths = []string{worktreeDirName, ".orchicon"}

// isPlatformArtifactPath reports whether a status path is one of ours.
func isPlatformArtifactPath(p string) bool {
	p = strings.Trim(strings.Trim(strings.ReplaceAll(p, "\\", "/"), "\""), "/")
	for _, dir := range platformArtifactPaths {
		if p == dir || strings.HasPrefix(p, dir+"/") {
			return true
		}
	}
	return false
}

// staleCheckoutOnly reports whether a `git status --porcelain` result is REPAIRABLE by a hard reset — i.e.
// every dirty entry is a tracked file that is merely absent from the index or the working tree — and, when it
// is not, says why so the log names the real reason.
//
// THE TWO SHAPES THAT MATTER, and the old version accepted only the second:
//
//	" D <path>"  deleted from the WORKING TREE, present in the index  — the tree lags the index
//	"D  <path>"  deleted from the INDEX, present at HEAD             — HEAD moved past a stale index
//
// The second is what a ref moved by `git update-ref` produces, which is exactly how this platform advances the
// integration branch (setBranchRef never touches a working tree). MEASURED against real git: moving a branch
// forward while index and worktree stay put yields `D  f1.txt` / `D  f2.txt` and `git diff --cached` reporting
// them as deletions — the operator's report verbatim ("55 staged DELETIONS"). The old function sent that branch
// to `default:` and answered "the working tree has staged or modified changes", so the ONE state a hard reset
// exists to repair was the one it refused to repair, on every pass.
func staleCheckoutOnly(status string) (bool, string) {
	lines := strings.Split(strings.TrimRight(status, "\n"), "\n")
	if len(lines) == 0 || strings.TrimSpace(status) == "" {
		return false, ""
	}
	missing := 0
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if len(line) < 3 {
			return false, ""
		}
		x, y := line[0], line[1]
		path := strings.TrimSpace(line[2:])
		switch {
		case x == '?' && y == '?':
			if isPlatformArtifactPath(path) {
				continue // OUR directory, not the operator's work
			}
			return false, "the working tree has untracked files that are not ours"
		case x == 'D' && y == ' ':
			// HEAD has the file, the index does not: the integration branch moved past a stale index.
			missing++
		case x == ' ' && y == 'D':
			// The index has it, the working tree does not: the tree lags the index.
			missing++
		default:
			return false, "the working tree has staged or modified changes"
		}
	}
	if missing == 0 {
		return false, ""
	}
	return true, "tracked files missing from the index or the working tree"
}

// checkoutReadiness is what the caller needs to know about the operator's checkout before a ref moves.
type checkoutReadiness struct {
	// onTarget is whether the main checkout is ON the integration branch. When it is not, its working tree
	// belongs to another branch and must not be touched or demanded anything.
	onTarget bool
	// repairable is whether it MAY be brought up to date: every dirty entry is a tracked file merely absent, or
	// one of our own artifact directories.
	repairable bool
	// reason explains a refusal in the log line ("" when repairable).
	reason string
	// status is the raw `git status --porcelain` the verdict was made from, carried so a refusal can say WHAT it
	// saw. Without it the log said "has uncommitted work" and nothing else, which is indistinguishable between
	// the operator's edits, a stale index, and one of our own directories.
	status string
}

// oldestBlockingRun names the earliest record holding a project's queue, for the log line. Empty when there
// is none, which the caller has already established.
func oldestBlockingRun(recs []db.LocalIntegrationRow) string {
	if len(recs) == 0 {
		return ""
	}
	return recs[0].WorkflowRunID
}
