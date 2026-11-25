package gitenv_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/gitenv"
)

// The three reproductions below are the actual incident, not an illustration. Each ran against a real
// repository and produced the stated damage. They are the reason this package exists, and they are
// written so that FAILING them means the operator's checkout is writable by a worker again.

// git runs one git command with an explicit environment.
func git(t *testing.T, dir string, env []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// mainRepo builds a repository with one commit, for use as "the operator's checkout".
func mainRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.name", "Operator"},
		{"config", "user.email", "operator@example.test"},
		{"commit", "-q", "--allow-empty", "-m", "base"},
	} {
		if out, err := git(t, dir, gitenv.CleanEnviron(), args...); err != nil {
			t.Fatalf("git %v: %v — %s", args, err, out)
		}
	}
	return dir
}

// pointerEnv is the environment a worker's child process carries: GIT_DIR/GIT_WORK_TREE aimed at the
// operator's repository, exactly as internal/runtime/agent.go's gitEnvFor produces.
func pointerEnv(t *testing.T, main string) []string {
	t.Helper()
	env := gitenv.Clean(os.Environ())
	return append(env, "GIT_DIR="+filepath.Join(main, ".git"), "GIT_WORK_TREE="+main)
}

// mainRepoInTemp builds a named repository under its own temp dir.
func mainRepoInTemp(t *testing.T, name string) string {
	t.Helper()
	d := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.name", "T"},
		{"config", "user.email", "t@localhost"},
	} {
		if out, err := git(t, d, gitenv.CleanEnviron(), args...); err != nil {
			t.Fatalf("git %v: %v — %s", args, err, out)
		}
	}
	return d
}

