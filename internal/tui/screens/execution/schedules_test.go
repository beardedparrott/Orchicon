package execution

// schedules_test.go — the Schedules pane: three lenses, a delete, and the jump to the run.
//
// The operator: "I noticed I don't see any 'Schedules' section in the TUI under Executions. We
// need to implement this in the TUI. We should be able to see upcoming, running, and finished
// just like the GUI and we should have delete operations, bulk delete operations, and a key that
// takes you to the workflow run."
//
// "Just like the GUI" is a testable claim, so the membership predicates are asserted against the
// same shapes the GUI handles: a SCHEDULED item is upcoming, a run-bound active item (or an
// active sequence parent) is running, and a run that has actually RUN is finished.

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// schedPlane is a plane with the mixed population the three views must sort apart.
func schedPlane() *fakePlane {
	return &fakePlane{
		items: []*apiv1.WorkItem{
			// upcoming: SCHEDULED.
			{Id: "wi-sched", Title: "Nightly", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_SCHEDULED,
				ScheduledStartAt: timestamppb.Now()},
			// running: RUNNING with a bound run.
			{Id: "wi-run", Title: "In flight", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_RUNNING,
				WorkflowRunId: "run-1"},
			// running: an active SEQUENCE PARENT — no bound run of its own.
			{Id: "wi-seq", Title: "Chain", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_RUNNING},
			{Id: "wi-seq-child", Title: "Chain child", ParentId: "wi-seq",
				Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING},
			// NOT running: active status but no run and no children.
			{Id: "wi-idle", Title: "Idle", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_RUNNING},
			// NOT upcoming: pending.
			{Id: "wi-pending", Title: "Later", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING},
		},
		runs: []*apiv1.WorkflowRun{
			// finished: it actually ran.
			{Id: "run-1", WorkflowId: "wf-1", WorkItemId: "wi-run",
				Status:    apiv1.WorkflowRunStatus_WORKFLOW_RUN_STATUS_COMPLETED,
				StartedAt: timestamppb.Now()},
			// NOT finished: queued, never started.
			{Id: "run-queued", WorkflowId: "wf-1",
				Status: apiv1.WorkflowRunStatus_WORKFLOW_RUN_STATUS_PENDING},
		},
		workflows: []*apiv1.Workflow{{Id: "wf-1", Name: "SDLC"}},
	}
}

// fetchIDs renders a fetch result as its row ids, for membership assertions.
func fetchIDs(t *testing.T, m *Model) []string {
	t.Helper()
	items, _, err := m.fetchSchedules(context.Background(), "")
	if err != nil {
		t.Fatalf("fetchSchedules: %v", err)
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.ID)
	}
	return out
}

func hasID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// --- the three lenses -------------------------------------------------------

// UPCOMING is the SCHEDULED set — and nothing else.
func TestSchedulesUpcomingIsTheScheduledSet(t *testing.T) {
	m := newModel(t, schedPlane())
	m.sched.scheduleView = schedUpcoming
	ids := fetchIDs(t, m)
	if !hasID(ids, "wi-sched") {
		t.Errorf("upcoming = %v, want the SCHEDULED item", ids)
	}
	for _, unwanted := range []string{"wi-run", "wi-pending", "wi-idle", "wi-seq"} {
		if hasID(ids, unwanted) {
			t.Errorf("upcoming = %v, must not contain %q", ids, unwanted)
		}
	}
}

// RUNNING is a bound in-flight run OR an active sequence parent — the GUI's predicate, and both
// halves matter (the parent has no run of its own to key on).
func TestSchedulesRunningIsBoundRunsAndSequenceParents(t *testing.T) {
	m := newModel(t, schedPlane())
	m.sched.scheduleView = schedRunning
	ids := fetchIDs(t, m)
	if !hasID(ids, "wi-run") {
		t.Errorf("running = %v, want the item with a bound in-flight run", ids)
	}
	if !hasID(ids, "wi-seq") {
		t.Errorf("running = %v, want the SEQUENCE PARENT (its children are pending, so nothing "+
			"else marks it)", ids)
	}
	// An active status with no run and no children is NOT running.
	if hasID(ids, "wi-idle") {
		t.Errorf("running = %v — an idle RUNNING item with no run and no children is not in flight", ids)
	}
	if hasID(ids, "wi-sched") {
		t.Errorf("running = %v, must not mix in upcoming items", ids)
	}
}

