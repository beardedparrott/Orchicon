package screenkit

// group_test.go — nesting a flat list under its category groups.
//
// The operator: "When an item (conversation, worker, workflow) is in a category, it should create a
// little arrow dropdown in their respective lists that can be collapsed or expanded."
//
// The arrow and the collapse come from the LIST widget (a row with a Parent hangs off the row whose id
// matches; a row with HasChildren draws the toggle). What these tests pin is the arrangement this
// package is responsible for, and the ADDITIVE property that keeps it from disturbing three lists.

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

// TestGroupingIsAdditiveWithNoCategories is the property that lets this land on three screens at once:
// with nothing assigned, the list is returned UNCHANGED — no parents, no depth. Every existing list and
// every existing test therefore behaves exactly as it did.
func TestGroupingIsAdditiveWithNoCategories(t *testing.T) {
	in := items("a", "b", "c")
	out := GroupItemsByCategory(in, func(string) (string, string) { return "", "" })

	if len(out) != len(in) {
		t.Fatalf("with no categories the list must be unchanged, got %d rows for %d items", len(out), len(in))
	}
	for i := range out {
		if out[i].ID != in[i].ID || out[i].Depth != 0 || out[i].Parent != "" || out[i].HasChildren {
			t.Fatalf("row %d gained tree metadata with no categories: %+v", i, out[i])
		}
	}
}

// TestGroupsNestMembersUnderTheirCategory: one parent per group, with its members as CHILDREN — which
// is what gives the operator the collapsible arrow.
func TestGroupsNestMembersUnderTheirCategory(t *testing.T) {
	in := items("w1", "w2", "w3")
	group := map[string]string{"w1": "catA", "w3": "catA"}
	out := GroupItemsByCategory(in, func(id string) (string, string) {
		if g, ok := group[id]; ok {
			return g, "Alpha"
		}
		return "", ""
	})

	// Parent, then its members, then the ungrouped row.
	if len(out) != 4 {
		t.Fatalf("want 4 rows (1 parent + 2 children + 1 ungrouped), got %d: %v", len(out), titles(out))
	}
	parent := out[0]
	if !parent.HasChildren {
		t.Fatalf("the category row must be a COLLAPSIBLE parent, got %+v", parent)
	}
	if parent.Parent != "" || parent.Depth != 0 {
		t.Fatalf("the category row must sit at the top level, got %+v", parent)
	}
	if parent.Title != "Alpha" {
		t.Fatalf("the category row must be named by its grouping, got %q", parent.Title)
	}
	if !IsGroupRow(parent.ID) {
		t.Fatalf("the category row's id must be recognizable as a group row, got %q", parent.ID)
	}
	// The members hang off the parent, at depth 1, and are NOT themselves parents.
	for _, child := range out[1:3] {
		if child.Parent != parent.ID {
			t.Fatalf("member %q must hang off the category row, got parent %q", child.ID, child.Parent)
		}
		if child.Depth != 1 {
			t.Fatalf("member %q must be indented one level, got depth %d", child.ID, child.Depth)
		}
		if child.HasChildren {
			t.Fatalf("member %q must not draw its own toggle", child.ID)
		}
	}
	// Order within the group is the ORIGINAL order, so a list does not shuffle when it is grouped.
	if out[1].ID != "w1" || out[2].ID != "w3" {
		t.Fatalf("members must keep their original order, got %s, %s", out[1].ID, out[2].ID)
	}
}

// TestUngroupedRowsStayAtTopLevel: a group is something an item is PUT INTO, not something every item
// is taken out of. Sweeping ungrouped items under an "Ungrouped" parent would hide work that used to be
// visible the moment the first category existed anywhere in the tenant.
func TestUngroupedRowsStayAtTopLevel(t *testing.T) {
	in := items("w1", "w2")
	out := GroupItemsByCategory(in, func(id string) (string, string) {
		if id == "w1" {
			return "catA", "Alpha"
		}
		return "", ""
	})
	if len(out) != 3 {
		t.Fatalf("want parent + member + ungrouped, got %d: %v", len(out), titles(out))
	}
	last := out[len(out)-1]
	if last.ID != "w2" {
		t.Fatalf("the ungrouped row must be last and unchanged, got %+v", last)
	}
	if last.Parent != "" || last.Depth != 0 {
		t.Fatalf("an ungrouped row must not be nested, got %+v", last)
	}
}

// TestGroupOrderFollowsFirstAppearance: the group order is the order the operator's items arrive in —
// stable, and derived from the list they are already reading rather than from a separate sort.
func TestGroupOrderFollowsFirstAppearance(t *testing.T) {
	in := items("w1", "w2", "w3")
	group := map[string]string{"w1": "catB", "w2": "catA", "w3": "catB"}
	out := GroupItemsByCategory(in, func(id string) (string, string) {
		if g, ok := group[id]; ok {
			return g, map[string]string{"catA": "Alpha", "catB": "Beta"}[g]
		}
		return "", ""
	})
	var parents []string
	for _, it := range out {
		if IsGroupRow(it.ID) {
			parents = append(parents, it.Title)
		}
	}
	if len(parents) != 2 || parents[0] != "Beta" || parents[1] != "Alpha" {
		t.Fatalf("groups must appear in first-appearance order, got %v", parents)
	}
}

// TestGroupRowIDsCannotBeMistakenForEntities: a category row carries a SYNTHETIC id, and every write
// chord has to be able to tell. A ULID cannot contain a NUL, so the prefix is unambiguous.
func TestGroupRowIDsCannotBeMistakenForEntities(t *testing.T) {
	if !IsGroupRow(GroupRowID("cat-1")) {
		t.Fatal("a category row's id must be recognized")
	}
	for _, real := range []string{"", "01KYQXQ95C2BFGDT1AFXFX5875", "cat-1", "grp:cat-1"} {
		if IsGroupRow(real) {
			t.Fatalf("%q is an entity id and must NOT read as a category row", real)
		}
	}
}
