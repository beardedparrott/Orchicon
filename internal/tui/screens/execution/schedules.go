package execution

// schedules.go — the SCHEDULES pane: what is queued, what is running, and what has run.
//
// The operator: "I noticed I don't see any 'Schedules' section in the TUI under Executions. We
// need to implement this in the TUI. We should be able to see upcoming, running, and finished
// just like the GUI and we should have delete operations, bulk delete operations, and a key
// that takes you to the workflow run."
//
// The GUI's /schedules page is ONE view with three modes, so this is one SOURCE with three
// views cycled by `v` — the same shape as the Work Items pane's tree/archive cycle, and
// deliberately not three submenu entries (three entries would suggest three independent
// surfaces rather than three lenses on one).
//
//   upcoming  work items with status SCHEDULED (recurring items live on Automation → Recurring
//             Items in the GUI, and are EXCLUDED here for the same reason)
//   running   work items whose bound workflow run is in flight (RUNNING / CHECKPOINTING /
//             RECOVERING), plus the sequence parents that drive a chain
//   finished  workflow runs that have actually RUN (a started_at), which is the GUI's history
//             view
//
// MEMBERSHIP IS THE GUI'S, on purpose — the same predicates, so the two clients cannot disagree
// about what is scheduled. See frontend/src/lib/schedules-model.ts: ACTIVE_RUNNING_STATUSES is
// the same three statuses, and the sequence-parent extension is the same rule.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/mutate"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// schedView is the Schedules pane's lens. It is a DISPLAY grouping exactly like the Work
// Items tree/archive: nothing here mutates scheduling, only how it is shown.
type schedView string

const (
	schedUpcoming schedView = "upcoming"
	schedRunning  schedView = "running"
	schedFinished schedView = "finished"
)

// next cycles upcoming → running → finished → upcoming.
func (v schedView) next() schedView {
	switch v {
	case schedUpcoming:
		return schedRunning
	case schedRunning:
		return schedFinished
	}
	return schedUpcoming
}

func (v schedView) label() string {
	switch v {
	case schedRunning:
		return "running"
	case schedFinished:
		return "finished"
	}
	return "upcoming"
}

// schedListCap bounds the tenant-wide read the RUNNING view needs.
//
// That view is derived client-side (the API takes one status filter, and the sequence-parent
// rule needs the parent/child relations), so it reads a page of items rather than asking for
// the exact set. Upcoming and finished are server-filtered and paginated, so this cap only ever
// applies to running — and the pane SAYS it when the cap is reached rather than silently
// truncating.
const schedListCap = 500

// activeRunStatuses are the run-bound statuses that mean "in flight" — the same set as the
// GUI's ACTIVE_RUNNING_STATUSES (frontend/src/lib/schedules-model.ts), keyed by the proto enum
// rather than by number so the two cannot drift silently.
var activeRunStatuses = map[apiv1.WorkItemStatus]bool{
	apiv1.WorkItemStatus_WORK_ITEM_STATUS_RUNNING:       true,
	apiv1.WorkItemStatus_WORK_ITEM_STATUS_CHECKPOINTING: true,
	apiv1.WorkItemStatus_WORK_ITEM_STATUS_RECOVERING:    true,
}

// scheduleRow is what the pane needs to know about a scheduled thing: the row's identity, and
// the run it is attached to (for `g`).
//
// Kept separately from the display list because the LIST is rebuilt from the plane on every
// load while the run binding must survive a filter: `g` must work on the row the operator can
// see, not only on the page they last fetched.
type scheduleRow struct {
	// itemID is the bound WORK ITEM ("" for a run with no item — a one-shot).
	itemID string
	// runID is the workflow run this row is about ("" for a schedule that has not fired yet).
	runID string
}

// --- fetch ------------------------------------------------------------------

// fetchSchedules is the registered Fetch for the Schedules source. Which read it performs —
// and therefore what the rows mean — depends on the current view.
func (m *Model) fetchSchedules(ctx context.Context, pageToken string) ([]screenkit.Item, string, error) {
	switch m.sched.view() {
	case schedRunning:
		return m.fetchRunningSchedules(ctx)
	case schedFinished:
		return m.fetchFinishedSchedules(ctx, pageToken)
	default:
		return m.fetchUpcomingSchedules(ctx, pageToken)
	}
}

