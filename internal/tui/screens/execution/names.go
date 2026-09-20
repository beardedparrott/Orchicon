package execution

// names.go — resolving the IDS a workflow run carries into the NAMES an operator reads.
//
// The operator: "Workflow Runs just show IDs right now. We should also show the name of the
// workflow and the name of the work item associated with it in the view."
//
// A WorkflowRun carries workflow_id and work_item_id and NO names (the proto has no name
// fields), so the names have to be resolved client-side — exactly what the GUI's schedules
// history view does (it builds itemsById from the work item list and workflowsById from the
// workflow list). This is the TUI equivalent: one cached index, shared by the runs list and
// the run detail.

import (
	"context"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
)

const (
	// nameIndexTTL bounds how long a resolved name is trusted. Names change rarely (a
	// workflow rename, a retitled work item), and the alternative — resolving on every runs
	// page — would add two RPCs to every fetch. The GUI's history view refetches its name
	// lists on the same 30s cadence, so the two clients stay in the same order of freshness.
	nameIndexTTL = 30 * time.Second
	// nameIndexItemPage is how many work items the index reads. It is bounded on purpose:
	// the list is a tenant-wide read and the PANE only needs the items its runs reference.
	// A run whose item falls outside the page still renders — it falls back to the id — so
	// this is a display limit, never a correctness one.
	nameIndexItemPage = 500
	// nameIndexWorkflowPage is the same idea for workflows. Workflows are few (tens), so this
	// covers every real tenant with room to spare.
	nameIndexWorkflowPage = 500
)

// runNames resolves run ids to names, caching the result for nameIndexTTL.
//
// The zero value is usable and always safe: before the first load, and in a screen with no
// client (tests), every lookup misses and callers fall back to the raw id — which is what the
// pane showed before this existed, so nothing can get WORSE than it was.
type runNames struct {
	mu        sync.Mutex
	workflows map[string]string
	items     map[string]string
	loadedAt  time.Time
}

// workflowName returns the workflow's name, or "" when it is not known.
func (n *runNames) workflowName(id string) string {
	if id == "" {
		return ""
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.workflows[id]
}

// itemTitle returns the work item's title, or "" when it is not known.
func (n *runNames) itemTitle(id string) string {
	if id == "" {
		return ""
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.items[id]
}

// stale reports whether the index needs (re)loading.
func (n *runNames) stale() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.workflows) == 0 && len(n.items) == 0 || time.Since(n.loadedAt) > nameIndexTTL
}

// ensure loads the name index if it is stale, for callers that need names before rendering.
func (n *runNames) ensure(ctx context.Context, m *Model) {
	if n.stale() {
		m.loadRunNames(ctx)
	}
}

// load fills the index, best effort.
//
// An error is NOT propagated to the caller's fetch: a worker-independent name lookup must
// never turn "list the runs" into an error, and every lookup already has an id fallback. A
// failed half leaves the other half's previous values alone.
func (m *Model) loadRunNames(ctx context.Context) {
	if m.cl == nil || m.cl.Workflows == nil || m.cl.WorkItems == nil {
		return
	}
	workflows := map[string]string{}
	if resp, err := m.cl.Workflows.ListWorkflows(ctx, connect.NewRequest(&apiv1.ListWorkflowsRequest{
		PageSize: nameIndexWorkflowPage,
	})); err == nil {
		for _, w := range resp.Msg.GetWorkflows() {
			if w.GetId() != "" {
				workflows[w.GetId()] = w.GetName()
			}
		}
	} else {
		workflows = nil
	}
	items := map[string]string{}
	if resp, err := m.cl.WorkItems.ListWorkItems(ctx, connect.NewRequest(&apiv1.ListWorkItemsRequest{
		PageSize: nameIndexItemPage,
	})); err == nil {
		for _, it := range resp.Msg.GetWorkItems() {
			if it.GetId() != "" {
				items[it.GetId()] = it.GetTitle()
			}
		}
	} else {
		items = nil
	}

	m.runNames.mu.Lock()
	defer m.runNames.mu.Unlock()
	if workflows != nil {
		m.runNames.workflows = workflows
	}
	if items != nil {
		m.runNames.items = items
	}
	m.runNames.loadedAt = time.Now()
}

// runsWorkflowField renders the run's workflow as "name (id)" so the row is readable AND
// still traceable to the id the plane uses. Falls back to the bare id when the name is
// unknown.
func (m *Model) runsWorkflowField(r *apiv1.WorkflowRun) string {
	id := r.GetWorkflowId()
	if id == "" {
		return ""
	}
	if name := m.runNames.workflowName(id); name != "" {
		return name + "  (" + id + ")"
	}
	return id
}

// runsWorkItemField is the same for the bound work item. A run with no bound item says so —
// it is a ONE-SHOT, and an empty field reads as missing data rather than as a shape.
func (m *Model) runsWorkItemField(r *apiv1.WorkflowRun) string {
	id := r.GetWorkItemId()
	if id == "" {
		return "(none — one-shot run)"
	}
	if title := m.runNames.itemTitle(id); title != "" {
		return title + "  (" + id + ")"
	}
	return id
}

// runsTitle renders a run's LIST title: the workflow's name and the work item's title.
//
// The separator is a spaced slash rather than the arrow it used to be. The operator, on this list:
// "Executions and workflow runs are still way too crowded. It's too noisy... We should clean them up
// more." The arrow is a heavy glyph that reads as a diagram rather than as a separator, and it arrived
// with the SAME double-space padding on both sides — three cells of a scannable row spent on
// decoration. A slash is one cell and reads as "this, in that context".
//
// The work item's title is BOUNDED for the same reason it is on an execution row: a sentence per row
// is what makes the list unscannable, and an unbounded title also eats the status, which is the field
// the operator is actually scanning for. The workflow name is left whole — it is short by convention
// and it is the leftmost column, so bounding it would make two different workflows look alike.
func (m *Model) runsTitle(r *apiv1.WorkflowRun) string {
	wfName := m.runNames.workflowName(r.GetWorkflowId())
	if wfName == "" {
		wfName = r.GetWorkflowId()
	}
	if r.GetWorkItemId() == "" {
		return wfName + " (one-shot)"
	}
	title := m.runNames.itemTitle(r.GetWorkItemId())
	if title == "" {
		title = r.GetWorkItemId()
	}
	return wfName + " / " + screenkit.TruncateRunes(strings.TrimSpace(title), runItemTitleMax)
}

// runItemTitleMax is how much of a work item's title a run row keeps, so the row stays a scan list
// rather than a paragraph. See runsTitle.
const runItemTitleMax = 15
