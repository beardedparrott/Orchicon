package main

// cleanup_script_safety_test.go — the sweep may remove a worktree only when its content exists
// elsewhere, and the report must not promise space the sweep will keep.

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

// guardFreePath drops the agent runtime's guard shim from PATH.
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

// A REFUSED REMOVAL MUST NAME ITS CAUSE.
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

// runCleanupCode also reports the exit status, for the paths that must REFUSE.
func runCleanupCode(t *testing.T, script string, env []string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(script, args...)
	cmd.Env = append(append(os.Environ(), "PATH="+guardFreePath()), env...)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("orchicon-cleanup.sh %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out), code
}

// THE SCOPE OF THIS SCRIPT IS CACHES AND LEFTOVERS.
func TestCleanupRejectsStateCategories(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns the script six times")
	}
	repo, script := cleanupSandbox(t)
	leaked := filepath.Join(repo, ".orchicon-worktrees", "leaked")
	if err := os.MkdirAll(leaked, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, flag := range []string{"--containers", "--telemetry", "--sessions", "--backups", "--instance-data", "--instance"} {
		out, code := runCleanupCode(t, script, nil, flag, "--apply")

		if code != 2 {
			t.Errorf("%s must be refused with exit 2, got %d:\n%s", flag, code, out)
		}
		if !strings.Contains(out, "gone") {
			t.Errorf("%s was refused without saying why — an operator who remembers the flag will read the "+
				"silence as a typo:\n%s", flag, out)
		}
		if _, err := os.Stat(leaked); err != nil {
			t.Fatalf("%s reached a removal before being refused, so a rejected flag still deleted something", flag)
		}
	}
}

// shellWithoutComments strips `#` comments so a source assertion cannot be satisfied by prose.
func shellWithoutComments(src string) []string {
	var out []string
	for _, line := range strings.Split(src, "\n") {
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		out = append(out, line)
	}
	return out
}

// THE STRUCTURE, not just the flags: nothing in this script may reach a container engine or an
// instance data dir.
func TestCleanupRemovalSurfaceIsCachesAndLeftoversOnly(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts", "orchicon-cleanup.sh"))
	if err != nil {
		t.Fatalf("read the cleanup script: %v", err)
	}
	lines := shellWithoutComments(string(body))

	for _, tok := range []string{"docker", "podman"} {
		for i, line := range lines {
			if strings.Contains(line, tok) {
				t.Errorf("line %d talks to a container engine (%q): containers are runtime state, not a cache, "+
					"and removing them belongs to the instance tooling", i+1, strings.TrimSpace(line))
			}
		}
	}

	// The only path inside an instance's data dir this may touch is the ROTATED log. `services.log`
	// itself does not match the `services.log.*` glob, which is what keeps the live log safe.
	for i, line := range lines {
		if !strings.Contains(line, ".local/share/orchicon-") {
			continue
		}
		if !strings.Contains(line, "services.log.") {
			t.Errorf("line %d reaches into an instance data dir, which holds the live database and secrets: %q",
				i+1, strings.TrimSpace(line))
		}
	}
}

// AND THE LIVE LOG SURVIVES. The user blessed cleaning rotated logs; the one being written is not one.
func TestCleanupNeverRemovesTheLiveServicesLog(t *testing.T) {
	repo, script := cleanupSandbox(t)
	home := t.TempDir()
	inst := filepath.Join(home, ".local", "share", "orchicon-dev")
	if err := os.MkdirAll(inst, 0o755); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(inst, "services.log")
	rotated := filepath.Join(inst, "services.log.20261108-1200")
	for _, f := range []string{live, rotated} {
		if err := os.WriteFile(f, []byte("line\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	out, code := runCleanupCode(t, script, []string{"HOME=" + home}, "--logs", "--apply", "--project-root", repo)
	if code != 0 {
		t.Fatalf("--logs failed (%d):\n%s", code, out)
	}

	if _, err := os.Stat(live); err != nil {
		t.Fatalf("the LIVE log was removed — it is being appended to by the running plane:\n%s", out)
	}
	if _, err := os.Stat(rotated); err == nil {
		t.Fatalf("a rotated services.log.* must be reclaimed:\n%s", out)
	}
}