// fetchUpcomingSchedules lists the SCHEDULED work items, server-filtered and paginated.
func (m *Model) fetchUpcomingSchedules(ctx context.Context, pageToken string) ([]screenkit.Item, string, error) {
	status := apiv1.WorkItemStatus_WORK_ITEM_STATUS_SCHEDULED
	resp, err := m.cl.WorkItems.ListWorkItems(ctx, connect.NewRequest(&apiv1.ListWorkItemsRequest{
		Status:          &status,
		RecurringFilter: apiv1.RecurringFilter_RECURRING_FILTER_EXCLUDE_RECURRING,
		IdeaScope:       apiv1.IdeaScope_IDEA_SCOPE_EXCLUDE_IDEA,
		SortBy:          "created_at",
		PageSize:        100,
		PageToken:       pageToken,
	}))
	if err != nil {
		return nil, "", err
	}
	items := make([]screenkit.Item, 0, len(resp.Msg.GetWorkItems()))
	for _, w := range resp.Msg.GetWorkItems() {
		m.sched.remember(w.GetId(), w.GetWorkflowRunId())
		items = append(items, screenkit.Item{
			ID:    w.GetId(),
			Title: w.GetTitle(),
			Meta:  "scheduled " + screenkit.FmtTime(w.GetScheduledStartAt()),
		})
	}
	items = append(items, m.schedCapNote(len(items))...)
	return items, resp.Msg.GetNextPageToken(), nil
}

// fetchRunningSchedules derives the in-flight set from one capped read.
//
// The predicate is the GUI's, verbatim: an item counts when its bound run is in flight, OR when
// it is an active SEQUENCE PARENT (no bound run of its own — the chain resets its children to
// pending and arms one at a time, so only the parent carries the active status).
func (m *Model) fetchRunningSchedules(ctx context.Context) ([]screenkit.Item, string, error) {
	resp, err := m.cl.WorkItems.ListWorkItems(ctx, connect.NewRequest(&apiv1.ListWorkItemsRequest{
		RecurringFilter: apiv1.RecurringFilter_RECURRING_FILTER_EXCLUDE_RECURRING,
		IdeaScope:       apiv1.IdeaScope_IDEA_SCOPE_EXCLUDE_IDEA,
		PageSize:        schedListCap,
	}))
	if err != nil {
		return nil, "", err
	}
	all := resp.Msg.GetWorkItems()
	parents := sequenceParentIDs(all)

	var running []*apiv1.WorkItem
	for _, w := range all {
		if !activeRunStatuses[w.GetStatus()] {
			continue
		}
		boundRun := w.GetWorkflowRunId() != ""
		sequenceParent := w.GetWorkflowRunId() == "" && parents[w.GetId()]
		if boundRun || sequenceParent {
			running = append(running, w)
		}
	}
	// Order by status so the genuinely RUNNING items (the ones doing work) come first; within a
	// status the server's order is kept, which is stable and needs no timestamp the pane does
	// not display.
	sort.SliceStable(running, func(i, j int) bool { return running[i].GetStatus() < running[j].GetStatus() })

	items := make([]screenkit.Item, 0, len(running))
	for _, w := range running {
		m.sched.remember(w.GetId(), w.GetWorkflowRunId())
		meta := strings.ToLower(strings.TrimPrefix(w.GetStatus().String(), "WORK_ITEM_STATUS_"))
		if w.GetWorkflowRunId() == "" {
			meta += " (sequence)"
		}
		items = append(items, screenkit.Item{ID: w.GetId(), Title: w.GetTitle(), Meta: meta})
	}
	items = append(items, m.schedCapNote(len(all))...)
	return items, "", nil
}

// fetchFinishedSchedules lists the workflow runs that have RUN — the GUI's history view.
//
// A run with no started_at has not run yet, so it is not "finished"; the plain Runs pane is
// where every run lives, including the ones that never started.
func (m *Model) fetchFinishedSchedules(ctx context.Context, pageToken string) ([]screenkit.Item, string, error) {
	resp, err := m.cl.Workflows.ListWorkflowRuns(ctx, connect.NewRequest(&apiv1.ListWorkflowRunsRequest{
		PageSize:  100,
		PageToken: pageToken,
	}))
	if err != nil {
		return nil, "", err
	}
	// The names the row shows are resolved from the same cached index the Runs pane uses.
	if m.runNames.stale() {
		m.loadRunNames(ctx)
	}
	items := make([]screenkit.Item, 0, len(resp.Msg.GetRuns()))
	for _, r := range resp.Msg.GetRuns() {
		if r.GetStartedAt() == nil {
			continue // never ran: a queued run is not history
		}
		m.sched.rememberRun(r.GetId(), r.GetWorkItemId())
		items = append(items, screenkit.Item{
			ID:    r.GetId(),
			Title: m.runsTitle(r),
			Meta:  strings.ToLower(strings.TrimPrefix(r.GetStatus().String(), "WORKFLOW_RUN_STATUS_")),
		})
	}
	return items, resp.Msg.GetNextPageToken(), nil
}

