package scheduler

// preflight_abort_test.go — A FAILED PREFLIGHT MUST ABORT THE RUN.

import (
	"os"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/domain"
)

// THE REPORTED STATE, pinned structurally: the failed check must precede the path check.
func TestFailedWorktreeIsCheckedBeforeThePath(t *testing.T) {
	src, err := os.ReadFile("reconciler.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	s := string(src)
	failedGate := strings.Index(s, "if exec.WorkflowRunID != \"\" && worktreeStatus == domain.WorktreeFailed {")
	if failedGate < 0 {
		t.Fatal("the failed-worktree gate is gone — a failed provision would dispatch again")
	}
	pathGate := strings.Index(s, "if worktreePath != \"\" {\n\t\texecCwd = worktreePath")
	if pathGate < 0 {
		t.Fatal("the path gate moved; re-establish this ordering assertion")
	}
	if failedGate > pathGate {
		t.Fatal("the PATH gate is evaluated before the FAILED check. A run whose worktree is failed but whose " +
			"path is recorded — which is exactly what mark() and retry leave behind — would dispatch, which is " +
			"the reported bug")
	}
	// And it must return, not merely log.
	arm := s[failedGate:]
	if end := strings.Index(arm, "}"); end > 0 {
		arm = arm[:end]
	}
	if !strings.Contains(arm, "return") {
		t.Error("the failed-worktree gate logs but does not RETURN — dispatch would continue")
	}
}

// THE OCCUPANCY ROOT CAUSE: a leftover directory is reclaimable whatever the run's status.
func TestLeftoverRunDirIsReclaimable(t *testing.T) {
	base := t.TempDir()

	// EMPTY — the reported leftover, pre-created before the branch was cut.
	emptyDir := base + "/empty"
	if err := os.MkdirAll(emptyDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	r := &WorktreeReconciler{log: quietSchedulerLogger()}
	if !r.isReclaimableRunDir(emptyDir) {
		t.Fatal("an EMPTY run directory was judged not reclaimable — that is the reported leftover, created by " +
			"a failed attempt before the branch existed, and a retry must be able to reclaim it")
	}

	// ONLY a .git file — a half-created worktree.
	halfDone := base + "/half"
	if err := os.MkdirAll(halfDone, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(halfDone+"/.git", []byte("gitdir: /x/.git/worktrees/y\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !r.isReclaimableRunDir(halfDone) {
		t.Fatal("a directory holding only a gitdir file was judged not reclaimable")
	}

	// REAL CONTENT is never reclaimable — the one unrecoverable mistake available.
	foreign := base + "/foreign"
	if err := os.MkdirAll(foreign, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(foreign+"/notes.txt", []byte("my work"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if r.isReclaimableRunDir(foreign) {
		t.Fatal("a directory holding real content was judged reclaimable — provisioning would DELETE it")
	}
}

// The dispatch hold must also exist for the pending/pruned statuses, or a fix to one arm breaks the others.
func TestDispatchStillHoldsForPendingAndPruned(t *testing.T) {
	src, err := os.ReadFile("reconciler.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	s := string(src)
	for _, want := range []string{
		"case domain.WorktreePending, domain.WorktreePruned:",
		"holding dispatch: worktree not ready",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q — a not-yet-provisioned worktree would dispatch without one", want)
		}
	}
	_ = domain.WorktreePending
}

// THE RETRY PATH must re-provision a FAILED worktree, not leave it failed-with-a-path.
func TestRetryResetsAFailedWorktreeToPending(t *testing.T) {
	src, err := os.ReadFile("../workflow/service.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	s := string(src)
	if !strings.Contains(s, "run.WorktreeStatus == domain.WorktreePruned || run.WorktreeStatus == domain.WorktreeFailed") {
		t.Fatal("retry no longer resets a FAILED run worktree — it stays failed-with-a-path, which is the " +
			"reported state that dispatched anyway and then refused its own leftover")
	}
	if !strings.Contains(s, "sr.WorktreeStatus == domain.WorktreePruned || sr.WorktreeStatus == domain.WorktreeFailed") {
		t.Error("retry does not reset a FAILED STEP worktree")
	}
	// The stale verdict must go with it: the retry has not attempted provisioning yet.
	if !strings.Contains(s, "runFields.ErrorMessage = &emptyErr") {
		t.Error("retry keeps the previous attempt's error message — a freshly-armed run would be labelled with " +
			"a provision it has not attempted")
	}
}

// A COMPLETED run must carry no failure: the fields describe the run that ENDED, not one that did not start.
func TestCompletedRunClearsTheFailureFields(t *testing.T) {
	src, err := os.ReadFile("workflow_reconciler.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	s := string(src)
	i := strings.Index(s, "Status:       strPtr(domain.WorkflowRunCompleted)")
	if i < 0 {
		t.Fatal("the run-completed write moved; re-establish this assertion")
	}
	arm := s[i:]
	if end := strings.Index(arm, "})"); end > 0 {
		arm = arm[:end]
	}
	if !strings.Contains(arm, "ErrorMessage: &emptyErr") || !strings.Contains(arm, "ErrorKind:    &emptyErr") {
		t.Fatal("a COMPLETED run does not clear error_message/error_kind — a run that actually ran would keep a " +
			"preflight verdict recorded before it started, which is the reported 'stamped with the stale " +
			"preflight error at the end'")
	}
}
