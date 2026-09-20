package work

// views.go — the Work screen's display groupings: the work-item Tree
// (the real Epic→Feature→Task→Subtask DAG from parent links), the Board
// (items grouped by status) and the Archive (terminal items hidden by
// every normal view).
//
// Invariant: these are DISPLAY groupings. Nothing here mutates sequence
// order — only ReorderWorkItems does that (see workitems.go), so a group
// or sort can never silently renumber the sequence.

import (
	"sort"
	"strconv"
	"strings"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// The Work Items views are TREE and ARCHIVE. A Board was removed: a
// status-grouped Kanban does not read as a list in a single-column terminal
// pane (every "column" became a header row), so it was a second, worse copy of
// the same data. Only ReorderWorkItems mutates sequence order.
type viewMode string

const (
	viewTree    viewMode = "tree"
	viewArchive viewMode = "archive"
)

// next cycles tree → archive → tree.
func (v viewMode) next() viewMode {
	if v == viewTree {
		return viewArchive
	}
	return viewTree
}

// kindBadge is the row's kind badge, rendered from the item's real kind
// field ("epic", "feature", "task", "subtask", …).
func kindBadge(k apiv1.WorkItemKind) string {
	name := strings.ToLower(k.String())
	name = strings.TrimPrefix(name, "work_item_kind_")
	if name == "" {
		return "?"
	}
	return name
}

// statusPill is the row's state pill, rendered from the item's real status
// field ("pending", "running", "succeeded", …).
func statusPill(s apiv1.WorkItemStatus) string {
	name := strings.ToLower(s.String())
	name = strings.TrimPrefix(name, "work_item_status_")
	if name == "" {
		return "?"
	}
	return name
}

// workItemMeta is the row's right-hand context: the state pill.
//
// IT CARRIES THE STATE AND NOTHING DERIVED FROM THE LEGACY WORKER REF. The `· assigned` suffix that used to
// sit here was doubly wrong: it duplicated the state pill (an item whose status IS `assigned` read
// "assigned · assigned"), and it was derived from assigned_worker_ref, which no longer describes how work is
// routed — WORKFLOWS carry the worker, per the operator: "work is set via workflows and not individual
// workers. That I believe was left over from old original code." Priority is deliberately not here either;
// the PR mark below is what the row reports beyond its state.
func workItemMeta(w *apiv1.WorkItem) string {
	return statusPill(w.GetStatus())
}

// rowTitle is a tree row's cell text: its kind badge and its title. The INDENT
// is deliberately not baked in here — the row now carries its Depth and the
// list pane draws the indent (and the +/- toggle), so padding the title as
// well would double it.
func rowTitle(w *apiv1.WorkItem) string {
	return "[" + kindBadge(w.GetKind()) + "] " + w.GetTitle()
}

// treeRows walks the real parent links (WorkItem.parent_id) depth-first from
// the roots, preserving sibling order (sort_order, then title). Orphans
// (parent not in the page) are rendered as roots so no item is ever dropped.
// stepNumber prefixes a row with its RUN position within its sibling sequence,
// so an execution order is visible in the list itself.
//
// Only a sibling GROUP of two or more is numbered, and only when the row is a
// CHILD (depth > 0): a top-level item is nobody's step, so numbering the roots
// implied a run order across epics that does not exist — and the operator
// rightly called that dangerous ("we can't sequentially kick off epics can
// we?"). The number is always the STORED sequence (the order the reconciler
// arms), never the display order.
func stepNumber(seqIndex map[string]int, groupSize, depth int, id string) string {
	if depth == 0 || groupSize < 2 {
		return ""
	}
	n, ok := seqIndex[id]
	if !ok {
		return ""
	}
	return strconv.Itoa(n+1) + ". "
}

// treeRows builds the tree rows from the fetched set.
//
// IT IS DUPLICATE-PROOF, which is a property of the data rather than tidiness: the server's list paged
// with a cursor that disagreed with its own page-1 ordering, so the SAME work item could arrive twice
// in one response and this function (which appends per byParent slot) rendered it as two rows. The
// operator reported exactly that — "There are two of them showing up in the TUI but only one in the
// GUI" — even though the database holds ONE row (verified: project 01KYQXQ95C2BFGDT1AFXFX5875, id
// 01M0NAYG0PKJ7EB7ZKNAQSMF9T, a single cancelled task).
//
// The fetch no longer walks that cursor, so this should never see a repeat. It is guarded anyway,
// because the failure mode is silent and a duplicated row is indistinguishable from a duplicated
// work item — the operator had no way to tell whether the DATA was wrong or the view was.
func treeRows(items []*apiv1.WorkItem, mode sortMode) []kit2.Item {
	// Dedupe by id, first occurrence wins, keeping the server's order for the survivors.
	deduped := make([]*apiv1.WorkItem, 0, len(items))
	seenID := map[string]bool{}
	for _, w := range items {
		if w.GetId() == "" || seenID[w.GetId()] {
			continue
		}
		seenID[w.GetId()] = true
		deduped = append(deduped, w)
	}
	items = deduped

	byParent := map[string][]*apiv1.WorkItem{}
	known := map[string]bool{}
	for _, w := range items {
		known[w.GetId()] = true
	}
	for _, w := range items {
		parent := w.GetParentId()
		if !known[parent] {
			parent = "" // root (or orphan)
		}
		byParent[parent] = append(byParent[parent], w)
	}
	var out []kit2.Item
	var walk func(parent string, depth int)
	walk = func(parent string, depth int) {
		kids := byParent[parent]
		// The STEP NUMBER comes from the stored sequence; the row ORDER comes
		// from the selected display mode. Computing both here keeps them
		// independent (and keeps the number honest under any sort).
		seq := append([]*apiv1.WorkItem{}, kids...)
		sortSiblings(seq, sortSequence)
		seqIndex := make(map[string]int, len(seq))
		for i, w := range seq {
			seqIndex[w.GetId()] = i
		}
		sortSiblings(kids, mode)
		for _, w := range kids {
			// The tree metadata is what makes the pane a REAL tree: Depth
			// indents the row, Parent lets a collapse hide the subtree, and
			// HasChildren decides whether the row draws a +/- toggle.
			out = append(out, kit2.Item{
				ID:          w.GetId(),
				Title:       stepNumber(seqIndex, len(kids), depth, w.GetId()) + rowTitle(w),
				Meta:        workItemMeta(w),
				Depth:       depth,
				Parent:      parent,
				HasChildren: len(byParent[w.GetId()]) > 0,
			})
			walk(w.GetId(), depth+1)
		}
	}
	walk("", 0)
	return out
}

// sortMode is the DISPLAY ordering of sibling work items — the control the
// operator asked for next to the search box ("What does reorder actually do on
// work items? I couldn't figure out what it was sorting by. Maybe we should
// have some actual sort controls at the top near the search box?").
type sortMode string

const (
	// sortSequence is the item's real sequence chain (sort_order, NULLs last,
	// then title). This is the ONLY mode that reflects the stored order, which
	// is what J/K (ReorderWorkItems) edits.
	sortSequence sortMode = "sequence"
	sortTitle    sortMode = "title"
	sortStatus   sortMode = "status"
	sortPriority sortMode = "priority"
)

// next cycles the sort control.
func (s sortMode) next() sortMode {
	switch s {
	case sortSequence:
		return sortTitle
	case sortTitle:
		return sortStatus
	case sortStatus:
		return sortPriority
	default:
		return sortSequence
	}
}

// labels the sort control shows.
func (s sortMode) label() string {
	return "sort: " + string(s)
}

// sortSiblings orders siblings for DISPLAY by the selected mode. Only
// sortSequence reflects the stored sequence; the others are views over the same
// rows and never renumber anything (the sequence is only ever mutated by
// ReorderWorkItems — see the invariant at the top of this file).
func sortSiblings(items []*apiv1.WorkItem, mode sortMode) {
	byTitle := func(a, b *apiv1.WorkItem) bool { return a.GetTitle() < b.GetTitle() }
	switch mode {
	case sortTitle:
		sort.SliceStable(items, func(i, j int) bool { return byTitle(items[i], items[j]) })
		return
	case sortStatus:
		sort.SliceStable(items, func(i, j int) bool {
			a, b := items[i], items[j]
			if statusPill(a.GetStatus()) != statusPill(b.GetStatus()) {
				return statusPill(a.GetStatus()) < statusPill(b.GetStatus())
			}
			return byTitle(a, b)
		})
		return
	case sortPriority:
		sort.SliceStable(items, func(i, j int) bool {
			a, b := items[i], items[j]
			if a.GetPriority() != b.GetPriority() {
				return a.GetPriority() > b.GetPriority() // higher priority first
			}
			return byTitle(a, b)
		})
		return
	}
	// sortSequence: the stored chain.
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		an, bn := a.SortOrder == 0, b.SortOrder == 0
		switch {
		case an && bn:
			return byTitle(a, b)
		case an:
			return false
		case bn:
			return true
		default:
			return a.GetSortOrder() < b.GetSortOrder()
		}
	})
}

