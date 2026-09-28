package scheduler

// worktree_restore_never_loses_test.go — the safety property of the post-run restore.
//
// restoreWorkTree resets a project's SHARED CHECKOUT — the operator's own working copy — and the
// only signal the caller has is "`git status --porcelain` printed something". That is exactly what a
// developer's own uncommitted work looks like, and nothing records which files a run touched, so the
// two are indistinguishable. Before this change the function went straight to `git reset --hard HEAD`
// + `git clean -fd`, which would discard the operator's modified files and DELETE their untracked
// ones with no way back.
//
// These assertions are on the property, not the implementation: after a restore, the checkout is
// clean AND everything that was there before is still recoverable.

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitInit makes a throwaway repo with one commit, so a working tree can be made dirty.
func gitInit(t *testing.T, dir string) {
	t.Helper()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("commit", "-q", "--allow-empty", "-m", "seed")
}

func TestRestoreWorkTreePreservesUncommittedWork(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)

	// THE OPERATOR'S WORK: one modified tracked file and one untracked file. `clean -fd` deletes the
	// second outright; `reset --hard` discards the first. Both must survive a restore.
	tracked := filepath.Join(dir, "notes.md")
	if err := os.WriteFile(tracked, []byte("committed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", dir, "add", "notes.md").CombinedOutput(); err != nil {
		t.Fatalf("add: %v %s", err, out)
	}
	if out, err := exec.Command("git", "-C", dir, "-c", "user.name=t", "-c", "user.email=t@t",
		"commit", "-q", "-m", "add notes").CombinedOutput(); err != nil {
		t.Fatalf("commit: %v %s", err, out)
	}
	if err := os.WriteFile(tracked, []byte("MY UNCOMMITTED EDIT\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	untracked := filepath.Join(dir, "scratch-user-file.txt")
	if err := os.WriteFile(untracked, []byte("MY NEW FILE\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The reconciler is built through NewWorktreeReconciler in production, which always sets this.
	r := &WorktreeReconciler{log: slog.Default()}
	if err := r.restoreWorkTree(context.Background(), dir); err != nil {
		t.Fatalf("restoreWorkTree: %v", err)
	}

	// THE CHECKOUT IS CLEAN — the restore still does its job.
	out, _ := exec.Command("git", "-C", dir, "status", "--porcelain").CombinedOutput()
	if strings.TrimSpace(string(out)) != "" {
		t.Fatalf("the checkout is not clean after the restore:\n%s", out)
	}

	// AND NOTHING WAS LOST. The stash is the proof: `git stash show -p` must contain both the edit and
	// the new file.
	list, err := exec.Command("git", "-C", dir, "stash", "list").CombinedOutput()
	if err != nil || !strings.Contains(string(list), "orchicon") {
		t.Fatalf("no orchicon stash entry after the restore:\n%s (%v)", list, err)
	}
	patch, err := exec.Command("git", "-C", dir, "stash", "show", "-p", "--include-untracked").CombinedOutput()
	if err != nil {
		t.Fatalf("stash show: %v", err)
	}
	if !strings.Contains(string(patch), "MY UNCOMMITTED EDIT") {
		t.Errorf("the operator's EDIT is not recoverable from the stash:\n%s", patch)
	}
	if !strings.Contains(string(patch), "MY NEW FILE") {
		t.Errorf("the operator's NEW FILE is not recoverable from the stash (this is what plain "+
			"`clean -fd` deleted silently):\n%s", patch)
	}
}

// TestRestoreWorkTreeRefusesWhenItCannotPreserve — the fail-closed half. If the stash cannot be made,
// the reset must NOT happen: tidying a checkout is never worth discarding work.
func TestRestoreWorkTreeRefusesWhenItCannotPreserve(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "dirty.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Block the stash: an index.lock makes `git stash` fail the way a concurrent git process would.
	lock := filepath.Join(dir, ".git", "index.lock")
	if err := os.WriteFile(lock, []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}

	r := &WorktreeReconciler{log: slog.Default()}
	if err := r.restoreWorkTree(context.Background(), dir); err == nil {
		t.Fatal("restoreWorkTree proceeded even though it could not preserve the working tree — " +
			"discarding work to tidy a checkout is the defect this guard exists to prevent")
	}
	// And the file is still there, because nothing destructive ran.
	if _, err := os.Stat(filepath.Join(dir, "dirty.txt")); err != nil {
		t.Fatalf("the working tree was modified despite the refusal: %v", err)
	}
	_ = os.Remove(lock)
}
