package checkpoint_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/checkpoint"
)

// gitRepo builds a throwaway repository with one commit; every write stays under t.TempDir().
func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.name", "Test"},
		{"config", "user.email", "test@localhost"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v — %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", "base"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v — %s", args, err, out)
		}
	}
	return dir
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v — %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// The whole point: edits a worker made but never committed must be COMMITTED on a stall/deadline, so
// the next attempt finds them instead of an empty worktree.
func TestCheckpointWorktreeCommitsUncommittedWork(t *testing.T) {
	dir := gitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "worker.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := checkpoint.CheckpointWorktree(context.Background(), dir, "", "wall_clock_timeout")
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if !res.Committed {
		t.Fatalf("expected a checkpoint commit, got %+v", res)
	}

	// The worktree must now be CLEAN — that is what makes the work visible to a retry.
	if status := gitOut(t, dir, "status", "--porcelain"); status != "" {
		t.Errorf("worktree still dirty after checkpoint:\n%s", status)
	}
	// The worker's file must be IN a commit, not just on disk.
	if !strings.Contains(gitOut(t, dir, "show", "--name-only", "--pretty=format:", "HEAD"), "worker.go") {
		t.Errorf("worker.go is not in the checkpoint commit")
	}
	// The reason must be recorded, so the next attempt can tell why the commit exists.
	if msg := gitOut(t, dir, "log", "-1", "--pretty=%s"); !strings.Contains(msg, "wall_clock_timeout") {
		t.Errorf("checkpoint message %q does not name the reason", msg)
	}
}

// A clean worktree is not an error and must not create an empty commit.
func TestCheckpointWorktreeSkipsWhenNothingToCommit(t *testing.T) {
	dir := gitRepo(t)
	before := gitOut(t, dir, "rev-parse", "HEAD")

	res, err := checkpoint.CheckpointWorktree(context.Background(), dir, "", "stalled")
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if res.Committed {
		t.Errorf("expected no commit, got %+v", res)
	}
	if before != gitOut(t, dir, "rev-parse", "HEAD") {
		t.Errorf("HEAD moved on a clean worktree")
	}
}

// A LOCAL run has no worktree of its own. Committing into the operator's checkout on a failure is not
// ours to do, so the checkpoint must decline — even though dirty files are sitting right there.
func TestCheckpointWorktreeNeverTouchesTheProjectDirWithoutAWorktree(t *testing.T) {
	dir := gitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "operator-work.txt"), []byte("not yours\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := checkpoint.CheckpointWorktree(context.Background(), "", dir, "stalled")
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if res.Committed {
		t.Fatalf("checkpoint committed into the project dir with no worktree: %+v", res)
	}
	if status := gitOut(t, dir, "status", "--porcelain"); !strings.Contains(status, "operator-work.txt") {
		t.Errorf("the operator's uncommitted file was disturbed: %q", status)
	}
}

// The helper must never fail the caller: a bad path returns an error the caller logs and ignores, and
// creates no commit anywhere.
func TestCheckpointWorktreeErrorsOnAMissingDir(t *testing.T) {
	if _, err := checkpoint.CheckpointWorktree(context.Background(), filepath.Join(t.TempDir(), "gone"), "", "stalled"); err == nil {
		t.Fatal("expected an error for a missing worktree")
	}
}
