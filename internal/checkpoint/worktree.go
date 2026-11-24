package checkpoint

// Worktree checkpoint: make a worker's in-progress edits durable when a session ends WITHOUT a
// success verdict — a stall, a wall-clock deadline, a crash, a cancelled turn.
//
// WHY THIS IS NOT THE ADAPTER'S OWN BUSINESS ANY MORE.
//
// The opencode adapter grew this first (internal/opencode/session_run.go: checkpointWorktree, wired
// into onStall), because a stalled run had burned 4,081,202 tokens and written a complete test file
// to a worktree that the next dispatch then reset away. A stall is a LIVENESS judgement about the
// MODEL, not a verdict on the WORK — and Orchicon's whole claim is that work is delivered regardless.
//
// But the checkpoint lived only in opencode, so the two other adapters behaved differently on the
// same failure, and nobody could see it: the native (orchicon) loop and the claude adapter both
// aborted a stalled or timed-out session and left its edits uncommitted in the worktree. The
// worktree reconciler later runs `git stash push -u` + `git reset --hard HEAD` + `git clean -fd`
// (internal/scheduler/worktree_reconciler.go: restoreWorkTree), which DOES preserve the work — in a
// stash nobody reads, on a branch the retry does not look at, under a message no operator knows to
// search for. In practice, for the next attempt, that is gone.
//
// A checkpoint commits on the EXECUTION'S OWN BRANCH, which the run already owns: no merge, no push,
// no PR, no change to the scheduler's integration path. It only makes the work findable by a retry.
//
// IT IS DELIBERATELY BEST-EFFORT AND SILENT ON FAILURE, ALWAYS. A checkpoint that errors must never
// turn one failure into a different, more confusing one: the caller is already failing the execution
// and the reason it reports must stay the real cause. Every error is returned for the caller to LOG
// and otherwise ignore — never to act on.

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/beardedparrott/orchicon/internal/procwin"
)

// CheckpointTimeout bounds a single checkpoint. A stalled session's worktree should be small, and a
// checkpoint that hangs must not hold the failing execution open.
const CheckpointTimeout = 60 * time.Second

// CheckpointWorktreeResult reports what a checkpoint did, for logging.
type CheckpointWorktreeResult struct {
	// Committed is true when a checkpoint commit was created.
	Committed bool
	// Skipped is non-empty when there was nothing to do, naming why.
	Skipped string
}

// CheckpointWorktree commits whatever a worker has already written to its own run branch.
//
// worktreePath is the run's worktree; projectDir is the fallback execution directory. When the run
// has NO worktree (it ran in place in the operator's checkout, or the mode is not a branch-per-run
// one), this is a NO-OP: committing into the operator's own checkout on a failure is not ours to do.
//
// reason names the end condition and is recorded in the commit message, so the next attempt (a
// recovery, a retry, or a human) can tell why the checkpoint exists.
func CheckpointWorktree(ctx context.Context, worktreePath, projectDir, reason string) (CheckpointWorktreeResult, error) {
	if strings.TrimSpace(worktreePath) == "" {
		return CheckpointWorktreeResult{Skipped: "no worktree: the run did not have its own branch"}, nil
	}
	dir := worktreePath
	if strings.TrimSpace(dir) == "" {
		dir = projectDir
	}
	return checkpointIn(ctx, dir, reason)
}

// checkpointIn runs the checkpoint against dir.
func checkpointIn(ctx context.Context, dir, reason string) (CheckpointWorktreeResult, error) {
	ctx, cancel := context.WithTimeout(ctx, CheckpointTimeout)
	defer cancel()

	status, err := gitIn(ctx, dir, "status", "--porcelain")
	if err != nil {
		return CheckpointWorktreeResult{}, fmt.Errorf("git status: %w", err)
	}
	if strings.TrimSpace(status) == "" {
		return CheckpointWorktreeResult{Skipped: "nothing to commit"}, nil
	}
	if _, err := gitIn(ctx, dir, "add", "-A"); err != nil {
		return CheckpointWorktreeResult{}, fmt.Errorf("git add -A: %w", err)
	}
	msg := "checkpoint: work in progress preserved (" + reason + ")"
	if _, err := gitIn(ctx, dir, "-c", "user.name=Orchicon", "-c", "user.email=orchicon@localhost",
		"commit", "--no-verify", "-m", msg); err != nil {
		return CheckpointWorktreeResult{}, fmt.Errorf("git commit: %w", err)
	}
	return CheckpointWorktreeResult{Committed: true}, nil
}

// gitIn runs one git command in dir and returns its trimmed stdout.
func gitIn(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := procwin.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
