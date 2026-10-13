package db

// workflow_runs_cursor_test.go — CLAUSE ORDER for ListWorkflowRuns.
//
// The bug this file pins was a clause-order bug, and it was invisible to every
// existing test because they all ran page ONE. Page one sends no cursor, so it
// emitted a perfectly valid statement; only a request carrying a page token
// appended its keyset predicate AFTER the ORDER BY and produced
//
//	WHERE tenant_id = $1 ORDER BY id DESC AND id < $2 LIMIT $3
//
// which PostgreSQL rejects outright (`syntax error at or near "AND"`). So the
// runs view's second page and every client following next_page_token failed while
// page one worked — a broken statement that presents as a UI glitch.
//
// The assertion is therefore about the STRING, not the results: clause order is
// exactly the property a database cannot report on when the query never parses,
// and the cheapest honest guard is to inspect the SQL the builder produces.

import (
	"strconv"
	"strings"
	"testing"
)

// orderByAndPredicate splits a statement at its ORDER BY clause.
func orderByAndPredicate(t *testing.T, sql string) (before, after string) {
	t.Helper()
	i := strings.Index(sql, "ORDER BY")
	if i < 0 {
		t.Fatalf("statement has no ORDER BY at all:\n%s", sql)
	}
	return sql[:i], sql[i:]
}

// assertNoPredicateAfterOrderBy is the core invariant: everything that filters
// rows must be emitted BEFORE the ORDER BY, because after it the clause is no
// longer valid.
func assertNoPredicateAfterOrderBy(t *testing.T, sql string) {
	t.Helper()
	_, after := orderByAndPredicate(t, sql)
	if strings.Contains(after, " AND ") {
		t.Fatalf("a predicate was emitted AFTER the ORDER BY — this statement does not parse:\n%s", sql)
	}
}

func TestListWorkflowRunsCursorPrecedesOrderBy(t *testing.T) {
	cases := []struct {
		name string
		f    ListWorkflowRunsFilter
	}{
		{
			name: "default id sort, desc cursor",
			f: ListWorkflowRunsFilter{
				TenantID: "tnt_dev", PageSize: 10, AfterID: "01ABC",
			},
		},
		{
			name: "id sort ascending",
			f: ListWorkflowRunsFilter{
				TenantID: "tnt_dev", PageSize: 10, AfterID: "01ABC", SortOrder: "asc",
			},
		},
		{
			name: "started_at tuple keyset",
			f: ListWorkflowRunsFilter{
				TenantID: "tnt_dev", PageSize: 10, AfterID: "01ABC", SortBy: "started_at",
			},
		},
		{
			name: "started_at ascending",
			f: ListWorkflowRunsFilter{
				TenantID: "tnt_dev", PageSize: 10, AfterID: "01ABC",
				SortBy: "started_at", SortOrder: "asc",
			},
		},
		{
			name: "every filter at once",
			f: ListWorkflowRunsFilter{
				TenantID: "tnt_dev", WorkflowID: "wf1", ProjectID: "p1",
				WorkItemID: "wi1", Status: "running", PageSize: 5, AfterID: "01ABC",
			},
		},
		{
			name: "first page, no cursor",
			f:    ListWorkflowRunsFilter{TenantID: "tnt_dev", PageSize: 10},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sql, args := listWorkflowRunsQuery(tc.f)
			assertNoPredicateAfterOrderBy(t, sql)

			before, _ := orderByAndPredicate(t, sql)
			if tc.f.AfterID != "" {
				// The cursor must actually BE there — otherwise the assertion
				// above passes vacuously on a statement that dropped pagination.
				if !strings.Contains(before, " AND ") {
					t.Fatalf("a cursor was requested but no keyset predicate was emitted before the ORDER BY:\n%s", sql)
				}
				if !strings.Contains(sql, "$2") {
					t.Fatalf("the cursor parameter is missing:\n%s", sql)
				}
			}

			// The LIMIT placeholder must be the LAST parameter, or the args
			// slice and the statement disagree about which $n is which.
			wantLimit := "$" + strconv.Itoa(len(args))
			if !strings.Contains(sql, "LIMIT "+wantLimit) {
				t.Fatalf("LIMIT is not %s while %d args were produced — the placeholders and args diverge:\n%s",
					wantLimit, len(args), sql)
			}
		})
	}
}

// TestListWorkflowRunsFirstPageIsUnpaginated guards the other direction: a page
// with no cursor must NOT carry a keyset predicate, so the first page stays a
// plain scan. This is what made the bug look like a UI problem — page one was
// correct, which is why nothing noticed page two was not.
func TestListWorkflowRunsFirstPageIsUnpaginated(t *testing.T) {
	sql, _ := listWorkflowRunsQuery(ListWorkflowRunsFilter{TenantID: "tnt_dev", PageSize: 10})
	if strings.Contains(sql, "AND id") {
		t.Fatalf("a cursor-less page emitted a keyset predicate:\n%s", sql)
	}
	if !strings.Contains(sql, "WHERE tenant_id = $1") {
		t.Fatalf("the tenant scope is not the first predicate:\n%s", sql)
	}
}
