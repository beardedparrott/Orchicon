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
	"sort"
	"strings"
)

// groupRowIDPrefix marks a synthesized category row. A real entity id is a ULID (uppercase Crockford
// base32) and a NUL can never appear in one, so a group row's id can never collide with an item's.
const groupRowIDPrefix = "\x00grp:"

// UncategorizedGroupID and UncategorizedGroupName are the synthetic folder for items in no
// grouping.
const (
	UncategorizedGroupID   = "uncategorized"
	UncategorizedGroupName = "Uncategorized"
)

// GroupSpec is one category to render as a folder.
type GroupSpec struct {
	ID   string
	Name string
	// SortOrder is the SERVER's ordering for this category. The list helper sorts by it, so every
	// surface renders folders in the same sequence no matter which response it built them from.
	SortOrder int
}

// SortGroupSpecs orders folders the way BOTH clients render them: the server's sort_order, then the
// name as a tie-break (folders created in the same transaction share an order.
func SortGroupSpecs(gs []GroupSpec) []GroupSpec {
	out := append([]GroupSpec(nil), gs...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].SortOrder != out[j].SortOrder {
			return out[i].SortOrder < out[j].SortOrder
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

// GroupRowID is the synthetic row id for a category's folder row.
func GroupRowID(categoryID string) string { return groupRowIDPrefix + categoryID }

// GroupCategoryID recovers the category id from a folder row's synthetic id ("" when it is not
// one).
func GroupCategoryID(rowID string) string {
	if !IsGroupRow(rowID) {
		return ""
	}
	return rowID[len(groupRowIDPrefix):]
}

// IsGroupRow reports whether a row id belongs to a synthesized category row rather than to an
// entity.
func IsGroupRow(id string) bool {
	return len(id) >= len(groupRowIDPrefix) && id[:len(groupRowIDPrefix)] == groupRowIDPrefix
}

// GroupItemsByCategory arranges a flat list into category folders.
func GroupItemsByCategory(items []Item, groups []GroupSpec, categoryOf func(entityID string) string) []Item {
	// ADDITIVE: with no categories anywhere, every row is returned exactly as it came in.
	if len(groups) == 0 || len(items) == 0 {
		return items
	}
	// THE ORDER IS APPLIED HERE, not left to each caller: three surfaces build folder lists from three
	// different responses, and "the GUI and the TUI list groupings the same way" is the whole point.
	groups = SortGroupSpecs(groups)
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

	// UNCATEGORIZED LAST, and only when it has members — the GUI's own rule.
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
