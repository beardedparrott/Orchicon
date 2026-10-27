package project

// git_setup_branch_test.go — THE INTEGRATION BRANCH, AND THE TWO CONDITIONS THAT ONLY WARN.
//
// The gap this closes was found by the operator asking "will it automatically build the develop branch?" — and
// the honest answer then was "no": `git init` puts the bootstrap commit on git's DEFAULT branch (`master` on a
// host with no `init.defaultBranch`), while every git rule this platform injects into a worker's prompt and
// every canned worker names `develop` as where work lands. The old code only named a branch when HEAD was
// DETACHED — which on the normal fresh-repository path never happens, so the one case that needed it was the one
// case that did not get it.
//
// The other two conditions do NOT stop a project being created; they stop its runs doing what the strategy
// promises. They are therefore warnings, and they are loud.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFreshRepositoryLandsOnTheIntegrationBranch is the reported gap, directly.
func TestFreshRepositoryLandsOnTheIntegrationBranch(t *testing.T) {
	withHermeticGit(t)
	dir := tempDirOutsideAnyRepo(t)

	rep := EnsureGitState(context.Background(), dir, "pr")
	if rep.Err != nil {
		t.Fatalf("setup failed: %v", rep.Err)
	}

	branch := strings.TrimSpace(mustGit(t, dir, "symbolic-ref", "--short", "HEAD"))
	if branch != integrationBranchName {
		t.Errorf("a fresh repository is on branch %q, want %q — runs are cut from and land on the integration "+
			"branch, and git's own default (master/main) is not it", branch, integrationBranchName)
	}
	// The branch EXISTS as a commit, not merely as a configured name.
	if ok, err := branchCommitExists(context.Background(), dir, integrationBranchName); err != nil || !ok {
		t.Errorf("the integration branch does not resolve to a commit: ok=%v err=%v", ok, err)
	}
	// AND THE REPOSITORY'S OWN DEFAULT AGREES, so this directory's next `git init` does too.
	if got := strings.TrimSpace(mustGit(t, dir, "config", "init.defaultBranch")); got != integrationBranchName {
		t.Errorf("init.defaultBranch = %q, want %q", got, integrationBranchName)
	}
	// THE RUN OPERATION THAT MATTERS works from it.
	if out, err := gitOut(context.Background(), dir, "worktree", "add", filepath.Join(tempDirOutsideAnyRepo(t), "wt"), "-b", "run-1", integrationBranchName); err != nil {
		t.Errorf("a run branch cannot be cut from %s: %v\n%s", integrationBranchName, err, out)
	}
}

// TestExistingRepositoryOnAnotherBranchKeepsItsBranch is the line this must not cross: the integration branch is
// created for runs, but the operator's own checkout stays where they put it.
func TestExistingRepositoryOnAnotherBranchKeepsItsBranch(t *testing.T) {
	withHermeticGit(t)
	dir := tempDirOutsideAnyRepo(t)
	mustGit(t, dir, "init", "-b", "trunk")
	mustGit(t, dir, "config", "user.name", "The Operator")
	mustGit(t, dir, "config", "user.email", "operator@example.invalid")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, dir, "add", "README.md")
	mustGitAs(t, dir, "The Operator", "operator@example.invalid", "commit", "-m", "mine")

	rep := EnsureGitState(context.Background(), dir, "pr")
	if rep.Err != nil {
		t.Fatalf("setup failed on a healthy repo: %v", rep.Err)
	}
	if branch := strings.TrimSpace(mustGit(t, dir, "symbolic-ref", "--short", "HEAD")); branch != "trunk" {
		t.Errorf("the operator's branch was changed to %q — the integration branch exists for runs to be cut "+
			"from, which does not require moving their HEAD", branch)
	}
	// But the integration branch now exists for those runs.
	if ok, _ := branchCommitExists(context.Background(), dir, integrationBranchName); !ok {
		t.Errorf("%s was not created, so runs have nothing to be cut from", integrationBranchName)
	}
	// AND THEIR HISTORY DID NOT MOVE.
	if n := mustGit(t, dir, "rev-list", "--count", "--all"); n != "1" {
		t.Errorf("commit count = %s, want 1 — setup must never add a commit to an existing repository", n)
	}
}

