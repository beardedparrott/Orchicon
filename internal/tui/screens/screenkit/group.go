package screenkit

// group.go — arranging a flat list into COLLAPSIBLE CATEGORY GROUPS.
//
// THE OPERATOR, IN TWO HALVES: "I created a conversation category and assigned a conversation to it,
// but it is not showing up in the UI" … "When an item (conversation, worker, workflow) is in a
// category, it should create a little arrow dropdown in their respective lists that can be collapsed
// or expanded."
//
// The second half is delivered by the list widget, not here: a row with a `Parent` is a CHILD of the
// row whose ID matches it, and a row with `HasChildren` draws the arrow and can be collapsed (see
// kit2.Table's VisibleRows/Toggle, which also re-seat the cursor so it never lands on a hidden row).
// So all this has to do is decide WHICH rows are parents and WHICH are children — which is what keeps
// grouping cheap on all three lists instead of three implementations of a collapsible tree.
//
// IT IS DELIBERATELY ADDITIVE. With no categories, the input is returned UNCHANGED — no parents, no
// depth, no behaviour — so every existing list and its tests are untouched until a grouping actually
// exists. That is the difference between adding a feature and rewriting three screens.

import (
	"fmt"
)

// groupRowIDPrefix marks a synthesized category row. A real entity id is a ULID (uppercase Crockford
// base32) and a NUL can never appear in one, so a group row's id can never collide with an item's.
const groupRowIDPrefix = "\x00grp:"

// GroupRowID is the synthetic row id for a category's parent row.
func GroupRowID(categoryID string) string { return groupRowIDPrefix + categoryID }

// IsGroupRow reports whether a row id belongs to a synthesized category row rather than to an entity.
//
// Callers that WRITE need this: a category row is not an item, so "edit the selected worker" on one
// would aim at an id the server has never heard of. The lists use it to refuse with a reason instead.
func IsGroupRow(id string) bool {
	return len(id) >= len(groupRowIDPrefix) && id[:len(groupRowIDPrefix)] == groupRowIDPrefix
}

// GroupItemsByCategory arranges a flat list into category groups.
//
// groupOf maps a row id to the category it is in (both "" = no grouping).
//
// THE LAYOUT, and why: one parent row per category in FIRST-APPEARANCE order, each followed by its
// members in their original order; then every UNGROUPED row, flat, at the top level. Ungrouped rows are
// deliberately NOT swept into an "Ungrouped" parent — the operator had a flat list before this existed,
// and hiding items that are in no category behind a collapsed placeholder would hide work that used to
// be visible. A group is something an item is PUT INTO, not something every item is taken out of.
func GroupItemsByCategory(items []Item, groupOf func(id string) (groupID, groupName string)) []Item {
	if groupOf == nil || len(items) == 0 {
		return items
	}
	type assign struct{ gid, gname string }
	assignOf := make([]assign, len(items))
	any := false
	for i, it := range items {
		// A row with no id is a HEADING (the Control pane's per-type rows use those): it cannot carry an
		// assignment, and asking the server about "" would group every heading together.
		if it.ID == "" {
			continue
		}
		gid, gname := groupOf(it.ID)
		if gid == "" {
			continue
		}
		assignOf[i] = assign{gid, gname}
		any = true
	}
	if !any {
		return items
	}

	out := make([]Item, 0, len(items)+8)
	emitted := make(map[string]bool)
	for i := range items {
		g := assignOf[i]
		if g.gid == "" || emitted[g.gid] {
			continue
		}
		emitted[g.gid] = true
		n := 0
		for _, b := range assignOf {
			if b.gid == g.gid {
				n++
			}
		}
		pid := GroupRowID(g.gid)
		out = append(out, Item{
			ID:          pid,
			Title:       g.gname,
			Meta:        fmt.Sprintf("%d items", n),
			HasChildren: true,
		})
		for j, child := range items {
			if assignOf[j].gid != g.gid {
				continue
			}
			child.Depth = 1
			child.Parent = pid
			child.HasChildren = false
			out = append(out, child)
		}
	}
	for i, it := range items {
		if assignOf[i].gid != "" {
			continue
		}
		out = append(out, it)
	}
	return out
}
