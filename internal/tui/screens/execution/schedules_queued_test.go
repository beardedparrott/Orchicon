package execution

// schedules_queued_test.go — the UPCOMING lens's SECOND membership source: the queued sequence
// children.
//
// The operator: "In the TUI, it is not showing the upcoming runs but the GUI does. It says 'nothing
// scheduled here — v switches to running / finished'." A sequence parent is the only item carrying
// a scheduled_start_at (the engine keeps its children PENDING and arms one at a time), so the
// children never matched the SCHEDULED query the pane used to issue alone. These tests pin the
// second source, the chain-order, the two-key behaviour on a queued row, and the parity of the
// predicate with the GUI's model over one shared fixture.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
)

// queuedPlane is the recorded reproduction: one ACTIVE sequence parent with pending children, a
// genuinely SCHEDULED item, and the decoys that must not leak into the derivation.
func queuedPlane() *fakePlane {
	return &fakePlane{
		items: []*apiv1.WorkItem{
			// The active sequence parent: RUNNING with NO bound run of its own.
			{Id: "wi-seq", Title: "Epic chain", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_RUNNING, SortOrder: 1},
			// The armed child — it HAS fired, so it is not queued.
			{Id: "wi-seq-1", Title: "Child one", ParentId: "wi-seq",
				Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_RUNNING, WorkflowRunId: "run-1", SortOrder: 1},
			// The queued remainder, deliberately listed OUT of chain order: chain order is the
			// assertion, not the order the broad read happened to return.
			{Id: "wi-seq-3", Title: "Child three", ParentId: "wi-seq",
				Status:    apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING,
				SortOrder: 3, CreatedAt: timestamppb.New(time.Date(2026, 1, 1, 0, 3, 0, 0, time.UTC))},
			{Id: "wi-seq-2", Title: "Child two", ParentId: "wi-seq",
				Status:    apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING,
				SortOrder: 2, CreatedAt: timestamppb.New(time.Date(2026, 1, 1, 0, 2, 0, 0, time.UTC))},
			// genuinely scheduled: the first membership source.
			{Id: "wi-sched", Title: "Nightly", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_SCHEDULED,
				ScheduledStartAt: timestamppb.New(time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)), SortOrder: 1},
			// decoy: a FINISHED parent with a pending child.
			{Id: "wi-done-parent", Title: "Finished chain", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_SUCCEEDED, SortOrder: 2},
			{Id: "wi-done-child", Title: "Never queued", ParentId: "wi-done-parent",
				Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING, SortOrder: 1},
			// decoy: a loose PENDING item with no parent.
			{Id: "wi-loose-pending", Title: "Loose", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING, SortOrder: 3},
		},
		workflows: []*apiv1.Workflow{{Id: "wf-1", Name: "SDLC"}},
	}
}

// upcomingItems fetches the upcoming lens on the model and returns the rendered rows.
func upcomingItems(t *testing.T, m *Model) []screenkit.Item {
	t.Helper()
	m.sched.scheduleView = schedUpcoming
	m.Base.SelectSource(srcSchedules)
	items, _, err := m.fetchSchedules(context.Background(), "")
	if err != nil {
		t.Fatalf("fetchSchedules(upcoming): %v", err)
	}
	return items
}

func idsOf(items []screenkit.Item) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.ID)
	}
	return out
}

func indexOf(ids []string, want string) int {
	for i, id := range ids {
		if id == want {
			return i
		}
	}
	return -1
}

// 1. THE REPRODUCTION: a running sequence parent's PENDING children show up in upcoming, and the
// decoys do not.
func TestSchedulesUpcomingIncludesQueuedSequenceChildren(t *testing.T) {
	m := newModel(t, queuedPlane())
	items := upcomingItems(t, m)
	ids := idsOf(items)

	for _, want := range []string{"wi-seq-2", "wi-seq-3", "wi-sched"} {
		if !hasID(ids, want) {
			t.Errorf("upcoming = %v, want the queued child %q", ids, want)
		}
	}
	// The parent is the CONTAINER, not a queued child; the armed child has fired; a finished
	// parent's child and a loose pending item are not part of any active chain.
	for _, unwanted := range []string{"wi-seq-1", "wi-done-child", "wi-loose-pending", "wi-seq"} {
		if hasID(ids, unwanted) {
			t.Errorf("upcoming = %v, must not contain %q", ids, unwanted)
		}
	}
	// The row SAYS what it is — wording parity with the GUI's QueuedCard.
	for _, it := range items {
		switch it.ID {
		case "wi-seq-2", "wi-seq-3":
			if it.Meta != "queued — waits for the current step" {
				t.Errorf("queued row %s meta = %q, want the queued wording", it.ID, it.Meta)
			}
		case "wi-sched":
			if !strings.HasPrefix(it.Meta, "scheduled ") {
				t.Errorf("scheduled row meta = %q, want a real start time", it.Meta)
			}
		}
	}
}

