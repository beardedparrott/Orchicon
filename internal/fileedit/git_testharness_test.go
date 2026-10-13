package fileedit

// git_testharness_test.go — the ONE place these tests invoke git, because every
// invocation needs the same protections and a second copy would drift.
//
// WHY THIS FILE EXISTS. The tests here create throwaway git repos, and a suite
// that shells out to git inherits two hazards that have nothing to do with the
// code under test — and one of them showed up as a FAILED REBUILD:
//
//	TempDir RemoveAll cleanup: unlinkat .../001/.git: directory not empty
//
//  1. GIT'S OWN BACKGROUND PROCESSES. `git commit` can fork maintenance and gc
//     into the background, and the process outlives the test body — so a single
//     RemoveAll races it and fails with "directory not empty". That is reported
//     as a TEST failure, which is how a suite fails on a race in git's own
//     bookkeeping rather than on anything it was written to check. Prevented
//     (gc/auto-maintenance/fsmonitor off) AND tolerated (a retrying cleanup):
//     either alone leaves the other half of the hazard open.
//
//  2. THE OPERATOR'S GIT ENVIRONMENT. The helpers used os.Environ() plus a single
//     GIT_CONFIG_NOSYSTEM override, so an ambient GIT_DIR, GIT_WORK_TREE,
//     GIT_INDEX_FILE, GIT_OBJECT_DIRECTORY, GIT_COMMON_DIR or a global gitconfig
//     reached the throwaway repo and could redirect its writes — which makes the
//     tests behave differently on the operator's machine than in CI. That is the
//     same class of ambient-environment leak that had ORCHICON_GUARD_* and
//     ORCHICON_DATA_DIR changing test behaviour on a live machine while CI stayed
//     green: the defect is the environment, so the fix is to stop inheriting it.
//
// The remainder of the environment (PATH, HOME) still passes through, because git
// needs it.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gitFlags disables every git feature that can spawn a writer into .git behind
// the command that triggered it. Passed to EVERY invocation, including `git init`,
// so the repo never acquires the feature in the first place.
var gitFlags = []string{
	"-c", "gc.auto=0",
	"-c", "maintenance.auto=false",
	"-c", "core.fsmonitor=false",
}

// gitEnv is the environment for every git command these tests run: the operator's
// environment with every GIT_* variable STRIPPED, and built-in config isolated.
func gitEnv() []string {
	out := make([]string, 0, len(os.Environ())+2)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GIT_") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
}

// gitCmd builds one git invocation against dir with the protections above.
func gitCmd(dir string, args ...string) *exec.Cmd {
	full := append([]string{"-C", dir}, gitFlags...)
	full = append(full, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = gitEnv()
	return cmd
}

// gitRun runs one git command, failing the test on error.
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := gitCmd(dir, args...).CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v: %s", args, err, out)
	}
}

// gitTestDir creates a directory for a throwaway repo.
//
// IT DELIBERATELY DOES NOT USE t.TempDir(). t.TempDir cleans up with ONE
// os.RemoveAll, so a git process still writing into .git makes that single call
// fail — and the failure is reported against the TEST, which passed. A temp
// directory is not an assertion, so this retries and then gives up SILENTLY
// rather than failing a suite over git's own housekeeping.
func gitTestDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "fileedit-git-*")
	if err != nil {
		t.Fatalf("mktemp: %v", err)
	}
	t.Cleanup(func() {
		deadline := time.Now().Add(2 * time.Second)
		for {
			if err := os.RemoveAll(dir); err == nil {
				return
			}
			if time.Now().After(deadline) {
				return // best-effort: never fail the suite over a temp dir
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
	return dir
}

// gitRepo creates a temp git repo with an initial tracked file so the
// "changed" state is observable via `git status`.
func gitRepo(t *testing.T) string {
	t.Helper()
	dir := gitTestDir(t)
	gitRun(t, dir, "init", "-q")
	gitRun(t, dir, "config", "user.email", "t@t")
	gitRun(t, dir, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("orig\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "tracked.txt")
	gitRun(t, dir, "commit", "-qm", "init")
	return dir
}
