package askorchicon

import (
	"testing"
)

// These pin the defects that made an agent misread usage.

func rows(n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = map[string]any{"ID": i}
	}
	return out
}

// TestCompactListDistinguishesExactFitFromTruncated is the regression for the misleading note. The old
// builder printed "1 more item(s) not shown" whenever len(rows) > listCap — and every full page is
// listCap+1 — so a 26-row filter and a 60-row filter produced IDENTICAL output (count 25, same note).
// An unfiltered and a filtered call were therefore indistinguishable, which is exactly how a filter that
// was working looks broken and a filter that is ignored looks fine.
func TestCompactListDistinguishesExactFitFromTruncated(t *testing.T) {
	// Exactly listCap matched: NO probe row, so there is nothing more to show.
	exact := newCompactList(rows(listCap), "")
	if exact.Truncated {
		t.Errorf("exactly %d rows is not truncated", listCap)
	}
	if exact.Note != "" {
		t.Errorf("exactly %d rows must not claim more items exist, got note %q", listCap, exact.Note)
	}
	if exact.Count != listCap {
		t.Errorf("count = %d, want %d", exact.Count, listCap)
	}

	// A probe row present: there IS more, and the count is exact.
	more := newCompactList(rows(listCap+1), "")
	if !more.Truncated {
		t.Fatalf("a returned probe row means there is more to fetch")
	}
	if more.Count != listCap {
		t.Errorf("count = %d, want %d (the page size)", more.Count, listCap)
	}
	if more.Note == "" {
		t.Errorf("a truncated list must say so")
	}
	// The two cases must NOT be identical — that was the bug.
	if exact.Note == more.Note && exact.Truncated == more.Truncated && exact.Count == more.Count {
		t.Fatalf("an exact-fit page and a truncated page rendered identically; a reader cannot tell them apart")
	}
}

// TestCompactUsageCarriesAttribution pins that a row can be tied to the thing it paid for. Without
// these keys an agent handed a page of the tenant's recent usage could not attribute any row to an
// execution, so a per-execution question got answered from unrelated rows.
func TestCompactUsageCarriesAttribution(t *testing.T) {
	rec := compactUsage(dbUsageRowForTest())
	for _, k := range []string{"ExecutionID", "TaskID", "WorkflowRunID", "SessionID"} {
		if _, ok := rec[k]; !ok {
			t.Errorf("compactUsage must carry %q so a row can be attributed to an execution/task/run/session", k)
		}
	}
}
