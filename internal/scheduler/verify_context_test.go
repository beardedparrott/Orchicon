package scheduler

import (
	"context"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
)

// The verifier's context isolation is a PROMPT property with a policy behind it, so it is tested at the
// prompt level: the composite is the artifact the model actually reads.

// TestIsVerifierStep pins which steps are treated as independent judges.
func TestIsVerifierStep(t *testing.T) {
	cases := []struct {
		name                   string
		stepID, stepName, role string
		want                   bool
	}{
		{"pr reviewer by id", "step-pr-reviewer", "", "", true},
		{"qa by name", "", "QA Engineer", "", true},
		{"approver", "step-approval", "Code Approver", "", true},
		{"reviewer by role prose", "", "", "You are a REVIEWER. Your job is to find what is wrong", true},
		{"implementer is not a verifier", "step-sse", "Senior Software Engineer", "You implement the feature", false},
		{"architect is not a verifier", "step-q4xlbg6v", "Principal Software Architect", "You produce the plan", false},
		{"devops is not a verifier", "step-devops-pr", "DevOps Engineer", "You ship it", false},
		{"quick worker is not a verifier", "step-quick", "Quick Software Engineer", "You own the task", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isVerifierStep(tc.stepID, tc.stepName, tc.role); got != tc.want {
				t.Fatalf("isVerifierStep = %v, want %v", got, tc.want)
			}
		})
	}
}

// priorRun builds a completed upstream step so hasPriorSteps is true.
func priorRun(stepID string) map[string]db.WorkflowStepRunRow {
	return map[string]db.WorkflowStepRunRow{
		stepID: {
			ID: stepID + "-run", StepID: stepID, StepName: "Senior Software Engineer",
			Status: domain.StepRunSucceeded,
			Result: []byte(`{"_summary":"Implemented everything perfectly. All tests pass.","_decision":"success"}`),
		},
	}
}

// TestVerifierPromptWithholdsTheImplementerSummary is the core assertion: the reviewer must not be pointed
// at the implementer's own account of what it did.
//
// This is the clean-room property. A summary written by the model that believes the work is complete is
// the CLAIM under review; handing it over primes the reviewer with the conclusion it exists to test
// independently. The reviewer keeps everything checkable — diff, criteria, facts ledger, issues.
func TestVerifierPromptWithholdsTheImplementerSummary(t *testing.T) {
	r := &WorkflowReconciler{}
	item := db.WorkItemRow{
		Title: "Add a thing", Status: "running", RuntimeImage: "img:1",
		WorkflowStepID:     "step-pr-reviewer",
		AcceptanceCriteria: "1. The thing works.",
	}
	reviewer := db.WorkerVersionRow{Role: "You are a REVIEWER. Your job is to find what is wrong"}
	out, _, err := r.buildCompositePrompt(context.Background(), nil, "tnt_test", item, reviewer, nil, priorRun("step-sse"))
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(out, "You are reviewing independently") {
		t.Error("the reviewer must be told it is reviewing independently")
	}
	if !strings.Contains(out, "withheld from you on purpose") {
		t.Error("the reviewer must be told the summary was withheld, or it will go looking for it")
	}
	if strings.Contains(out, "- `.orchicon/") && strings.Contains(out, "/summary` — what the previous worker did") {
		t.Error("the reviewer must NOT be pointed at the implementer's summary as required reading")
	}
	// It keeps what it can CHECK.
	for _, want := range []string{"facts_learned", "issues", "touched_files", "The thing works."} {
		if !strings.Contains(out, want) {
			t.Errorf("the reviewer must still receive %q — isolation removes the narrative, not the evidence", want)
		}
	}
}

// TestImplementerStillGetsTheSummary is the control: the isolation must not leak onto producing steps,
// which legitimately need the upstream summary to continue the work.
func TestImplementerStillGetsTheSummary(t *testing.T) {
	r := &WorkflowReconciler{}
	item := db.WorkItemRow{Title: "Add a thing", Status: "running", RuntimeImage: "img:1", WorkflowStepID: "step-sse"}
	sse := db.WorkerVersionRow{Role: "You implement the feature"}
	out, _, err := r.buildCompositePrompt(context.Background(), nil, "tnt_test", item, sse, nil, priorRun("step-architect"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "what the previous worker did") {
		t.Error("a producing step must still be pointed at the upstream summary")
	}
	if strings.Contains(out, "You are reviewing independently") {
		t.Error("the verifier isolation block must not appear for a producing step")
	}
}
