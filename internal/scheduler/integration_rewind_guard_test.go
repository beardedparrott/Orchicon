package scheduler

// integration_rewind_guard_test.go — INTEGRATION MUST NEVER MOVE A BRANCH BACKWARD.
//
// REPORTED: "the branch was force-set back to 558d52e at the start", where 558d52e is an EMPTY ROOT commit and
// the integration branch had already reached f421a7c. Separately: "A run was provisioned an empty worktree.
// Location's branch was created at the empty root commit 558d52e while develop was already at f421a7c."
//
// Whether or not integration is what moved it, that is the exact shape that would lose every commit on the
// branch while leaving the work intact on run branches — and `git update-ref` would do it without complaint,
// because ref movement has no notion of direction. The guard lives in setBranchRef so the invariant does not
// depend on every future caller remembering to check ancestry.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A branch whose tip is NOT a descendant of where it already points must not move there.
func TestSetBranchRefRefusesARewind(t *testing.T) {
	repo := t.TempDir()
	gitRun(t, repo, "init", "-b", "develop")
	gitRun(t, repo, "config", "user.email", "x@orchicon.dev")
	gitRun(t, repo, "config", "user.name", "X")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-m", "first")

	// An UNRELATED root commit — the reported empty-root shape: not an ancestor of develop's tip.
	gitRun(t, repo, "checkout", "--orphan", "empty-root")
	gitRun(t, repo, "rm", "-rf", ".")
	gitRun(t, repo, "commit", "--allow-empty", "-m", "empty root")
	emptyRoot := strings.TrimSpace(gitRun(t, repo, "rev-parse", "HEAD"))
	gitRun(t, repo, "checkout", "develop")
	developTip := strings.TrimSpace(gitRun(t, repo, "rev-parse", "HEAD"))
	if emptyRoot == developTip {
		t.Fatal("test setup: the two commits are the same")
	}

	r := &WorktreeReconciler{log: quietSchedulerLogger()}
	err := r.setBranchRef(context.Background(), repo, "develop", emptyRoot)
	if err == nil {
		t.Fatal("setBranchRef MOVED develop to an unrelated root commit — every commit on the branch would be " +
			"lost and the working tree would come back empty, while the run branches still held the work")
	}
	if !strings.Contains(err.Error(), "REWIND") {
		t.Errorf("the refusal does not say what it prevented: %v", err)
	}
	if got := strings.TrimSpace(gitRun(t, repo, "rev-parse", "develop")); got != developTip {
		t.Fatalf("develop moved to %s despite the refusal — want it still at %s", got, developTip)
	}
}

// ...and a genuine forward move still works, or the guard would disable integration.
func TestSetBranchRefAllowsAFastForward(t *testing.T) {
	repo := t.TempDir()
	gitRun(t, repo, "init", "-b", "develop")
	gitRun(t, repo, "config", "user.email", "x@orchicon.dev")
	gitRun(t, repo, "config", "user.name", "X")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-m", "first")

	// A descendant commit, the ordinary case.
	gitRun(t, repo, "checkout", "-b", "run-1")
	if err := os.WriteFile(filepath.Join(repo, "b.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-m", "run work")
	tip := strings.TrimSpace(gitRun(t, repo, "rev-parse", "HEAD"))
	gitRun(t, repo, "checkout", "develop")

	r := &WorktreeReconciler{log: quietSchedulerLogger()}
	if err := r.setBranchRef(context.Background(), repo, "develop", tip); err != nil {
		t.Fatalf("setBranchRef refused a legitimate fast-forward: %v", err)
	}
	if got := strings.TrimSpace(gitRun(t, repo, "rev-parse", "develop")); got != tip {
		t.Fatalf("develop = %s, want the run tip %s", got, tip)
	}
}

// Creating a branch that does not exist yet is still allowed — that is the bootstrap case, not a move.
func TestSetBranchRefMayCreateANewBranch(t *testing.T) {
	repo := t.TempDir()
	gitRun(t, repo, "init", "-b", "main")
	gitRun(t, repo, "config", "user.email", "x@orchicon.dev")
	gitRun(t, repo, "config", "user.name", "X")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-m", "first")
	head := strings.TrimSpace(gitRun(t, repo, "rev-parse", "HEAD"))

	r := &WorktreeReconciler{log: quietSchedulerLogger()}
	if err := r.setBranchRef(context.Background(), repo, "staging", head); err != nil {
		t.Fatalf("setBranchRef refused to create a branch that does not exist: %v", err)
	}
	if got := strings.TrimSpace(gitRun(t, repo, "rev-parse", "staging")); got != head {
		t.Fatalf("staging = %s, want %s", got, head)
	}
}
