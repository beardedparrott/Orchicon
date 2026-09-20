package work

// names.go — resolving the IDENTIFIERS a work item's DETAIL pane carries into the NAMES an
// operator reads.
//
// The operator, on this screen: "in the TUI under work items, it is showing all guids in the
// display instead of the actual names. That won't mean anything to anyone. We need to resolve the
// real names in the cases where they have them."
//
// The LIST rows were already correct (they render titles); the defect was confined to the DETAIL
// pane, which built its fields straight from the proto ids. The rendering convention and the SHAPE
// of the index are copied from execution/names.go — mutex-guarded maps, accessors that return ""
// when unknown, `name + "  (" + id + ")"`, and the raw id as the fallback. That helper is NOT
// reused, deliberately: it fills its maps from TWO RPCs, while every index this screen needs is
// ALREADY in memory (see the setNameIndex callers in screen.go), so reusing it would add two
// fetches to a screen that needs none — the opposite of the no-per-row-RPC rule this exists to
// satisfy.
//
// NO TTL on the project and work-item maps. A TTL bounds how stale a CACHED RPC may be; those two
// maps are rewritten by the very fetch that rewrites the pane they feed (the Projects page, the
// work-item page), so they can never be staler than the rows drawn beside them.
//
// The WORKFLOW map is the exception, and it is why this file also owns ONE fetch. Nothing else on
// this screen lists workflows — the only caller was the form prep (prepCreateItem / prepEditItem),
// so a screen whose operator had not opened a form yet (the common case: the detail pane is drawn
// the moment the list lands) still printed the raw workflow id. The GUI resolves it from the
// workflow list its page loads on entry, so the TUI now loads the same list itself, TTL-cached and
// piggybacked on a source fetch this screen already makes (see loadWorkflowNames), in the shape of
// execution/names.go. That is the ONE cached fetch the no-per-row-RPC rule allows — never a
// GetWorkflow per rendered row.
//
// The mutex is a CORRECTNESS requirement, not decoration: kit2's DetailFn runs inside a tea.Cmd
// closure (kit2/base.go:1529, :1546), i.e. OFF the update loop, while the indices are written ON
// it. That is the same hazard m.viewMu exists for (screen.go:58-60).
//
// The ZERO VALUE is usable and every lookup misses — the pane then shows the raw ids, exactly as
// it did before this file existed, so nothing can get worse and AC4's fallback is the default.

import (
	"context"
	"sync"
	"time"

	"connectrpc.com/connect"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
)

const (
	// nameIndexWorkflowTTL bounds how long a resolved workflow name is trusted. Names change
	// rarely (a workflow rename), and the alternative — listing workflows on every source fetch —
	// would add an RPC to every reload of a 5s refresh window. It is the same 30s cadence
	// execution/names.go uses, so the two clients stay in the same order of freshness.
	nameIndexWorkflowTTL = 30 * time.Second
	// nameIndexWorkflowPage is how many workflows the index reads. Workflows are few (tens), so
	// this covers every real tenant with room to spare; a workflow outside the page still renders
	// — it falls back to its raw id — so this is a display limit, never a correctness one.
	nameIndexWorkflowPage = 500
)

// shortRunIDWidth is how many runes of a workflow-run id are kept. It is the GUI's own rule
// (work-items_.$id.tsx:852 does `slice(0, 12) + "…"`), so the two clients shorten a run the same
// way: TruncateRunes(s, 13) = 12 runes + the ellipsis.
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

// setWorkflows swaps the workflow index. An EMPTY map is ignored rather than assigned: only some
// paths fetch workflows (the edit prep does not list projects, for instance), and a message that
// carries no list must not erase one an earlier fetch established — the same rule screen.go
// already applies to m.workflows (`if len(msg.workflows) > 0`).
//
// The TIMESTAMP is stamped either way: it answers "when did we last look", and a tenant with no
// workflows at all must not make every source fetch ask again.
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
//
// It is called from the WORK-ITEM source fetch (fetchWorkItems), i.e. inside the fetch the screen
// already performs, so the names are in memory before the pane that reads them is drawn — the shape
// execution/names.go uses for its run index. A failure is swallowed on purpose: a name lookup is
// decoration over an id that already renders, and it must never turn "list the work items" into an
// error. Callers where m.cl is absent (unit tests with no client) simply keep the raw ids.
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

// setItems swaps the work-item index with the page the caller just loaded. This one ASSIGNS
// unconditionally: the page is the authoritative set of titles, so a page that came back empty
// (every item archived, a filtered view) must empty the index rather than leave titles for rows
// that are no longer there.
func (n *nameIndex) setItems(m map[string]string) {
	if m == nil {
		m = map[string]string{}
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.items = m
}

// named renders an identifier as "name  (id)" — the name the operator reads FIRST, with the id it
// came from kept beside it because that is the form the operator quotes in a ticket or a support
// request (execution/names.go:133 is the same convention).
//
// An id that cannot be resolved (no name known, no index at all) renders as the RAW ID: never a
// placeholder, never an empty value, never an invented name.
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
//
// A run has NO NAME of its own — it is not a named entity, and pretending otherwise (a synthetic
// label, a derived title) would be inventing data. What the id is FOR here is identification, and a
// 26-character ULID is unreadable in a field; so it is shortened exactly the way the GUI shortens
// it (work-items_.$id.tsx:852). An unset run stays empty: TruncateRunes returns "" for "" (it
// returns s unchanged when it fits), so this cannot turn "no run yet" into "…".
func shortRunID(id string) string {
	if id == "" {
		return ""
	}
	return screenkit.TruncateRunes(id, shortRunIDWidth)
}