// FINISHED is the runs that have actually RUN — a queued run that never started is not history.
func TestSchedulesFinishedIsRunsThatRan(t *testing.T) {
	m := newModel(t, schedPlane())
	m.sched.scheduleView = schedFinished
	ids := fetchIDs(t, m)
	if !hasID(ids, "run-1") {
		t.Errorf("finished = %v, want the run that started", ids)
	}
	if hasID(ids, "run-queued") {
		t.Errorf("finished = %v — a run with no started_at has not run, so it is not history", ids)
	}
	// The rows are RUNS here, and they carry the resolved names.
	items, _, err := m.fetchSchedules(context.Background(), "")
	if err != nil {
		t.Fatalf("fetchSchedules: %v", err)
	}
	for _, it := range items {
		if it.ID == "run-1" && !strings.Contains(it.Title, "SDLC") {
			t.Errorf("finished row = %q, want the workflow name", it.Title)
		}
	}
}

// `v` cycles the lens, and the rows change with it.
func TestScheduleViewCycles(t *testing.T) {
	m := newModel(t, schedPlane())
	if m.sched.view() != schedUpcoming {
		t.Fatalf("the pane must start on upcoming, got %q", m.sched.view())
	}
	seen := map[schedView]bool{m.sched.view(): true}
	for i := 0; i < 3; i++ {
		m.cycleScheduleView()
		seen[m.sched.view()] = true
		if !strings.Contains(m.notice, m.sched.view().label()) {
			t.Errorf("the notice %q does not name the view it moved to (%q)", m.notice, m.sched.view())
		}
	}
	for _, want := range []schedView{schedUpcoming, schedRunning, schedFinished} {
		if !seen[want] {
			t.Errorf("cycling never reached %q", want)
		}
	}
	// It wraps back round.
	if m.sched.view() != schedUpcoming {
		t.Errorf("after three steps the view is %q, want it wrapped to upcoming", m.sched.view())
	}
}

// A view change DROPS the run bindings: they were derived from the previous read, so keeping
// them would let `g` jump to a run the row no longer represents.
func TestScheduleViewChangeDropsRunBindings(t *testing.T) {
	m := newModel(t, schedPlane())
	m.sched.scheduleView = schedUpcoming
	_ = fetchIDs(t, m)
	if m.sched.runFor("wi-sched") != "" && m.sched.runFor("wi-sched") != "" {
		// wi-sched has no run; use a bound one.
	}
	m.sched.scheduleView = schedRunning
	_ = fetchIDs(t, m)
	if got := m.sched.runFor("wi-run"); got != "run-1" {
		t.Fatalf("fixture: the running row should know its run, got %q", got)
	}
	m.cycleScheduleView() // running → finished
	if got := m.sched.runFor("wi-run"); got != "" {
		t.Errorf("the bindings survived a view change (%q) — a jump could then target a row "+
			"the pane no longer shows", got)
	}
}

// --- go to the run ----------------------------------------------------------

// `g` moves to the Runs pane and asks for the run's DETAIL, so the jump lands even when the run
// is not on the list page the operator is about to see.
func TestGoToRunJumpsToTheRunsPane(t *testing.T) {
	m := newModel(t, schedPlane())
	m.Base.SelectSource(srcSchedules)
	m.sched.scheduleView = schedRunning
	items, _, err := m.fetchSchedules(context.Background(), "")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	m.Base.LoadItems(srcSchedules, items, "")
	if !m.Base.SelectItem(srcSchedules, "wi-run") {
		t.Fatal("fixture: could not select the running row")
	}

	cmd := m.goToScheduleRun()
	if m.Base.ActiveSourceName() != srcRuns {
		t.Errorf("g left the pane on %q, want the Runs pane", m.Base.ActiveSourceName())
	}
	if cmd == nil {
		t.Error("g produced no command — it must reload the runs and request the run's detail")
	}
}

// `g` on a schedule that has NOT fired explains why there is nowhere to go, rather than
// silently doing nothing.
func TestGoToRunRefusesWhenNothingHasFired(t *testing.T) {
	m := newModel(t, schedPlane())
	m.Base.SelectSource(srcSchedules)
	m.sched.scheduleView = schedUpcoming
	items, _, err := m.fetchSchedules(context.Background(), "")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	m.Base.LoadItems(srcSchedules, items, "")
	if !m.Base.SelectItem(srcSchedules, "wi-sched") {
		t.Fatal("fixture: could not select the scheduled row")
	}
	m.goToScheduleRun()
	if !strings.Contains(m.notice, "has not fired") {
		t.Errorf("notice = %q — a schedule with no run must say so", m.notice)
	}
	if m.Base.ActiveSourceName() != srcSchedules {
		t.Error("a refused jump must not move the pane")
	}
}