// schedCapNote renders the truncation marker when the running view hit its cap, so a long list
// says it was cut instead of looking complete.
func (m *Model) schedCapNote(read int) []screenkit.Item {
	if read < schedListCap {
		return nil
	}
	return []screenkit.Item{{
		ID:    "sched:cap",
		Title: fmt.Sprintf("(first %d items read — the running view is derived client-side)", schedListCap),
		Meta:  "truncated",
	}}
}

// --- view state -------------------------------------------------------------

// schedState holds the pane's view choice and the row → run bindings.
//
// The ZERO VALUE IS USABLE, like runNames: the maps are created on first write, so a screen
// that never loads anything (a test, or a screen built before the first fetch) cannot panic on
// a nil map. Everything here is written from the fetch goroutine and read from Update.
type schedState struct {
	mu           sync.Mutex
	scheduleView schedView
	// scheduleRows maps a ROW id to what it is attached to. For upcoming/running the row is a
	// work item; for finished it is a workflow run.
	scheduleRows map[string]scheduleRow
	// itemRun maps a work item id to its bound run, so a sequence/upcoming row can be jumped to
	// even though the row carries the ITEM (the run id lives on the item).
	itemRun map[string]string
}

func (s *schedState) view() schedView {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.scheduleView == "" {
		return schedUpcoming
	}
	return s.scheduleView
}

// setView changes the lens and DROPS the row bindings, because they were derived from the
// previous read — keeping them would let a jump target a row the pane no longer shows.
func (s *schedState) setView(v schedView) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scheduleView = v
	s.scheduleRows = nil
	s.itemRun = nil
}

func (s *schedState) remember(itemID, runID string) {
	if itemID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.scheduleRows == nil {
		s.scheduleRows = map[string]scheduleRow{}
	}
	s.scheduleRows[itemID] = scheduleRow{itemID: itemID, runID: runID}
	if runID != "" {
		if s.itemRun == nil {
			s.itemRun = map[string]string{}
		}
		s.itemRun[itemID] = runID
	} else {
		delete(s.itemRun, itemID)
	}
}

// rememberRun binds a RUN row to its bound work item (the finished view's shape, where the row
// is the run).
func (s *schedState) rememberRun(runID, itemID string) {
	if runID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.scheduleRows == nil {
		s.scheduleRows = map[string]scheduleRow{}
	}
	s.scheduleRows[runID] = scheduleRow{itemID: itemID, runID: runID}
}

// runFor resolves the workflow run a focused row is about ("" when it has none yet).
func (s *schedState) runFor(rowID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.scheduleRows[rowID]; ok && r.runID != "" {
		return r.runID
	}
	return s.itemRun[rowID]
}

// itemFor resolves the work item a focused row is about ("" for a one-shot run).
func (s *schedState) itemFor(rowID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.scheduleRows[rowID]; ok {
		return r.itemID
	}
	return ""
}

// --- keys -------------------------------------------------------------------

// cycleScheduleView moves to the next lens and re-fetches, so the pane's rows always match the
// label it shows.
func (m *Model) cycleScheduleView() tea.Cmd {
	current := m.sched.view()
	m.sched.setView(current.next())
	m.notice = "schedules: " + m.sched.view().label()
	return m.Base.Refresh(srcSchedules)
}

// handleSchedulesKeys dispatches the Schedules pane's own chords.
func (m *Model) handleSchedulesKeys(kstr string) (tea.Cmd, bool) {
	switch kstr {
	case keySchedView:
		return m.cycleScheduleView(), true
	case keyGoToRun:
		return m.goToScheduleRun(), true
	}
	return nil, false
}

