package execution

// schedules_span_test.go — the RUNNING TIME and the FINISHED TIME on the Schedules pane.
//
// The operator: "It would be nice to apply the current running time and finished time onto schedules in
// the schedules section for both run and history as well as schedule details in the TUI."
//
// Three surfaces, one feature, so they are tested together:
//
//	the RUNNING lens rows     status · <time so far>
//	the HISTORY lens rows     status · <time it took>
//	the schedule/run DETAIL   "running for"/"ran for", plus what the number is measured from
//
// The spans themselves are the GUI's forms (screenkit.FmtElapsed), so the assertions below spell out the
// exact strings an operator would see in BOTH clients — "2m 0s", not "2m0s" — which is what makes a
// silent drift between the two clients a test failure rather than a support question.

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/timestamppb"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
)

// spanPlane is a plane whose running item and finished run have KNOWN spans, so the rendered strings
// are computable rather than merely "non-empty".
func spanPlane() *fakePlane {
	return &fakePlane{
		items: []*apiv1.WorkItem{
			// Running for ~90s, with the run bound. Its last update is the item's own evidence of when
			// it started (runningSince — the GUI's fallback chain), which is the case the row reports.
			{
				Id: "wi-running", Title: "Long build",
				Status:        apiv1.WorkItemStatus_WORK_ITEM_STATUS_RUNNING,
				WorkflowRunId: "run-ended",
				UpdatedAt:     timestamppb.New(timestamppb.Now().AsTime().Add(-90 * secondNs)),
			},
			// Upcoming: nothing has run, so nothing is timed.
			{
				Id: "wi-scheduled", Title: "Nightly",
				Status:           apiv1.WorkItemStatus_WORK_ITEM_STATUS_SCHEDULED,
				ScheduledStartAt: timestamppb.Now(),
			},
		},
		runs: []*apiv1.WorkflowRun{
			// A run that took exactly 2 minutes: 150s ago → 30s ago.
			{
				Id: "run-ended", WorkflowId: "wf-1", WorkItemId: "wi-running",
				Status:    apiv1.WorkflowRunStatus_WORKFLOW_RUN_STATUS_COMPLETED,
				StartedAt: timestamppb.New(timestamppb.Now().AsTime().Add(-150 * secondNs)),
				EndedAt:   timestamppb.New(timestamppb.Now().AsTime().Add(-30 * secondNs)),
			},
		},
		workflows: []*apiv1.Workflow{{Id: "wf-1", Name: "SDLC"}},
	}
}

// secondNs is one second in nanoseconds, for the fixture's offsets. Named so the spans above read as
// times rather than as magic numbers.
const secondNs = 1_000_000_000

// rowMeta finds a fetched row's Meta by id.
func rowMeta(t *testing.T, m *Model) map[string]string {
	t.Helper()
	items, _, err := m.fetchSchedules(context.Background(), "")
	if err != nil {
		t.Fatalf("fetchSchedules: %v", err)
	}
	out := make(map[string]string, len(items))
	for _, it := range items {
		out[it.ID] = it.Meta
	}
	return out
}

// fieldValue finds a detail field's value by key ("" when absent).
func fieldValue(fields []screenkit.Field, key string) string {
	for _, f := range fields {
		if f.Key == key {
			return f.Value
		}
	}
	return ""
}

// THE RUNNING LENS SHOWS THE CURRENT RUNNING TIME, beside the status it already showed.
func TestRunningRowShowsTheCurrentRunningTime(t *testing.T) {
	m := newModel(t, spanPlane())
	m.sched.scheduleView = schedRunning
	meta := rowMeta(t, m)
	got := meta["wi-running"]
	if !strings.HasPrefix(got, "running") {
		t.Fatalf("running row meta = %q, want it to start with the status", got)
	}
	// 90 seconds elapsed → the GUI's own form ("1m 30s"), not Go's "1m30s".
	if !strings.Contains(got, "1m 30s") {
		t.Errorf("running row meta = %q, want the current running time (≈\"1m 30s\") — the operator "+
			"asked for the running time on the schedules list, not only in the detail", got)
	}

	// UPCOMING IS UNCHANGED: nothing has run, so there is no time to report and the row keeps its
	// scheduled-start wording. (A "<1s" here would be a fabricated measurement.) The elapsed is
	// appended as " · <span>", so its absence is exactly the absence of that separator.
	m.sched.scheduleView = schedUpcoming
	for id, meta := range rowMeta(t, m) {
		if strings.Contains(meta, " · ") {
			t.Errorf("upcoming row %s carries a span (%q) — nothing has run yet", id, meta)
		}
	}
}

