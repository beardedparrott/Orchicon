package scheduler

// worktree_no_remote_test.go — A REPOSITORY WITH NO REMOTE IS A SUPPORTED SHAPE, NOT A FAILING ONE.
//
// Reported from a real host: every provision and prune logged
//
//	git fetch origin develop failed: fatal: 'origin' does not appear to be a git repository
//	worktree: remote-branch-gone probe fetch failed; keeping branch
//	worktree: remote branch probe failed; skipping remote delete
//
// for a project that simply has no `origin`. Each is a failure nobody can act on, repeated per pass, and it
// buries the failures that matter while an operator is reading the log to diagnose one.
//
// These tests drive REAL git against a local-only repository — the shape that produced it — rather than
// asserting on the source, because the property is behavioural: the calls are not attempted.

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// localOnlyRepo makes a committed git repository with NO remote and returns its path.
func localOnlyRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "."},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-qm", "init"}} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	return dir
}

// TestNoRemoteIsDetectedWithoutNetwork proves the pre-check itself, on the shape that reported the problem.
func TestNoRemoteIsDetectedWithoutNetwork(t *testing.T) {
	dir := localOnlyRepo(t)
	r := &WorktreeReconciler{}
	if r.hasOriginRemote(context.Background(), dir) {
		t.Fatal("hasOriginRemote reported a remote on a local-only repository — every origin-touching call " +
			"would be attempted and fail with a warning per pass")
	}
}

// TestRemoteProbesAreSkippedWithoutARemote is the behavioural assertion: the probes return the SAME
// fail-closed answers as a failed attempt, without touching a remote.
func TestRemoteProbesAreSkippedWithoutARemote(t *testing.T) {
	dir := localOnlyRepo(t)
	ctx := context.Background()
	var logs strings.Builder
	r := &WorktreeReconciler{log: testLoggerTo(&logs)}

	// remoteHeadBranch: no answer, and no `remote show` attempted.
	if got := r.remoteHeadBranch(ctx, dir); got != "" {
		t.Errorf("remoteHeadBranch = %q on a repository with no origin, want empty", got)
	}
	// remoteBranchRefGone: cannot tell -> false (fail-closed: never delete on an unanswered probe).
	if r.remoteBranchRefGone(ctx, dir, "feature") {
		t.Error("remoteBranchRefGone reported GONE on a repository with no origin — a branch could be " +
			"deleted on an unanswerable probe")
	}
	// branchRemoteExists: NOT determined, which is what tells the caller to skip the remote delete.
	if _, determined := r.branchRemoteExists(ctx, dir, "feature"); determined {
		t.Error("branchRemoteExists claimed a DETERMINED answer with no origin")
	}
	// branchMergedIntoBase: still answers from LOCAL refs (the conservative fallback), without a fetch.
	_ = r.branchMergedIntoBase(ctx, dir, "HEAD")

	// AND NOTHING WAS LOGGED. The reported symptom was the noise, not just the wasted work.
	if logs.Len() > 0 {
		t.Errorf("a repository with no origin produced log output, which is the reported noise:\n%s",
			logs.String())
	}
}

// testLoggerTo returns a logger that writes into w, so a test can assert that NOTHING was logged.
func testLoggerTo(w *strings.Builder) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelWarn}))
}
