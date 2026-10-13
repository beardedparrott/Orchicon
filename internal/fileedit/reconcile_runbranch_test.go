package fileedit

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
)

// reconcile_runbranch_test.go — THE REGRESSION for a diff viewer that has never
// once populated for an opencode execution.
//
// MEASURED BEFORE THIS FIX: `adp_opencode_dev` had produced ZERO ledger rows
// across 2,398 executions — every one of them, succeeded and failed alike. The
// newest execution ledger row in the whole table predated the opencode switch.
//
// WHY, AND WHY IT IS INVISIBLE FROM THE CODE ALONE. The reconcile asked
// `git status --porcelain` what changed, and the worker is ORDERED to leave that
// empty: "commit ALL changes to the run branch ... verify `git status
// --porcelain` is clean" (internal/db/seed_workers.go). So the run's entire
// contribution was in COMMITS, which `git status` cannot report, and the
// reconcile found nothing — not as an error, just as an empty list. The two
// subsystems had contradictory requirements and neither was wrong on its own.
//
// The second half hid behind the first: even given the paths, the entry's
// before-side was read from HEAD — which for a committed run IS the commit, so
// before and after were the same bytes and every diff would have rendered empty.
// Both are covered here.

// committedRunRepo builds the SHAPE OF A REAL RUN: a base branch, a run branch
// forked from it, the run's work COMMITTED, and a CLEAN working tree — which is
// exactly the state the worker's brief requires and the state in which the old
// reconcile could see nothing.
func committedRunRepo(t *testing.T) string {
	t.Helper()
	dir := gitRepo(t) // initial commit of tracked.txt
	gitRun(t, dir, "branch", "-m", "develop")
	gitRun(t, dir, "checkout", "-qb", "run-branch")

	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("orig\nwork\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "added.txt"), []byte("new file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-qm", "the run's work")
	return dir
}

func TestReconcileGitSeesACommittedRunBranch(t *testing.T) {
	dir := committedRunRepo(t)
	ctx := context.Background()

	// THE PRECONDITION THAT MAKES THIS A REPRODUCTION. If the tree is not clean,
	// the old code would have found the changes via git status and this test
	// would prove nothing about the committed case.
	if out, err := gitCmd(dir, "status", "--porcelain").Output(); err != nil {
		t.Fatalf("git status: %v", err)
	} else if len(out) != 0 {
		t.Fatalf("fixture: the working tree must be CLEAN to reproduce the bug (got %q)", out)
	}

	base := BaseRefFor(dir)
	if base == "" {
		t.Fatal("fixture: BaseRefFor resolved no base ref, so the branch half cannot be exercised")
	}
	if base != "develop" {
		t.Fatalf("BaseRefFor = %q, want the base branch %q", base, "develop")
	}

	store := &fakeReconcileStore{}
	if err := ReconcileGit(ctx, store, dir, "tnt", db.FileEditOwnerExecution, "exec-1", base, nil); err != nil {
		t.Fatalf("ReconcileGit: %v", err)
	}

	got := make(map[string]Entry, len(store.appended))
	for _, e := range store.appended {
		got[e.Path] = e
	}
	for _, want := range []string{"tracked.txt", "added.txt"} {
		if _, ok := got[want]; !ok {
			t.Errorf("no ledger row for the committed path %q — the run's work is invisible, which is the "+
				"operator's empty diff viewer (rows: %v)", want, store.appended)
		}
	}

	// THE SECOND HALF: a row with an EMPTY diff is still an empty diff viewer.
	// The before-side must come from the fork point, not HEAD.
	if e, ok := got["tracked.txt"]; ok {
		if e.UnifiedDiff == "" {
			t.Errorf("the committed modification produced an EMPTY diff — the before-side was read from HEAD, "+
				"which for a committed run is the change itself: %+v", e)
		}
		if e.BeforeSHA == e.AfterSHA {
			t.Errorf("before and after are the same content (%s) — the diff compares the commit with itself",
				e.BeforeSHA)
		}
	}
	if e, ok := got["added.txt"]; ok && e.UnifiedDiff == "" {
		t.Errorf("the committed new file produced an empty diff: %+v", e)
	}
}

// TestReconcileGitWithoutABaseStaysWorkingTreeOnly is the CONTROL, and it pins
// the ASK path's semantics as much as the run's.
//
// An Ask conversation edits the project dir on whatever branch it is on and never
// commits, so `git status` is exactly right for it — and passing a base would
// attribute the WHOLE BRANCH's history to the conversation. The same committed
// tree must therefore yield NOTHING when no base is given; if this ever starts
// producing rows, the Ask path has begun claiming the branch's commits as its own
// edits.
func TestReconcileGitWithoutABaseStaysWorkingTreeOnly(t *testing.T) {
	dir := committedRunRepo(t)
	ctx := context.Background()

	store := &fakeReconcileStore{}
	if err := ReconcileGit(ctx, store, dir, "tnt", db.FileEditOwnerAskConversation, "conv-1", "", nil); err != nil {
		t.Fatalf("ReconcileGit: %v", err)
	}
	if len(store.appended) != 0 {
		t.Errorf("an empty base must NOT pick up committed work — the Ask path would claim the whole branch's "+
			"history as the conversation's edits: %v", store.appended)
	}
	if len(store.marked) != 0 {
		t.Errorf("an empty base must mark nothing confirmed: %v", store.marked)
	}
}

// TestBaseRefForPrefersDevelopThenFallsBack pins the resolution order, because
// it must MATCH the worktree reconciler's — that ref is what the run branch was
// actually cut from, so resolving it differently would anchor the diff on a
// guess.
func TestBaseRefForPrefersDevelopThenFallsBack(t *testing.T) {
	dir := gitRepo(t)
	gitRun(t, dir, "branch", "-m", "trunk")
	// No develop / origin/develop: the last resort is the current branch, which
	// is also what makes the run's own branch (and thus an empty diff) the
	// answer — never a crash, never a wrong ref.
	if got := BaseRefFor(dir); got != "trunk" {
		t.Fatalf("BaseRefFor = %q, want the fallback %q", got, "trunk")
	}
	gitRun(t, dir, "branch", "develop")
	if got := BaseRefFor(dir); got != "develop" {
		t.Fatalf("BaseRefFor = %q, want %q once it exists", got, "develop")
	}
}
