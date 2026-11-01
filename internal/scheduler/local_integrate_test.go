package scheduler

// local_integrate_test.go — LOCAL INTEGRATION against a real git repository.
//
// REPORTED: "A commit sitting only on a run branch is unfinished." A `git_strategy=local` project has no
// remote, so nothing ever merged the run branch into `develop`, and every run's work was stranded on a
// branch nobody reads.
//
// These tests drive the git half directly (no database): the strategy lookup and the run listing are the
// reconciler's plumbing, but the behaviour that matters — fast-forward, divergence, conflict, the stale
// main checkout, and idempotency — is all git.

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// quietSchedulerLogger keeps the git output of a passing test out of the report.
func quietSchedulerLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// integrateHarness builds a repository whose main checkout sits on `develop`, plus a run branch off it.
type integrateHarness struct {
	repo string
	r    *WorktreeReconciler
}

// newIntegrateHarness creates a repo with develop checked out and one commit.
func newIntegrateHarness(t *testing.T) *integrateHarness {
	t.Helper()
	repo := t.TempDir()
	gitRun(t, repo, "init", "-b", "develop")
	gitRun(t, repo, "config", "user.email", "integrate-test@orchicon.dev")
	gitRun(t, repo, "config", "user.name", "Integrate Test")
	gitRun(t, repo, "config", "gc.auto", "0")
	gitRun(t, repo, "config", "maintenance.auto", "false")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# base\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-m", "base")
	return &integrateHarness{repo: repo, r: &WorktreeReconciler{log: quietSchedulerLogger()}}
}

// commitOn creates a branch at HEAD, writes a file on it, and returns the new commit sha.
func (h *integrateHarness) commitOn(t *testing.T, branch, file, content string) string {
	t.Helper()
	dir := t.TempDir()
	work := filepath.Join(dir, "wt")
	gitRun(t, h.repo, "worktree", "add", "-b", branch, work, "HEAD")
	if err := os.WriteFile(filepath.Join(work, file), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", file, err)
	}
	gitRun(t, work, "add", ".")
	gitRun(t, work, "commit", "-m", "work on "+branch)
	sha := strings.TrimSpace(gitRun(t, work, "rev-parse", "HEAD"))
	gitRun(t, h.repo, "worktree", "remove", "--force", work)
	return sha
}

func (h *integrateHarness) head(t *testing.T, ref string) string {
	t.Helper()
	return strings.TrimSpace(gitRun(t, h.repo, "rev-parse", ref))
}

// THE REPORTED GAP: a completed local run's branch is fast-forwarded into develop.
func TestLocalIntegrationFastForwardsTheIntegrationBranch(t *testing.T) {
	h := newIntegrateHarness(t)
	sha := h.commitOn(t, "run-1", "feature.txt", "one\n")

	if !h.r.isAncestorOf(context.Background(), h.repo, "run-1", integrationBranch) {
		// precondition: the work is NOT integrated yet
	} else {
		t.Fatal("test setup: the run branch is already an ancestor")
	}

	// ensureIntegrationBranch then the fast-forward — the same two steps integrateLocalRun takes.
	if err := h.r.ensureIntegrationBranch(context.Background(), h.repo); err != nil {
		t.Fatalf("ensureIntegrationBranch: %v", err)
	}
	if err := h.r.setBranchRef(context.Background(), h.repo, integrationBranch, sha); err != nil {
		t.Fatalf("setBranchRef: %v", err)
	}

	if got := h.head(t, integrationBranch); got != sha {
		t.Fatalf("develop = %s, want the run branch tip %s — the work did not land", got, sha)
	}
	if !h.r.isAncestorOf(context.Background(), h.repo, "run-1", integrationBranch) {
		t.Fatal("after integration the run branch is still not an ancestor of develop")
	}
}