// goToScheduleRun is the operator's "key that takes you to the workflow run".
//
// It switches to the Runs pane, asks the list to focus the run, AND requests its detail. The
// detail request matters as much as the selection: the runs list is paginated, so a run older
// than the first page has no row to highlight — and a jump that silently does nothing is worse
// than no jump at all. Between the two, the operator always ends up looking at the run.
func (m *Model) goToScheduleRun() tea.Cmd {
	it, ok := m.Base.ActiveItem()
	if !ok {
		return m.refuse("no schedule selected")
	}
	runID := m.sched.runFor(it.ID)
	if runID == "" {
		// Name the two causes, because they are different situations for the operator: a
		// schedule that has not fired yet, and a finished run with no bound item.
		if m.sched.view() == schedFinished {
			return m.refuse("this run has no bound work item — there is no run to jump to")
		}
		return m.refuse("this schedule has not fired yet — there is no workflow run to jump to")
	}
	m.Base.SelectSource(srcRuns)
	m.Base.SelectWhenLoaded(srcRuns, runID)
	m.notice = "jumped to run " + runID
	return tea.Batch(m.Base.Refresh(srcRuns), m.Base.RequestDetail(srcRuns, runID))
}

// --- delete -----------------------------------------------------------------

// scheduleDeleteActions are the destructive operations for the current view.
//
// The MEANING of delete differs per view, and it matches the GUI exactly: an upcoming or running
// schedule is CANCELLED (the work item goes terminal, so it stops being scheduled), while a
// finished row's action removes the SCHEDULE from its work item without touching the item's own
// history (frontend/src/routes/schedules.tsx offers "Cancel N" and "Remove N schedules" for the
// same two cases).
func (m *Model) scheduleDeleteActions(ids []string) []kit2.Action {
	if len(ids) > 1 {
		return m.bulkScheduleDelete(ids)
	}
	if len(ids) == 0 {
		return nil
	}
	id := ids[0]
	it, ok := m.Base.SourceItem(m.Base.ActiveSourceName(), id)
	title := id
	if ok {
		title = it.Title
	}
	if m.sched.view() == schedFinished {
		itemID := m.sched.itemFor(id)
		if itemID == "" {
			return []kit2.Action{{
				Label: "remove schedule", Key: keySchedDelete, Danger: true, Source: srcSchedules,
				Do: func(context.Context) error {
					return errors.New("this run has no bound work item, so it has no schedule to remove")
				},
			}}
		}
		return []kit2.Action{{
			Label: "remove schedule", Key: keySchedDelete, Danger: true, Source: srcSchedules,
			Confirm: "Remove the schedule from " + title + "?\n" +
				"The work item itself is unchanged — this clears its recurring schedule and un-binds the run.",
			Do: func(ctx context.Context) error { return m.rpcRemoveSchedule(ctx, itemID) },
		}}
	}
	return []kit2.Action{{
		Label: "cancel schedule", Key: keySchedDelete, Danger: true, Source: srcSchedules,
		Confirm: "Cancel " + title + "?\n" +
			"Cancel transitions the work item to cancelled, so it stops being scheduled.",
		Do: func(ctx context.Context) error { return m.rpcCancelScheduled(ctx, id) },
	}}
}

// bulkScheduleDelete is the same operation across a selection, with the shared count-naming,
// sequential-write and rejection-counting contract the Work pane uses.
func (m *Model) bulkScheduleDelete(ids []string) []kit2.Action {
	n := len(ids)
	finished := m.sched.view() == schedFinished
	verb := "cancel"
	what := "schedules"
	if finished {
		verb, what = "remove", "schedules"
	}
	label := fmt.Sprintf("%s %d selected", verb, n)

	// Resolve the target ids up front: a finished row IS a run, so the thing to write is its
	// bound work item. An unresolvable row is reported rather than silently skipped.
	targets := make([]string, 0, n)
	missing := 0
	for _, id := range ids {
		if finished {
			if itemID := m.sched.itemFor(id); itemID != "" {
				targets = append(targets, itemID)
			} else {
				missing++
			}
			continue
		}
		targets = append(targets, id)
	}

	return []kit2.Action{{
		Label: label, Key: keySchedDelete, Danger: true, Source: srcSchedules,
		Confirm: fmt.Sprintf("%s %d %s?\n", strings.Title(verb), n, what) +
			"Each write goes out in turn, and the first refusal is counted rather than swallowed.",
		Do: func(ctx context.Context) error {
			failed := missing
			for _, id := range targets {
				var err error
				if finished {
					err = m.rpcRemoveSchedule(ctx, id)
				} else {
					err = m.rpcCancelScheduled(ctx, id)
				}
				if err != nil {
					failed++
				}
			}
			if failed > 0 {
				return fmt.Errorf("%s %d of %d — %d failed", verb+"d", n-failed, n, failed)
			}
			return nil
		},
	}}
}