// TestMissingRemoteIsReportedLoudly pins the condition that breaks what `local` and `pr` PROMISE: both need an
// origin, and without one the work is done and then cannot be delivered — at the end of a run.
func TestMissingRemoteIsReportedLoudly(t *testing.T) {
	withHermeticGit(t)
	dir := tempDirOutsideAnyRepo(t)

	rep := EnsureGitState(context.Background(), dir, "local")
	if rep.Err != nil {
		t.Fatalf("a missing remote must not be an error — the project must still be creatable: %v", rep.Err)
	}
	if rep.Warning == "" {
		t.Fatal("no warning for a repository with no origin — the local strategy cannot push without one")
	}
	for _, want := range []string{"origin", "local", "push"} {
		if !strings.Contains(rep.Warning, want) {
			t.Errorf("warning must name %q; got: %s", want, rep.Warning)
		}
	}
	if !rep.Loud() {
		t.Error("the report must be Loud() so the service surfaces it in the transcript, not only the log")
	}
	// The summary carries it too, so every rendering of the report includes it.
	if !strings.Contains(rep.Summary(), "origin") {
		t.Errorf("Summary() dropped the warning: %s", rep.Summary())
	}
}

// TestWithRemoteTheReportIsQuiet is the other side: once an origin exists, nothing is warned about.
func TestWithRemoteTheReportIsQuiet(t *testing.T) {
	withHermeticGit(t)
	dir := tempDirOutsideAnyRepo(t)
	// A bare origin must be created FROM ITS PARENT: `git -C <path> init --bare` requires the path to exist
	// already, and `git init --bare <path>` creates it. (This is the same -C convention gitOut uses.)
	originParent := tempDirOutsideAnyRepo(t)
	origin := filepath.Join(originParent, "origin.git")
	mustGit(t, originParent, "init", "--bare", origin)
	if err := EnsureGitState(context.Background(), dir, "pr").Err; err != nil {
		t.Fatalf("setup: %v", err)
	}
	mustGit(t, dir, "remote", "add", "origin", origin)

	rep := EnsureGitState(context.Background(), dir, "pr")
	if rep.Err != nil {
		t.Fatalf("setup: %v", rep.Err)
	}
	if rep.Warning != "" {
		t.Errorf("warned about a remote that exists: %s", rep.Warning)
	}
}

// TestNoneStrategyIsNotWarnedAboutARemote pins that the warning is about the STRATEGY's promise: an ephemeral run
// keeps no branch and needs no remote, so warning would be noise on a strategy working as designed.
func TestNoneStrategyIsNotWarnedAboutARemote(t *testing.T) {
	withHermeticGit(t)
	rep := EnsureGitState(context.Background(), tempDirOutsideAnyRepo(t), "none")
	if rep.Warning != "" || rep.Needed {
		t.Errorf("the none strategy must be a no-op: needed=%v warning=%q", rep.Needed, rep.Warning)
	}
}

// TestMissingGitIsItsOwnLoudState pins that "git is not installed" is distinguishable from a repository problem.
//
// The operator's action differs completely — install git versus fix a repository — and on Windows this is the
// realistic shape: git inside WSL, while `orchicon serve` runs on the native host.
func TestMissingGitIsItsOwnLoudState(t *testing.T) {
	// Emptying PATH makes exec.LookPath fail without touching anything else.
	t.Setenv("PATH", "")
	dir := os.TempDir()

	rep := EnsureGitState(context.Background(), dir, "pr")
	if rep.Err == nil {
		t.Fatal("setup with no git on the host must report a failure")
	}
	if !rep.GitMissing {
		t.Error("the report must set GitMissing, so the operator is told to install git rather than to look at " +
			"their directory")
	}
	if !rep.Loud() {
		t.Error("a host without git must be reported loudly")
	}
	if !strings.Contains(rep.Err.Error(), "git is not installed") {
		t.Errorf("the failure must name git: %v", rep.Err)
	}
}
