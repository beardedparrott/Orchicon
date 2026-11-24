package opencode

// stall_checkpoint_test.go — A STALL MUST NOT DESTROY THE WORK THE MODEL ALREADY DID.
//
// THE LOSS. A Quick Work run at 01:12:59 spent 4,081,202 tokens over fourteen minutes and had written a
// COMPLETE test file — then died with
//
//	stalled:no_progress; execution ended before the final model step completed
//
// The stall monitor is a LIVENESS judgement: the model stopped producing tokens. It is not a verdict on
// the artefact. But the artefact was left uncommitted in a worktree the next dispatch overwrites, so a
// run whose work was essentially finished delivered nothing.
//
// Orchicon's premise is that work is delivered regardless. A stalled session's edits are still edits.
// The adapter now commits them, on the execution's own branch, before the session is aborted.

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// gitRepo makes a real repo with an initial commit, so a checkpoint has somewhere to land.
func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "work"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "T"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "add", "-A")
	cmd.Dir = dir
	_ = cmd.Run()
	cmd = exec.Command("git", "commit", "-q", "-m", "init")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("initial commit: %v (%s)", err, out)
	}
	return dir
}

func runWithWorktree(dir string) *sessionRun {
	a := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	return &sessionRun{
		a:        a,
		execRow:  db.ExecutionRow{ID: "exec-1"},
		manifest: scheduler.ExecutionManifest{WorktreePath: dir, ProjectDir: dir},
	}
}

// THE REPORTED LOSS, reproduced: uncommitted work must survive a stall.
func TestAStallCommitsTheWorkersInProgressChanges(t *testing.T) {
	dir := gitRepo(t)
	// The artefact the worker had finished when the model stopped talking.
	victim := filepath.Join(dir, "ask_card_answer_settles_test.go")
	if err := os.WriteFile(victim, []byte("package tui // the finished test\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	r := runWithWorktree(dir)
	r.checkpointWorktree("stalled:no_progress")

	// The tree is clean because the work is COMMITTED...
	out, err := gitIn(context.Background(), dir, "status", "--porcelain")
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("the worktree is still dirty after the checkpoint:\n%s", out)
	}
	// ...and the file is IN the branch, not lost.
	log, err := gitIn(context.Background(), dir, "log", "--oneline", "-1")
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	if !strings.Contains(log, "checkpoint") {
		t.Errorf("the newest commit is not the stall checkpoint: %q", log)
	}
	// The content is really there, recoverable by the next attempt.
	body, err := gitIn(context.Background(), dir, "show", "HEAD:"+filepath.Base(victim))
	if err != nil {
		t.Fatalf("the committed file is not readable from HEAD: %v", err)
	}
	if !strings.Contains(body, "the finished test") {
		t.Errorf("the committed content is not the worker's work: %q", body)
	}
}

// NOTHING TO COMMIT IS NOT AN ERROR: a stall before any edit is a clean no-op.
func TestAStallWithCleanTreeIsANoOp(t *testing.T) {
	dir := gitRepo(t)
	r := runWithWorktree(dir)
	r.checkpointWorktree("stalled:no_progress")

	log, err := gitIn(context.Background(), dir, "log", "--oneline")
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	if n := len(strings.Split(strings.TrimSpace(log), "\n")); n != 1 {
		t.Errorf("the checkpoint added a commit to a clean tree (%d commits):\n%s", n, log)
	}
}

// AND IT NEVER COMMITS INTO THE OPERATOR'S CHECKOUT. With no worktree there is no branch of ours to
// commit to, and touching the project dir on a stall would be worse than losing the work.
func TestAStallWithoutAWorktreeTouchesNothing(t *testing.T) {
	dir := gitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "scratch.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := &sessionRun{
		a:       a,
		execRow: db.ExecutionRow{ID: "exec-2"},
		// WorktreePath deliberately empty: an in-place execution.
		manifest: scheduler.ExecutionManifest{ProjectDir: dir},
	}
	r.checkpointWorktree("stalled:no_progress")

	out, err := gitIn(context.Background(), dir, "status", "--porcelain")
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	if strings.TrimSpace(out) == "" {
		t.Error("the checkpoint committed into the project directory — an in-place execution must never be " +
			"committed on a stall")
	}
}

// THE WIRING: a fatal stall must actually call it. A checkpoint nobody invokes is the bug.
func TestAFatalStallInvokesTheCheckpoint(t *testing.T) {
	raw, err := os.ReadFile("session_run.go")
	if err != nil {
		t.Fatalf("read session_run.go: %v", err)
	}
	src := string(raw)
	i := strings.Index(src, "func (r *sessionRun) onStall(reason string) {")
	if i < 0 {
		t.Fatal("onStall is gone")
	}
	body := src[i:]
	body = body[:strings.Index(body, "\n}\n")]
	if !strings.Contains(body, "checkpointWorktree(reason)") {
		t.Error("the fatal-stall branch does not checkpoint the worktree — a stalled run would again " +
			"discard 4M tokens of finished work")
	}
	// It must run BEFORE finish(), because finish() is first-arrival-wins and the session is aborted
	// immediately after.
	cp := strings.Index(body, "checkpointWorktree(reason)")
	fin := strings.Index(body, "r.finish(false, reason)")
	if cp < 0 || fin < 0 || cp > fin {
		t.Error("the checkpoint must run BEFORE finish() records the terminal reason")
	}
}
