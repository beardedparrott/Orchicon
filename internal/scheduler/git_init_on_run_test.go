package scheduler

// git_init_on_run_test.go — A MISSING REPOSITORY IS INITIALISED, AND SAID SO AS A NOTICE.
//
// REPORTED: a project created through Ask Orchicon had no git repository (the Ask tool writes the project row
// directly, and the project SERVICE is the only caller of EnsureGitState — so no `git init` ever ran). The first
// run then found no work tree, was SKIPPED, and had its reason written into workflow_runs.error_message — which
// the run page renders in red as a failure. Meanwhile the DAG gate treats "skipped" as dispatchable, so the
// worker carried on. The operator saw a failed run with a working worker and aborted it.
//
// Two behaviours are pinned here, both from the operator's own instruction:
//
//	3. "on workflow runs, if set to any git type strategy with no local git initialized, run a git init"
//	4. "change from an error to a warning clearly stating what happened and what it did to remedy it"

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
)

// nonRepoProject builds a DB-backed project whose directory is NOT a git repository — the reported state.
//
// It uses the queue harness because the fix is reached through reconcileOne, which needs the pool, the project
// row and a pending run; the pure-git harness has none of those.
func nonRepoProject(t *testing.T, strategy string) (*queueTestEnv, string, string) {
	t.Helper()
	env := newQueueTestEnv(t)
	// OUTSIDE ANY REPOSITORY, deliberately. t.TempDir() lives under the test binary's temp root, which on this
	// project IS inside the Orchicon checkout — so `git rev-parse` there finds the ENCLOSING repository and the
	// directory looks git-backed. A fixture that claims "not a repo" has to actually not be in one.
	dir, err := os.MkdirTemp("", "orchicon-nonrepo-")
	if err != nil {
		t.Fatalf("mktemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# not a repo\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Retarget the project at the bare directory. Version is taken from the row, so the optimistic-concurrency
	// guard is satisfied rather than guessed.
	ttx, terr := env.pool.BeginTenantTx(env.ctx, approvalTestTenant)
	if terr != nil {
		t.Fatalf("begin: %v", terr)
	}
	defer ttx.Rollback(env.ctx)
	cur, err := db.GetProject(env.ctx, ttx.Tx, approvalTestTenant, env.proj.ID)
	if err != nil {
		t.Fatalf("read project: %v", err)
	}
	updated, err := db.UpdateProject(env.ctx, ttx.Tx, approvalTestTenant, env.proj.ID, cur.Version,
		db.UpdateProjectFields{ProjectDir: &dir, GitStrategy: &strategy})
	if err != nil {
		t.Fatalf("retarget project: %v", err)
	}
	// THE CACHE MUST NOT REMEMBER THE OLD DIRECTORY, or the run would be judged against the harness repo.
	if _, err := db.UpdateProjectGitDetection(env.ctx, ttx.Tx, approvalTestTenant, env.proj.ID, updated.Version,
		false, ""); err != nil {
		t.Fatalf("clear git detection: %v", err)
	}
	if err := ttx.Commit(env.ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return env, env.seedPendingRun(t), dir
}

// A `local` STRATEGY WITH NO REPOSITORY GETS ONE, and the run then branches normally.
func TestARunInitialisesAMissingRepository(t *testing.T) {
	env, runID, dir := nonRepoProject(t, "local")
	ctx := env.ctx

	// Premise: no repository.
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		t.Fatal("test premise wrong: the directory is already a repository")
	}

	if err := env.wrec.reconcileOne(ctx, approvalTestTenant, runID); err != nil {
		t.Fatalf("reconcileOne: %v", err)
	}

	// 1. THE REPOSITORY NOW EXISTS.
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Fatalf("the platform did not initialise a repository for a local-strategy run: %v", err)
	}
	// 2. AND THE RUN PROCEEDED — not skipped, not failed: it has a real worktree on a real branch.
	run := env.run(t, runID)
	if run.WorktreeStatus != domain.WorktreeReady {
		t.Fatalf("worktree_status = %q after initialising the repository, want ready (a repaired project must "+
			"proceed exactly like a set-up one); error=%q", run.WorktreeStatus, run.ErrorMessage)
	}
	if run.WorktreeBranch == "" {
		t.Fatal("the run got a worktree but no branch")
	}
	// 3. AND THE CACHED DETECTION WAS REFRESHED, or the next pass would re-detect from a stale fact.
	if !env.wrec.isInsideWorkTree(ctx, dir) {
		t.Fatal("the directory is a repository but isInsideWorkTree disagrees")
	}
}

// REQUIREMENT 4: THE MESSAGE IS A NOTICE, NOT A FAILURE.
//
// A `none` project is the case that has nothing to repair (no repository is required by design) and therefore
// takes the notice path directly — and it is also the case that produced the red card.
func TestANonRepoRunIsANoticeNotAFailure(t *testing.T) {
	env, runID, _ := nonRepoProject(t, "none")
	ctx := env.ctx

	if err := env.wrec.reconcileOne(ctx, approvalTestTenant, runID); err != nil {
		t.Fatalf("reconcileOne: %v", err)
	}
	run := env.run(t, runID)
	// The run is SKIPPED — the supported "works in place" path — not failed.
	if run.WorktreeStatus != domain.WorktreeSkipped {
		t.Fatalf("worktree_status = %q, want skipped", run.WorktreeStatus)
	}
	// AND ITS MESSAGE IS CLASSED AS A NOTICE, which is what stops the UI painting it red.
	if run.ErrorKind != worktreeNoticeKind {
		t.Fatalf("error_kind = %q, want %q — a skipped run whose reason is written to error_message without a "+
			"notice kind renders as a RED FAILURE CARD while its worker carries on, which is exactly what made "+
			"an operator abort a healthy run", run.ErrorKind, worktreeNoticeKind)
	}
	// THE TEXT SAYS WHAT HAPPENED AND WHAT WAS DONE — the operator's requirement for the wording.
	msg := run.ErrorMessage
	for _, want := range []string{"not a git work tree", "ephemeral", "in place", "exactly as configured"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the notice does not say %q:\n%s", want, msg)
		}
	}
	// AND IT DOES NOT CLAIM A FAILURE. "failed"/"error"/"could not" would reproduce the misleading card even
	// under a notice kind.
	lower := strings.ToLower(msg)
	for _, banned := range []string{"failed", " failure"} {
		if strings.Contains(lower, banned) {
			t.Errorf("the notice says %q, which reads as a failure:\n%s", banned, msg)
		}
	}
}

// A REPAIR THAT CANNOT HAPPEN IS STILL NOT A FAILURE — the run proceeds in place and says why.
func TestAnUnrepairableRepositoryIsANoticeToo(t *testing.T) {
	env, runID, dir := nonRepoProject(t, "local")
	ctx := env.ctx
	// Make the directory unwritable so `git init` cannot succeed, without removing it.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Skipf("cannot chmod (running as a user that ignores mode bits?): %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	if err := env.wrec.reconcileOne(ctx, approvalTestTenant, runID); err != nil {
		t.Fatalf("reconcileOne: %v", err)
	}
	run := env.run(t, runID)
	if run.WorktreeStatus == domain.WorktreeFailed {
		t.Fatalf("an unrepairable repository FAILED the run (%q) — it must skip and work in place",
			run.ErrorMessage)
	}
	if run.ErrorKind != worktreeNoticeKind {
		t.Fatalf("error_kind = %q, want the notice kind", run.ErrorKind)
	}
	if !strings.Contains(run.ErrorMessage, "IN PLACE") && !strings.Contains(run.ErrorMessage, "in place") {
		t.Errorf("the notice does not say the run proceeds in place:\n%s", run.ErrorMessage)
	}
}
