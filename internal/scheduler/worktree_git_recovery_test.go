package scheduler

// worktree_git_recovery_test.go — THE TWO REPOSITORY SHAPES THAT BROKE PROVISIONING.
//
// Both emerged from one attempt to start an execution on Windows, and both recorded the same useless sentence:
//
//	{"worktree_error": "git worktree add: exit status 128"}
//
// These tests are REAL GIT, in temp directories, because the defect lived in the interaction between git's
// ref layout and our resolution order — a stubbed `runGit` would have agreed with every assumption that was
// wrong. They are skipped when git is not installed, since a fake would defeat the point.
//
// THE REPOSITORIES ARE BUILT BY init + remote add + push -u, NEVER BY `git clone`. That is not a shortcut: a
// clone always creates `refs/remotes/origin/HEAD`, which is exactly the symref whose ABSENCE produced
// `fatal: invalid reference: origin/HEAD`. A test built on a clone cannot see the bug at all.

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// requireGit skips a test when git is unavailable rather than failing: these tests depend on a real git, and
// a machine without one cannot answer the question they ask.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed — this test exercises real git ref resolution")
	}
}

// gitIn runs git in dir with a hermetic identity, so the test does not depend on the operator's config.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Orchicon Test", "GIT_AUTHOR_EMAIL=test@localhost",
		"GIT_COMMITTER_NAME=Orchicon Test", "GIT_COMMITTER_EMAIL=test@localhost",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// handBuiltRepo builds the repository shape that defeated provisioning:
//
//	a bare origin, and a WORKING repo created by init + remote add + push -u (never clone).
//
// Nothing creates refs/remotes/origin/HEAD — the defining property of a hand-built repo, and the reason the
// resolver's first step must be able to observe its absence. The pushed branch is configurable so the test can
// also reproduce the dangling-`origin/HEAD` variant.
func handBuiltRepo(t *testing.T, pushedBranch string) (workDir, bareDir string) {
	t.Helper()
	root := t.TempDir()
	bareDir = filepath.Join(root, "origin.git")
	workDir = filepath.Join(root, "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, root, "init", "--bare", "--initial-branch="+pushedBranch, bareDir)
	gitIn(t, workDir, "init", "--initial-branch="+pushedBranch)
	gitIn(t, workDir, "remote", "add", "origin", bareDir)
	if err := os.WriteFile(filepath.Join(workDir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, workDir, "add", "README.md")
	gitIn(t, workDir, "commit", "-m", "initial")
	// push -u, NOT clone: the branch lands on origin with no remote-HEAD symref.
	gitIn(t, workDir, "push", "-u", "origin", pushedBranch)
	return workDir, bareDir
}

func testReconciler() *WorktreeReconciler {
	return &WorktreeReconciler{log: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))}
}

// TestResolveBaseRefSucceedsWithoutRemoteHeadSymref is defect 2's first acceptance criterion: resolution works
// with `origin/HEAD` ABSENT. This is the hand-built repository, and it must resolve to the pushed branch rather
// than to the echoed literal "origin/HEAD".
func TestResolveBaseRefSucceedsWithoutRemoteHeadSymref(t *testing.T) {
	requireGit(t)
	workDir, _ := handBuiltRepo(t, "develop")
	r := testReconciler()

	// The defining precondition: no remote-HEAD symref exists.
	if _, err := runGit(context.Background(), workDir, "symbolic-ref", "--quiet", "refs/remotes/origin/HEAD"); err == nil {
		t.Fatal("precondition failed: this fixture must NOT have refs/remotes/origin/HEAD — a clone would, " +
			"and the bug is invisible with one")
	}

	ref, how, err := r.resolveBaseRefWithProvenance(context.Background(), workDir)
	if err != nil {
		t.Fatalf("must resolve a base ref on a hand-built repo: %v", err)
	}
	if ref == "origin/HEAD" {
		t.Fatal("resolved to the literal string origin/HEAD — the rev-parse echo trap")
	}
	// `remote show origin` may or may not reach the remote here (a local path, so it will), and the
	// configured-default step also knows `develop`; either provenance is acceptable, but the RESULT must be a
	// ref that really is a commit.
	if ok, _ := r.refIsCommit(context.Background(), workDir, ref); !ok {
		t.Fatalf("resolved base %q (%s) is not a commit", ref, how)
	}
	if !strings.Contains(ref, "develop") {
		t.Errorf("base = %q (%s), want the repo's only branch (develop)", ref, how)
	}
}