// archiveRows lists archived items with the status they will be restored to
// (archived_from_status) — the archive view's whole point.
func archiveRows(items []*apiv1.WorkItem, mode sortMode) []kit2.Item {
	sorted := append([]*apiv1.WorkItem{}, items...)
	sortSiblings(sorted, mode)
	out := make([]kit2.Item, 0, len(sorted))
	for _, w := range sorted {
		from := w.GetArchivedFromStatus()
		if from == "" {
			from = statusPill(w.GetStatus())
		}
		out = append(out, kit2.Item{
			ID:    w.GetId(),
			Title: "[" + kindBadge(w.GetKind()) + "] " + w.GetTitle(),
			Meta:  "archived · restores to " + from,
		})
	}
	return out
}

// rowsFor maps a page of work items into the rows of the selected view.
func rowsFor(view viewMode, items []*apiv1.WorkItem, mode sortMode) []kit2.Item {
	if view == viewArchive {
		return archiveRows(items, mode)
	}
	return treeRows(items, mode)
}

// descendants returns the ids of every transitive child of id.
func descendants(items []*apiv1.WorkItem, id string) []string {
	var out []string
	byParent := map[string][]string{}
	for _, w := range items {
		byParent[w.GetParentId()] = append(byParent[w.GetParentId()], w.GetId())
	}
	var walk func(string)
	walk = func(p string) {
		for _, c := range byParent[p] {
			out = append(out, c)
			walk(c)
		}
	}
	walk(id)
	return out
}
