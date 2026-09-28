package execution

// schedules_queued_qa_render_test.go — QA (step 4) surface verification for the Upcoming fix.
//
// The engineer's tests assert the FETCH's membership. This file asserts the SURFACE the operator
// actually reads: the rendered pane text. It is the TUI equivalent of a screenshot check — the
// rendered frame is dumped to /tmp/orchicon/ so it can be read back and compared against the
// acceptance criteria (queued children visible, the Schedules empty state GONE for the
// reproduction, and still present for a tenant with genuinely nothing scheduled).

import (
	"context"
	"os"
	"strings"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

const qaEmptyMsg = "nothing scheduled here — v switches to running / finished"

// qaRenderUpcoming loads the upcoming lens into the pane and returns the rendered frame.
func qaRenderUpcoming(t *testing.T, m *Model) string {
	t.Helper()
	m.sched.scheduleView = schedUpcoming
	m.Base.SelectSource(srcSchedules)
	items, _, err := m.fetchSchedules(context.Background(), "")
	if err != nil {
		t.Fatalf("fetchSchedules(upcoming): %v", err)
	}
	m.Base.LoadItems(srcSchedules, items, "")
	m.Base.SetSize(160, 30)
	return m.Base.View()
}

// 1. THE REPRODUCTION, ON THE SURFACE: with an active sequence parent and pending children the
// rendered upcoming pane lists the queued children and does NOT show the empty state.
func TestQASchedulesUpcomingRendersTheQueuedChildrenNotTheEmptyState(t *testing.T) {
	m := newModel(t, queuedPlane())
	view := qaRenderUpcoming(t, m)
	_ = os.WriteFile("/tmp/orchicon/tui-upcoming-queued.txt", []byte(view), 0o644)

	if strings.Contains(view, qaEmptyMsg) {
		t.Errorf("the rendered upcoming pane still shows the Schedules empty state:\n%s", view)
	}
	for _, want := range []string{"Child two", "Child three", "Nightly", "queued — waits for the current step"} {
		if !strings.Contains(view, want) {
			t.Errorf("rendered upcoming pane is missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "Child one") {
		t.Errorf("the ARMED child (already fired) must not be listed as upcoming:\n%s", view)
	}
	if strings.Contains(view, "Never queued") {
		t.Errorf("a finished parent's pending child leaked into upcoming:\n%s", view)
	}
}

// 2. NO FALSE POSITIVE, ON THE SURFACE: a tenant with nothing scheduled and nothing queued still
// shows the pane's declared empty state.
func TestQASchedulesUpcomingStillRendersTheEmptyStateWhenNothingIsScheduled(t *testing.T) {
	m := newModel(t, &fakePlane{items: []*apiv1.WorkItem{
		{Id: "wi-done-parent", Title: "Finished chain", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_SUCCEEDED},
		{Id: "wi-done-child", Title: "Never queued", ParentId: "wi-done-parent",
			Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING},
	}})
	view := qaRenderUpcoming(t, m)
	_ = os.WriteFile("/tmp/orchicon/tui-upcoming-empty.txt", []byte(view), 0o644)

	if !strings.Contains(view, qaEmptyMsg) {
		t.Errorf("a genuinely empty upcoming pane lost its empty state:\n%s", view)
	}
	if strings.Contains(view, "Never queued") {
		t.Errorf("the derivation produced a false positive:\n%s", view)
	}
}
