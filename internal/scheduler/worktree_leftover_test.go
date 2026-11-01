package scheduler

// worktree_leftover_test.go — the PRUNE side of worktree ownership.
//
// REPORTED: "refusing to remove non-worktree directory on <step run id>" fired repeatedly, so worktree
// cleanup never completed. Measured on the real install: the recorded worktree path held an EMPTY
// directory with no `.git` file. The prune path asked the narrow isOrchiconWorktreeArtifact question,
// which REQUIRES that `.git` file — so a worktree that was never fully created, or whose admin data an
// earlier `git worktree prune` already swept, could never satisfy it and was refused forever. The
// provision path had always used the richer isOrchiconContainer predicate; the two disagreed.
//
// These tests drive the predicate directly against a real git repo, so they need no database.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// The reported case: an empty leftover directory at the recorded worktree path is OURS to remove.
func TestPruneLeftoverAcceptsAnEmptyRecordedWorktreeDirectory(t *testing.T) {
	projectDir := newTestRepo(t)
	r := &WorktreeReconciler{}
	path := filepath.Join(projectDir, worktreeDirName, "01M69NP73XWV31MK53GE0EKFNV")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir recorded worktree path: %v", err)
	}

	ours, err := r.pruneWorktreeLeftover(context.Background(), projectDir, path)
	if err != nil {
		t.Fatalf("pruneWorktreeLeftover: %v", err)
	}
	if !ours {
		t.Fatal("an EMPTY directory at the recorded worktree path was judged NOT ours — this is the reported " +
			"bug: the leftover can never satisfy a `.git`-file ownership proof, so cleanup refuses it on " +
			"every pass and re-warns forever")
	}
}

// A partial worktree (the `.git` gitdir file) is still ours — the original, narrower case must not regress.
func TestPruneLeftoverAcceptsAPartialWorktreeArtifact(t *testing.T) {
	projectDir := newTestRepo(t)
	r := &WorktreeReconciler{}
	path := filepath.Join(projectDir, worktreeDirName, "run-artifact")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir artifact: %v", err)
	}
	gitdir := "gitdir: " + filepath.Join(projectDir, ".git", "worktrees", "run-artifact") + "\n"
	if err := os.WriteFile(filepath.Join(path, ".git"), []byte(gitdir), 0o644); err != nil {
		t.Fatalf("write .git gitdir file: %v", err)
	}

	ours, err := r.pruneWorktreeLeftover(context.Background(), projectDir, path)
	if err != nil {
		t.Fatalf("pruneWorktreeLeftover: %v", err)
	}
	if !ours {
		t.Fatal("a partial worktree (.git gitdir file) was judged NOT ours — the original artifact case regressed")
	}
}

// THE SAFETY HALF, and the reason the predicate is not simply "delete anything under this path": a
// directory holding something that is NOT provably ours is still refused, because deleting user data is
// the one unrecoverable mistake available here.
func TestPruneLeftoverRefusesADirectoryHoldingForeignContent(t *testing.T) {
	projectDir := newTestRepo(t)
	r := &WorktreeReconciler{}
	path := filepath.Join(projectDir, worktreeDirName, "run-with-user-data")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "notes-from-the-operator.txt"), []byte("do not delete"), 0o644); err != nil {
		t.Fatalf("write foreign file: %v", err)
	}

	ours, err := r.pruneWorktreeLeftover(context.Background(), projectDir, path)
	if err != nil {
		t.Fatalf("pruneWorktreeLeftover: %v", err)
	}
	if ours {
		t.Fatal("a directory holding a foreign file was judged OURS — the prune would delete operator data")
	}
}

// A nested worktree artifact inside a run-namespaced container is ours (the parallel-branch shape the
// provision path already recognised); the prune path must agree with it rather than disagree.
func TestPruneLeftoverAcceptsARunContainerHoldingOnlyArtifacts(t *testing.T) {
	projectDir := newTestRepo(t)
	r := &WorktreeReconciler{}
	runDir := filepath.Join(projectDir, worktreeDirName, "run-container")
	inner := filepath.Join(runDir, "step-run")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	gitdir := "gitdir: " + filepath.Join(projectDir, ".git", "worktrees", "step-run") + "\n"
	if err := os.WriteFile(filepath.Join(inner, ".git"), []byte(gitdir), 0o644); err != nil {
		t.Fatalf("write nested .git: %v", err)
	}

	ours, err := r.pruneWorktreeLeftover(context.Background(), projectDir, runDir)
	if err != nil {
		t.Fatalf("pruneWorktreeLeftover: %v", err)
	}
	if !ours {
		t.Fatal("a run container holding only provably-ours artifacts was judged NOT ours")
	}
}