// TestResolveBaseRefSurvivesDanglingBareHead is defect 2's second shape: `origin/HEAD -> master` where master
// does not exist. The symref RESOLVES, so a resolver that stops at it returns an invalid ref — this pins that
// the commit guard catches it and the chain moves on.
func TestResolveBaseRefSurvivesDanglingBareHead(t *testing.T) {
	requireGit(t)
	workDir, bareDir := handBuiltRepo(t, "develop")

	// Give the BARE origin a HEAD pointing at a branch that will never exist, and create the local
	// remote-HEAD symref that follows it — the exact "HEAD -> master, master does not exist" state.
	//
	// The symref is written DIRECTLY, not via `git remote set-head origin master`: that command refuses a
	// ref it cannot find ("error: Not a valid ref: refs/remotes/origin/master"), which is exactly why a
	// real hand-built repo ends up with this state through other means (a deleted branch on the remote, a
	// stale local symref, a bare repo whose HEAD was moved). Writing it directly is what reproduces the
	// shape the resolver has to survive.
	gitIn(t, bareDir, "symbolic-ref", "HEAD", "refs/heads/master")
	gitIn(t, workDir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/master")

	r := testReconciler()
	ref, how, err := r.resolveBaseRefWithProvenance(context.Background(), workDir)
	if err != nil {
		t.Fatalf("a dangling origin/HEAD must not fail resolution: %v", err)
	}
	if ref == "origin/HEAD" {
		t.Fatal("resolved to the literal string origin/HEAD")
	}
	if ok, _ := r.refIsCommit(context.Background(), workDir, ref); !ok {
		t.Fatalf("resolved base %q (%s) is not a commit — the guard did not catch a dangling symref", ref, how)
	}
}

// TestResolveBaseRefFailsNamingEveryAttempt is the acceptance criterion for the UNRESOLVABLE case: it must
// fail, and it must say what was tried. `could not resolve` alone would be the same silence as `exit status
// 128`, one layer up.
func TestResolveBaseRefFailsNamingEveryAttempt(t *testing.T) {
	requireGit(t)
	// An empty repository: no commit, no branch, no remote. Nothing can satisfy any step.
	dir := t.TempDir()
	gitIn(t, dir, "init", "--initial-branch=develop")

	r := testReconciler()
	_, _, err := r.resolveBaseRefWithProvenance(context.Background(), dir)
	if err == nil {
		t.Fatal("an unborn repository must not resolve a base ref")
	}
	msg := err.Error()
	for _, want := range []string{"origin/HEAD", "remote show origin", "configured defaults", "HEAD"} {
		if !strings.Contains(msg, want) {
			t.Errorf("failure must name the attempt %q; got: %s", want, msg)
		}
	}
	if !strings.Contains(msg, dir) {
		t.Errorf("failure must name the directory (a wrong cwd is the invisible Windows cause); got: %s", msg)
	}
}

// TestResolveBaseRefDetachedHeadIsNotAFailure pins the one legitimate empty result: a detached checkout
// resolves to HEAD position, and "" means "start at HEAD" — not "unresolved".
func TestResolveBaseRefDetachedHeadIsNotAFailure(t *testing.T) {
	requireGit(t)
	workDir, _ := handBuiltRepo(t, "develop")
	gitIn(t, workDir, "checkout", "--detach")

	r := testReconciler()
	// Remove the refs the chain prefers, so the detached-HEAD step is the one that answers.
	gitIn(t, workDir, "branch", "-D", "develop")
	gitIn(t, workDir, "remote", "remove", "origin")

	ref, how, err := r.resolveBaseRefWithProvenance(context.Background(), workDir)
	if err != nil {
		t.Fatalf("a detached HEAD is a legitimate state, not a resolution failure: %v", err)
	}
	if ref != "" {
		t.Errorf("base = %q, want empty (start at HEAD) for a detached checkout", ref)
	}
	if how != "detached-HEAD" {
		t.Errorf("provenance = %q, want detached-HEAD", how)
	}
}

// --- defect 1: the failure must carry git's own words -------------------------------------------------

// TestWorktreeProvisionFailureRecordsGitsFatalLine is defect 1's core: force the failure and prove the
// recorded error contains git's `fatal:` line — not an exit status, and not nothing.
func TestWorktreeProvisionFailureRecordsGitsFatalLine(t *testing.T) {
	requireGit(t)
	workDir, _ := handBuiltRepo(t, "develop")
	r := testReconciler()
	ctx := context.Background()

	// A base ref that cannot exist: this is what `git worktree add` refuses with a fatal line.
	perr := r.provisionWorktree(ctx, workDir, filepath.Join(t.TempDir(), "wt"), "orchicon-test-branch", "refs/heads/does-not-exist")
	if perr == nil {
		t.Fatal("provisioning from a nonexistent base ref must fail")
	}

	// THE RAW FAILURE carries git's output.
	var gitErr *GitError
	if !errors.As(perr, &gitErr) {
		t.Fatalf("error %v (%T) is not a *GitError — the captured output was thrown away again", perr, perr)
	}
	if !strings.Contains(gitErr.Output, "fatal:") {
		t.Errorf("captured output has no `fatal:` line: %q", gitErr.Output)
	}
	if fatal := gitErr.FatalLine(); fatal == "" {
		t.Errorf("FatalLine() found no cause in %q", gitErr.Output)
	}

	// AND THE RECORDED MESSAGE carries it too — plus branch, base ref and directory, which is the
	// acceptance criterion in full.
	recorded := worktreeProvisionError(perr, "local", "orchicon-test-branch", "refs/heads/does-not-exist", workDir)
	for _, want := range []string{"orchicon-test-branch", "refs/heads/does-not-exist", workDir, "fatal"} {
		if !strings.Contains(recorded, want) {
			t.Errorf("recorded failure must contain %q; got: %s", want, recorded)
		}
	}
	if strings.Contains(recorded, "git worktree add: exit status") {
		t.Errorf("recorded failure still reduces to an exit status: %s", recorded)
	}
}

// TestFailedProvisionLeavesNoOrphanedArtifacts is the last acceptance criterion: after a forced failure there is
// no stray branch and no half-built directory for the next attempt to trip over.
func TestFailedProvisionLeavesNoOrphanedArtifacts(t *testing.T) {
	requireGit(t)
	workDir, _ := handBuiltRepo(t, "develop")
	r := testReconciler()
	ctx := context.Background()

	path := filepath.Join(t.TempDir(), "wt")
	const branch = "orchicon-test-orphan"
	perr := r.provisionWorktree(ctx, workDir, path, branch, "refs/heads/nope")
	if perr == nil {
		t.Fatal("provisioning from a nonexistent base ref must fail")
	}
	leftovers := r.cleanupFailedProvision(ctx, workDir, path, branch, true)
	if len(leftovers) > 0 {
		t.Fatalf("cleanup reported leftovers: %v", leftovers)
	}
	if r.branchExists(ctx, workDir, branch) {
		t.Errorf("branch %q survived a failed provision", branch)
	}
	if _, err := os.Stat(path); err == nil {
		t.Errorf("worktree directory %s survived a failed provision", path)
	}
	// And the repository is usable afterwards: a good provision on the same name succeeds.
	if err := r.provisionWorktree(ctx, workDir, path, branch, "develop"); err != nil {
		t.Errorf("a later provision must not be blocked by the failed attempt: %v", err)
	}
}

// TestSuccessfulProvisionIsUnchanged is the regression guard for the success path: everything above must have
// changed only how failures are handled.
func TestSuccessfulProvisionIsUnchanged(t *testing.T) {
	requireGit(t)
	workDir, _ := handBuiltRepo(t, "develop")
	r := testReconciler()
	ctx := context.Background()

	base, _, err := r.resolveBaseRefWithProvenance(ctx, workDir)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	path := filepath.Join(t.TempDir(), "wt")
	const branch = "orchicon-test-happy"
	if err := r.provisionWorktree(ctx, workDir, path, branch, base); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if _, err := os.Stat(filepath.Join(path, "README.md")); err != nil {
		t.Errorf("worktree was not populated from the base: %v", err)
	}
	if !r.branchExists(ctx, workDir, branch) {
		t.Errorf("branch %q was not created", branch)
	}
}