// THE HISTORY LENS SHOWS HOW LONG THE RUN TOOK — from the run's own started_at/ended_at.
func TestHistoryRowShowsHowLongTheRunTook(t *testing.T) {
	m := newModel(t, spanPlane())
	m.sched.scheduleView = schedFinished
	got := rowMeta(t, m)["run-ended"]
	if !strings.HasPrefix(got, "completed") {
		t.Fatalf("history row meta = %q, want it to start with the run's status", got)
	}
	if !strings.Contains(got, "2m 0s") {
		t.Errorf("history row meta = %q, want the finished span (\"2m 0s\") — the operator asked for "+
			"the finished time on run and history alike", got)
	}
}

// A LIVE RUN IN HISTORY reports the time SO FAR rather than nothing: History membership is "it has a
// started_at" (frontend/src/lib/schedules-model.ts), so an in-flight run is rendered here too.
func TestHistoryRowShowsTimeSoFarForALiveRun(t *testing.T) {
	plane := spanPlane()
	plane.runs[0].EndedAt = nil
	plane.runs[0].Status = apiv1.WorkflowRunStatus_WORKFLOW_RUN_STATUS_RUNNING
	m := newModel(t, plane)
	m.sched.scheduleView = schedFinished
	got := rowMeta(t, m)["run-ended"]
	if !strings.Contains(got, "2m 30s") {
		t.Errorf("history row meta = %q, want the span so far (\"2m 30s\") for a run that has not ended", got)
	}
}

// THE DETAIL SAYS THE SPAN *AND* WHAT IT IS MEASURED FROM. The running number is derived from the
// item's own timestamps (a WorkItem has no run-start field), so the pane names the timestamp it used
// instead of presenting an approximation as a stored fact.
func TestScheduleDetailShowsTheRunningSpanAndItsSource(t *testing.T) {
	m := newModel(t, spanPlane())
	m.sched.scheduleView = schedRunning
	_, fields, _, err := m.detail(context.Background(), srcSchedules, "wi-running")
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if got := fieldValue(fields, "running for"); !strings.Contains(got, "1m 30s") {
		t.Errorf("detail \"running for\" = %q, want the span (≈\"1m 30s\")", got)
	}
	if got := fieldValue(fields, "measured from"); !strings.Contains(got, "its last update") {
		t.Errorf("detail \"measured from\" = %q, want it to name the timestamp the span is derived from "+
			"(the item carries no run start, so the derivation must be visible)", got)
	}
}

// THE FINISHED-RUN DETAIL SAYS HOW LONG IT RAN, from the run's own record.
func TestFinishedRunDetailShowsHowLongItRan(t *testing.T) {
	m := newModel(t, spanPlane())
	m.sched.scheduleView = schedFinished
	_, fields, _, err := m.detail(context.Background(), srcSchedules, "run-ended")
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if got := fieldValue(fields, "ran for"); !strings.Contains(got, "2m 0s") {
		t.Errorf("finished-run detail \"ran for\" = %q, want \"2m 0s\"", got)
	}
	// The two raw timestamps stay: the span is added beside them, not instead of them.
	if fieldValue(fields, "started") == "" || fieldValue(fields, "ended") == "" {
		t.Errorf("the run detail lost its started/ended fields: %+v", fields)
	}
}

// THE RUNS PANE'S OWN DETAIL gets the same field — one helper, two surfaces, so a run opened from
// either place answers "how long did that take?".
func TestRunsDetailShowsHowLongItRan(t *testing.T) {
	m := newModel(t, spanPlane())
	_, fields, _, err := m.detail(context.Background(), srcRuns, "run-ended")
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if got := fieldValue(fields, "ran for"); !strings.Contains(got, "2m 0s") {
		t.Errorf("runs detail \"ran for\" = %q, want \"2m 0s\"", got)
	}
}
