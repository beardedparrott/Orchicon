package work

// views.go — the Work screen's display groupings: the work-item Tree (the real
// Epic→Feature→Task→Subtask DAG from parent links).

import (
	"sort"
	"strconv"
	"strings"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// The Work Items views are TREE and ARCHIVE.
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
func workItemMeta(w *apiv1.WorkItem, prs prIndex) string {
	meta := statusPill(w.GetStatus())
	if prs.merged(w.GetId()) {
		meta += " · PR merged"
	}
	return meta
}

// rowTitle is a tree row's cell text: its kind badge and its title.
func rowTitle(w *apiv1.WorkItem) string {
	return "[" + kindBadge(w.GetKind()) + "] " + w.GetTitle()
}

// treeRows walks the real parent links (WorkItem.parent_id) depth-first from the roots, preserving
// sibling order (sort_order, then title).
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

// nestedRows renders rows for the item set rooted at parentID, walking the parent links
// depth-first.
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
		// The STEP NUMBER comes from the stored sequence; the row ORDER comes from the selected display
		// mode.
		seq := append([]*apiv1.WorkItem{}, kids...)
		sortSiblings(seq, sortSequence)
		seqIndex := make(map[string]int, len(seq))
		for i, w := range seq {
			seqIndex[w.GetId()] = i
		}
		sortSiblings(kids, mode)
		for _, w := range kids {
			// The tree metadata is what makes the pane a REAL tree: Depth indents the row, Parent lets a
			// collapse hide the subtree, and HasChildren decides whether the row draws a +/- toggle.
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

// indexChildren dedupes by id (first occurrence wins, keeping the server's order for the survivors)
// and groups the items by their EFFECTIVE parent.
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

// sortMode is the DISPLAY ordering of sibling work items — the control the operator asked for next
// to the search box ("What does reorder actually do on work items?
type sortMode string

const (
	// sortSequence is the item's real sequence chain (sort_order, NULLs last, then title). This is the
	// ONLY mode that reflects the stored order, which is what J/K (ReorderWorkItems) edits.
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

// sortSiblings orders siblings for DISPLAY by the selected mode.
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

// archiveGhostPrefix marks a row that is NOT archived: an ACTIVE ancestor shown only to keep an
// archived item's hierarchy connected.
const archiveGhostPrefix = "active"

// isArchivedRowTitle reports whether a row's title came from an archived item.
func isArchivedRowTitle(title string) bool {
	return strings.Contains(title, "(active ")
}

// archiveRows renders the ARCHIVE view as a real tree.
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

	// Walk an archived item's ancestor chain until it reaches something archived (already a row, so a
	// normal parent edge) or an ACTIVE item (an anchor).
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

	// The rendered set is the archived page PLUS its active anchors.
	rows := make([]*apiv1.WorkItem, 0, len(items)+len(ghostID))
	rows = append(rows, items...)
	for id := range ghostID {
		rows = append(rows, activeByID[id])
	}

	// A row's parent follows the REAL parent link, whichever set the parent is in.
	parentOf := func(w *apiv1.WorkItem) string { return w.GetParentId() }
	_, byParent, known := indexChildren(rows, parentOf)

	return nestedRows(byParent, func(id string) bool { return known[id] },
		parentOf, rows, mode,
		func(w *apiv1.WorkItem) string { return archiveRowMeta(w, ghostID[w.GetId()]) },
		func(id string) bool { return ghostID[id] })
}

// archiveRowMeta is an archive row's right-hand field: the state it will be RESTORED TO, which is
// the archive view's whole point.
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

// rowsFor maps a page of work items into the rows of the selected view.
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