// DIVERGENCE, which the report noted is not hypothetical: two runs branched from the same commit, the
// first advanced develop, so the second cannot fast-forward and needs a real merge.
func TestLocalIntegrationMergesADivergedRunBranch(t *testing.T) {
	h := newIntegrateHarness(t)
	first := h.commitOn(t, "run-first", "a.txt", "first\n")
	h.commitOn(t, "run-second", "b.txt", "second\n")

	// run-first lands first and advances develop.
	if err := h.r.setBranchRef(context.Background(), h.repo, integrationBranch, first); err != nil {
		t.Fatalf("setBranchRef: %v", err)
	}
	// run-second was branched from the ORIGINAL commit, so it is now diverged.
	if h.r.isAncestorOf(context.Background(), h.repo, integrationBranch, "run-second") {
		t.Fatal("test setup: run-second should NOT be a descendant of the advanced develop")
	}

	merged, err := h.r.mergeRunBranchInScratch(context.Background(), h.repo, "run-second", "run-2-id")
	if err != nil {
		t.Fatalf("mergeRunBranchInScratch: %v", err)
	}
	if merged == "" {
		t.Fatal("the merge reported a CONFLICT for two non-overlapping branches")
	}
	if got := h.head(t, integrationBranch); got != merged {
		t.Fatalf("develop = %s, want the merge commit %s", got, merged)
	}
	// BOTH sides' work must be present in the MERGED COMMIT. Checked against git, not the working tree:
	// setBranchRef moves the ref without touching any checkout (deliberately), so the files are in the
	// tree the commit names, not on disk in the main checkout.
	for _, f := range []string{"a.txt", "b.txt"} {
		if _, err := runGit(context.Background(), h.repo, "cat-file", "-e", "develop:"+f); err != nil {
			t.Fatalf("%s is missing from the merge commit — one side's work was lost", f)
		}
	}
}

