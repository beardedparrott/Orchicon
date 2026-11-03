package execution

// run_failure_field_test.go — a FAILED run must say why, in the TUI, without decoding anything.
//
// The GUI half is RunFailureCard; this is the TUI half of the same gap. A run that fails PREFLIGHT never
// produces an execution, so there is no worker summary to read and the blockage card is silent (nothing can
// block a run that is already terminal).

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/timestamppb"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// The reported message, verbatim in shape: a remedy line, a blank line, then the raw git detail.
func reportedRun() *apiv1.WorkflowRun {
	return &apiv1.WorkflowRun{
		Id:        "01M6FCQX70K2W4YQFE8HZ5J8XF",
		Status:    apiv1.WorkflowRunStatus_WORKFLOW_RUN_STATUS_FAILED,
		ErrorKind: "worktree_no_base_ref",
		ErrorMessage: "This repository has no valid HEAD to branch from.\n\n" +
			`Detail: could not create the run worktree: base ref HEAD — fatal: invalid reference: HEAD`,
		CreatedAt: timestamppb.Now(),
	}
}

func TestFailureTextIsNonEmptyForAFailedRun(t *testing.T) {
	if got := tuiFailureText(reportedRun()); got == "" {
		t.Fatal("a failed run produced no failure text — the TUI would show its steps and nothing about why " +
			"it ended, which is the reported gap")
	}
}

// BOTH halves must survive: the remedy AND the raw detail. Truncating at the newline would hide the part that
// names the branch and directory.
func TestFailureTextKeepsTheRemedyAndTheDetail(t *testing.T) {
	got := tuiFailureText(reportedRun())
	for _, want := range []string{"no valid HEAD", "invalid reference"} {
		if !strings.Contains(got, want) {
			t.Errorf("failure text lost %q: %s", want, got)
		}
	}
	// It is a single field VALUE, so the blank line must not survive as a layout break.
	if strings.Contains(got, "\n") {
		t.Errorf("failure text kept a newline, which would break the field row: %q", got)
	}
}

func TestFailureKindIsExposed(t *testing.T) {
	if got := tuiFailureKind(reportedRun()); got != "worktree_no_base_ref" {
		t.Fatalf("failure kind = %q, want the run's error_kind", got)
	}
}

// A healthy run contributes NOTHING — an empty field would render a blank row on every run.
func TestHealthyRunHasNoFailureFields(t *testing.T) {
	ok := &apiv1.WorkflowRun{Id: "r1", Status: apiv1.WorkflowRunStatus_WORKFLOW_RUN_STATUS_COMPLETED}
	if got := tuiFailureText(ok); got != "" {
		t.Errorf("a completed run produced failure text: %q", got)
	}
	if got := tuiFailureKind(ok); got != "" {
		t.Errorf("a completed run produced a failure kind: %q", got)
	}
}
