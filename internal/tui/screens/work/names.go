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
// NO TTL. A TTL bounds how stale a CACHED RPC may be; these maps are rewritten by the very fetch
// that rewrites the pane they feed (the work-item page, the Projects page, the form prep), so they
// can never be staler than the rows drawn beside them.
//
// The mutex is a CORRECTNESS requirement, not decoration: kit2's DetailFn runs inside a tea.Cmd
// closure (kit2/base.go:1529, :1546), i.e. OFF the update loop, while the indices are written ON
// it. That is the same hazard m.viewMu exists for (screen.go:58-60).
//
// The ZERO VALUE is usable and every lookup misses — the pane then shows the raw ids, exactly as
// it did before this file existed, so nothing can get worse and AC4's fallback is the default.

import (
	"sync"

	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
)

// shortRunIDWidth is how many runes of a workflow-run id are kept. It is the GUI's own rule
// (work-items_.$id.tsx:852 does `slice(0, 12) + "…"`), so the two clients shorten a run the same
// way: TruncateRunes(s, 13) = 12 runes + the ellipsis.
const shortRunIDWidth = 13

type nameIndex struct {
	mu        sync.Mutex
	workflows map[string]string // workflow id   → name  (the form prep's workflow list)
	projects  map[string]string // project id    → name  (the Projects page + the form prep)
	items     map[string]string // work item id  → title (the loaded work-item page)
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
func (n *nameIndex) setWorkflows(m map[string]string) {
	if len(m) == 0 {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.workflows = m
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