// A CONFLICT is reported as a conflict, NOT as an error, and it leaves both branches untouched. A
// controller cannot resolve a conflict semantically — only a human can.
func TestLocalIntegrationConflictLeavesBothBranchesUntouched(t *testing.T) {
	h := newIntegrateHarness(t)
	conflicting := h.commitOn(t, "run-conflict", "README.md", "from the run\n")
	// The integration branch changed the same file the run branch changed.
	if err := os.WriteFile(filepath.Join(h.repo, "README.md"), []byte("from develop\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	gitRun(t, h.repo, "commit", "-am", "develop changes README")
	developBefore := h.head(t, integrationBranch)

	merged, err := h.r.mergeRunBranchInScratch(context.Background(), h.repo, "run-conflict", "run-c-id")
	if err != nil {
		t.Fatalf("a conflict must be reported as a conflict, not an error: %v", err)
	}
	if merged != "" {
		t.Fatalf("the merge returned commit %q for two conflicting branches", merged)
	}
	if got := h.head(t, integrationBranch); got != developBefore {
		t.Fatalf("develop moved to %s on a CONFLICT — nothing may be merged", got)
	}
	if got := h.head(t, "run-conflict"); got != conflicting {
		t.Fatalf("the run branch moved to %s on a conflict — it must be left exactly as it was", got)
	}
	// And the scratch worktree must not be left behind holding a half-applied merge.
	wt := gitRun(t, h.repo, "worktree", "list", "--porcelain")
	if strings.Count(wt, "worktree ") > 1 {
		t.Errorf("a scratch worktree was left registered after a conflicted merge:\n%s", wt)
	}
}

// IDEMPOTENCY: integrating an already-integrated branch changes nothing.
func TestLocalIntegrationIsIdempotent(t *testing.T) {
	h := newIntegrateHarness(t)
	sha := h.commitOn(t, "run-idem", "x.txt", "x\n")
	if err := h.r.setBranchRef(context.Background(), h.repo, integrationBranch, sha); err != nil {
		t.Fatalf("setBranchRef: %v", err)
	}
	before := h.head(t, integrationBranch)
	// The ancestry check IS the idempotency key.
	if !h.r.isAncestorOf(context.Background(), h.repo, "run-idem", integrationBranch) {
		t.Fatal("after integration the ancestry check does not report it integrated — the no-op gate is broken")
	}
	if err := h.r.setBranchRef(context.Background(), h.repo, integrationBranch, sha); err != nil {
		t.Fatalf("re-applying an already-integrated ref: %v", err)
	}
	if got := h.head(t, integrationBranch); got != before {
		t.Fatalf("develop moved from %s to %s on a redundant integration", before, got)
	}
}

// THE STALE-CHECKOUT HAZARD, from the report: develop is checked out in the main worktree, so advancing the
// ref leaves its FILES behind. A CLEAN tree is refreshed; a DIRTY one is left alone and reported.
func TestRefreshMainCheckoutHandlesCleanAndDirtyTrees(t *testing.T) {
	h := newIntegrateHarness(t)
	sha := h.commitOn(t, "run-refresh", "brand-new.txt", "content\n")

	// READINESS IS READ BEFORE THE REF MOVES. Afterwards the index is stale by construction, so a check
	// made then can never pass — that ordering is the bug this test found.
	ready, blockedBy := h.r.mainCheckoutRefreshReadiness(context.Background(), h.repo)
	if !ready || blockedBy != "" {
		t.Fatalf("a clean checkout on the integration branch was judged not refreshable (ready=%v reason=%q)",
			ready, blockedBy)
	}
	if err := h.r.setBranchRef(context.Background(), h.repo, integrationBranch, sha); err != nil {
		t.Fatalf("setBranchRef: %v", err)
	}
	// Before the refresh the file the ref now contains is NOT in the working tree — the reported symptom.
	if _, err := os.Stat(filepath.Join(h.repo, "brand-new.txt")); err == nil {
		t.Fatal("test premise wrong: the working tree already shows the new file")
	}

	if err := h.r.refreshMainCheckout(context.Background(), h.repo, ready, blockedBy); err != nil {
		t.Fatalf("refreshMainCheckout: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.repo, "brand-new.txt")); err != nil {
		t.Fatalf("the CLEAN main checkout was not refreshed — its files are stale relative to its own HEAD, "+
			"which is what the operator sees as the whole tree reverting: %v", err)
	}

	// A DIRTY tree must be left alone: uncommitted work is the operator's.
	if err := os.WriteFile(filepath.Join(h.repo, "uncommitted.txt"), []byte("mine\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	// NOW the tree is dirty, so the next integration must refuse to refresh.
	ready2, blocked2 := h.r.mainCheckoutRefreshReadiness(context.Background(), h.repo)
	if ready2 || blocked2 == "" {
		t.Fatalf("a DIRTY checkout was judged refreshable (ready=%v reason=%q)", ready2, blocked2)
	}
	sha2 := h.commitOn(t, "run-refresh-2", "second-new.txt", "content\n")
	if err := h.r.setBranchRef(context.Background(), h.repo, integrationBranch, sha2); err != nil {
		t.Fatalf("setBranchRef: %v", err)
	}
	if err := h.r.refreshMainCheckout(context.Background(), h.repo, ready2, blocked2); err != nil {
		t.Fatalf("refreshMainCheckout (dirty): %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.repo, "uncommitted.txt")); err != nil {
		t.Fatalf("the refresh DESTROYED uncommitted work in the operator's checkout: %v", err)
	}
	// And a dirty tree must NOT have been reset to the new tip.
	if _, err := os.Stat(filepath.Join(h.repo, "second-new.txt")); err == nil {
		t.Fatal("a DIRTY main checkout was reset to the new tip — that is uncommitted work being destroyed")
	}
}

// Creating the integration branch when the project has none keeps the existing history as the base.
func TestEnsureIntegrationBranchBootstrapsAtHead(t *testing.T) {
	repo := t.TempDir()
	gitRun(t, repo, "init", "-b", "main") // note: no develop
	gitRun(t, repo, "config", "user.email", "x@orchicon.dev")
	gitRun(t, repo, "config", "user.name", "X")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-m", "base")
	head := strings.TrimSpace(gitRun(t, repo, "rev-parse", "HEAD"))

	r := &WorktreeReconciler{log: quietSchedulerLogger()}
	if err := r.ensureIntegrationBranch(context.Background(), repo); err != nil {
		t.Fatalf("ensureIntegrationBranch: %v", err)
	}
	if got := strings.TrimSpace(gitRun(t, repo, "rev-parse", integrationBranch)); got != head {
		t.Fatalf("%s = %s, want the existing HEAD %s — bootstrapping must not adopt a run branch as the base",
			integrationBranch, got, head)
	}
}