// 2. CHAIN ORDER, and the queued section sits ABOVE the agenda (the GUI's QueuedSection).
func TestSchedulesUpcomingOrdersQueuedChildrenByChainOrder(t *testing.T) {
	m := newModel(t, queuedPlane())
	ids := idsOf(upcomingItems(t, m))

	two, three, sched := indexOf(ids, "wi-seq-2"), indexOf(ids, "wi-seq-3"), indexOf(ids, "wi-sched")
	if two < 0 || three < 0 || sched < 0 {
		t.Fatalf("upcoming = %v, want both queued children and the scheduled item", ids)
	}
	if two > three {
		t.Errorf("upcoming = %v — chain order is wi-seq-2 then wi-seq-3 (sort_order 2 then 3), not "+
			"the order the broad read returned", ids)
	}
	if two > sched || three > sched {
		t.Errorf("upcoming = %v — the queued section sits above the agenda", ids)
	}
}

// 3. A queued row is registered with its EMPTY run id, which is what makes both keys behave with
// no new code path: `g` refuses with the existing "has not fired yet" reason, `x` cancels the CHILD.
func TestSchedulesUpcomingQueuedChildGoRefusesAndCancelTargetsTheChild(t *testing.T) {
	p := queuedPlane()
	m := newModel(t, p)
	items := upcomingItems(t, m)
	m.Base.LoadItems(srcSchedules, items, "")
	if !m.Base.SelectItem(srcSchedules, "wi-seq-2") {
		t.Fatal("fixture: could not select the queued row")
	}

	m.goToScheduleRun()
	if !strings.Contains(m.notice, "has not fired") {
		t.Errorf("notice = %q — a queued child has no run to jump to, and must say so", m.notice)
	}
	if m.Base.ActiveSourceName() != srcSchedules {
		t.Error("a refused jump must not move the pane")
	}

	acts := m.scheduleDeleteActions([]string{"wi-seq-2"})
	if len(acts) != 1 {
		t.Fatalf("actions = %d, want the single cancel", len(acts))
	}
	if !strings.Contains(acts[0].Label, "cancel") {
		t.Errorf("label = %q, want a cancel", acts[0].Label)
	}
	if err := acts[0].Do(context.Background()); err != nil {
		t.Fatalf("cancel failed: %v", err)
	}
	if len(p.deleted) != 1 || p.deleted[0] != "wi-seq-2" {
		t.Errorf("DeleteWorkItem calls = %v, want the CHILD (wi-seq-2)", p.deleted)
	}
}

// 4. NO FALSE POSITIVE: a tenant with nothing queued and nothing scheduled still shows the empty
// state, because the fetch returns no rows at all.
func TestSchedulesUpcomingIsEmptyWhenNothingIsQueued(t *testing.T) {
	p := &fakePlane{items: []*apiv1.WorkItem{
		{Id: "wi-done-parent", Title: "Finished chain", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_SUCCEEDED},
		{Id: "wi-done-child", Title: "Never queued", ParentId: "wi-done-parent",
			Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING},
	}}
	m := newModel(t, p)
	if ids := idsOf(upcomingItems(t, m)); len(ids) != 0 {
		t.Errorf("upcoming = %v, want the empty state (no false positive from the derivation)", ids)
	}
}

