package db

// ephemeral_scope.go — the ONE definition of the ephemeral visibility gate,
// shared by every table that can hold a machine-managed transient record
// (Ask Orchicon Quick Work): work_items, workers, workflows.
//
// The operator, on Quick Work: "I don't want these showing up inside the
// console." Quick Work creates a worker, a workflow and a work item per job
// and removes them when the job ends; while they exist they must be invisible
// to humans. This file is the read half of that guarantee.

// ephemeralPredicate returns the SQL predicate for an ephemeral scope,
// unqualified — for queries over a single table with no alias.
//
//	""/"exclude" (default) → only non-ephemeral rows — every human view.
//	"only"                 → only ephemeral rows.
//	"include"              → both.
//
// This is a pure function, and the default is a SAFETY property rather than a
// preference: any existing or future caller that has not thought about
// ephemeral records MUST land on "exclude". A new human-facing surface that
// forgets the gate is then a no-op instead of a leak of machine-managed rows
// into the operator's console. Do not make "include" the default to "help" a
// caller — have that caller ask for it.
//
// Note what the default protects and what it must NOT break: the DISPATCH path
// does not come through any of these list queries. ListReadyTasks /
// ListBlockedTasks (execution.go) are separate statements, and worker/workflow
// resolution during dispatch reads by id (GetWorker / GetWorkflow), which are
// deliberately ungated — a run must be able to execute the very records this
// gate hides. Gating a by-id read would make every Quick Work job inert with no
// error anywhere.
func ephemeralPredicate(scope string) string {
	return ephemeralPredicateOn("", scope)
}

// ephemeralPredicateOn is ephemeralPredicate for a query that joins and must
// qualify the column, e.g. ephemeralPredicateOn("w", scope) → " AND NOT w.ephemeral".
//
// The qualifier is passed rather than inferred because "ephemeral" is a real
// column on three tables: an unqualified predicate in a join over workers and
// worker_versions would be ambiguous, and Postgres would reject it — but only
// when the query runs, which is the expensive time to find out.
func ephemeralPredicateOn(qualifier, scope string) string {
	col := "ephemeral"
	if qualifier != "" {
		col = qualifier + ".ephemeral"
	}
	switch scope {
	case "only":
		return " AND " + col
	case "include":
		return ""
	default:
		return " AND NOT " + col
	}
}
