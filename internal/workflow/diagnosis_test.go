package workflow

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
)

// The run-blockage classifier, pinned to the field incident it was written for.
//
// Run 01M53DXZSF5G2T2Z592F7G5ETE sat "running" with an empty current step and
// no execution anywhere while the UI said "waiting for dispatch". Nothing in
// the run view could name the cause: the project's dispatch cap was full, and
// the ten executions holding it were invisible (they carried `unhealthy`, which
// the reaper's old status list did not include). These cases make the
// classification a fact rather than an inference.
//
// No database: buildRunDiagnosis is a function of the state handed to it, which
// is exactly why it is structured that way.
func TestBuildRunDiagnosisClassifiesBlockage(t *testing.T) {
	stale := time.Now().Add(-3 * time.Hour)
	unhealthy := []db.ExecutionRow{{
		ID: "exec-unhealthy", Status: domain.ExecutionUnhealthy,
		HealthState: domain.HealthUnhealthy, StartedAt: &stale, WorkerName: "PR Reviewer",
	}}
	stepWaiting := func(status string, result map[string]any) []db.WorkflowStepRunRow {
		raw, _ := json.Marshal(result)
		return []db.WorkflowStepRunRow{{ID: "sr-1", StepID: "step-1", Status: status, Result: raw}}
	}

	cases := []struct {
		name         string
		in           diagnosisInput
		wantBlocked  bool
		wantKind     string
		wantClear    bool
		wantStuckLen int
	}{
		{
			name: "the field incident: project pinned at cap by unreaped unhealthy executions",
			in: diagnosisInput{
				runStatus: domain.WorkflowRunRunning, runtimeReady: true,
				projectID: "proj-1", active: 10, limit: 10,
				stepRuns: stepWaiting(domain.StepRunRunning, map[string]any{
					"_dispatch_wait": "project capacity 10/10 — waiting for a slot",
				}),
				stuck: unhealthy, now: time.Now(),
			},
			wantBlocked: true, wantKind: blockageProjectAtCapacity,
			wantClear: true, wantStuckLen: 1,
		},
		{
			name: "at cap with only LIVE holders: still blocked, but nothing to clear",
			in: diagnosisInput{
				runStatus: domain.WorkflowRunRunning, runtimeReady: true,
				projectID: "proj-1", active: 10, limit: 10,
				stepRuns: stepWaiting(domain.StepRunReady, nil),
				stuck:    nil, now: time.Now(),
			},
			// The distinction matters: clearable=false tells the operator the
			// slots are held by real work, so waiting is the correct action
			// rather than a fruitless button press.
			wantBlocked: true, wantKind: blockageProjectAtCapacity,
			wantClear: false, wantStuckLen: 0,
		},
		{
			name: "a runtime that is not ready outranks capacity — freeing a slot would not help",
			in: diagnosisInput{
				runStatus: domain.WorkflowRunRunning, runtimeReady: false,
				projectID: "proj-1", active: 10, limit: 10,
				stuck: unhealthy, now: time.Now(),
			},
			wantBlocked: true, wantKind: blockageDispatchDeferred,
			wantClear: true, wantStuckLen: 1,
		},
		{
			name: "deferred dispatch below the cap: named from the step's own note",
			in: diagnosisInput{
				runStatus: domain.WorkflowRunRunning, runtimeReady: true,
				projectID: "proj-1", active: 1, limit: 10,
				stepRuns: stepWaiting(domain.StepRunRunning, map[string]any{
					"_dispatch_wait": "worker capacity 2/2 — waiting for a slot",
				}),
				now: time.Now(),
			},
			wantBlocked: true, wantKind: blockageDispatchDeferred,
		},
		{
			name: "a recovering step that cannot re-dispatch",
			in: diagnosisInput{
				runStatus: domain.WorkflowRunRunning, runtimeReady: true,
				projectID: "proj-1", active: 0, limit: 10,
				stepRuns: stepWaiting(domain.StepRunRecovering, nil),
				now:      time.Now(),
			},
			wantBlocked: true, wantKind: blockageRecoveringUnresumable,
		},
		{
			name: "an approval is a wait, reported as such",
			in: diagnosisInput{
				runStatus: domain.WorkflowRunRunning, runtimeReady: true,
				projectID: "proj-1", limit: 10,
				stepRuns: stepWaiting(domain.StepRunApprovalPending, nil),
				now:      time.Now(),
			},
			wantBlocked: true, wantKind: blockageAwaitingApproval,
		},
		{
			name: "a HEALTHY run must not report itself blocked",
			in: diagnosisInput{
				runStatus: domain.WorkflowRunRunning, runtimeReady: true,
				projectID: "proj-1", active: 1, limit: 10,
				// pending is "not armed yet", not "waiting"; running WITH an
				// execution is work in progress. Neither is a blockage, and
				// calling them one would make every healthy run cry wolf.
				stepRuns: []db.WorkflowStepRunRow{
					{ID: "sr-a", StepID: "step-a", Status: domain.StepRunPending},
					{ID: "sr-b", StepID: "step-b", Status: domain.StepRunRunning, WorkerExecutionID: "exec-1"},
					{ID: "sr-c", StepID: "step-c", Status: domain.StepRunSucceeded},
				},
				now: time.Now(),
			},
			wantBlocked: false, wantKind: blockageNone,
		},
		{
			name: "a terminal run is not blocked",
			in: diagnosisInput{
				runStatus: domain.WorkflowRunCompleted, runtimeReady: true,
				stepRuns: stepWaiting(domain.StepRunReady, nil), now: time.Now(),
			},
			wantBlocked: false, wantKind: blockageNone,
		},
		{
			name: "waiting with no identified cause says so instead of guessing",
			in: diagnosisInput{
				runStatus: domain.WorkflowRunRunning, runtimeReady: true,
				projectID: "proj-1", active: 0, limit: 10,
				stepRuns: stepWaiting(domain.StepRunRunning, nil),
				now:      time.Now(),
			},
			wantBlocked: true, wantKind: blockageStalledUnexplained,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := buildRunDiagnosis(tc.in)
			if d.Blocked != tc.wantBlocked {
				t.Errorf("blocked = %v, want %v (kind=%q detail=%q)", d.Blocked, tc.wantBlocked, d.BlockageKind, d.Detail)
			}
			if d.BlockageKind != tc.wantKind {
				t.Errorf("blockage_kind = %q, want %q", d.BlockageKind, tc.wantKind)
			}
			if tc.wantBlocked && d.Detail == "" {
				t.Error("a blocked run reported no detail sentence — the operator is left guessing, which is the whole defect")
			}
			if d.Clearable != tc.wantClear {
				t.Errorf("clearable = %v, want %v", d.Clearable, tc.wantClear)
			}
			if len(d.StuckExecutions) != tc.wantStuckLen {
				t.Errorf("stuck_executions = %d, want %d", len(d.StuckExecutions), tc.wantStuckLen)
			}
			// The capacity numbers are the gate's own, reported verbatim.
			if int(d.ActiveExecutions) != tc.in.active {
				t.Errorf("active = %d, want %d", d.ActiveExecutions, tc.in.active)
			}
			if int(d.DispatchLimit) != tc.in.limit {
				t.Errorf("limit = %d, want %d", d.DispatchLimit, tc.in.limit)
			}
		})
	}
}

