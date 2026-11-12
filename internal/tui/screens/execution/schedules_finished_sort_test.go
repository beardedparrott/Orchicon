package execution

// schedules_finished_sort_test.go — the finished lens asks for the GUI's sort.

import (
	"context"
	"testing"
)

func TestSchedulesFinishedAsksForTheGuiSort(t *testing.T) {
	plane := schedPlane()
	m := newModel(t, plane)
	m.sched.scheduleView = schedFinished

	if _, _, err := m.fetchSchedules(context.Background(), ""); err != nil {
		t.Fatalf("fetchSchedules: %v", err)
	}

	if len(plane.runListReqs) != 1 {
		t.Fatalf("ListWorkflowRuns calls = %d, want exactly 1 for the finished lens", len(plane.runListReqs))
	}
	if got := plane.runListReqs[0].GetSortBy(); got != "started_at" {
		t.Errorf("the finished lens asked for sort_by = %q, want \"started_at\".\n"+
			"The GUI's History passes sortBy: \"started_at\" (schedules.tsx:1058) and the server "+
			"supports it with (started_at, id) keyset pagination; without it this lens takes the "+
			"id DESC default, which only agrees with the GUI while a run's start order happens to "+
			"match its id order.", got)
	}
	// sort_order is deliberately NOT asserted: this client leaves it to the server default ("desc" =
	// newest first), which is also the GUI History view's default.
}

// The other two lenses read different sources and must not have acquired a run sort by accident —
// an upcoming/running fetch that started issuing ListWorkflowRuns would mean a membership change.
func TestOtherScheduleLensesDoNotReadWorkflowRuns(t *testing.T) {
	for _, view := range []schedView{schedUpcoming, schedRunning} {
		plane := schedPlane()
		m := newModel(t, plane)
		m.sched.scheduleView = view

		if _, _, err := m.fetchSchedules(context.Background(), ""); err != nil {
			t.Fatalf("%s fetchSchedules: %v", view.label(), err)
		}
		if len(plane.runListReqs) != 0 {
			t.Errorf("the %s lens issued %d ListWorkflowRuns call(s) — that read belongs to the "+
				"finished lens alone, so this is a membership change", view.label(), len(plane.runListReqs))
		}
	}
}
