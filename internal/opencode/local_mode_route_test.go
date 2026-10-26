package opencode

// local_mode_route_test.go — A LOCAL RUN MUST NEVER ROUTE TO A CONTAINER.
//
// THE DEFECT THIS PINS. Reported as: an item with execution_mode=local still
// produced a runtime container. Measured on a live plane:
//
//	pool checkout: created fresh container  run=01M5X66J54CFZQKQRBDADAVWJZ
//
// for a run whose item's execution_mode column read `local` and whose project
// read `runtime`. The arm gate was innocent (it correctly skipped EnsureForRun)
// and so was db.EffectiveExecutionMode (it correctly returns local for an item
// override). The cause was in TaskReconciler.startExecution:
//
//	executionMode := db.ExecutionModeRuntime
//	if task.WorkflowRunID != "" { …; executionMode = executionModeFor(…) }
//
// The resolve sat INSIDE the run-bound branch, so an execution without a
// workflow run kept the DECLARATION'S initialiser — the string "runtime". This
// gate reads that field, and an empty value is not "local", so the adapter
// created a container the run was explicitly configured never to use.
//
// WHY THE OLD TEST MISSED IT. TestLocalItemInRuntimeProjectSkipsEnsureForRun
// (internal/scheduler) asserted the resolver and a helper boolean. Both were
// right. It never asked the gate that actually decides container creation, so
// the suite stayed green while the wrong string reached here.
//
// These assertions are on the REAL predicate, not a copy of its logic: a copied
// `x != local` would keep passing after the real gate changed, which is how the
// defect survived in the first place.

import (
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// TestLocalModeNeverRoutesToAContainer is the direct regression.
func TestLocalModeNeverRoutesToAContainer(t *testing.T) {
	m := scheduler.ExecutionManifest{RuntimeWorkflowID: "run-1", ExecutionMode: db.ExecutionModeLocal}
	if runtimeContainerRouteEnabled(true, m) {
		t.Fatal("a LOCAL-mode manifest with a run and a live client routed to a container — this is " +
			"the container a local run was explicitly configured not to use")
	}
}

// TestRuntimeModeStillRoutes pins the other direction, so a fix for local cannot
// make the gate inert and silently take every runtime run off its container.
func TestRuntimeModeStillRoutes(t *testing.T) {
	m := scheduler.ExecutionManifest{RuntimeWorkflowID: "run-1", ExecutionMode: db.ExecutionModeRuntime}
	if !runtimeContainerRouteEnabled(true, m) {
		t.Fatal("a runtime-mode manifest must route to its container")
	}
}

// TestEmptyExecutionModeRoutesToAContainerAndThatIsTheTrap states the zero-value
// behaviour OUT LOUD, because it is the mechanism of the defect and the reason
// startExecution must always COMPUTE the field rather than default it.
//
// It is written as an explicit expectation, not an endorsement: if this ever
// flips to "empty means local", the defensive default in startExecution becomes
// redundant — but nobody should change it without reading the incident above.
func TestEmptyExecutionModeRoutesToAContainerAndThatIsTheTrap(t *testing.T) {
	m := scheduler.ExecutionManifest{RuntimeWorkflowID: "run-1"} // ExecutionMode left empty
	if !runtimeContainerRouteEnabled(true, m) {
		t.Fatal("an EMPTY ExecutionMode no longer routes to a container. That is a behaviour change " +
			"in the fail-open direction this gate was built on: revisit TaskReconciler.startExecution " +
			"(which must still compute the field) and this expectation together")
	}
}

// TestRouteNeedsAClientAndARun keeps the gate's other two clauses honest.
func TestRouteNeedsAClientAndARun(t *testing.T) {
	withRun := scheduler.ExecutionManifest{RuntimeWorkflowID: "run-1", ExecutionMode: db.ExecutionModeRuntime}
	if runtimeContainerRouteEnabled(false, withRun) {
		t.Error("no daemon client must never route to a container")
	}
	noRun := scheduler.ExecutionManifest{ExecutionMode: db.ExecutionModeRuntime}
	if runtimeContainerRouteEnabled(true, noRun) {
		t.Error("an execution with no workflow run must never route to a container")
	}
}