// REPRO A: `git init` in a fresh directory must create a repository THERE and must not touch any other
// repository's config.
//
// THE EXACT SHAPE MATTERS, and getting it wrong produces a silent false pass. With
// GIT_DIR=<main>/.git and GIT_WORK_TREE=<main> — the coherent pair gitEnvFor generates — git treats the
// command as "reinitialise the repo I was pointed at": it prints
//
//	Reinitialized existing Git repository in <main>/.git/
//
// and writes NOTHING. The config damage happens when GIT_WORK_TREE names a DIFFERENT worktree from the one
// GIT_DIR's own config claims — which is a live worktree under a shared repo, and is precisely what a run
// worktree is. That is where `worktree = …` and `bare = false` get written into the SHARED
// .git/config, and that stray core.worktree is what later redirected the operator's `git pull` into a
// worker's worktree. This test therefore builds that shape.
func TestInitInAFreshDirNeverWritesTheSharedRepoConfig(t *testing.T) {
	shared := mainRepo(t)
	// A second branch checked out at its own path: the shared repo now "owns" two worktrees, and
	// `git -C <second> worktree` is the state a run worktree is really in.
	second := filepath.Join(t.TempDir(), "second-worktree")
	if out, err := git(t, shared, gitenv.CleanEnviron(), "worktree", "add", "-q", "--detach", second); err != nil {
		t.Fatalf("worktree add: %v — %s", err, out)
	}
	// The pointer set that does the damage: the second worktree's admin dir + its path.
	pointer := filepath.Join(shared, ".git", "worktrees", "second-worktree")
	env := append(gitenv.Clean(os.Environ()), "GIT_DIR="+pointer, "GIT_WORK_TREE="+second)

	before, err := os.ReadFile(filepath.Join(shared, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}

	fresh := filepath.Join(t.TempDir(), "brand-new-project")
	if err := os.MkdirAll(fresh, 0o755); err != nil {
		t.Fatal(err)
	}

	// THE FIX: the invocation starts from a cleaned environment.
	if out, err := git(t, fresh, gitenv.Clean(env), "init", "-q"); err != nil {
		t.Fatalf("git init in the fresh dir failed: %v — %s", err, out)
	}

	// 1. The repository must exist where it was asked for.
	if _, statErr := os.Stat(filepath.Join(fresh, ".git")); statErr != nil {
		t.Errorf("git init did not create a repository in %s: %v — it operated on another repo instead", fresh, statErr)
	}
	// 2. The operator's repository must be untouched, byte for byte.
	after, err := os.ReadFile(filepath.Join(shared, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("a repository other than the target was modified.\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
}

// REPRO B: writing a commit identity for a project being set up must land in THAT project, never in a
// repository named by an ambient pointer. This is how `worktree-test@orchicon.dev` got into the
// operator's config.
func TestConfigIdentityNeverLandsInTheSharedRepoConfig(t *testing.T) {
	main := mainRepo(t)
	before, err := os.ReadFile(filepath.Join(main, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}

	target := mainRepoInTemp(t, "target")
	env := pointerEnv(t, main)

	if out, err := git(t, target, gitenv.Clean(env), "config", "user.name", "bootstrap"); err != nil {
		t.Fatalf("config user.name: %v — %s", err, out)
	}
	if out, err := git(t, target, gitenv.Clean(env), "config", "user.email", "bootstrap@localhost"); err != nil {
		t.Fatalf("config user.email: %v — %s", err, out)
	}

	// The identity must reach the TARGET.
	got, err := git(t, target, gitenv.Clean(env), "config", "user.email")
	if err != nil || got != "bootstrap@localhost" {
		t.Errorf("identity did not land in the target repo: got %q err %v", got, err)
	}
	// And must NOT reach the other repository.
	after, err := os.ReadFile(filepath.Join(main, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("the commit identity leaked into another repository.\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
}

// REPRO C: `git status` writes nothing and still reports the WRONG TREE when only GIT_WORK_TREE is set.
// This is the failure mode that made a clean checkout look 21 files dirty. It is pinned because it is the
// one that wastes an operator's afternoon without leaving any evidence behind.
func TestStatusDoesNotReportAnotherTreesChanges(t *testing.T) {
	main := mainRepo(t)
	other := mainRepoInTemp(t, "other")

	// Make the OTHER tree dirty. A correctly-scoped status must not see this.
	if err := os.WriteFile(filepath.Join(other, "dirty-from-elsewhere.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Point GIT_WORK_TREE at the other repo, as a leaked worker env does, and ask about `main`.
	env := append(gitenv.Clean(os.Environ()), "GIT_DIR="+filepath.Join(main, ".git"), "GIT_WORK_TREE="+other)
	out, err := git(t, main, gitenv.Clean(env), "status", "--porcelain")
	if err != nil {
		t.Fatalf("status: %v — %s", err, out)
	}
	if strings.Contains(out, "dirty-from-elsewhere.txt") {
		t.Errorf("git status reported another tree's change (%q) — a leaked GIT_WORK_TREE makes a clean "+
			"checkout look dirty, which is indistinguishable from real uncommitted work", out)
	}
}

// The sanitizer itself.
func TestCleanRemovesRepoPointersAndKeepsEverythingElse(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		"GIT_DIR=/somewhere/.git",
		"GIT_WORK_TREE=/somewhere",
		"GIT_INDEX_FILE=/tmp/idx",
		"GIT_OBJECT_DIRECTORY=/tmp/obj",
		"GIT_COMMON_DIR=/tmp/common",
		"GIT_NAMESPACE=ns",
		"GIT_PREFIX=sub/",
		"GIT_CEILING_DIRECTORIES=/tmp",
		"GIT_DISCOVERY_ACROSS_FILESYSTEM=1",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES=/tmp/alt",
		"GIT_SHALLOW_FILE=/tmp/shallow",
		"GIT_GRAFT_FILE=/tmp/graft",
		// These must SURVIVE: a caller may legitimately need them, and removing them would change
		// attribution, prompting or transport behaviour.
		"GIT_AUTHOR_NAME=Orchicon",
		"GIT_COMMITTER_NAME=Orchicon",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_SSH_COMMAND=ssh -i key",
		"HOME=/home/op",
	}
	out := gitenv.Clean(in)
	joined := strings.Join(out, "\n")
	for _, banned := range []string{
		"GIT_DIR=", "GIT_WORK_TREE=", "GIT_INDEX_FILE=", "GIT_OBJECT_DIRECTORY=",
		"GIT_COMMON_DIR=", "GIT_NAMESPACE=", "GIT_PREFIX=", "GIT_CEILING_DIRECTORIES=",
		"GIT_DISCOVERY_ACROSS_FILESYSTEM=", "GIT_ALTERNATE_OBJECT_DIRECTORIES=",
		"GIT_SHALLOW_FILE=", "GIT_GRAFT_FILE=",
	} {
		if strings.Contains(joined, banned) {
			t.Errorf("%s survived Clean", banned)
		}
	}
	for _, keep := range []string{
		"PATH=/usr/bin", "GIT_AUTHOR_NAME=Orchicon", "GIT_COMMITTER_NAME=Orchicon",
		"GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND=ssh -i key", "HOME=/home/op",
	} {
		if !strings.Contains(joined, keep) {
			t.Errorf("%s was removed but must pass through", keep)
		}
	}
}

// Windows environments are case-insensitive; `git_dir` is the same variable as `GIT_DIR` there.
func TestCleanIsCaseInsensitive(t *testing.T) {
	out := gitenv.Clean([]string{"git_dir=/x", "Git_Work_Tree=/y", "PATH=/bin"})
	joined := strings.Join(out, ",")
	if strings.Contains(strings.ToLower(joined), "git_dir") || strings.Contains(strings.ToLower(joined), "git_work_tree") {
		t.Errorf("lowercase variants survived Clean: %s", joined)
	}
	if !strings.Contains(joined, "PATH=/bin") {
		t.Errorf("Clean dropped an unrelated variable: %s", joined)
	}
}

func TestCleanOfEmptyIsEmpty(t *testing.T) {
	if got := gitenv.Clean(nil); got != nil {
		t.Errorf("Clean(nil) = %v", got)
	}
}

// A name with no "=" (malformed but legal in some environments) must not panic or be mistaken for a key.
func TestCleanToleratesMalformedEntries(t *testing.T) {
	out := gitenv.Clean([]string{"NOEQUALS", "GIT_DIR=/x", "PATH=/bin"})
	if len(out) != 2 {
		t.Errorf("got %d entries, want 2: %v", len(out), out)
	}
}