// --- delete -----------------------------------------------------------------

// UPCOMING/RUNNING delete CANCELS the work item (status → cancelled), which is what the GUI's
// "Cancel N" does.
func TestScheduleDeleteCancelsInUpcomingAndRunning(t *testing.T) {
	for _, view := range []schedView{schedUpcoming, schedRunning} {
		p := schedPlane()
		// (the plane records DeleteWorkItem calls; no injected error needed here)
		m := newModel(t, p)
		m.Base.SelectSource(srcSchedules)
		m.sched.scheduleView = view
		acts := m.scheduleDeleteActions([]string{"wi-sched"})
		if len(acts) != 1 {
			t.Fatalf("%s: actions = %d, want the single cancel", view, len(acts))
		}
		if !strings.Contains(acts[0].Label, "cancel") {
			t.Errorf("%s: label = %q, want a cancel", view, acts[0].Label)
		}
		if !acts[0].Danger {
			t.Errorf("%s: a destructive action must be marked Danger", view)
		}
		if acts[0].Confirm == "" {
			t.Errorf("%s: a destructive action must confirm", view)
		}
		if err := acts[0].Do(context.Background()); err != nil {
			t.Errorf("%s: cancel failed: %v", view, err)
		}
		if len(p.deleted) != 1 || p.deleted[0] != "wi-sched" {
			t.Errorf("%s: DeleteWorkItem calls = %v, want the row's item", view, p.deleted)
		}
	}
}

// FINISHED delete REMOVES THE SCHEDULE from the bound work item and leaves the item alone —
// the GUI's "Remove N schedules".
func TestScheduleDeleteRemovesTheScheduleInFinished(t *testing.T) {
	p := schedPlane()
	m := newModel(t, p)
	m.Base.SelectSource(srcSchedules)
	m.sched.scheduleView = schedFinished
	items, _, err := m.fetchSchedules(context.Background(), "")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	m.Base.LoadItems(srcSchedules, items, "")

	acts := m.scheduleDeleteActions([]string{"run-1"})
	if len(acts) != 1 || !strings.Contains(acts[0].Label, "remove schedule") {
		t.Fatalf("finished actions = %+v, want a remove-schedule", acts)
	}
	if err := acts[0].Do(context.Background()); err != nil {
		t.Fatalf("remove schedule failed: %v", err)
	}
	if len(p.updated) != 1 {
		t.Fatalf("UpdateWorkItem calls = %d, want 1", len(p.updated))
	}
	up := p.updated[0]
	// The bound ITEM is the target, not the run — and all three fields move together, or the
	// backend can re-fire a fresh run the moment the binding is cleared.
	if up.GetId() != "wi-run" {
		t.Errorf("updated item = %q, want the run's bound work item", up.GetId())
	}
	if up.GetWorkflowRunId() != "" {
		t.Errorf("workflow_run_id = %q, want it cleared", up.GetWorkflowRunId())
	}
	if up.GetAutoStartWorkflow() {
		t.Error("auto_start_workflow is still true — clearing the binding alone would let a new run fire")
	}
	if up.GetRecurringSchedule() == nil {
		t.Error("recurring_schedule must be cleared (an empty message, not nil)")
	}
}

// A finished run with NO bound item has no schedule to remove, and says so rather than writing
// against a non-existent target.
func TestScheduleDeleteRefusesWithoutABoundItem(t *testing.T) {
	p := schedPlane()
	// A one-shot run that HAS run.
	p.runs = append(p.runs, &apiv1.WorkflowRun{
		Id: "run-oneshot", WorkflowId: "wf-1",
		Status:    apiv1.WorkflowRunStatus_WORKFLOW_RUN_STATUS_COMPLETED,
		StartedAt: timestamppb.Now(),
	})
	m := newModel(t, p)
	m.Base.SelectSource(srcSchedules)
	m.sched.scheduleView = schedFinished
	items, _, err := m.fetchSchedules(context.Background(), "")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	m.Base.LoadItems(srcSchedules, items, "")
	if !m.Base.SelectItem(srcSchedules, "run-oneshot") {
		t.Fatal("fixture: could not select the one-shot run row")
	}

	acts := m.scheduleDeleteActions([]string{"run-oneshot"})
	if len(acts) != 1 {
		t.Fatalf("actions = %d, want one", len(acts))
	}
	if err := acts[0].Do(context.Background()); err == nil {
		t.Error("removing a schedule from a run with no bound item reported success")
	}
	if len(p.updated) != 0 {
		t.Errorf("UpdateWorkItem was called %d time(s) for a run with no bound item", len(p.updated))
	}
	// And the KEY explains itself rather than being inert for no stated reason.
	if why := m.unavailableReason(keySchedDelete); why == "" {
		t.Error("the delete key has no explanation for a run with no bound item")
	}
}

