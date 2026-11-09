package db

// ephemeral_scope.go — the ONE definition of the ephemeral visibility gate, shared by every table
// that can hold a machine-managed transient record (Ask Orchicon Quick Work): work_items, workers.

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

// ephemeralPredicateOn is ephemeralPredicate for a query that joins and must qualify the column,
// e.g. ephemeralPredicateOn("w", scope) → " AND NOT w.ephemeral".
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
