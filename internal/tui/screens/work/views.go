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

// workItemMeta is the row's right-hand context: the state pill, plus the PR mark.
//
// IT CARRIES THE STATE AND NOTHING DERIVED FROM THE LEGACY WORKER REF. The `· assigned` suffix that used to
// sit here was doubly wrong: it duplicated the state pill (an item whose status IS `assigned` read
// "assigned · assigned"), and it was derived from assigned_worker_ref, which no longer describes how work is
// routed — WORKFLOWS carry the worker, per the operator: "work is set via workflows and not individual
// workers. That I believe was left over from old original code."
//
// PRIORITY IS GONE and the PR MARK TOOK ITS PLACE, which is the operator's trade: "I don't really care about
// viewing the priority levels in the work item list. Let's remove the priority level in list and put PR Merged
// if a PR has merged." Priority is still on the details pane, where there is room for it; the list row spends
// its one token on the fact an operator scans a list for — whether the work actually LANDED.
func workItemMeta(w *apiv1.WorkItem, prs prIndex) string {
	meta := statusPill(w.GetStatus())
	if prs.merged(w.GetId()) {
		meta += " · PR merged"
	}
	return meta
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

// nestedRows renders rows for the item set rooted at parentID, walking the
// parent links depth-first. It is the ONE tree walker, shared by the active Tree
// and the Archive view, so the two views cannot disagree about what a tree is.
//
// parentOf resolves a row's parent (the archive view's differs — see
// archiveRows). known reports whether an id is in the rendered set at all:
// an id whose parent is NOT present renders as a root, so no item is ever
// dropped, which is also how a ghost-anchor chain stays connected.
//
// byParent is the deduped set's child index; rows is where the output lands.
func nestedRows(
	byParent map[string][]*apiv1.WorkItem,
	known func(string) bool,
	parentOf func(*apiv1.WorkItem) string,
	items []*apiv1.WorkItem,
	mode sortMode,
	metaOf func(*apiv1.WorkItem) string,
	isGhost func(string) bool,
) []kit2.Item {
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
				Meta:        metaOf(w),
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

// indexChildren dedupes by id (first occurrence wins, keeping the server's
// order for the survivors) and groups the items by their EFFECTIVE parent.
//
// IT IS DUPLICATE-PROOF, which is a property of the data rather than tidiness: the server's list paged
// with a cursor that disagreed with its own page-1 ordering, so the SAME work item could arrive twice
// in one response and the walkers (which appended per byParent slot) rendered it as two rows. The
// operator reported exactly that — "There are two of them showing up in the TUI but only one in the
// GUI" — even though the database holds ONE row (verified: project 01KYQXQ95C2BFGDT1AFXFX5875, id
// 01M0NAYG0PKJ7EB7ZKNAQSMF9T, a single cancelled task).
//
// The fetch no longer walks that cursor, so this should never see a repeat. It is guarded anyway,
// because the failure mode is silent and a duplicated row is indistinguishable from a duplicated
// work item — the operator had no way to tell whether the DATA was wrong or the view was.
func indexChildren(items []*apiv1.WorkItem, parentOf func(*apiv1.WorkItem) string) ([]*apiv1.WorkItem, map[string][]*apiv1.WorkItem, map[string]bool) {
	deduped := make([]*apiv1.WorkItem, 0, len(items))
	seenID := map[string]bool{}
	for _, w := range items {
		if w.GetId() == "" || seenID[w.GetId()] {
			continue
		}
		seenID[w.GetId()] = true
		deduped = append(deduped, w)
	}
	known := map[string]bool{}
	for _, w := range deduped {
		known[w.GetId()] = true
	}
	byParent := map[string][]*apiv1.WorkItem{}
	for _, w := range deduped {
		parent := parentOf(w)
		if !known[parent] {
			parent = "" // root (or orphan / ghost-anchored)
		}
		byParent[parent] = append(byParent[parent], w)
	}
	return deduped, byParent, known
}

// treeRows builds the Tree view's rows: the real DAG from parent_id.
func treeRows(items []*apiv1.WorkItem, mode sortMode, prs prIndex) []kit2.Item {
	_, byParent, _ := indexChildren(items, func(w *apiv1.WorkItem) string { return w.GetParentId() })
	return nestedRows(byParent, func(string) bool { return true },
		func(w *apiv1.WorkItem) string { return w.GetParentId() },
		items, mode,
		func(w *apiv1.WorkItem) string { return workItemMeta(w, prs) },
		func(string) bool { return false })
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

// archiveGhostPrefix marks a row that is NOT archived: an ACTIVE ancestor shown
// only to keep an archived item's hierarchy connected. It is carried in the
// row's Meta (alongside the state word the operator reads) so the action layer
// can refuse to restore something that was never archived.
//
// It is a MARKER rather than a flag on kit2.Item because kit2.Item is the shared
// list-row contract: adding an archive-only field there would put archive
// vocabulary on every pane.
const archiveGhostPrefix = "active"

// isArchivedRowTitle reports whether a row's title came from an archived item.
func isArchivedRowTitle(title string) bool {
	return strings.Contains(title, "(active ")
}

// archiveRows renders the ARCHIVE view as a real tree, exactly as the GUI does
// (frontend/src/components/work-items/work-items-archive-view.tsx over
// dependency-utils.ts buildArchiveTreeData).
//
// WHY THIS IS NOT A FLAT LIST. It was one — it sorted the archived items and
// emitted a row each, with no Depth/Parent/HasChildren — while the Tree view next
// to it rendered the true Epic→Feature→Task→Subtask DAG. The operator: "The TUI
// archive view is still not a tree view." Archiving does not flatten anything
// (full subtrees survive archiving), so the archive IS a forest and must be drawn
// as one.
//
// GHOST ANCHORS — the half that makes the tree CONNECTED. The archive fetch asks
// the plane for archived rows only, so an archived item whose parent is still
// ACTIVE has no parent row to hang under; without one it would float to the top
// level beside the epics, which is the flat list again by another route. So the
// ACTIVE ancestors are resolved (from the separately fetched active page) and
// rendered as muted, non-restorable rows — the cross-boundary link stays visible
// without turning the archive into the active tree.
//
// activeByName is the active items indexed by id, used ONLY to resolve those
// anchors. An id that resolves nowhere renders as a root, so nothing is dropped.
func archiveRows(items []*apiv1.WorkItem, active []*apiv1.WorkItem, mode sortMode) []kit2.Item {
	archivedByID := map[string]*apiv1.WorkItem{}
	for _, w := range items {
		if w.GetId() != "" {
			archivedByID[w.GetId()] = w
		}
	}
	activeByID := map[string]*apiv1.WorkItem{}
	for _, w := range active {
		if w.GetId() != "" {
			activeByID[w.GetId()] = w
		}
	}

	// Walk an archived item's ancestor chain until it reaches something archived
	// (already a row, so a normal parent edge) or an ACTIVE item (an anchor). A
	// missing ancestor stops the walk rather than looping — the guard bounds a
	// cycle, which a corrupted parent link could otherwise produce.
	ghostID := map[string]bool{}
	for _, w := range items {
		parent := w.GetParentId()
		for guard := 0; parent != "" && guard < 16; guard++ {
			if _, ok := archivedByID[parent]; ok {
				break // an archived ancestor already carries the edge
			}
			a, ok := activeByID[parent]
			if !ok {
				break // unresolvable (deleted, or outside this page)
			}
			ghostID[a.GetId()] = true
			parent = a.GetParentId()
		}
	}

	// The rendered set is the archived page PLUS its active anchors. Anchors are
	// keyed by presence in ghostID rather than by "is it archived", because an id
	// could in principle appear in both sets (a stale page).
	rows := make([]*apiv1.WorkItem, 0, len(items)+len(ghostID))
	rows = append(rows, items...)
	for id := range ghostID {
		rows = append(rows, activeByID[id])
	}

	// A row's parent follows the REAL parent link, whichever set the parent is in.
	// An anchor's own ancestors are anchors too (the walk above recorded the whole
	// chain), so the chain stays connected up to the first active root.
	parentOf := func(w *apiv1.WorkItem) string { return w.GetParentId() }
	_, byParent, known := indexChildren(rows, parentOf)

	return nestedRows(byParent, func(id string) bool { return known[id] },
		parentOf, rows, mode,
		func(w *apiv1.WorkItem) string { return archiveRowMeta(w, ghostID[w.GetId()]) },
		func(id string) bool { return ghostID[id] })
}

// archiveRowMeta is an archive row's right-hand field: the state it will be
// RESTORED TO, which is the archive view's whole point.
//
// A GHOST ANCHOR says so instead — it is not archived, so "restores to" would be a
// lie about a row the operator cannot restore at all. The marker is what the
// action layer reads to refuse the restore (see itemActions).
func archiveRowMeta(w *apiv1.WorkItem, ghost bool) string {
	state := statusPill(w.GetStatus())
	if ghost {
		return archiveGhostPrefix + " ancestor — not archived (" + state + ")"
	}
	from := w.GetArchivedFromStatus()
	if from == "" {
		from = state
	}
	return "archived · restores to " + from
}

// rowsFor maps a page of work items into the rows of the selected view. prs is the PR index for this page (nil
// when the executions fetch is unavailable), used by the Tree rows' meta so a merged PR is visible in the list.
//
// `active` is the ACTIVE work-item page, needed only by the Archive view to
// resolve its ghost anchors (see archiveRows). It is nil in the Tree view — the
// Tree renders the active set itself, so it has nothing to anchor.
func rowsFor(view viewMode, items []*apiv1.WorkItem, active []*apiv1.WorkItem, mode sortMode, prs prIndex) []kit2.Item {
	if view == viewArchive {
		return archiveRows(items, active, mode)
	}
	return treeRows(items, mode, prs)
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
