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
//
// THE LAYOUT MIRRORS THE GUI, because the operator's requirement is that someone moving between the
// two clients "feel at home no matter what interface they are using":
//
//	for every category, IN THE SERVER'S ORDER   → a collapsible folder holding its members
//	then, if anything is ungrouped               → an "Uncategorized" folder holding those
//
// Both details come from the GUI's own rendering (workers.tsx / workflows.tsx build `groups` from
// `prefs.state.categories` sorted by `order`, then append an "uncategorized" group only when it has
// items). Empty categories therefore render as EMPTY FOLDERS rather than vanishing: an operator who
// created "Frontend" in the GUI and finds no trace of it in the TUI has been told the two clients
// disagree, which is exactly the confusion this is fixing.

import (
	"fmt"
)

// groupRowIDPrefix marks a synthesized category row. A real entity id is a ULID (uppercase Crockford
// base32) and a NUL can never appear in one, so a group row's id can never collide with an item's.
const groupRowIDPrefix = "\x00grp:"

// UncategorizedGroupID and UncategorizedGroupName are the synthetic folder for items in no grouping.
//
// The id is the GUI's OWN sentinel (`category.id === "uncategorized"` in workers.tsx and
// CategoryFolder.tsx) rather than a new one, so the two clients name the same thing the same way.
const (
	UncategorizedGroupID   = "uncategorized"
	UncategorizedGroupName = "Uncategorized"
)

// GroupSpec is one category to render as a folder, in display order.
type GroupSpec struct {
	ID   string
	Name string
}

// GroupRowID is the synthetic row id for a category's folder row.
func GroupRowID(categoryID string) string { return groupRowIDPrefix + categoryID }

// GroupCategoryID recovers the category id from a folder row's synthetic id ("" when it is not one).
//
// The inverse of GroupRowID, and the reason a screen can act on the GROUPING behind the row it is
// pointing at: rename and delete apply to the category, so the pane has to get its id back out.
func GroupCategoryID(rowID string) string {
	if !IsGroupRow(rowID) {
		return ""
	}
	return rowID[len(groupRowIDPrefix):]
}

// IsGroupRow reports whether a row id belongs to a synthesized category row rather than to an entity.
//
// Callers that WRITE need this: a category row is not an item, so "edit the selected worker" on one
// would aim at an id the server has never heard of. The lists use it to refuse with a reason instead —
// except for the two chords that DO apply to a grouping (rename, delete).
func IsGroupRow(id string) bool {
	return len(id) >= len(groupRowIDPrefix) && id[:len(groupRowIDPrefix)] == groupRowIDPrefix
}

// GroupItemsByCategory arranges a flat list into category folders.
//
// groups is every category to render, ALREADY IN DISPLAY ORDER (the shell sorts by the server's
// sort_order). Empty ones are rendered as empty folders, matching the GUI.
//
// categoryOf maps an entity id to the category it is in ("" = ungrouped).
func GroupItemsByCategory(items []Item, groups []GroupSpec, categoryOf func(entityID string) string) []Item {
	// ADDITIVE: with no categories anywhere, every row is returned exactly as it came in. This is what
	// lets the same helper serve three lists without changing any of them for a tenant that has never
	// made a grouping.
	if len(groups) == 0 || len(items) == 0 {
		return items
	}
	inGroup := make([]string, len(items))
	for i, it := range items {
		// A row with no id is a HEADING (the Control pane's per-type rows use those): it cannot carry an
		// assignment, and asking the server about "" would file every heading together.
		if it.ID == "" || IsGroupRow(it.ID) {
			continue
		}
		inGroup[i] = categoryOf(it.ID)
	}

	emit := func(gid, gname string, member func(i int) bool) (Item, []Item) {
		parent := Item{ID: GroupRowID(gid), Title: gname, HasChildren: true}
		var kids []Item
		for i, it := range items {
			if !member(i) {
				continue
			}
			it.Depth = 1
			it.Parent = parent.ID
			it.HasChildren = false
			kids = append(kids, it)
		}
		parent.Meta = fmt.Sprintf("%d items", len(kids))
		return parent, kids
	}

	out := make([]Item, 0, len(items)+len(groups)+2)
	known := make(map[string]bool, len(groups))
	for _, g := range groups {
		known[g.ID] = true
		parent, kids := emit(g.ID, g.Name, func(i int) bool { return inGroup[i] == g.ID })
		out = append(out, parent)
		out = append(out, kids...)
	}

	// UNCATEGORIZED LAST, and only when it has members — the GUI's own rule. An assignment can name a
	// category the caller did not pass in (a stale cache, a group deleted between fetches), so those
	// items are treated as ungrouped rather than dropped: losing a row is worse than misfiling it.
	ungrouped := func(i int) bool {
		if inGroup[i] == "" {
			return items[i].ID != ""
		}
		return !known[inGroup[i]]
	}
	parent, kids := emit(UncategorizedGroupID, UncategorizedGroupName, ungrouped)
	if len(kids) > 0 {
		out = append(out, parent)
		out = append(out, kids...)
	}
	return out
}
