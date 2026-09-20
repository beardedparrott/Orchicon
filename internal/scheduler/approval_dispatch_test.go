package scheduler

import (
	"encoding/json"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/runtime"
	"github.com/beardedparrott/orchicon/internal/workflow"
)

// TestResolveApprovalWorkItems covers the ticket-resolution rules for a
// worker-backed approval step. The execution must run against the run's
// SHARED work item (never a per-step artifact), resolved exactly like a
// TASK step: recovering → the ticket recorded in _work_item_id;
// otherwise WORK_ITEM markers upstream; otherwise the run's bound item.
func TestResolveApprovalWorkItems(t *testing.T) {
	const (
		ticketID   = "ticket-01"
		markerID   = "marker-02"
		runBoundID = "bound-03"
	)
	approvalStep := workflow.StepWire{
		ID:        "approval-1",
		Name:      "Approve",
		Kind:      "approval",
		Ref:       "w_se_code_approver",
		DependsOn: []string{"workitem-1"},
	}
	// A bare approval step with no upstream WORK_ITEM marker — used for
	// the bound-item and no-ticket cases, since markers take precedence
	// over the run's bound item (same as TASK steps).
	bareApprovalStep := workflow.StepWire{
		ID:   "approval-2",
		Name: "Approve (bare)",
		Kind: "approval",
		Ref:  "w_se_code_approver",
	}
	markerStep := workflow.StepWire{
		ID:     "workitem-1",
		Name:   "Ticket",
		Kind:   "work_item",
		Config: `{"work_item_id":"` + markerID + `"}`,
	}

	cases := []struct {
		name       string
		sr         db.WorkflowStepRunRow
		step       workflow.StepWire
		runBoundID string
		want       []string
	}{
		{
			name:       "recovering step re-uses recorded ticket",
			sr:         db.WorkflowStepRunRow{Status: "recovering", Result: []byte(`{"_work_item_id":"` + ticketID + `"}`)},
			step:       approvalStep,
			runBoundID: "",
			want:       []string{ticketID},
		},
		{
			name:       "recovering with empty recorded ticket falls through to markers",
			sr:         db.WorkflowStepRunRow{Status: "recovering", Result: []byte(`{}`)},
			step:       approvalStep,
			runBoundID: "",
			want:       []string{markerID},
		},
		{
			name:       "normal step uses upstream WORK_ITEM marker",
			sr:         db.WorkflowStepRunRow{Status: "ready", Result: []byte(`{}`)},
			step:       approvalStep,
			runBoundID: "",
			want:       []string{markerID},
		},
		{
			name:       "normal step without markers uses the run's bound item",
			sr:         db.WorkflowStepRunRow{Status: "ready", Result: []byte(`{}`)},
			step:       bareApprovalStep,
			runBoundID: runBoundID,
			want:       []string{runBoundID},
		},
		{
			name:       "no ticket at all resolves to nil",
			sr:         db.WorkflowStepRunRow{Status: "ready", Result: []byte(`{}`)},
			step:       bareApprovalStep,
			runBoundID: "",
			want:       nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveApprovalWorkItems(tc.sr, tc.step, []workflow.StepWire{approvalStep, markerStep, bareApprovalStep}, tc.runBoundID)
			if len(got) != len(tc.want) {
				t.Fatalf("resolveApprovalWorkItems() = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("resolveApprovalWorkItems() = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// TestBuildApprovalStepResult verifies the step-run record written when a
// worker-backed approval dispatches: the shared ticket id, the composite
// prompt, the approver worker pin, the upstream review context, and the
// pending decision marker — with the recovery narrative preserved across
// a re-dispatch.
func TestBuildApprovalStepResult(t *testing.T) {
	prev := []byte(`{"_recovery_summary":"the first attempt failed"}`)
	got := buildApprovalStepResult(
		"ticket-01", "composite prompt", "fp1234", "w_se_code_approver", 7,
		"Senior SWE", "the upstream summary", []string{"a.go", "b.go"}, "the AC",
		prev,
	)
	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("buildApprovalStepResult returned invalid JSON: %v", err)
	}
	want := map[string]any{
		"_work_item_id":     "ticket-01",
		"_prompt":           "composite prompt",
		"_worker_id":        "w_se_code_approver",
		"_worker_version":   float64(7),
		"_upstream_worker":  "Senior SWE",
		"_upstream_summary": "the upstream summary",
		"_decision":         "pending",
		"_recovery_summary": "the first attempt failed",
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("buildApprovalStepResult()[%q] = %v, want %v", k, m[k], v)
		}
	}
	if files, ok := m["_upstream_files"].([]any); !ok || len(files) != 2 {
		t.Errorf("buildApprovalStepResult() _upstream_files = %v, want [a.go b.go]", m["_upstream_files"])
	}
	if m["_ac"] != "the AC" {
		t.Errorf("buildApprovalStepResult() _ac = %v, want the AC", m["_ac"])
	}

	// A fresh dispatch (no prior result) must NOT carry a recovery summary.
	fresh := buildApprovalStepResult("ticket-01", "p", "fp", "w_se_code_approver", 7, "", "", nil, "", nil)
	var fm map[string]any
	_ = json.Unmarshal(fresh, &fm)
	if _, ok := fm["_recovery_summary"]; ok {
		t.Errorf("fresh dispatch carried a stale _recovery_summary: %v", fm)
	}
}

// TestApprovalDecisionFromSources verifies that the step run's own
// _decision is authoritative (the step run is the approval record) and
// the work item's decision is only a legacy fallback for step runs that
// carry none. A stale decision left on a shared ticket by a prior
// run/step must never override the current step run's real decision.
func TestApprovalDecisionFromSources(t *testing.T) {
	wiFailure := []byte(`{"_decision":"failure"}`)
	wiEmpty := []byte(`{}`)
	cases := []struct {
		name       string
		srDecision string
		wiResults  []byte
		want       string
	}{
		{"step-run success wins over stale ticket failure", "success", wiFailure, "success"},
		{"step-run rejected wins over stale ticket success", "rejected", []byte(`{"_decision":"success"}`), "rejected"},
		{"step-run empty falls back to ticket decision", "", wiFailure, "failure"},
		{"step-run empty falls back to approved ticket decision", "", []byte(`{"_decision":"approved"}`), "approved"},
		{"step-run empty + empty ticket results stays empty", "", wiEmpty, ""},
		{"step-run empty + no ticket results stays empty", "", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := approvalDecisionFromSources(tc.srDecision, tc.wiResults); got != tc.want {
				t.Errorf("approvalDecisionFromSources(%q, %s) = %q, want %q", tc.srDecision, tc.wiResults, got, tc.want)
			}
		})
	}
}

// typedNilLifecycle returns a RuntimeLifecycle interface wrapping a nil
// *runtime.Lifecycle — the shape produced by the old server.go wiring
// (`var runtimeLifecycle *runtime.Lifecycle` left unset) that caused the
// headless-serve crash.
func typedNilLifecycle() RuntimeLifecycle {
	var lc *runtime.Lifecycle
	return lc
}

// TestRuntimeEnabledNilGuard covers the headless-serve crash: a
// typed-nil *runtime.Lifecycle wrapped in the RuntimeLifecycle interface
// is non-nil to the interface, and calling its methods would
// nil-dereference the receiver. runtimeEnabled must treat it as
// disabled, exactly like a nil interface.
func TestRuntimeEnabledNilGuard(t *testing.T) {
	var nilInterface *WorkflowReconciler
	_ = nilInterface

	r := &WorkflowReconciler{}
	if r.runtimeEnabled() {
		t.Error("nil interface runtime must be disabled")
	}

	// Simulate the server.go wiring bug: a typed-nil *runtime.Lifecycle
	// passed as the RuntimeLifecycle interface.
	r.runtime = typedNilLifecycle()
	if r.runtimeEnabled() {
		t.Error("typed-nil runtime must be disabled (this crashed the plane on headless serve)")
	}
}