// The stuck-execution list is an AGING view: the operator needs to see how long
// a slot has been held, because a three-week-old unhealthy execution and a
// two-minute-old one are different situations.
func TestStuckExecutionsReportAge(t *testing.T) {
	started := time.Now().Add(-72 * time.Hour)
	d := buildRunDiagnosis(diagnosisInput{
		runStatus: domain.WorkflowRunRunning, runtimeReady: true,
		projectID: "proj-1", active: 1, limit: 10,
		stuck: []db.ExecutionRow{{
			ID: "exec-1", Status: domain.ExecutionUnhealthy, StartedAt: &started,
			WorkerName: "PR Reviewer", WorkflowRunID: "run-old",
		}},
		now: time.Now(),
	})
	if len(d.StuckExecutions) != 1 {
		t.Fatalf("stuck_executions = %d, want 1", len(d.StuckExecutions))
	}
	got := d.StuckExecutions[0]
	if got.AgeSeconds < 71*3600 {
		t.Errorf("age_seconds = %d, want roughly 72h", got.AgeSeconds)
	}
	if got.WorkerName != "PR Reviewer" || got.RunId != "run-old" || got.Status != domain.ExecutionUnhealthy {
		t.Errorf("stuck execution = %+v, want the worker/run/status carried through", got)
	}
}

// A stuck execution with no recorded start must report 0 rather than a
// negative age or a wild value — the card formats it.
func TestStuckExecutionsWithoutStartTime(t *testing.T) {
	d := buildRunDiagnosis(diagnosisInput{
		runStatus: domain.WorkflowRunRunning, runtimeReady: true,
		projectID: "proj-1", limit: 10,
		stuck: []db.ExecutionRow{{ID: "exec-1", Status: domain.ExecutionUnhealthy}},
		now:   time.Now(),
	})
	if len(d.StuckExecutions) != 1 {
		t.Fatalf("stuck_executions = %d, want 1", len(d.StuckExecutions))
	}
	if d.StuckExecutions[0].AgeSeconds != 0 || d.StuckExecutions[0].StartedAtUnix != 0 {
		t.Errorf("age/started_at = %d/%d, want 0/0 when no start was recorded",
			d.StuckExecutions[0].AgeSeconds, d.StuckExecutions[0].StartedAtUnix)
	}
}
