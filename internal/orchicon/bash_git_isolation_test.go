package orchicon

// A worker's bash must not be able to reach the operator's repository through git's environment.
//
// THE INCIDENT. A worker's `go test` ran fixtures that shell out to `git init` / `git config` in a
// t.TempDir(). The worker's environment carried GIT_DIR and GIT_WORK_TREE aimed at ITS worktree
// (internal/runtime/agent.go: gitEnvFor) so container git can resolve a pointer file — and those
// variables override `-C`, so the fixtures wrote into the MAIN repository instead of the temp dir:
//
//	core.worktree = <worker worktree>      -> redirected every later git command in the operator's
//	                                          checkout, which is why `git pull` reported conflicts in
//	                                          files they had never touched
//	user.email = worktree-test@orchicon.dev -> their commit identity
//
// Scrubbing the variables from the bash environment covers every invocation at once, including the
// fixtures inside a worktree cut from before any source fix.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitConfigAfterInit runs `git init -q` in a fresh directory under a given environment and returns the
// MAIN repo's .git/config afterwards. This is the reproduction in miniature.
func gitConfigAfterInit(t *testing.T, mainRepo, freshDir string, env []string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", freshDir, "init", "-q")
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v — %s", err, out)
	}
	b, err := os.ReadFile(filepath.Join(mainRepo, ".git", "config"))
	if err != nil {
		t.Fatalf("read main config: %v", err)
	}
	return string(b)
}

// mainRepo builds a repository with one commit, for use as the operator's checkout.
func mainRepoForGitTest(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	env := cleanGitTestEnv(t)
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.name", "Operator"},
		{"config", "user.email", "operator@example.test"},
		{"commit", "-q", "--allow-empty", "-m", "base"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v — %s", args, err, out)
		}
	}
	return dir
}

// cleanGitTestEnv is the test's own environment with git pointers removed, so building fixtures cannot
// itself leak into the real checkout.
func cleanGitTestEnv(t *testing.T) []string {
	t.Helper()
	out := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GIT_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// THE TEST THAT MATTERS: with the worker's real environment (pointers present), the scrub must prevent
// `git init` from writing the shared repo.
func TestBashGitPointersCannotReachTheOperatorRepo(t *testing.T) {
	main := mainRepoForGitTest(t)
	before, err := os.ReadFile(filepath.Join(main, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}

	// A second worktree of the shared repo — the shape that makes git WRITE `worktree = …` rather than
	// silently reinitialising (see internal/gitenv's Repro A).
	second := filepath.Join(t.TempDir(), "run-worktree")
	{
		cmd := exec.Command("git", "-C", main, "worktree", "add", "-q", "--detach", second)
		cmd.Env = cleanGitTestEnv(t)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("worktree add: %v — %s", err, out)
		}
	}
	pointer := filepath.Join(main, ".git", "worktrees", "run-worktree")

	// The worker's environment as the plane builds it, before the scrub.
	workerEnv := append(cleanGitTestEnv(t),
		"GIT_DIR="+pointer,
		"GIT_WORK_TREE="+second,
		"XDG_DATA_HOME=/should-be-repointed",
	)

	// THE SCRUB UNDER TEST.
	scrubbed := sandboxedBashEnviron(workerEnv, t.TempDir())

	fresh := filepath.Join(t.TempDir(), "test-fixture-repo")
	if err := os.MkdirAll(fresh, 0o755); err != nil {
		t.Fatal(err)
	}
	after := gitConfigAfterInit(t, main, fresh, scrubbed)

	if after != string(before) {
		t.Errorf("a worker's bash reached the operator's repository config.\n--- before ---\n%s\n--- after ---\n%s",
			before, after)
	}
	// And the fixture's own repo must exist where it asked.
	if _, statErr := os.Stat(filepath.Join(fresh, ".git")); statErr != nil {
		t.Errorf("git init did not create a repository in the fixture dir: %v — the command was "+
			"redirected elsewhere", statErr)
	}

	// The control: WITHOUT the scrub the damage happens. If this stops reproducing, the test above has
	// stopped proving anything and must be revisited.
	control := filepath.Join(t.TempDir(), "unsanitized-fixture")
	if err := os.MkdirAll(control, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := gitConfigAfterInit(t, main, control, workerEnv); got == string(before) {
		t.Logf("NOTE: the unsanitized control no longer reproduces the redirect on this git version — " +
			"the sanitizer assertion above is still valid, but its control has gone stale on this host")
	}
}

// The scrub must remove the pointers and repoint the plane state root, and leave everything else alone.
func TestBashEnvironScrubsGitPointers(t *testing.T) {
	base := []string{
		"PATH=/usr/bin",
		"GIT_DIR=/shared/.git/worktrees/run",
		"GIT_WORK_TREE=/somewhere/run",
		"GIT_INDEX_FILE=/tmp/idx",
		"GIT_AUTHOR_NAME=Orchicon",
		"ORCHICON_DATA_DIR=/plane/state",
	}
	got := strings.Join(sandboxedBashEnviron(base, t.TempDir()), "\n")
	for _, banned := range []string{"GIT_DIR=", "GIT_WORK_TREE=", "GIT_INDEX_FILE=", "ORCHICON_DATA_DIR=/plane/state"} {
		if strings.Contains(got, banned) {
			t.Errorf("%s survived the scrub", banned)
		}
	}
	for _, keep := range []string{"PATH=/usr/bin", "GIT_AUTHOR_NAME=Orchicon"} {
		if !strings.Contains(got, keep) {
			t.Errorf("%s was removed but must pass through", keep)
		}
	}
	// The scratch redirection still happens.
	if !strings.Contains(got, "ORCHICON_DATA_DIR=") {
		t.Errorf("the worker's data dir was not repointed at its scratch")
	}
}
