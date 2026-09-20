package db

// ephemeral_scope_test.go — the gate that keeps machine-managed transients
// (Ask Orchicon Quick Work) out of every human-facing view.
//
// The operator's requirement: "I don't want a ton of invisible records out
// there." The answer is a real hard delete, and this file pins the half of the
// contract that lives in the read path — that an item marked ephemeral is
// hidden from every list that has not explicitly asked for it.
//
// These are pure tests (no pool), which is deliberate: the DEFAULT is a safety
// property, and a safety property that can only be checked against a live
// database is a safety property that will be checked rarely.

import (
	"strings"
	"testing"
)

// THE DEFAULT HIDES. Every human-facing surface — board, tree, list,
// sequence, workflows, the dependency graph and every derived count — rides on
// ListWorkItems, so an unset or unrecognised scope has to hide ephemeral rows
// rather than reveal them. If this test ever needs updating to make a new
// surface "work", the new surface wanted "include" and should say so.
func TestEphemeralPredicateHidesByDefault(t *testing.T) {
	cases := []struct {
		scope string
		want  string
		why   string
	}{
		{"", " AND NOT ephemeral", "the zero value — a caller that never thought about ephemeral items"},
		{"exclude", " AND NOT ephemeral", "the explicit spelling of the default"},
		{"only", " AND ephemeral", "the Quick Work agent inspecting its own items"},
		{"include", "", "both — the agent managing its own alongside real work"},
		{"yes", " AND NOT ephemeral", "an unrecognised value must not leak transients"},
		{"true", " AND NOT ephemeral", "nor a boolean spelled as a word"},
		{"INCLUDE", " AND NOT ephemeral", "the scope is case-sensitive on purpose: a near-miss is a typo, and a typo must fail closed"},
		{"include_ephemeral", " AND NOT ephemeral", "nor the TOOL's parameter name leaking into the db scope"},
	}
	for _, c := range cases {
		if got := ephemeralPredicate(c.scope); got != c.want {
			t.Errorf("ephemeralPredicate(%q) = %q, want %q (%s)", c.scope, got, c.want, c.why)
		}
	}
}

// THE FILTER'S ZERO VALUE IS THE HIDING VALUE. This is asserted separately
// from the predicate table because it is the property that actually protects a
// future caller: `db.ListWorkItemsFilter{ProjectID: p}` — the shape every
// existing call site uses — must hide ephemeral rows without the author doing
// anything at all.
func TestListWorkItemsFilterZeroValueHidesEphemeral(t *testing.T) {
	if got := ephemeralPredicate(ListWorkItemsFilter{}.EphemeralScope); got != " AND NOT ephemeral" {
		t.Fatalf("the zero-value EphemeralScope produced %q, not the hiding predicate — a caller that omits the "+
			"field would leak machine-managed rows into a human view", got)
	}
}

// THE COLUMN LIST AND THE SCAN POINTERS MUST STAY IN STEP.
//
// They are positional, they are maintained by hand, and the failure mode when
// they drift is nasty and silent: every column after the insertion point is
// read into the wrong field. The `ephemeral` column was added to both in this
// change, which is exactly when such a drift is introduced.
func TestWorkItemSelectColsMatchesScanPointers(t *testing.T) {
	cols := strings.Split(WorkItemSelectCols, ",")
	ptrs := WorkItemScanPtrs(&WorkItemRow{})
	if len(cols) != len(ptrs) {
		t.Fatalf("WorkItemSelectCols has %d columns but WorkItemScanPtrs returns %d pointers — every column "+
			"after the mismatch reads into the wrong field", len(cols), len(ptrs))
	}
	// And the new column must be present, or the gate would filter on a column
	// that is never loaded (and the INSERT/RETURNING round-trip would drop the
	// flag before the row ever reaches a list).
	found := false
	for _, c := range cols {
		if strings.TrimSpace(c) == "ephemeral" {
			found = true
			break
		}
	}
	if !found {
		t.Error("WorkItemSelectCols does not select `ephemeral`, so a created item comes back with the flag " +
			"cleared and the gate filters on a value it never loaded")
	}
}
