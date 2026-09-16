package screenkit

// group_test.go — nesting a flat list under its category folders.
//
// The operator: "When an item (conversation, worker, workflow) is in a category, it should create a
// little arrow dropdown in their respective lists that can be collapsed or expanded" … "Both should
// honor the same and work in the same way."
//
// The arrow and the collapse come from the LIST widget (a row with a Parent hangs off the row whose id
// matches; a row with HasChildren draws the toggle). What these tests pin is the arrangement this
// package is responsible for, its ADDITIVE property (which keeps it from disturbing three lists), and
// the two details taken from the GUI's own rendering: an ordered folder per category — EMPTY ONES
// INCLUDED — and a trailing "Uncategorized" folder for whatever is left.

import "testing"

// items builds a flat list of ids.
func items(ids ...string) []Item {
	out := make([]Item, 0, len(ids))
	for _, id := range ids {
		out = append(out, Item{ID: id, Title: "item " + id, Meta: "meta"})
	}
	return out
}

func titles(in []Item) []string {
	out := make([]string, 0, len(in))
	for _, it := range in {
		out = append(out, it.Title)
	}
	return out
}

// memberOf builds a categoryOf function from a map.
func memberOf(m map[string]string) func(string) string {
	return func(id string) string { return m[id] }
}

// TestGroupingIsAdditiveWithNoCategories is the property that lets this land on three lists at once:
// with no categories, the input is returned UNCHANGED — no folders, no depth. Every existing list and
// every existing test therefore behaves exactly as it did.
func TestGroupingIsAdditiveWithNoCategories(t *testing.T) {
	in := items("a", "b", "c")
	out := GroupItemsByCategory(in, nil, memberOf(nil))

	if len(out) != len(in) {
		t.Fatalf("with no categories the list must be unchanged, got %d rows for %d items", len(out), len(in))
	}
	for i := range out {
		if out[i].ID != in[i].ID || out[i].Depth != 0 || out[i].Parent != "" || out[i].HasChildren {
			t.Fatalf("row %d gained folder metadata with no categories: %+v", i, out[i])
		}
	}
}

// TestFoldersNestMembersUnderTheirCategory: one folder per category, with its members as CHILDREN —
// which is what gives the operator the collapsible arrow.
func TestFoldersNestMembersUnderTheirCategory(t *testing.T) {
	in := items("w1", "w2", "w3")
	groups := []GroupSpec{{ID: "catA", Name: "Alpha"}}
	out := GroupItemsByCategory(in, groups, memberOf(map[string]string{"w1": "catA", "w3": "catA"}))

	// Folder, its two members, then the Uncategorized folder and the remaining row.
	if len(out) != 5 {
		t.Fatalf("want 5 rows, got %d: %v", len(out), titles(out))
	}
	folder := out[0]
	if !folder.HasChildren {
		t.Fatalf("the category row must be a COLLAPSIBLE folder, got %+v", folder)
	}
	if folder.Parent != "" || folder.Depth != 0 {
		t.Fatalf("the category row must sit at the top level, got %+v", folder)
	}
	if folder.Title != "Alpha" {
		t.Fatalf("the category row must be named by its grouping, got %q", folder.Title)
	}
	if GroupCategoryID(folder.ID) != "catA" {
		t.Fatalf("the folder row's id must yield the category id back, got %q", folder.ID)
	}
	// The members hang off the folder, at depth 1, and are NOT themselves folders.
	for _, child := range out[1:3] {
		if child.Parent != folder.ID {
			t.Fatalf("member %q must hang off the folder, got parent %q", child.ID, child.Parent)
		}
		if child.Depth != 1 {
			t.Fatalf("member %q must be indented one level, got depth %d", child.ID, child.Depth)
		}
		if child.HasChildren {
			t.Fatalf("member %q must not draw its own toggle", child.ID)
		}
	}
	// Order within the folder is the ORIGINAL order, so a list does not shuffle when it is grouped.
	if out[1].ID != "w1" || out[2].ID != "w3" {
		t.Fatalf("members must keep their original order, got %s, %s", out[1].ID, out[2].ID)
	}
}

// TestUncategorizedIsATrailingFolder mirrors the GUI: ungrouped items go into an "Uncategorized"
// folder, AFTER every real category, and it is collapsible like any other.
func TestUncategorizedIsATrailingFolder(t *testing.T) {
	in := items("w1", "w2")
	groups := []GroupSpec{{ID: "catA", Name: "Alpha"}}
	out := GroupItemsByCategory(in, groups, memberOf(map[string]string{"w1": "catA"}))

	if len(out) != 4 {
		t.Fatalf("want folder + member + Uncategorized + member, got %d: %v", len(out), titles(out))
	}
	unc := out[2]
	if GroupCategoryID(unc.ID) != UncategorizedGroupID {
		t.Fatalf("the trailing folder must be Uncategorized, got %q", unc.ID)
	}
	if unc.Title != UncategorizedGroupName || !unc.HasChildren {
		t.Fatalf("Uncategorized must be a named collapsible folder, got %+v", unc)
	}
	if out[3].ID != "w2" || out[3].Parent != unc.ID || out[3].Depth != 1 {
		t.Fatalf("the ungrouped row must nest under Uncategorized, got %+v", out[3])
	}
}

