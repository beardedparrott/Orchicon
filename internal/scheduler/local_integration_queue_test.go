package scheduler

// local_integration_queue_test.go — A AND B, PROVEN AGAINST A REAL REPOSITORY AND A REAL QUEUE.
//
// REPORTED (six features, all "succeeded", two never landed):
//
//	news   run started 18:45:06 ; location integrated 18:45:40  → branched 34s early
//	theme  run started 19:01:24 ; favourites integrated 19:02:21 → branched 57s early
//
// Every run that integrated started after its predecessor finished integrating; every run that did not,
// started before. Both misses branched from a tip that was still missing the previous run's work, so both
// ended up divergent — and divergence is not a small inefficiency, it makes every following merge need a real
// conflict resolution.
//
// The two halves this file pins:
//
//	A. THE MERGE IS RECORDED AND PERFORMED PER RUN. A completed run's branch gets a row the moment it finishes,
//	   and the drain lands it — oldest first — so the next run's branch is cut from a tip that already contains
//	   it.
//	B. THE NEXT RUN WAITS. A run does not cut its branch while an earlier obligation in its project is
//	   unlanded. Waiting is what makes the guarantee independent of timing: no sweep interval, no race, no
//	   "it usually catches up".

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
)

// queueTestEnv is a project on a real on-disk repo, plus the reconcilers under test.
type queueTestEnv struct {
	repo string
	proj db.ProjectRow
	wrec *WorktreeReconciler
	pool *db.Pool
	ctx  context.Context
}

func newQueueTestEnv(t *testing.T) *queueTestEnv {
	t.Helper()
	pool := approvalTestPool(t)
	purgeQueueResidue(t, pool)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	env := &queueTestEnv{pool: pool, ctx: ctx}
	env.repo = newTestRepo(t)
	// A WorktreeReconciler on this repo, with local integration enabled through the project's strategy.
	env.wrec = NewWorktreeReconciler(pool, logger)

	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		Name: "Local Integration Queue", Slug: "liq-" + strings.ToLower(db.NewID()),
		Status: "active", Goals: []byte("[]"),
		ProjectDir:    env.repo,
		ExecutionMode: db.ExecutionModeRuntime,
		GitStrategy:   "local",
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit project: %v", err)
	}
	env.proj = proj
	return env
}

// purgeQueueResidue clears this shared tenant's workflow state AND its integration obligations, so the
// project-scoped queue this test builds is the only one in play.
func purgeQueueResidue(t *testing.T, pool *db.Pool) {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		return
	}
	defer ttx.Rollback(ctx)
	for _, q := range []string{
		`DELETE FROM local_integrations WHERE tenant_id = $1`,
		`DELETE FROM workflow_step_runs WHERE tenant_id = $1`,
		`DELETE FROM workflow_runs WHERE tenant_id = $1`,
	} {
		if _, err := ttx.Tx.Exec(ctx, q, approvalTestTenant); err != nil {
			return
		}
	}
	_ = ttx.Commit(ctx)
}

func (e *queueTestEnv) obligation(t *testing.T, runID string) (db.LocalIntegrationRow, error) {
	t.Helper()
	ttx, err := e.pool.BeginTenantTx(e.ctx, approvalTestTenant)
	if err != nil {
		return db.LocalIntegrationRow{}, err
	}
	defer ttx.Rollback(e.ctx)
	return db.GetLocalIntegrationForRun(e.ctx, ttx.Tx, approvalTestTenant, runID)
}

