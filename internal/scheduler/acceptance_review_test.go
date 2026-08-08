package scheduler

import (
	"strings"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
)

// TestFormatAcceptanceReviewCompleted verifies the deterministic
// acceptance-review aggregation for a completed run: succeeded steps with
// non-empty summaries are listed under "What was delivered", skipped and
// empty-summary steps are omitted, superseded iterations are omitted, and
// the header records run id + status.
func TestFormatAcceptanceReviewCompleted(t *testing.T) {
	ended := time.Now().UTC()
	run := db.WorkflowRunRow{
		ID: db.NewID(), WorkflowID: db.NewID(),
		Status: domain.WorkflowRunCompleted, Version: 1, EndedAt: &ended,
	}
	stepRuns := []db.WorkflowStepRunRow{
		{
			ID: db.NewID(), StepID: "a", StepName: "Implement",
			Status: domain.StepRunSucceeded,
			Result: []byte(`{"_summary":"Built the feature with tests."}`),
		},
		{
			ID: db.NewID(), StepID: "b", StepName: "SkippedGate",
			Status: domain.StepRunSkipped,
			Result: []byte(`{"_summary":"unused"}`),
		},
		{
			ID: db.NewID(), StepID: "c", StepName: "EmptySummary",
			Status: domain.StepRunSucceeded,
			Result: []byte(`{"_summary":"   "}`),
		},
		{
			ID: db.NewID(), StepID: "d", StepName: "Superseded",
			Status: domain.StepRunSucceeded, SupersededBy: db.NewID(),
			Result: []byte(`{"_summary":"old iteration"}`),
		},
	}

	out := formatAcceptanceReview(run, stepRuns, nil, domain.WorkflowRunCompleted)

	for _, want := range []string{
		"## Acceptance Review",
		"**Run:** `" + run.ID + "` · **Status:** completed",
		"### What was delivered",
		"- **Implement** (succeeded): Built the feature with tests.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("completed review missing %q; got:\n%s", want, out)
		}
	}
	for _, notWant := range []string{
		"SkippedGate",
		"EmptySummary",
		"Superseded",
		"Not delivered",
	} {
		if strings.Contains(out, notWant) {
			t.Errorf("completed review should not contain %q; got:\n%s", notWant, out)
		}
	}
}

// TestFormatAcceptanceReviewFailed verifies the failed-run review: the
// failed step's _summary/_issues land under "Not delivered / needs
// attention", and a succeeded sibling still appears under delivered.
func TestFormatAcceptanceReviewFailed(t *testing.T) {
	ended := time.Now().UTC()
	run := db.WorkflowRunRow{
		ID: db.NewID(), WorkflowID: db.NewID(),
		Status: domain.WorkflowRunFailed, Version: 1, EndedAt: &ended,
	}
	stepRuns := []db.WorkflowStepRunRow{
		{
			ID: db.NewID(), StepID: "a", StepName: "Implement",
			Status: domain.StepRunSucceeded,
			Result: []byte(`{"_summary":"Shipped the happy path."}`),
		},
		{
			ID: db.NewID(), StepID: "b", StepName: "Review",
			Status: domain.StepRunFailed,
			Result: []byte(`{"_summary":"","_issues":"Edge case not handled."}`),
		},
	}

	out := formatAcceptanceReview(run, stepRuns, nil, domain.WorkflowRunFailed)

	for _, want := range []string{
		"**Status:** failed",
		"### What was delivered",
		"- **Implement** (succeeded): Shipped the happy path.",
		"### Not delivered / needs attention",
		"- **Review** (failed): Edge case not handled.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("failed review missing %q; got:\n%s", want, out)
		}
	}
}

// TestFormatAcceptanceReviewRecovery verifies recovery episodes are listed
// under "Recovery" when present.
func TestFormatAcceptanceReviewRecovery(t *testing.T) {
	ended := time.Now().UTC()
	run := db.WorkflowRunRow{
		ID: db.NewID(), WorkflowID: db.NewID(),
		Status: domain.WorkflowRunCompleted, Version: 1, EndedAt: &ended,
	}
	stepRuns := []db.WorkflowStepRunRow{
		{
			ID: db.NewID(), StepID: "a", StepName: "Implement",
			Status: domain.StepRunSucceeded,
			Result: []byte(`{"_summary":"Work done."}`),
		},
	}
	recoveries := []db.RecoveryExecutionRow{
		{ID: db.NewID(), Status: "succeeded", TriggerReason: "execution_failed", Summary: "Retried with more context."},
	}
	out := formatAcceptanceReview(run, stepRuns, recoveries, domain.WorkflowRunCompleted)
	if !strings.Contains(out, "### Recovery") || !strings.Contains(out, "Retried with more context.") {
		t.Errorf("recovery section missing; got:\n%s", out)
	}
}

// TestFormatAcceptanceReviewEmptyFallback verifies a run whose steps carry
// no summaries still produces a non-empty review recording the terminal
// outcome (never an indistinguishable empty field).
func TestFormatAcceptanceReviewEmptyFallback(t *testing.T) {
	run := db.WorkflowRunRow{ID: db.NewID(), WorkflowID: db.NewID(), Status: domain.WorkflowRunCompleted, Version: 1}
	out := formatAcceptanceReview(run, nil, nil, domain.WorkflowRunCompleted)
	if !strings.Contains(out, "No step summaries were recorded.") {
		t.Errorf("empty run should fall back to a non-empty review; got:\n%s", out)
	}
}
