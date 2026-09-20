package askorchicon

// tool_ephemeral_test.go — the ephemeral (Quick Work) layer at the TOOL
// boundary.
//
// The operator's requirement for Quick Work: "a flag is fine, but hard-delete
// the work item once complete (no invisible records)". So a transient item is
// marked `ephemeral`, kept out of every human view while it lives, and really
// removed when its job ends. The hard delete itself is pinned in
// hard_delete_tool_test.go; this file pins the flag: that it is advertised,
// that it defaults to hidden on the read side, and that the one placement it
// cannot honour (a child) is refused before any work happens.
//
// These tests are pure — no pool, no database — because the guard runs before
// the transaction opens. That ordering is itself part of what is asserted.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// THE GUARD REFUSES THE ONE SHAPE THE HIDING CONTRACT CANNOT HONOUR, and
// permits every other shape.
//
// A child is refused because it would sit in a contradiction: invisible in the
// list (which filters ephemeral) yet rendered inside its parent's tree (where
// ListDirectChildren deliberately does not filter, because the sequence engine
// needs every child). Hard-deleting it would then remove a row from a tree
// that is still displaying it.
func TestEphemeralPlacementRefusesAChildAndPermitsTopLevel(t *testing.T) {
	cases := []struct {
		name      string
		ephemeral bool
		parentID  string
		wantErr   bool
		why       string
	}{
		{"ephemeral child", true, "01PARENT", true,
			"invisible in the list but visible in the parent's tree, and deleting it rips a row out of a live tree"},
		{"ephemeral top-level", true, "", false,
			"the shape Quick Work actually creates"},
		{"ephemeral top-level, whitespace parent", true, "   ", false,
			"whitespace is not a parent — the same trim the rest of the create path applies"},
		{"ordinary child", false, "01PARENT", false,
			"the guard must not touch real hierarchy; only ephemeral placement is special"},
		{"ordinary top-level", false, "", false,
			"the overwhelmingly common case"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateEphemeralPlacement(c.ephemeral, c.parentID)
			if c.wantErr && err == nil {
				t.Fatalf("validateEphemeralPlacement(%v, %q) = nil, want an error: %s",
					c.ephemeral, c.parentID, c.why)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("validateEphemeralPlacement(%v, %q) = %v, want nil: %s",
					c.ephemeral, c.parentID, err, c.why)
			}
			if c.wantErr && !strings.Contains(err.Error(), "cannot have a parent") {
				t.Errorf("the refusal does not say what is wrong with the input: %v", err)
			}
		})
	}
}

// THE CHECK RUNS BEFORE THE TRANSACTION.
//
// This is asserted by passing a NIL POOL: if the guard had been placed after
// `pool.BeginTenantTx`, this call would panic rather than return an error.
// Ordering matters beyond tidiness — a rejected request should not have opened
// a tenant transaction first.
func TestCreateWorkItemRefusesAnEphemeralChildBeforeTouchingThePool(t *testing.T) {
	args := json.RawMessage(`{"title":"Ephemeral child","project_id":"01PROJ","parent_id":"01PARENT","ephemeral":true}`)
	_, err := toolCreateWorkItem(context.Background(), nil, args)
	if err == nil {
		t.Fatal("create_work_item accepted an ephemeral item with a parent_id — it would be hidden from the " +
			"list but rendered inside its parent's tree, and hard-deleting it would tear a row out of a live tree")
	}
	if !strings.Contains(err.Error(), "cannot have a parent") {
		t.Fatalf("expected the placement refusal, got: %v", err)
	}
}

// THE SCOPE OPT-IN IS EXPLICIT, NEVER A SIDE EFFECT.
func TestEphemeralScopeOptInIsExplicit(t *testing.T) {
	if got := ephemeralScopeFor(false); got != "exclude" {
		t.Errorf("ephemeralScopeFor(false) = %q, want \"exclude\" — omitting the flag must HIDE transients", got)
	}
	if got := ephemeralScopeFor(true); got != "include" {
		t.Errorf("ephemeralScopeFor(true) = %q, want \"include\"", got)
	}
	// The mapping must produce values the db gate understands; "exclude" is the
	// spelling that hides, and it must not silently be a no-op string.
	if ephemeralScopeFor(false) == "" {
		t.Error("the default scope is the empty string, which the gate treats as hiding — but a future gate " +
			"read might treat empty as \"no filter\". Spell the intent out.")
	}
}

// THE CREATE TOOL ADVERTISES THE FLAG AND SAYS HOW TO END ITS LIFE.
func TestCreateWorkItemToolAdvertisesEphemeral(t *testing.T) {
	r := NewToolRegistry(nil, nil, nil)
	td, ok := r.Get("create_work_item")
	if !ok {
		t.Fatal("create_work_item is not registered")
	}
	prop, ok := td.Properties["ephemeral"]
	if !ok {
		t.Fatal("create_work_item has no `ephemeral` property, so Quick Work cannot mark a transient item and " +
			"the hard delete would be operating on rows indistinguishable from real work")
	}
	if prop.Type != "boolean" {
		t.Errorf("the `ephemeral` property is %q, want boolean", prop.Type)
	}
	desc := strings.ToLower(td.Description)
	for _, want := range []string{"ephemeral", "hidden", "hard-delete", "top-level"} {
		if !strings.Contains(desc, want) {
			t.Errorf("the create description does not mention %q, so the model would not know the item is "+
				"hidden or how to dispose of it: %q", want, td.Description)
		}
	}
	// The critical instruction: a transient item must be REMOVED, not cancelled.
	// A cancel is precisely the invisible record the operator objected to.
	if !strings.Contains(desc, "hard_delete_work_item") {
		t.Error("the create description does not name hard_delete_work_item, so an agent finishing a Quick " +
			"Work job would cancel the item and leave behind exactly the invisible record this exists to avoid")
	}
}

// THE LIST TOOL HIDES BY DEFAULT AND SAYS SO.
func TestListWorkItemsToolHidesEphemeralByDefault(t *testing.T) {
	r := NewToolRegistry(nil, nil, nil)
	td, ok := r.Get("list_work_items")
	if !ok {
		t.Fatal("list_work_items is not registered")
	}
	prop, ok := td.Properties["include_ephemeral"]
	if !ok {
		t.Fatal("list_work_items has no `include_ephemeral` property, so the Quick Work agent has no way to " +
			"enumerate the items it created")
	}
	if prop.Type != "boolean" {
		t.Errorf("`include_ephemeral` is %q, want boolean", prop.Type)
	}
	// The default must be documented on a mutating-adjacent read: a tool that
	// silently omits rows is worse than one that documents the omission.
	if !strings.Contains(strings.ToLower(td.Description), "excluded by default") {
		t.Errorf("the list description does not state that ephemeral items are excluded by default, so a "+
			"missing row would read as a bug: %q", td.Description)
	}
	// `ephemeral` is NOT a required field — creating ordinary work must stay a
	// two-argument call.
	for _, req := range td.Required {
		if req == "ephemeral" {
			t.Error("`ephemeral` is required, which would force every ordinary create to state it")
		}
	}
}
