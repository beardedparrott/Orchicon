package main

// cleanup_script_safety_test.go — the sweep may remove a worktree only when its content exists elsewhere,
// and the report must not promise space the sweep will keep.
//
// Both properties were asserted by the script's own header and by nothing else, so both were false: a clean
// DETACHED-HEAD worktree holds commits that appear on no branch, and `git worktree remove` left them with
// nothing to recover from; and the inventory summed every worktree while labelling the total "swept by
// --worktrees". A destructive script whose only guard is a comment is a script with no guard.
//
// Each test copies the script beside the sandbox so REPO_ROOT resolves there: the inventory otherwise reads
// the operator's real .dev/tools and telemetry, which is both slow and ambient.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// sandboxCleanup copies the script to <sandbox>/scripts/ so REPO_ROOT is the sandbox, not this checkout.
func sandboxCleanup(t *testing.T, sandbox string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts", "orchicon-cleanup.sh"))
	if err != nil {
		t.Fatalf("read the cleanup script: %v", err)
	}
	dir := filepath.Join(sandbox, "scripts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "orchicon-cleanup.sh")
	if err := os.WriteFile(dst, body, 0o755); err != nil {
		t.Fatal(err)
	}
	return dst
}

// guardFreePath drops the agent runtime's guard shim from PATH. The shim BLOCKS `rm` whenever the guard
// env is absent — and this package's init() unsets exactly that env — so the script would meet a refusal
// here that never exists in CI.
func guardFreePath() string {
	var kept []string
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		if strings.Contains(d, "orchicon-guard") {
			continue
		}
		kept = append(kept, d)
	}
	return strings.Join(kept, string(os.PathListSeparator))
}

