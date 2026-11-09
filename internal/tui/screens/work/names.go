package work

// names.go — resolving the IDENTIFIERS a work item's DETAIL pane carries into the NAMES an operator
// reads.

import (
	"context"
	"sync"
	"time"

	"connectrpc.com/connect"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
)

const (
	// nameIndexWorkflowTTL bounds how long a resolved workflow name is trusted.
	nameIndexWorkflowTTL = 30 * time.Second
	// nameIndexWorkflowPage is how many workflows the index reads.
	nameIndexWorkflowPage = 500
)

// shortRunIDWidth is how many runes of a workflow-run id are kept.
const shortRunIDWidth = 13

type nameIndex struct {
	mu        sync.Mutex
	workflows map[string]string // workflow id   → name  (the workflow list + the form prep)
	projects  map[string]string // project id    → name  (the Projects page + the form prep)
	items     map[string]string // work item id  → title (the loaded work-item page)
	// workflowsAt is when the workflow map was last (re)read. The zero value is "never loaded",
	// so the first source fetch loads it and a screen with no client simply stays on raw ids.
	workflowsAt time.Time
}

// workflowName returns the workflow's name, or "" when it is not known.
func (n *nameIndex) workflowName(id string) string {
	if id == "" {
		return ""
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.workflows[id]
}

// projectName returns the project's name, or "" when it is not known.
func (n *nameIndex) projectName(id string) string {
	if id == "" {
		return ""
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.projects[id]
}

// itemTitle returns the work item's title, or "" when it is not known. This is how a detail pane's
// `parent` is rendered without a GetWorkItem per row: the parent is usually ON the loaded page.
func (n *nameIndex) itemTitle(id string) string {
	if id == "" {
		return ""
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.items[id]
}

// setWorkflows swaps the workflow index.
func (n *nameIndex) setWorkflows(m map[string]string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.workflowsAt = time.Now()
	if len(m) == 0 {
		return
	}
	n.workflows = m
}

// workflowNamesStale reports whether the workflow map needs (re)reading.
func (n *nameIndex) workflowNamesStale() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return time.Since(n.workflowsAt) > nameIndexWorkflowTTL
}

// loadWorkflowNames reads the workflow list into the name index, at most once per TTL, best effort.
func (m *Model) loadWorkflowNames(ctx context.Context) {
	if !m.names.workflowNamesStale() {
		return
	}
	if m.cl == nil || m.cl.Workflows == nil {
		return
	}
	resp, err := m.cl.Workflows.ListWorkflows(ctx, connect.NewRequest(&apiv1.ListWorkflowsRequest{
		PageSize: nameIndexWorkflowPage,
	}))
	if err != nil {
		return
	}
	opts := make([]workflowOpt, 0, len(resp.Msg.GetWorkflows()))
	for _, w := range resp.Msg.GetWorkflows() {
		opts = append(opts, workflowOpt{ID: w.GetId(), Name: w.GetName()})
	}
	m.names.setWorkflows(workflowNameIndex(opts))
}

// setProjects swaps the project index, under the same empty-map rule as setWorkflows.
func (n *nameIndex) setProjects(m map[string]string) {
	if len(m) == 0 {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.projects = m
}

// setItems swaps the work-item index with the page the caller just loaded.
func (n *nameIndex) setItems(m map[string]string) {
	if m == nil {
		m = map[string]string{}
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.items = m
}

// named renders an identifier as "name (id)" — the name the operator reads FIRST, with the id it
// came from kept beside it because that is the form the operator quotes in a ticket or a support.
func named(name, id string) string {
	if id == "" {
		return ""
	}
	if name == "" {
		return id
	}
	return name + "  (" + id + ")"
}

// shortRunID renders a workflow-run id as something an operator can read.
func shortRunID(id string) string {
	if id == "" {
		return ""
	}
	return screenkit.TruncateRunes(id, shortRunIDWidth)
}
