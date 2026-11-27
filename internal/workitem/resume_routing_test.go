package workitem

import (
	"os"
	"strings"
	"testing"
)

// WHY THIS FILE EXISTS — the operator's exact error.
//
// Resuming a PAUSED work item failed with:
//
//	"cannot RESUME: the run is paused — only a failed or aborted run can be resumed
//	 (use RESTART for a new one)"
//
// Leaf RESUME called `retryRunFn` unconditionally. Retry re-arms a TERMINAL run's failed steps; it
// does not continue a parked one. So the single run state that RESUME exists for was the one it
// refused — the same shape as the original "leaf RESUME is a no-op" defect, one layer down.
//
// RESUME now routes on the RUN'S STATE:
//
//	paused              -> resumePausedRunFn  (continue the parked run)
//	failed | aborted    -> retryRunFn         (re-arm the terminal run)

func TestResumeRoutesPausedRunsToTheResumePath(t *testing.T) {
	b, err := os.ReadFile("service.go")
	if err != nil {
		t.Fatalf("read service.go: %v", err)
	}
	src := strings.ReplaceAll(string(b), "\t", "")

	if !strings.Contains(src, "case domain.WorkflowRunPaused:") {
		t.Fatal("RESUME does not handle a paused run — resuming a paused item will fail with " +
			"\"only a failed or aborted run can be resumed\"")
	}
	if !strings.Contains(src, "s.resumePausedRunFn(ctx, tenantID, runID)") {
		t.Error("RESUME never calls the paused-run continuation, so paused runs cannot be resumed")
	}
	// And the retry path must NOT be what a paused run takes.
	i := strings.Index(src, "switch status {")
	if i < 0 {
		t.Fatal("the run-state switch is gone; RESUME is unconditional again")
	}
	seg := src[i:]
	if end := strings.Index(seg, "recordAudit"); end > 0 {
		seg = seg[:end]
	}
	if !strings.Contains(seg, "domain.WorkflowRunPaused") {
		t.Error("the run-state switch does not mention paused")
	}
}

// TestResumePausedSeamIsDistinctFromRetry: the two operations are different and must not collapse.
func TestResumePausedSeamIsDistinctFromRetry(t *testing.T) {
	b, err := os.ReadFile("service.go")
	if err != nil {
		t.Fatalf("read service.go: %v", err)
	}
	src := string(b)
	for _, want := range []string{
		"type ResumePausedRunStarter func",
		"func (s *Service) SetResumePausedRunStarter",
		"s.resumePausedRunFn = fn",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the paused-run seam is incomplete: missing %q", want)
		}
	}
}

// TestResumePausedSeamIsWired — a seam nothing calls is not a feature.
func TestResumePausedSeamIsWired(t *testing.T) {
	b, err := os.ReadFile("../api/api.go")
	if err != nil {
		t.Fatalf("read api.go: %v", err)
	}
	if !strings.Contains(string(b), "SetResumePausedRunStarter") {
		t.Fatal("SetResumePausedRunStarter is never wired in api.go, so a leaf RESUME has no " +
			"implementation to call at runtime")
	}
}