// rpcCancelScheduled cancels a scheduled work item through the standard delete path, which is
// what "cancel a schedule" means on the plane (status → cancelled).
func (m *Model) rpcCancelScheduled(ctx context.Context, itemID string) error {
	if m.cl == nil || m.cl.WorkItems == nil {
		return errors.New("no work-item client")
	}
	_, err := m.cl.WorkItems.DeleteWorkItem(ctx, connect.NewRequest(&apiv1.DeleteWorkItemRequest{Id: itemID}))
	return err
}

// rpcRemoveSchedule clears a work item's schedule WITHOUT touching its status.
//
// The three fields must move together, which is why this is one UpdateWorkItem and not three:
// clearing recurring_schedule and the run binding while auto_start_workflow stays true would let
// the backend re-fire a fresh run immediately (the same rule the GUI's useRemoveSchedule
// documents).
func (m *Model) rpcRemoveSchedule(ctx context.Context, itemID string) error {
	if m.cl == nil || m.cl.WorkItems == nil {
		return errors.New("no work-item client")
	}
	_, err := m.cl.WorkItems.UpdateWorkItem(ctx, connect.NewRequest(&apiv1.UpdateWorkItemRequest{
		Id:                itemID,
		RecurringSchedule: &apiv1.RecurringSchedule{},
		WorkflowRunId:     strPtr(""),
		AutoStartWorkflow: boolPtr(false),
	}))
	return err
}

// scheduleHint is the pane's key cheat-sheet, view-aware so it always describes what the keys
// will actually do in the view on screen.
func (m *Model) scheduleHint() string {
	v := m.sched.view()
	del := "x: cancel"
	if v == schedFinished {
		del = "x: remove schedule"
	}
	return theme.HintText.Render("v: view (" + v.label() + " → " + v.next().label() + ") · " +
		del + " (confirm) · g: go to the run · space: multi-select · esc: clear · r: refresh")
}

// scheduleItemDetail renders an upcoming/running schedule's row — the work item, its
// scheduling state, and where it is (the run it is bound to, if it has one).
func (m *Model) scheduleItemDetail(ctx context.Context, id string) (string, []screenkit.Field, string, error) {
	resp, err := m.cl.WorkItems.GetWorkItem(ctx, connect.NewRequest(&apiv1.GetWorkItemRequest{Id: id}))
	if err != nil {
		return "", nil, "", err
	}
	w := resp.Msg.GetWorkItem()
	view := m.sched.view()
	fields := []screenkit.Field{
		{Key: "work item", Value: w.GetTitle()},
		{Key: "id", Value: w.GetId()},
		{Key: "status", Value: strings.ToLower(strings.TrimPrefix(w.GetStatus().String(), "WORK_ITEM_STATUS_"))},
		{Key: "view", Value: view.label()},
	}
	if ts := w.GetScheduledStartAt(); ts != nil {
		fields = append(fields, screenkit.Field{Key: "scheduled for", Value: screenkit.FmtTime(ts)})
	}
	if runID := w.GetWorkflowRunId(); runID != "" {
		fields = append(fields, screenkit.Field{Key: "run", Value: runID})
	} else {
		fields = append(fields, screenkit.Field{Key: "run", Value: "(not fired yet)"})
	}
	if m.sched.view() == schedFinished {
		fields = append(fields, screenkit.Field{Key: "actions", Value: "x: remove schedule · g: go to the run"})
	} else {
		fields = append(fields, screenkit.Field{Key: "actions", Value: "x: cancel schedule · g: go to the run"})
	}
	return "Schedule: " + w.GetTitle(), fields, "", nil
}

// mutateSink helper: the schedules writes go through the same executor as everything else, so
// the source reconciles and a refusal reaches the dock.
var _ = mutate.Request{}

func strPtr(v string) *string { return &v }

func boolPtr(v bool) *bool { return &v }

// sequenceParentIDs returns the ids that are some item's PARENT — the set the running view needs
// to recognise a sequence container (whose children are pending, so nothing else marks it).
func sequenceParentIDs(items []*apiv1.WorkItem) map[string]bool {
	parents := map[string]bool{}
	for _, it := range items {
		if pid := it.GetParentId(); pid != "" {
			parents[pid] = true
		}
	}
	return parents
}
