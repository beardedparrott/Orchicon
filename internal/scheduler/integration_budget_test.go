package scheduler

// integration_budget_test.go — LOCAL INTEGRATION MUST NOT BE STARVED.
//
// REPORTED: a green, committed local-strategy run was never integrated; develop's reflog never moved. Two
// independent defects in the integration path, both mine:
//
//  1. STARVATION. integrateLocalRuns ran LAST in scanSlow, sharing the single 15s deadline that three
//     git-heavy sweeps consume first. On a host where those sweeps are slow, the budget was spent before
//     integration was reached, so it returned immediately on EVERY pass — forever, while the reconciler
//     reported itself healthy.
//  2. THE FILTER EXCLUDED THE CASE IT EXISTS FOR. The candidate query skipped rows with
//     worktree_status='pruned' — but a completed run whose branch is NOT provably merged has its worktree
//     pruned and its branch KEPT (markStepPruned runs unconditionally after a refused branch delete). So
//     "pruned" is precisely the state of an un-integrated run.

import (
	"os"
	"strings"
	"testing"
)

// THE STARVATION, pinned structurally: integration is called before the sweeps that consume the budget, and
// with a deadline of its own rather than theirs.
func TestIntegrationRunsBeforeTheBudgetConsumingSweeps(t *testing.T) {
	src, err := os.ReadFile("worktree_reconciler.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	body := funcBody(t, string(src), "func (r *WorktreeReconciler) scanSlow(")

	integrate := strings.Index(body, "r.integrateLocalRuns(")
	if integrate < 0 {
		t.Fatal("scanSlow no longer calls integrateLocalRuns — nothing integrates a local run")
	}
	for _, sweep := range []string{"r.sweepOrphanBranches(", "r.sweepOrphanDirs(", "r.sweepSkippedTerminalRuns("} {
		i := strings.Index(body, sweep)
		if i < 0 {
			t.Fatalf("scanSlow no longer calls %s — this test's premise is stale", sweep)
		}
		if integrate > i {
			t.Errorf("integrateLocalRuns runs AFTER %s. Those sweeps consume the shared slow-pass budget, so "+
				"integration gets whatever is left — which on a slow host is nothing, and it then returns "+
				"immediately on every pass while the reconciler reports itself healthy", sweep)
		}
	}
	// Its own budget, not the shared deadline the sweeps take turns on.
	if !strings.Contains(body, "integrateLocalRuns(ctx, tenantID, time.Now().Add(slowPassBudget))") {
		t.Error("integrateLocalRuns does not get its OWN deadline — sharing the sweeps' deadline is the " +
			"starvation this test exists for")
	}
}

// THE FILTER: the query must not exclude pruned worktrees, because that is the shape of an un-integrated run,
// and it must terminate on the stamp rather than on the worktree state.
func TestIntegrationQueryDoesNotFilterOnPrunedWorktrees(t *testing.T) {
	src, err := os.ReadFile("../db/workflow.go")
	if err != nil {
		t.Fatalf("read db source: %v", err)
	}
	start := strings.Index(string(src), "func ListCompletedRunsWithBranch(")
	if start < 0 {
		t.Fatal("ListCompletedRunsWithBranch is gone")
	}
	end := strings.Index(string(src)[start:], "\n}\n")
	body := string(src)[start : start+end]

	if strings.Contains(body, "worktree_status <> 'pruned'") {
		t.Fatal("the candidate query still excludes pruned worktrees. A completed run whose branch is NOT " +
			"provably merged has its worktree pruned and its branch kept, so that filter skips exactly the " +
			"runs integration exists for — which is why a green run was never integrated")
	}
	if !strings.Contains(body, "local_integrated_at") {
		t.Error("the candidate query has no termination stamp. Without it the set grows forever with rows " +
			"whose branch the orphan sweep already deleted, and the LIMIT starves newer runs behind " +
			"ancient no-ops")
	}
}

// The stamp is written for every terminal outcome EXCEPT a conflict — the one case a human can change.
func TestConflictIsNotStampedSoARetryCanStillIntegrate(t *testing.T) {
	src, err := os.ReadFile("local_integrate.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	s := string(src)
	conflict := strings.Index(s, "hit a CONFLICT")
	if conflict < 0 {
		t.Fatal("the conflict branch is gone")
	}
	// The conflict arm ends at the next `return nil`.
	arm := s[conflict:]
	if end := strings.Index(arm, "return nil"); end > 0 {
		arm = arm[:end]
	}
	if strings.Contains(arm, "stampIntegrated(") {
		t.Fatal("a CONFLICT stamps the run as done. A conflict is the one outcome that can change without " +
			"this platform acting — a human resolving it makes the next pass succeed — so stamping there " +
			"strands the run permanently")
	}
}

// A run whose project is not local-strategy, or whose branch is gone, must be stamped so it is not rescanned
// forever; the branch-gone case is also LOGGED, because it is a finding.
func TestTerminalNonIntegrationOutcomesAreStamped(t *testing.T) {
	src, err := os.ReadFile("local_integrate.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	s := string(src)
	for _, want := range []string{
		`stampIntegrated(ctx, tenantID, run.ID, "not a local-strategy project")`,
		`stampIntegrated(ctx, tenantID, run.ID, "recorded branch does not exist")`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing stamp: %s", want)
		}
	}
	if !strings.Contains(s, "found NO branch for a completed run") {
		t.Error("a recorded branch that does not exist is not logged — the reported run 1 recorded a branch " +
			"that was never created at all, and silence about that is what made it look like integration " +
			"was running")
	}
}

// funcBody returns the body of the function whose declaration line is decl (the file's own copy, since this
// package has none).
func funcBody(t *testing.T, src, decl string) string {
	t.Helper()
	i := strings.Index(src, decl)
	if i < 0 {
		t.Fatalf("the source no longer declares %q — re-establish the ordering assertion or explain the "+
			"replacement", decl)
	}
	rest := src[i+len(decl):]
	if j := strings.Index(rest, "\nfunc "); j >= 0 {
		return rest[:j]
	}
	return rest
}
