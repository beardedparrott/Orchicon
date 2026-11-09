package scheduler

// worktree_dirt_test.go — THE PLATFORM'S OWN ARTEFACTS ARE NOT UNCOMMITTED WORK.
//
// REPORTED through nine prune tests failing the moment a database was available: a terminal run's worktree
// was never pruned. The cause was not the prune path but the dirt check feeding it —
// ensureRunWorktreeStepIgnore writes an UNTRACKED .gitignore at the worktree root, worktreeIsDirty counted
// every untracked entry as work that exists nowhere else, and removeWorktree therefore refused every one.
// Every provisioned worktree was permanently unpurgeable: the leak class, silent because the suite that
// would have seen it never ran.
//
// The rule is pinned here in both directions, because the exemption is only safe if real work still counts.

import (
	"os"
	"path/filepath"
	"testing"
)

// A FRESHLY PROVISIONED WORKTREE IS CLEAN. This is the reported bug.
func TestThePlatformsOwnGitignoreIsNotDirt(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(
		platformGitignoreMarker+" (parallel-branch children)\n01*/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if worktreeDirtyFrom("?? .gitignore\n", dir) {
		t.Fatal("the platform's own ignore file was reported as uncommitted work, so the worktree can never " +
			"be pruned — every provisioned worktree leaks")
	}
}

// THE OPERATOR'S OWN UNTRACKED .gitignore IS WORK: it is exempt only when it carries the marker.
func TestAnOperatorsOwnGitignoreIsStillDirt(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("node_modules/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !worktreeDirtyFrom("?? .gitignore\n", dir) {
		t.Fatal("an untracked .gitignore the platform did not write was treated as its own artefact — that " +
			"is the operator's work and removing it would lose it")
	}
}

func TestRealWorkIsStillDirt(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(platformGitignoreMarker+"\n01*/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, porcelain string }{
		{"untracked user file", "?? notes.txt\n"},
		{"platform ignore plus a user file", "?? .gitignore\n?? notes.txt\n"},
		{"modified tracked file", " M server.go\n"},
		{"staged file", "A  added.go\n"},
		{"deleted tracked file", " D gone.go\n"},
		{"renamed", "R  old.go -> new.go\n"},
	} {
		if !worktreeDirtyFrom(tc.porcelain, dir) {
			t.Errorf("%s was NOT reported as dirt — the prune would destroy it", tc.name)
		}
	}
}

func TestACleanWorktreeIsClean(t *testing.T) {
	if worktreeDirtyFrom("", t.TempDir()) {
		t.Fatal("an empty status was reported as dirty")
	}
}

// END TO END, against a real provisioned worktree: the bug was only visible here.
func TestAFreshlyProvisionedWorktreeIsNotDirty(t *testing.T) {
	env := newWorktreeTestEnv(t)
	if res := env.rec.Reconcile(t.Context(), env.run.ID); res.Error != nil {
		t.Fatalf("reconcile: %v", res.Error)
	}
	dirty, err := env.rec.worktreeIsDirty(t.Context(), env.expectedPath())
	if err != nil {
		t.Fatalf("dirty check: %v", err)
	}
	if dirty {
		out, _ := runGit(t.Context(), env.expectedPath(), "status", "--porcelain")
		t.Fatalf("a freshly provisioned worktree is reported dirty, so it can never be pruned.\n"+
			"git status --porcelain says:\n%s", out)
	}
}