// TestUncategorizedIsOmittedWhenEmpty: a folder with nothing in it is noise, and the GUI omits it too
// (`if (uncategorizedItems.length > 0)`).
func TestUncategorizedIsOmittedWhenEmpty(t *testing.T) {
	in := items("w1")
	groups := []GroupSpec{{ID: "catA", Name: "Alpha"}}
	out := GroupItemsByCategory(in, groups, memberOf(map[string]string{"w1": "catA"}))
	for _, it := range out {
		if GroupCategoryID(it.ID) == UncategorizedGroupID {
			t.Fatalf("Uncategorized must not render when nothing is ungrouped: %v", titles(out))
		}
	}
}

// TestEmptyCategoriesStillRenderAsFolders: the GUI builds a group for EVERY category it knows about, so
// an operator who created "Frontend" sees the folder before anything is in it. Dropping empty folders
// would tell them the two clients disagree — the confusion this work exists to remove.
func TestEmptyCategoriesStillRenderAsFolders(t *testing.T) {
	in := items("w1")
	groups := []GroupSpec{{ID: "catA", Name: "Alpha"}, {ID: "catB", Name: "Beta"}}
	out := GroupItemsByCategory(in, groups, memberOf(map[string]string{"w1": "catA"}))

	folders := 0
	for _, it := range out {
		if IsGroupRow(it.ID) && GroupCategoryID(it.ID) != UncategorizedGroupID {
			folders++
		}
	}
	if folders != 2 {
		t.Fatalf("both categories must render, empty or not, got %d folders: %v", folders, titles(out))
	}
	// And the EMPTY one carries a zero count rather than a blank.
	for _, it := range out {
		if GroupCategoryID(it.ID) == "catB" && it.Meta != "0 items" {
			t.Fatalf("an empty folder must say it is empty, got meta %q", it.Meta)
		}
	}
}

// TestFolderOrderFollowsTheGroupsArgument: the order is the CALLER'S (the server's sort_order), not
// first-appearance — that is what keeps the two clients' folder order identical.
func TestFolderOrderFollowsTheGroupsArgument(t *testing.T) {
	in := items("w1", "w2")
	groups := []GroupSpec{{ID: "catB", Name: "Beta"}, {ID: "catA", Name: "Alpha"}}
	out := GroupItemsByCategory(in, groups, memberOf(map[string]string{"w1": "catA", "w2": "catB"}))

	var folders []string
	for _, it := range out {
		if IsGroupRow(it.ID) {
			folders = append(folders, it.Title)
		}
	}
	if len(folders) != 2 || folders[0] != "Beta" || folders[1] != "Alpha" {
		t.Fatalf("folders must follow the caller's order, got %v", folders)
	}
}

// TestAssignmentToAnUnknownCategoryIsTreatedAsUngrouped: a stale cache or a group deleted between
// fetches can leave an assignment naming a category the caller did not pass in. The item must land in
// Uncategorized rather than VANISH — losing a row is worse than misfiling it.
func TestAssignmentToAnUnknownCategoryIsTreatedAsUngrouped(t *testing.T) {
	in := items("w1")
	groups := []GroupSpec{{ID: "catA", Name: "Alpha"}}
	out := GroupItemsByCategory(in, groups, memberOf(map[string]string{"w1": "cat_GONE"}))

	found := false
	for _, it := range out {
		if it.ID == "w1" {
			found = true
			if GroupCategoryID(it.Parent) != UncategorizedGroupID {
				t.Fatalf("a row assigned to an unknown category must fall into Uncategorized, got parent %q", it.Parent)
			}
		}
	}
	if !found {
		t.Fatalf("the row must not be dropped: %v", titles(out))
	}
}

// TestGroupRowIDsCannotBeMistakenForEntities: a folder row carries a SYNTHETIC id, and every write
// chord has to be able to tell. A ULID cannot contain a NUL, so the prefix is unambiguous.
func TestGroupRowIDsCannotBeMistakenForEntities(t *testing.T) {
	if !IsGroupRow(GroupRowID("cat-1")) {
		t.Fatal("a category row's id must be recognized")
	}
	if got := GroupCategoryID(GroupRowID("cat-1")); got != "cat-1" {
		t.Fatalf("GroupCategoryID must invert GroupRowID, got %q", got)
	}
	for _, real := range []string{"", "01KYQXQ95C2BFGDT1AFXFX5875", "cat-1", "grp:cat-1", "uncategorized"} {
		if IsGroupRow(real) {
			t.Fatalf("%q is an entity id and must NOT read as a category row", real)
		}
		if GroupCategoryID(real) != "" {
			t.Fatalf("%q is not a folder row, so GroupCategoryID must be empty", real)
		}
	}
}
