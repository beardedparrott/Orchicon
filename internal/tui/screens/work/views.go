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
	"fmt"
	"sort"
	"strings"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// viewMode selects the work-items pane's display grouping.
type viewMode string

const (
	viewTree    viewMode = "tree"
	viewBoard   viewMode = "board"
	viewArchive viewMode = "archive"
)

// next cycles tree → board → archive → tree.
func (v viewMode) next() viewMode {
	switch v {
	case viewTree:
		return viewBoard
	case viewBoard:
		return viewArchive
	default:
		return viewTree
	}
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

// workItemMeta is the row's right-hand context: state pill + priority.
func workItemMeta(w *apiv1.WorkItem) string {
	meta := statusPill(w.GetStatus())
	if p := w.GetPriority(); p != 0 {
		meta += fmt.Sprintf(" · p%d", p)
	}
	if w.GetAssignedWorkerRef() != "" {
		meta += " · assigned"
	}
	return meta
}

// rowTitle renders a tree row's cell: indentation from the item's DEPTH in
// the real parent-child DAG, its kind badge, and its title.
func rowTitle(w *apiv1.WorkItem, depth int) string {
	return strings.Repeat("  ", depth) + "[" + kindBadge(w.GetKind()) + "] " + w.GetTitle()
}

// treeRows walks the real parent links (WorkItem.parent_id) depth-first from
// the roots, preserving sibling order (sort_order, then title). Orphans
// (parent not in the page) are rendered as roots so no item is ever dropped.
func treeRows(items []*apiv1.WorkItem) []kit2.Item {
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
		sortSiblings(kids)
		for _, w := range kids {
			out = append(out, kit2.Item{ID: w.GetId(), Title: rowTitle(w, depth), Meta: workItemMeta(w)})
			walk(w.GetId(), depth+1)
		}
	}
	walk("", 0)
	return out
}

// sortSiblings orders siblings by the sequence chain (sort_order, NULLs
// last), then title — a stable DISPLAY order only.
func sortSiblings(items []*apiv1.WorkItem) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		an, bn := a.SortOrder == 0, b.SortOrder == 0
		switch {
		case an && bn:
			return a.GetTitle() < b.GetTitle()
		case an:
			return false
		case bn:
			return true
		default:
			return a.GetSortOrder() < b.GetSortOrder()
		}
	})
}

// boardOrder is the Kanban column order (the lifecycle, then the
// system-managed states).
var boardOrder = []apiv1.WorkItemStatus{
	apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING,
	apiv1.WorkItemStatus_WORK_ITEM_STATUS_READY,
	apiv1.WorkItemStatus_WORK_ITEM_STATUS_SCHEDULED,
	apiv1.WorkItemStatus_WORK_ITEM_STATUS_ASSIGNED,
	apiv1.WorkItemStatus_WORK_ITEM_STATUS_RUNNING,
	apiv1.WorkItemStatus_WORK_ITEM_STATUS_BLOCKED,
	apiv1.WorkItemStatus_WORK_ITEM_STATUS_SUCCEEDED,
	apiv1.WorkItemStatus_WORK_ITEM_STATUS_FAILED,
	apiv1.WorkItemStatus_WORK_ITEM_STATUS_CANCELLED,
	apiv1.WorkItemStatus_WORK_ITEM_STATUS_SKIPPED,
}

// boardRows groups the items by their real status field. Every column is
// emitted with its count (an empty column still shows, so the board is a
// stable map of the lifecycle rather than a shrinking list).
func boardRows(items []*apiv1.WorkItem) []kit2.Item {
	byStatus := map[apiv1.WorkItemStatus][]*apiv1.WorkItem{}
	for _, w := range items {
		byStatus[w.GetStatus()] = append(byStatus[w.GetStatus()], w)
	}
	seen := map[apiv1.WorkItemStatus]bool{}
	var out []kit2.Item
	emit := func(st apiv1.WorkItemStatus) {
		seen[st] = true
		group := byStatus[st]
		sortSiblings(group)
		out = append(out, kit2.Item{ID: "col:" + statusPill(st), Title: "── " + statusPill(st) + fmt.Sprintf(" (%d)", len(group))})
		for _, w := range group {
			out = append(out, kit2.Item{ID: w.GetId(), Title: "[" + kindBadge(w.GetKind()) + "] " + w.GetTitle(), Meta: workItemMeta(w)})
		}
	}
	for _, st := range boardOrder {
		emit(st)
	}
	// Any status outside the canonical column order still gets a column.
	var extra []apiv1.WorkItemStatus
	for st := range byStatus {
		if !seen[st] {
			extra = append(extra, st)
		}
	}
	sort.Slice(extra, func(i, j int) bool { return extra[i].String() < extra[j].String() })
	for _, st := range extra {
		emit(st)
	}
	return out
}

// archiveRows lists archived items with the status they will be restored to
// (archived_from_status) — the archive view's whole point.
func archiveRows(items []*apiv1.WorkItem) []kit2.Item {
	sorted := append([]*apiv1.WorkItem{}, items...)
	sortSiblings(sorted)
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
func rowsFor(view viewMode, items []*apiv1.WorkItem) []kit2.Item {
	switch view {
	case viewBoard:
		return boardRows(items)
	case viewArchive:
		return archiveRows(items)
	default:
		return treeRows(items)
	}
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
