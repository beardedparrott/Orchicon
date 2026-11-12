package db

// ephemeral_scope_test.go — the gate that keeps machine-managed transients (Ask Orchicon Quick
// Work) out of every human-facing view.

import (
	"strings"
	"testing"
)

// THE DEFAULT HIDES.
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

// THE FILTER'S ZERO VALUE IS THE HIDING VALUE.
func TestListWorkItemsFilterZeroValueHidesEphemeral(t *testing.T) {
	if got := ephemeralPredicate(ListWorkItemsFilter{}.EphemeralScope); got != " AND NOT ephemeral" {
		t.Fatalf("the zero-value EphemeralScope produced %q, not the hiding predicate — a caller that omits the "+
			"field would leak machine-managed rows into a human view", got)
	}
}

// THE COLUMN LIST AND THE SCAN POINTERS MUST STAY IN STEP.
func TestWorkItemSelectColsMatchesScanPointers(t *testing.T) {
	cols := strings.Split(WorkItemSelectCols, ",")
	ptrs := WorkItemScanPtrs(&WorkItemRow{})
	if len(cols) != len(ptrs) {
		t.Fatalf("WorkItemSelectCols has %d columns but WorkItemScanPtrs returns %d pointers — every column "+
			"after the mismatch reads into the wrong field", len(cols), len(ptrs))
	}
	// And the new column must be present, or the gate would filter on a column that is never loaded
	// (and the INSERT/RETURNING round-trip would drop the flag before the row ever reaches a list).
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