// 5. DEDUPE: union by id, never concatenation — a row must appear exactly once.
func TestSchedulesUpcomingHasNoDuplicateRows(t *testing.T) {
	m := newModel(t, queuedPlane())
	ids := idsOf(upcomingItems(t, m))
	seen := map[string]int{}
	for _, id := range ids {
		seen[id]++
		if seen[id] > 1 {
			t.Errorf("upcoming = %v — %q appears %d times", ids, id, seen[id])
		}
	}
}

// 6. THE QUEUE IS DERIVED ON PAGE ONE ONLY: a later page would otherwise repeat it on every page.
func TestSchedulesUpcomingSecondPageDoesNotRepeatTheQueue(t *testing.T) {
	m := newModel(t, queuedPlane())
	m.sched.scheduleView = schedUpcoming
	items, _, err := m.fetchUpcomingSchedules(context.Background(), "tok")
	if err != nil {
		t.Fatalf("fetchUpcomingSchedules(page 2): %v", err)
	}
	for _, id := range idsOf(items) {
		if strings.HasPrefix(id, "wi-seq") {
			t.Errorf("page 2 returned %q — the client-derived queue must not repeat per page", id)
		}
	}
}

// 7. THE CAP NOTE NAMES THE LENS it truncated, now that both client-derived lenses share the cap.
func TestSchedulesCapNoteNamesTheLens(t *testing.T) {
	p := &fakePlane{}
	for i := 0; i < schedListCap; i++ {
		p.items = append(p.items, &apiv1.WorkItem{
			Id: fmt.Sprintf("wi-%d", i), Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING,
		})
	}
	m := newModel(t, p)

	assertCap := func(view schedView) {
		t.Helper()
		m.sched.scheduleView = view
		for _, it := range upcomingItemsFor(t, m, view) {
			if it.ID == "sched:cap" {
				if !strings.Contains(it.Title, view.label()) {
					t.Errorf("%s: cap note = %q, want it to name the lens", view, it.Title)
				}
				return
			}
		}
		t.Errorf("%s: no cap note at schedListCap items", view)
	}
	assertCap(schedUpcoming)
	assertCap(schedRunning)
}

// upcomingItemsFor fetches whichever lens the caller names and returns its rows.
func upcomingItemsFor(t *testing.T, m *Model, view schedView) []screenkit.Item {
	t.Helper()
	m.sched.scheduleView = view
	items, _, err := m.fetchSchedules(context.Background(), "")
	if err != nil {
		t.Fatalf("fetchSchedules(%s): %v", view, err)
	}
	return items
}

// 8. THE PARITY AC: the SAME fixture file the GUI's vitest reads
// (frontend/src/lib/schedules-fixture.test.ts) is run through THIS pane's predicate, so the two
// clients cannot drift on membership or on chain order.
func TestUpcomingMembershipMatchesTheGuiModel(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..",
		"frontend", "src", "lib", "schedules-fixture.json"))
	if err != nil {
		t.Fatalf("read the shared fixture: %v", err)
	}
	var fixture struct {
		Items        []json.RawMessage `json:"items"`
		WantQueued   []string          `json:"wantQueued"`
		WantUpcoming []string          `json:"wantUpcoming"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("parse the shared fixture: %v", err)
	}

	items := make([]*apiv1.WorkItem, 0, len(fixture.Items))
	for i, rawItem := range fixture.Items {
		wi := &apiv1.WorkItem{}
		if err := protojson.Unmarshal(rawItem, wi); err != nil {
			t.Fatalf("fixture item %d: %v", i, err)
		}
		items = append(items, wi)
	}

	// (a) the PREDICATE, in chain order — the same assertion the GUI's vitest makes.
	queued := queuedSequenceChildren(items)
	if got := itemIDs(queued); !equalStrings(got, fixture.WantQueued) {
		t.Errorf("queuedSequenceChildren = %v, want %v (the fixture both clients are judged against)",
			got, fixture.WantQueued)
	}

	// (b) the PANE's full upcoming set: queued ∪ SCHEDULED, deduped, queued first.
	m := newModel(t, &fakePlane{items: items})
	if got := idsOf(upcomingItems(t, m)); !equalStrings(got, fixture.WantUpcoming) {
		t.Errorf("the TUI's upcoming set = %v, want %v (the GUI model's union)", got, fixture.WantUpcoming)
	}
}

func itemIDs(items []*apiv1.WorkItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.GetId())
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
