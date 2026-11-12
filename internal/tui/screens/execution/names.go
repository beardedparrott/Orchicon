package execution

// names.go — resolving the IDS a workflow run carries into the NAMES an operator reads.

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
	// nameIndexTTL bounds how long a resolved name is trusted.
	nameIndexTTL = 30 * time.Second
	// nameIndexItemPage is how many work items the index reads.
	nameIndexItemPage = 500
	// nameIndexWorkflowPage is the same idea for workflows. Workflows are few (tens), so this
	// covers every real tenant with room to spare.
	nameIndexWorkflowPage = 500
)

// runNames resolves run ids to names, caching the result for nameIndexTTL.
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

// runsWorkflowField renders the run's workflow as "name (id)" so the row is readable AND still
// traceable to the id the plane uses.
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
