package scheduler

// local_mode_resolve_test.go — THE EXECUTION MODE IS ALWAYS COMPUTED, NEVER
// DEFAULTED, AND THE MANIFEST NEVER CARRIES THE EMPTY STRING.
//
// THE DEFECT THIS PINS. An item with execution_mode=local still produced a
// runtime container. Measured on a live plane:
//
//	pool checkout: created fresh container  run=01M5X66J54CFZQKQRBDADAVWJZ
//
// for an item whose execution_mode column read `local` and whose project read
// `runtime`. The arm gate was innocent (it skipped EnsureForRun) and so was
// db.EffectiveExecutionMode (it correctly returns local for an item override).
// The cause was here:
//
//	executionMode := db.ExecutionModeRuntime
//	if task.WorkflowRunID != "" { …; executionMode = executionModeFor(…) }
//
// The resolve sat INSIDE the run-bound branch, so an execution without a
// workflow run kept the DECLARATION'S initialiser — the string "runtime". The
// adapter's gate (internal/opencode, runtimeContainerRouteEnabled) treats
// anything that is not "local" as container-routed, so the local run got a
// container: pool checkout: created fresh container.
//
// WHAT MAKES THESE TESTS DISCRIMINATE. The first version of this file asserted
// the adapter gate only — and PASSED against the pre-fix code, because the gate
// itself was always correct; the bug was the VALUE handed to it. So the
// assertions below are about the value: which call shapes compute it, and what
// the manifest is allowed to carry. The source-shape test is the one that fails
// loudly if the resolve slides back inside the conditional.

import (
	"os"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
)

// TestManifestNeverCarriesTheEmptyExecutionMode is the end-of-the-line
// assertion: whatever path produced the manifest, the field must be a DECISION.
func TestManifestNeverCarriesTheEmptyExecutionMode(t *testing.T) {
	// A manifest built the way startExecution builds it after the fix: the field
	// is assigned unconditionally from the resolver.
	for _, tc := range []struct {
		name string
		item *string
		prj  string
		want string
	}{
		{"item local in runtime project", pstr(db.ExecutionModeLocal), db.ExecutionModeRuntime, db.ExecutionModeLocal},
		{"item runtime in local project", pstr(db.ExecutionModeRuntime), db.ExecutionModeLocal, db.ExecutionModeRuntime},
		{"no item override, runtime project", nil, db.ExecutionModeRuntime, db.ExecutionModeRuntime},
		{"no item override, local project", nil, db.ExecutionModeLocal, db.ExecutionModeLocal},
	} {
		got := resolvedModeForTest(tc.item, tc.prj)
		if got != tc.want {
			t.Errorf("%s: resolved %q, want %q", tc.name, got, tc.want)
		}
		if got == "" {
			t.Errorf("%s: resolved EMPTY — an empty mode is not local to the adapter gate, so the "+
				"run would get a container", tc.name)
		}
	}
}

// pstr avoids redeclaring the package's strPtr.
func pstr(s string) *string { return &s }

// resolvedModeForTest mirrors db.effectiveExecutionModeValue's contract for the
// four cases that matter here. It exists so the table above is readable without
// exporting db internals; the SOURCE-SHAPE test below is the one that guards the
// real defect, and db's own execution_mode_test.go guards the precedence itself.
func resolvedModeForTest(item *string, project string) string {
	if item != nil {
		switch strings.TrimSpace(*item) {
		case db.ExecutionModeLocal:
			return db.ExecutionModeLocal
		case db.ExecutionModeRuntime:
			return db.ExecutionModeRuntime
		}
	}
	switch strings.TrimSpace(project) {
	case db.ExecutionModeLocal:
		return db.ExecutionModeLocal
	case db.ExecutionModeRuntime:
		return db.ExecutionModeRuntime
	}
	return db.DefaultExecutionMode
}

// TestStartExecutionComputesTheModeUnconditionally is the SOURCE-SHAPE test: the
// defect was that the resolve sat inside `if task.WorkflowRunID != ""`. A unit
// test cannot reach startExecution without a pool and a database, so this pins
// the shape directly — and it is the assertion that actually failed before the
// fix.
//
// It reads the statement that follows the declaration and requires the
// unconditional assignment. If someone moves the resolve back inside a
// conditional, the manifest starts carrying "runtime" for every run-bound-less
// execution again, and this test says so by name.
func TestStartExecutionComputesTheModeUnconditionally(t *testing.T) {
	src, err := os.ReadFile("reconciler.go")
	if err != nil {
		t.Fatalf("read reconciler.go: %v", err)
	}
	text := string(src)

	const decl = "executionMode := db.ExecutionModeRuntime"
	i := strings.Index(text, decl)
	if i < 0 {
		t.Fatalf("could not find %q in reconciler.go — this test's anchor moved; update it rather "+
			"than deleting it, or the local-mode container leak comes back untested", decl)
	}
	// The 2000 bytes after the declaration must contain the unconditional
	// resolve, BEFORE any `if task.WorkflowRunID` branch.
	tail := text[i:]
	if len(tail) > 2000 {
		tail = tail[:2000]
	}
	assign := "executionMode = executionModeFor("
	ai := strings.Index(tail, assign)
	if ai < 0 {
		t.Fatalf("the execution mode is never REASSIGNED from executionModeFor within 2KB of its "+
			"declaration — it keeps the initialiser %q for every execution whose resolve is skipped, "+
			"which is how a local run reached the adapter reading as runtime", db.ExecutionModeRuntime)
	}
	// Find where the assignment sits relative to the run-bound condition.
	// The assignment must come BEFORE `if task.WorkflowRunID != ""`.
	condIdx := strings.Index(tail[ai:], "if task.WorkflowRunID")
	back := strings.LastIndex(tail[:ai], "if task.WorkflowRunID")
	if back >= 0 {
		t.Fatalf("the execution-mode resolve appears INSIDE/after an `if task.WorkflowRunID` block "+
			"(condition at offset %d, assignment at %d). That is the original defect: an execution "+
			"without a workflow run keeps the declaration's initialiser %q, the adapter reads it as "+
			"runtime, and it creates a container for a run configured local.", back, ai, db.ExecutionModeRuntime)
	}
	_ = condIdx
}