// --- bulk -------------------------------------------------------------------

// THE THRESHOLD: bulk appears above one, and not at one — the shared kit2 rule.
func TestSchedulesBulkAppearsOnlyAboveOneSelection(t *testing.T) {
	m := newModel(t, schedPlane())
	m.Base.SelectSource(srcSchedules)

	single := m.scheduleDeleteActions([]string{"wi-sched"})
	if len(single) == 1 && strings.Contains(single[0].Label, "selected") {
		t.Fatalf("one row produced the bulk label %q — one row is not a bulk operation", single[0].Label)
	}

	bulk := m.scheduleDeleteActions([]string{"wi-sched", "wi-run"})
	if len(bulk) != 1 {
		t.Fatalf("bulk actions = %d, want one", len(bulk))
	}
	if !strings.Contains(bulk[0].Label, "2 selected") {
		t.Errorf("bulk label = %q, want it to name the count", bulk[0].Label)
	}
	if !strings.Contains(bulk[0].Confirm, "2") {
		t.Errorf("bulk confirm = %q, want it to name the count", bulk[0].Confirm)
	}
}

// The bulk write goes out once per row and COUNTS a rejection instead of claiming a sweep.
func TestSchedulesBulkDeleteWritesEachAndCountsFailures(t *testing.T) {
	p := schedPlane()
	p.deleteItemErr = connect.NewError(connect.CodeFailedPrecondition, context.DeadlineExceeded)
	m := newModel(t, p)
	m.Base.SelectSource(srcSchedules)
	m.sched.scheduleView = schedUpcoming

	acts := m.scheduleDeleteActions([]string{"wi-sched", "wi-pending"})
	if len(acts) != 1 {
		t.Fatalf("actions = %d, want one bulk action", len(acts))
	}
	err := acts[0].Do(context.Background())
	if err == nil {
		t.Fatal("a bulk delete where every write FAILED reported success")
	}
	if len(p.deleted) != 2 {
		t.Errorf("DeleteWorkItem calls = %d, want one per selected row", len(p.deleted))
	}
	if !strings.Contains(err.Error(), "0 of 2") {
		t.Errorf("the failure does not report the partial result: %v", err)
	}
}

// --- key reachability -------------------------------------------------------

// The pane's chords are reachable through the SCREEN's dispatch, not merely callable — the
// shell could otherwise shadow `v` or `g` before the screen sees them.
func TestScheduleKeysReachThroughDispatch(t *testing.T) {
	m := newModel(t, schedPlane())
	m.Base.SelectSource(srcSchedules)
	m.sched.scheduleView = schedUpcoming

	// `v` cycles the view through handleActionKey.
	if _, handled := m.handleActionKey(keySchedView); !handled {
		t.Fatal("v was not handled on the Schedules pane")
	}
	if m.sched.scheduleView != schedRunning {
		t.Errorf("v left the view at %q, want running", m.sched.scheduleView)
	}
	// `g` is handled (its refusal is still handling).
	if _, handled := m.handleActionKey(keyGoToRun); !handled {
		t.Error("g was not handled on the Schedules pane")
	}
	// `x` is NOT handled here: it is an ACTION key, dispatched through the action list so the
	// bar and the key agree on what it does. Asserts it is not claimed twice.
	if _, handled := m.handleActionKey(keySchedDelete); handled {
		t.Error("x was handled by the pane's own chord switch — it belongs to the action list")
	}
}

// The hint line describes the view on screen, so the keys it advertises are the ones that will
// actually run (cancel vs remove-schedule).
func TestScheduleHintFollowsTheView(t *testing.T) {
	m := newModel(t, schedPlane())
	m.Base.SelectSource(srcSchedules)

	m.sched.scheduleView = schedUpcoming
	up := m.HintLine()
	if !strings.Contains(up, "cancel") {
		t.Errorf("the upcoming hint does not advertise the cancel: %s", up)
	}
	if !strings.Contains(up, "g: go to the run") {
		t.Errorf("the hint does not advertise the jump: %s", up)
	}

	m.sched.scheduleView = schedFinished
	fin := m.HintLine()
	if !strings.Contains(fin, "remove schedule") {
		t.Errorf("the finished hint does not advertise remove-schedule: %s", fin)
	}
	if strings.Contains(fin, "cancel") {
		t.Errorf("the finished hint still advertises cancel: %s", fin)
	}
}