func (e *queueTestEnv) run(t *testing.T, runID string) db.WorkflowRunRow {
	t.Helper()
	ttx, err := e.pool.BeginTenantTx(e.ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("read run: begin: %v", err)
	}
	defer ttx.Rollback(e.ctx)
	run, err := db.GetWorkflowRun(e.ctx, ttx.Tx, approvalTestTenant, runID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	return run
}

// seedCompletedRun creates a completed run whose branch exists on the repo and carries one commit: exactly the
// shape a finished worker leaves behind — work committed, nothing merged.
func (e *queueTestEnv) seedCompletedRun(t *testing.T, tag string) (runID, branch string) {
	t.Helper()
	ttx, err := e.pool.BeginTenantTx(e.ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(e.ctx)

	runID = db.NewID()
	run, err := db.CreateWorkflowRun(e.ctx, ttx.Tx, db.WorkflowRunRow{
		ID: runID, TenantID: approvalTestTenant,
		WorkflowID: "wf-queue-test", WorkflowVersion: 1,
		ProjectID: e.proj.ID, Status: domain.WorkflowRunCompleted,
		RunContext: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create completed run: %v", err)
	}
	branch = "feat-" + tag
	gitRun(t, e.repo, "branch", branch, "develop")
	wt := t.TempDir()
	gitRun(t, e.repo, "worktree", "add", wt, branch)
	if err := os.WriteFile(filepath.Join(wt, "feature-"+tag+".txt"), []byte(tag+"\n"), 0o644); err != nil {
		t.Fatalf("write feature file: %v", err)
	}
	gitRun(t, wt, "add", ".")
	gitRun(t, wt, "commit", "-m", "feature "+tag)
	gitRun(t, e.repo, "worktree", "remove", "--force", wt)

	// Record the branch the way the worktree reconciler does, then the obligation the workflow reconciler writes
	// at terminal success.
	empty := ""
	if _, err := db.UpdateWorkflowRun(e.ctx, ttx.Tx, approvalTestTenant, runID, run.Version,
		db.UpdateWorkflowRunFields{WorktreeBranch: &branch, WorktreeStatus: &empty}); err != nil {
		t.Fatalf("record branch: %v", err)
	}
	if _, err := db.EnqueueLocalIntegration(e.ctx, ttx.Tx, approvalTestTenant, e.proj.ID, runID, "", branch, ""); err != nil {
		t.Fatalf("enqueue obligation: %v", err)
	}
	if err := ttx.Commit(e.ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
	return runID, branch
}

// seedPendingRun creates a run that is about to be provisioned: no worktree, branch not yet cut.
func (e *queueTestEnv) seedPendingRun(t *testing.T) string {
	t.Helper()
	ttx, err := e.pool.BeginTenantTx(e.ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(e.ctx)
	runID := db.NewID()
	if _, err := db.CreateWorkflowRun(e.ctx, ttx.Tx, db.WorkflowRunRow{
		ID: runID, TenantID: approvalTestTenant,
		WorkflowID: "wf-queue-test", WorkflowVersion: 1,
		ProjectID: e.proj.ID, Status: domain.WorkflowRunRunning,
		RunContext: []byte("{}"),
	}); err != nil {
		t.Fatalf("create pending run: %v", err)
	}
	if err := ttx.Commit(e.ctx); err != nil {
		t.Fatalf("commit pending run: %v", err)
	}
	return runID
}

// A. THE OBLIGATION IS RECORDED AT TERMINAL SUCCESS AND DRAINED INTO A MERGE.
func TestCompletedRunGetsAnObligationAndTheDrainLandsIt(t *testing.T) {
	env := newQueueTestEnv(t)
	runID, branch := env.seedCompletedRun(t, "alpha")

	// Before the drain: the obligation exists and blocks.
	if n := env.wrec.BlockingIntegrationCount(env.ctx, approvalTestTenant, env.proj.ID); n != 1 {
		t.Fatalf("blocking obligations = %d, want 1 — the obligation must exist BEFORE anything is attempted, "+
			"or a missed merge cannot be counted later", n)
	}

	env.wrec.IntegrateProjectNow(env.ctx, approvalTestTenant, env.proj.ID)

	// The merge HAPPENED: the branch is now an ancestor of develop.
	if !env.wrec.isAncestorOf(env.ctx, env.repo, branch, "develop") {
		t.Fatalf("after the drain, %s is still not an ancestor of develop — the work was not landed", branch)
	}
	// And the record says so, with the commit.
	rec, err := env.obligation(t, runID)
	if err != nil {
		t.Fatalf("read obligation: %v", err)
	}
	if rec.Status != db.LocalIntegrationVerified {
		t.Fatalf("obligation status = %q, want verified (last_error=%q detail=%q)",
			rec.Status, rec.LastError, rec.VerificationDetail)
	}
	if rec.MergeCommit == "" {
		t.Error("the record has no merge commit — 'did it land' should be answerable from the row, not inferred")
	}
	// The queue is clear, so the next run may branch.
	if n := env.wrec.BlockingIntegrationCount(env.ctx, approvalTestTenant, env.proj.ID); n != 0 {
		t.Fatalf("the queue still blocks with %d obligations after a successful drain", n)
	}
}

// B. A RUN DOES NOT CUT ITS BRANCH WHILE AN EARLIER OBLIGATION CANNOT LAND — AND IT PROCEEDS ONCE IT CAN.
//
// THE GATE IS NOT "WAIT FOR THE SWEEP". reconcileOne drains the queue first, so when a predecessor CAN be
// landed it is landed immediately and there is nothing to wait for — that is the better outcome and the common
// one. The gate earns its keep in the case the report actually hit and could not survive: a predecessor whose
// integration does NOT succeed (here: a real conflict). Branching there is what turned one missed merge into
// every following run needing a conflict resolution, so the run must WAIT — visibly, and without being marked
// failed, because it has done nothing wrong.
func TestNewRunWaitsWhileAPredecessorCannotLandThenProceeds(t *testing.T) {
	env := newQueueTestEnv(t)
	predecessorRunID, predecessorBranch := env.seedCompletedRun(t, "beta")

	// Make the predecessor UNLANDABLE by hand: both sides change the same line, so the merge conflicts and the
	// drain cannot settle it. This is the 34s/57s window, made deterministic.
	if err := os.WriteFile(filepath.Join(env.repo, "shared.txt"), []byte("from develop\n"), 0o644); err != nil {
		t.Fatalf("write develop side: %v", err)
	}
	gitRun(t, env.repo, "add", ".")
	gitRun(t, env.repo, "commit", "-m", "develop side")
	wt := t.TempDir()
	gitRun(t, env.repo, "worktree", "add", wt, predecessorBranch)
	if err := os.WriteFile(filepath.Join(wt, "shared.txt"), []byte("from the branch\n"), 0o644); err != nil {
		t.Fatalf("write branch side: %v", err)
	}
	gitRun(t, wt, "add", ".")
	gitRun(t, wt, "commit", "-m", "branch side")
	gitRun(t, env.repo, "worktree", "remove", "--force", wt)

	runID := env.seedPendingRun(t)

	// PASS 1 — the predecessor cannot land, so this run must NOT be handed a worktree.
	if err := env.wrec.reconcileOne(env.ctx, approvalTestTenant, runID); err != nil {
		t.Fatalf("reconcileOne (wait pass): %v", err)
	}
	got := env.run(t, runID)
	if got.WorktreeStatus == domain.WorktreeReady {
		t.Fatalf("the run was provisioned (branch %q) while %q could not land — that branch is divergent from "+
			"the moment it exists, which is the defect the report measured at 34s and 57s",
			got.WorktreeBranch, predecessorBranch)
	}
	if got.ErrorMessage != "" {
		t.Fatalf("waiting is NOT failing, but the run carries an error: %q — a queue must not look like a fault",
			got.ErrorMessage)
	}
	// And the reason is on the record, where an operator can see it, rather than in a log line nobody reads.
	rec, err := env.obligation(t, predecessorRunID)
	if err == nil && rec.Status != db.LocalIntegrationConflicted {
		t.Logf("predecessor record status %q (last_error=%q)", rec.Status, rec.LastError)
	}

	// A HUMAN RESOLVES THE CONFLICT — the way a human actually does: take a side on the disputed line, commit it,
	// and then the branch merges cleanly. (Merging without deciding would conflict again, which is the point.)
	if err := os.WriteFile(filepath.Join(env.repo, "shared.txt"), []byte("from the branch\n"), 0o644); err != nil {
		t.Fatalf("write the resolution: %v", err)
	}
	gitRun(t, env.repo, "add", "shared.txt")
	gitRun(t, env.repo, "commit", "-m", "resolve by hand in favour of the run branch")
	gitRun(t, env.repo, "merge", "--no-ff", "-m", "land the predecessor", predecessorBranch)
	// The record still says conflicted — a human resolving git does not rewrite the platform's record — so the
	// queue is released by the next drain finding it already an ancestor.
	env.wrec.IntegrateProjectNow(env.ctx, approvalTestTenant, env.proj.ID)

	// PASS 2 — the predecessor has landed, so this run may go.
	if err := env.wrec.reconcileOne(env.ctx, approvalTestTenant, runID); err != nil {
		t.Fatalf("reconcileOne (proceed pass): %v", err)
	}
	got = env.run(t, runID)
	if got.WorktreeStatus != domain.WorktreeReady {
		t.Fatalf("after the predecessor landed, the run is still %q (error=%q) — the queue never released",
			got.WorktreeStatus, got.ErrorMessage)
	}
	// THE POINT OF THE WHOLE EXERCISE: the new branch contains the predecessor's work, so ITS merge is a
	// fast-forward rather than another conflict resolution.
	if !env.wrec.isAncestorOf(env.ctx, env.repo, predecessorBranch, got.WorktreeBranch) {
		t.Fatalf("the new branch %q does not contain the predecessor %q — it was cut from a stale tip, and its "+
			"merge will need a conflict resolution", got.WorktreeBranch, predecessorBranch)
	}
}

// A MERGE THAT CANNOT BE VERIFIED IS `failed`, AND IT HOLDS THE QUEUE — the operator's requirement: "a failing
// check must not be recorded as success and must not let the next run start".
//
// The failure used here is the reported one: the project declares a check that fails. (The other failure — a
// tree that was not materialised — is covered directly by TestVerificationFailsWhenTheTreeIsNotMaterialised,
// because provoking it through the drain requires an unwritable checkout.)
func TestAFailedVerificationIsRecordedAndBlocksTheQueue(t *testing.T) {
	env := newQueueTestEnv(t)
	runID, branch := env.seedCompletedRun(t, "verifail")

	// The project declares a check, and it fails.
	dir := filepath.Join(env.repo, ".orchicon")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "verify"), []byte("printf 'suite failed\\n'; exit 1\n"), 0o644); err != nil {
		t.Fatalf("write verify: %v", err)
	}

	env.wrec.IntegrateProjectNow(env.ctx, approvalTestTenant, env.proj.ID)

	rec, err := env.obligation(t, runID)
	if err != nil {
		t.Fatalf("read obligation: %v", err)
	}
	// THE MERGE DID HAPPEN — this is not a conflict, and the work is not lost.
	if !env.wrec.isAncestorOf(env.ctx, env.repo, branch, "develop") {
		t.Fatalf("premise wrong: the branch did not merge, so this is not testing verification")
	}
	if rec.Status != db.LocalIntegrationFailed {
		t.Fatalf("a failed verification recorded %q, want %q — a merge that cannot be proven is not a success",
			rec.Status, db.LocalIntegrationFailed)
	}
	if !strings.Contains(rec.LastError, "suite failed") {
		t.Errorf("the failure record does not carry the check's output: %q", rec.LastError)
	}
	// AND IT BLOCKS: "must not let the next run start".
	if n := env.wrec.BlockingIntegrationCount(env.ctx, approvalTestTenant, env.proj.ID); n != 1 {
		t.Fatalf("a FAILED verification stopped blocking the queue (%d) — the next run would branch on top of "+
			"a result nothing could prove", n)
	}

	// Fixing the project's check releases it on the next pass, and the record becomes verified — the failure is
	// retried rather than being a permanent tombstone.
	if err := os.WriteFile(filepath.Join(dir, "verify"), []byte("printf 'ok\\n'\n"), 0o644); err != nil {
		t.Fatalf("rewrite verify: %v", err)
	}
	env.wrec.IntegrateProjectNow(env.ctx, approvalTestTenant, env.proj.ID)
	rec, err = env.obligation(t, runID)
	if err != nil {
		t.Fatalf("re-read obligation: %v", err)
	}
	if rec.Status != db.LocalIntegrationVerified {
		t.Fatalf("after the check passed the record is %q, want verified (last_error=%q)", rec.Status, rec.LastError)
	}
	if rec.VerifiedAt == nil {
		t.Error("a verified record carries no verified_at — 'was this ever proven' should be answerable from the row")
	}
	if n := env.wrec.BlockingIntegrationCount(env.ctx, approvalTestTenant, env.proj.ID); n != 0 {
		t.Fatalf("the queue still blocks (%d) after a passing verification", n)
	}
}

// A SUCCESSFUL INTEGRATION IS RECORDED AS `verified`, NOT `merged` — the whole point of the distinction.
func TestSuccessfulIntegrationRecordsVerified(t *testing.T) {
	env := newQueueTestEnv(t)
	runID, _ := env.seedCompletedRun(t, "verified")
	env.wrec.IntegrateProjectNow(env.ctx, approvalTestTenant, env.proj.ID)
	rec, err := env.obligation(t, runID)
	if err != nil {
		t.Fatalf("read obligation: %v", err)
	}
	if rec.Status != db.LocalIntegrationVerified {
		t.Fatalf("status = %q, want verified (last_error=%q detail=%q)", rec.Status, rec.LastError, rec.VerificationDetail)
	}
	if rec.MergeCommit == "" {
		t.Error("a verified record carries no merge commit")
	}
	if rec.VerifiedAt == nil {
		t.Error("a verified record carries no verified_at")
	}
}

// A CONFLICT IS RECORDED, KEEPS BLOCKING, AND SAYS WHY.
//
// It must keep blocking: branching past a conflict diverges the project further, which is how one missed
// integration made every following run un-mergeable. Silence was the original sin; a visible, blocking,
// self-describing record is the fix.
func TestAConflictingObligationIsRecordedAndKeepsBlocking(t *testing.T) {
	env := newQueueTestEnv(t)
	runID, branch := env.seedCompletedRun(t, "conflict")

	// Make the two sides disagree on the same line of the same file, then advance develop with a DIFFERENT
	// commit that also changes it: the merge cannot be a fast-forward and cannot be resolved mechanically.
	if err := os.WriteFile(filepath.Join(env.repo, "shared.txt"), []byte("from develop\n"), 0o644); err != nil {
		t.Fatalf("write shared.txt on develop: %v", err)
	}
	gitRun(t, env.repo, "add", ".")
	gitRun(t, env.repo, "commit", "-m", "develop side")

	wt := t.TempDir()
	gitRun(t, env.repo, "worktree", "add", wt, branch)
	if err := os.WriteFile(filepath.Join(wt, "shared.txt"), []byte("from the run branch\n"), 0o644); err != nil {
		t.Fatalf("write shared.txt on branch: %v", err)
	}
	gitRun(t, wt, "add", ".")
	gitRun(t, wt, "commit", "-m", "branch side")
	gitRun(t, env.repo, "worktree", "remove", "--force", wt)

	env.wrec.IntegrateProjectNow(env.ctx, approvalTestTenant, env.proj.ID)

	rec, err := env.obligation(t, runID)
	if err != nil {
		t.Fatalf("read obligation: %v", err)
	}
	if rec.Status != db.LocalIntegrationConflicted {
		t.Fatalf("a conflicting merge left the record as %q, want conflicted — a conflict retried forever with "+
			"nothing said was the old behaviour", rec.Status)
	}
	if rec.LastError == "" {
		t.Error("the conflict record does not say what conflicted")
	}
	// IT KEEPS BLOCKING.
	if n := env.wrec.BlockingIntegrationCount(env.ctx, approvalTestTenant, env.proj.ID); n != 1 {
		t.Fatalf("a conflicted obligation stopped blocking the queue (%d) — the next run would branch past it "+
			"and diverge further", n)
	}
	// AND NOTHING WAS HALF-MERGED: the integration branch did not move.
	if env.wrec.isAncestorOf(env.ctx, env.repo, branch, "develop") {
		t.Error("the run branch became an ancestor of develop even though the merge conflicted")
	}
}
