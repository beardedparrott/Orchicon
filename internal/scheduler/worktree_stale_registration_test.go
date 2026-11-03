package scheduler

// worktree_stale_registration_test.go — A BRANCH CLAIMED BY A REGISTRATION NOBODY CAN SATISFY MUST BE FREED.
//
// REPORTED, immediately after the misclassification fix:
//
//	git worktree add C:\projects\weatherapp\.orchicon-worktrees\01M6HRDZX3FB7NGGJNBR8J4CHS <branch>
//	  failed: fatal: '<branch>' is already used by worktree at
//	          '/mnt/c/projects/weatherapp/.orchicon-worktrees/01M6HRDZX3FB7NGGJNBR8J4CHS'
//
// Two layers of the same mistake, and both mine:
//
//  1. #906 rewrote the BACK-pointer too — `<repo>/.git/worktrees/<n>/gitdir`, which is the REPOSITORY's
//     registry, i.e. the HOST's. It now names a container path that does not exist on Windows, so the host
//     believes the branch is permanently checked out somewhere it cannot look.
//  2. Removing a worktree's DIRECTORY does not remove that registration, so reclaiming the leftover freed the
//     path but not the branch claim.
//
// These tests use a REAL git repo, because the whole failure is git's own bookkeeping.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// THE REPORTED FAILURE, reproduced: a registration whose back-pointer names an unresolvable container path.
func TestStaleRegistrationIsPrunedAndTheAddRetried(t *testing.T) {
	repo := newTestRepo(t)
	runID := "01M6HRDZX3FB7NGGJNBR8J4CHS"
	branch := "location-search-and-use-my-location-fb7nggjnbr8j4chs"
	worktreePath := filepath.Join(repo, worktreeDirName, runID)

	// A real worktree, then its registration corrupted exactly as a container-path rewrite would.
	gitRun(t, repo, "worktree", "add", worktreePath, "-b", branch)
	backPointer := filepath.Join(repo, ".git", "worktrees", runID, "gitdir")
	if err := os.WriteFile(backPointer, []byte("/mnt/c/projects/weatherapp/"+worktreeDirName+"/"+runID+"\n"), 0o644); err != nil {
		t.Fatalf("corrupt back-pointer: %v", err)
	}
	// ...and the directory gone, as a reclaim would leave it.
	if err := os.RemoveAll(worktreePath); err != nil {
		t.Fatalf("remove worktree dir: %v", err)
	}

	r := &WorktreeReconciler{log: quietSchedulerLogger()}

	// THE REAL ENTRY POINT: provisionWorktree, which is what the reconciler calls and which ATTACHES to an
	// existing branch — the arm a retry takes, and the arm that bypassed the repair before this change.
	// Before the fix this returns the reported error and never recovers.
	if err := r.provisionWorktree(context.Background(), repo, worktreePath, branch, "develop"); err != nil {
		t.Fatalf("provisionWorktree did not free a stale registration: %v", err)
	}
	// It must have produced a USABLE worktree, not merely have stopped erroring.
	if st, err := os.Stat(filepath.Join(worktreePath, ".git")); err != nil || st.IsDir() {
		t.Fatalf("no worktree was created at %s: %v", worktreePath, err)
	}
	if got := strings.TrimSpace(gitRun(t, repo, "worktree", "list", "--porcelain")); !strings.Contains(got, worktreePath) {
		t.Fatalf("the new worktree is not registered:\n%s", got)
	}
}

// A GENUINE IN-USE BRANCH MUST NOT BE STOLEN. The prune is targeted at the "already used by worktree" refusal,
// and it must not turn into "delete whatever is in the way" — a branch checked out in a live worktree is a real
// state, and destroying it would lose that worktree's uncommitted work.
func TestAddStillRefusesALiveWorktreeOnTheSameBranch(t *testing.T) {
	repo := newTestRepo(t)
	branch := "live-branch"
	first := filepath.Join(repo, worktreeDirName, "run-a")
	gitRun(t, repo, "worktree", "add", first, "-b", branch)

	r := &WorktreeReconciler{log: quietSchedulerLogger()}
	second := filepath.Join(repo, worktreeDirName, "run-b")
	if err := r.provisionWorktree(context.Background(), repo, second, branch, "develop"); err == nil {
		t.Fatal("a second worktree was created on a branch that is LIVE in another worktree — that worktree's " +
			"uncommitted work is now at risk")
	}
	// The original must still be registered and intact.
	if st, err := os.Stat(filepath.Join(first, ".git")); err != nil || st.IsDir() {
		t.Fatalf("the live worktree was destroyed: %v", err)
	}
}

// A FAILED WORKTREE IS REAPED; A READY ONE IS RETAINED.
//
// That distinction is the whole point of the retention feature and the whole of this bug. A 'ready' worktree
// of a failed run is deliberate resumability — a retry re-attaches to the branch and its uncommitted work. A
// 'failed' worktree is debris: provisioning never completed, there is nothing to resume, and retaining it is
// what left a directory behind that blocked four consecutive runs.
//
// The pruner could not reap it because of TWO guards: the scan selected only worktree_status='ready', and
// pruneOne/pruneStepRunOne returned early on anything else. Both are pinned here.
func TestFailedWorktreeIsSelectedAndReadyIsRetained(t *testing.T) {
	src, err := os.ReadFile("../db/workflow.go")
	if err != nil {
		t.Fatalf("read db source: %v", err)
	}
	s := string(src)

	// THE SCAN must select both, or the pruner never sees a failed one.
	if !strings.Contains(s, "worktree_status IN ('ready', 'failed') AND worktree_path <> ''") {
		t.Fatal("the run scan still selects only 'ready' worktrees — a failed one is never offered to the " +
			"pruner, which is how the leftover survived every sweep")
	}
	if !strings.Contains(s, "s.worktree_status IN ('ready', 'failed')") {
		t.Fatal("the STEP-RUN scan still selects only 'ready' worktrees")
	}

	// THE PRUNE GUARD must admit both.
	rec, err := os.ReadFile("worktree_reconciler.go")
	if err != nil {
		t.Fatalf("read scheduler source: %v", err)
	}
	r := string(rec)
	for _, want := range []string{
		"(run.WorktreeStatus != domain.WorktreeReady && run.WorktreeStatus != domain.WorktreeFailed)",
		"(sr.WorktreeStatus != domain.WorktreeReady && sr.WorktreeStatus != domain.WorktreeFailed)",
	} {
		if !strings.Contains(r, want) {
			t.Errorf("a prune guard still refuses a 'failed' worktree: missing %q", want)
		}
	}

	// 'ready' MUST REMAIN RETAINED — the resumability feature this must not break.
	if !strings.Contains(r, "domain.WorktreeReady && run.WorktreeStatus != domain.WorktreeFailed") {
		t.Fatal("the guard no longer admits 'ready' — retained failed runs would lose their worktree, which " +
			"is the deliberate resumability feature")
	}
}