func runCleanup(t *testing.T, script string, args ...string) string {
	t.Helper()
	cmd := exec.Command(script, args...)
	cmd.Env = append(os.Environ(), "PATH="+guardFreePath())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("orchicon-cleanup.sh %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", dir, "-c", "user.email=test@example.invalid", "-c", "user.name=test"}, args...)
	out, err := exec.Command("git", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// cleanupSandbox is a repo on develop with a .orchicon-worktrees dir and the script beside it.
func cleanupSandbox(t *testing.T) (repo, script string) {
	t.Helper()
	sandbox := t.TempDir()
	repo = filepath.Join(sandbox, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".orchicon-worktrees"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "init", "-q", "-b", "develop")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", ".")
	gitIn(t, repo, "commit", "-qm", "base")
	return repo, sandboxCleanup(t, sandbox)
}

// A commit that exists on no branch is the one thing the header promises never to destroy.
func TestCleanupKeepsADetachedHeadWorktreeWithUniqueCommits(t *testing.T) {
	repo, script := cleanupSandbox(t)
	wt := filepath.Join(repo, ".orchicon-worktrees", "detached")
	gitIn(t, repo, "worktree", "add", "-q", "--detach", wt, "develop")
	if err := os.WriteFile(filepath.Join(wt, "only-here.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, wt, "add", ".")
	gitIn(t, wt, "commit", "-qm", "exists on no branch")

	runCleanup(t, script, "--worktrees", "--apply", "--project-root", repo)

	if _, err := os.Stat(filepath.Join(wt, "only-here.txt")); err != nil {
		t.Fatalf("the sweep removed a clean detached-HEAD worktree whose only commit is on no branch, so that "+
			"commit now exists nowhere and there is nothing left to recover it from (%v)", err)
	}
}

// THE CONTROLS. Without these the test above passes by removing nothing at all.
func TestCleanupRemovesACleanMergedWorktree(t *testing.T) {
	repo, script := cleanupSandbox(t)
	wt := filepath.Join(repo, ".orchicon-worktrees", "merged")
	gitIn(t, repo, "worktree", "add", "-q", "-b", "feat-merged", wt, "develop")

	out := runCleanup(t, script, "--worktrees", "--apply", "--project-root", repo)

	if _, err := os.Stat(wt); err == nil {
		t.Fatalf("a clean worktree merged into develop must be reclaimed — nothing is lost by removing it:\n%s", out)
	}
}

func TestCleanupKeepsAWorktreeWithUncommittedChanges(t *testing.T) {
	repo, script := cleanupSandbox(t)
	wt := filepath.Join(repo, ".orchicon-worktrees", "dirty")
	gitIn(t, repo, "worktree", "add", "-q", "-b", "feat-dirty", wt, "develop")
	if err := os.WriteFile(filepath.Join(wt, "work.txt"), []byte("uncommitted\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := runCleanup(t, script, "--worktrees", "--apply", "--project-root", repo)

	if _, err := os.Stat(filepath.Join(wt, "work.txt")); err != nil {
		t.Fatalf("an uncommitted file exists nowhere else and must survive the sweep:\n%s", out)
	}
	if !strings.Contains(out, "uncommitted changes") {
		t.Errorf("the sweep kept it but did not say why:\n%s", out)
	}
}

// A directory whose registration is gone is the leaked shape and IS reclaimed.
func TestCleanupRemovesALeakedWorktreeDir(t *testing.T) {
	repo, script := cleanupSandbox(t)
	leaked := filepath.Join(repo, ".orchicon-worktrees", "leaked")
	if err := os.MkdirAll(leaked, 0o755); err != nil {
		t.Fatal(err)
	}

	out := runCleanup(t, script, "--worktrees", "--apply", "--project-root", repo)

	if _, err := os.Stat(leaked); err == nil {
		t.Fatalf("a worktree dir with no git registration outlived its run and must be reclaimed:\n%s", out)
	}
}

// THE NUMBER THE OPERATOR DECIDES ON: kept worktrees must not be counted as reclaimable.
func TestCleanupInventoryCountsOnlyReclaimableWorktrees(t *testing.T) {
	repo, script := cleanupSandbox(t)
	merged := filepath.Join(repo, ".orchicon-worktrees", "merged")
	gitIn(t, repo, "worktree", "add", "-q", "-b", "feat-merged", merged, "develop")

	unmerged := filepath.Join(repo, ".orchicon-worktrees", "unmerged")
	gitIn(t, repo, "worktree", "add", "-q", "-b", "feat-unmerged", unmerged, "develop")
	if err := os.WriteFile(filepath.Join(unmerged, "wip.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, unmerged, "add", ".")
	gitIn(t, unmerged, "commit", "-qm", "not merged anywhere")

	out := runCleanup(t, script, "--project-root", repo)

	if !strings.Contains(out, "worktrees (2, 1 reclaimable)") {
		t.Fatalf("the inventory must report 2 worktrees with only 1 reclaimable — the unmerged one is kept:\n%s", out)
	}
}

// A REFUSED REMOVAL MUST NAME ITS CAUSE. rm's stderr was discarded and every failure — a session guard
// rejecting the command, an immutable file, a read-only parent — printed as "(permissions?)", which is
// right for exactly one of them.
func TestCleanupNamesWhyARemovalFailed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions, so the removal cannot be made to fail")
	}
	repo, script := cleanupSandbox(t)
	leaked := filepath.Join(repo, ".orchicon-worktrees", "leaked")
	if err := os.MkdirAll(leaked, 0o755); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Dir(leaked)
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })

	out := runCleanup(t, script, "--worktrees", "--apply", "--project-root", repo)

	if !strings.Contains(out, "could not remove") {
		t.Fatalf("a read-only parent must make the removal fail:\n%s", out)
	}
	if strings.Contains(out, "(permissions?)") {
		t.Fatalf("the failure was reported as the guess \"(permissions?)\" instead of rm's own reason:\n%s", out)
	}
	if !strings.Contains(strings.ToLower(out), "permission denied") {
		t.Errorf("the report should carry rm's message so the operator can act on it:\n%s", out)
	}
}
