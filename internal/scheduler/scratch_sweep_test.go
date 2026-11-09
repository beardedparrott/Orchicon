package scheduler

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// scratchSweepRepo builds the exact shape a lost cleanup leaves behind: a real, REGISTERED worktree
// named in the baseline scratch namespace, under the project's own worktree root.
func scratchSweepRepo(t *testing.T) (repo, scratch string) {
	t.Helper()
	repo = newTestRepo(t)
	if err := os.MkdirAll(filepath.Join(repo, worktreeDirName), 0o755); err != nil {
		t.Fatalf("mkdir worktree root: %v", err)
	}
	scratch = filepath.Join(repo, worktreeDirName, scratchBaselinePrefix+"1234567890")
	if _, err := runGit(context.Background(), repo, "worktree", "add", "--detach", scratch, "HEAD"); err != nil {
		t.Fatalf("materialise scratch checkout: %v", err)
	}
	return repo, scratch
}

// scratchSweepReconciler needs no pool: the reclaim path touches only git and the logger.
func scratchSweepReconciler() *WorktreeReconciler {
	return NewWorktreeReconciler(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// THE LEAKED CASE IS THE WHOLE POINT: the checkout is registered, so every existing rule in the
// sweep treats it as legitimate.
func TestTheSweepReclaimsALeakedBaselineScratchCheckout(t *testing.T) {
	repo, scratch := scratchSweepRepo(t)
	stale := time.Now().Add(-scratchBaselineMaxAge - time.Minute)
	if err := os.Chtimes(scratch, stale, stale); err != nil {
		t.Fatalf("backdate scratch checkout: %v", err)
	}

	scratchSweepReconciler().reclaimStaleScratchDir(context.Background(), repo, scratch)

	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Fatalf("a scratch checkout older than the capture's own deadline was not reclaimed: %v", err)
	}
	out, _ := runGit(context.Background(), repo, "worktree", "list")
	if strings.Contains(out, filepath.Base(scratch)) {
		t.Fatalf("the worktree registration survived the reclaim:\n%s", out)
	}
}

// The opposite state, asserted with its own fixture: a checkout young enough to belong to a capture
// that is still measuring must be left completely alone. Deleting it would corrupt a live baseline.
func TestTheSweepLeavesAFreshScratchCheckoutForALiveCapture(t *testing.T) {
	repo, scratch := scratchSweepRepo(t)

	scratchSweepReconciler().reclaimStaleScratchDir(context.Background(), repo, scratch)

	if _, err := os.Stat(scratch); err != nil {
		t.Fatalf("a fresh scratch checkout must be left alone (a capture may be measuring in it): %v", err)
	}
	if out, _ := runGit(context.Background(), repo, "worktree", "list"); !strings.Contains(out, filepath.Base(scratch)) {
		t.Fatalf("the fresh checkout lost its registration:\n%s", out)
	}
}

// The second shape of the leak: a scratch dir whose registration was already pruned, or that a
// failed reclaim left behind.
func TestTheSweepReclaimsAStaleScratchDirWithNoRegistration(t *testing.T) {
	repo := newTestRepo(t)
	root := filepath.Join(repo, worktreeDirName)
	scratch := filepath.Join(root, scratchBaselinePrefix+"0987654321")
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		t.Fatalf("mkdir scratch: %v", err)
	}
	if err := os.WriteFile(filepath.Join(scratch, "leftover.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write leftover: %v", err)
	}
	stale := time.Now().Add(-scratchBaselineMaxAge - time.Minute)
	if err := os.Chtimes(scratch, stale, stale); err != nil {
		t.Fatalf("backdate scratch: %v", err)
	}

	scratchSweepReconciler().reclaimStaleScratchDir(context.Background(), repo, scratch)

	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Fatalf("an unregistered stale scratch dir was left behind: %v", err)
	}
}

// THIS ASSERTS WIRING, NOT BEHAVIOUR.
func TestTheSweepConsultsTheScratchNamespaceBeforeTheWorktreeSkip(t *testing.T) {
	src, err := os.ReadFile("worktree_reconciler.go")
	if err != nil {
		t.Fatalf("read reconciler source: %v", err)
	}
	// Comments are stripped first: the ordering comment names validPaths, so an unstripped read would
	// satisfy this assertion from prose alone.
	body := stripLineComments(string(src))
	scratchAt := strings.Index(body, "strings.HasPrefix(e.Name(), scratchBaselinePrefix)")
	skipAt := strings.Index(body, "if validPaths[filepath.Clean(child)]")
	if scratchAt < 0 {
		t.Fatalf("the sweep never consults scratchBaselinePrefix, so a leaked checkout is never reclaimed")
	}
	if skipAt < 0 {
		t.Fatalf("could not locate the registered-worktree skip to compare the ordering against")
	}
	if scratchAt > skipAt {
		t.Fatalf("the scratch check runs after the registered-worktree skip, so it can never fire")
	}
}

func stripLineComments(src string) string {
	var b strings.Builder
	for _, line := range strings.Split(src, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}
