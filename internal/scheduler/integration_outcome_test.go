package scheduler

// There is no field anywhere recording whether integration happened." That is the gap this covers,
// and it is derivable rather than storable.

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestIntegrationStatusIsDerivedFromTheBranchLedger(t *testing.T) {
	src, err := os.ReadFile("../workflow/service.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	s := string(src)
	i := strings.Index(s, "func integrationStatus(")
	if i < 0 {
		t.Fatal("integrationStatus is gone — a run that never landed is indistinguishable from one that did")
	}
	fn := s[i:]
	if end := strings.Index(fn, "\n}\n"); end > 0 {
		fn = fn[:end]
	}
	// Completed + no branch ⇒ landed.
	if !strings.Contains(fn, `strings.TrimSpace(r.WorktreeBranch) == ""`) {
		t.Error("the derivation no longer reads worktree_branch — that column is the only record of whether the " +
			"branch survived, and a surviving branch means the work did not land")
	}
	// Completed + branch ⇒ NOT landed.
	if !strings.Contains(fn, `return "not_merged"`) {
		t.Error("a completed run with a surviving branch is not reported as not_merged")
	}
	// Anything not completed is not_applicable, not a false verdict.
	if !strings.Contains(fn, `return "not_applicable"`) {
		t.Error("a non-completed run is given an integration verdict")
	}
}

// THE REAPER MUST NOT DESTROY UNCOMMITTED WORK.
func TestRemoveWorktreeRefusesUncommittedWork(t *testing.T) {
	repo := newTestRepo(t)
	wt := repo + "/" + worktreeDirName + "/run-dirty"
	gitRun(t, repo, "worktree", "add", wt, "-b", "dirty-branch")

	// A file the worker wrote but never committed — work that exists nowhere else.
	if err := os.WriteFile(wt+"/work-in-progress.txt", []byte("hours of work"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	r := &WorktreeReconciler{log: quietSchedulerLogger()}
	if err := r.removeWorktree(context.Background(), repo, wt); err != nil {
		t.Logf("removeWorktree returned %v (a refusal is acceptable; a crash is not)", err)
	}
	// THE DIRECTORY AND ITS CONTENT MUST SURVIVE.
	if _, err := os.Stat(wt + "/work-in-progress.txt"); err != nil {
		t.Fatal("the reaper DELETED a worktree holding uncommitted work — that content exists nowhere else " +
			"and cannot be recovered")
	}
	if dirty, _ := r.worktreeIsDirty(context.Background(), wt); !dirty {
		t.Fatal("test premise: the worktree should still read as dirty")
	}
}

// A CLEAN worktree is still reclaimed — the guard must not turn into "never reap anything".
func TestRemoveWorktreeStillRemovesACleanOne(t *testing.T) {
	repo := newTestRepo(t)
	wt := repo + "/" + worktreeDirName + "/run-clean"
	gitRun(t, repo, "worktree", "add", wt, "-b", "clean-branch")

	r := &WorktreeReconciler{log: quietSchedulerLogger()}
	if dirty, _ := r.worktreeIsDirty(context.Background(), wt); dirty {
		t.Fatalf("test premise: a fresh worktree should be clean")
	}
	if err := r.removeWorktree(context.Background(), repo, wt); err != nil {
		t.Fatalf("a CLEAN worktree was not removed: %v", err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("a clean worktree survived removal: %v", err)
	}
}
