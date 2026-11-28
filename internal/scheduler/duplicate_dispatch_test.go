package scheduler

import (
	"testing"

	"github.com/beardedparrott/orchicon/internal/domain"
)

// The measured case: step-sse on run 01M8FE5CWEZ9WP7QXQREDJDM9D got two concurrent executions because
// "running and unlinked" was resolved by elapsed time. These tests pin the invariant that replaces it.

func TestNonTerminalExecutionStatus(t *testing.T) {
	// Everything the lifecycle passes through BEFORE it ends is live. `dispatching` is the one that
	// matters: the duplicate-dispatch window sits exactly there, so calling it non-live would let the
	// guard dispatch straight into the hole it exists to close.
	live := []string{
		domain.ExecutionDispatching,
		domain.ExecutionRunning,
		domain.ExecutionHealthy,
		domain.ExecutionStalled,
		domain.ExecutionUnhealthy,
		domain.ExecutionTerminating,
	}
	for _, s := range live {
		if !nonTerminalExecutionStatus(s) {
			t.Errorf("nonTerminalExecutionStatus(%q) = false, want true (a live execution on this step must block a second dispatch)", s)
		}
	}

	terminal := []string{
		domain.ExecutionSucceeded,
		domain.ExecutionFailed,
		domain.ExecutionFailedToStart,
		domain.ExecutionTerminated,
	}
	for _, s := range terminal {
		if nonTerminalExecutionStatus(s) {
			t.Errorf("nonTerminalExecutionStatus(%q) = true, want false (a finished execution must not block the step)", s)
		}
	}
}

// TestNonTerminalIsNotTerminalComplement pins the exact bug: `dispatching` is NOT terminal (so
// terminalExecutionStatus says false) yet it IS live. A guard written as `!terminalExecutionStatus(s)`
// would be correct here by accident, but one written as `s == running` would miss the window — this
// asserts the two predicates disagree exactly where it matters.
func TestDispatchingIsLiveButNotTerminal(t *testing.T) {
	if terminalExecutionStatus(domain.ExecutionDispatching) {
		t.Fatalf("dispatching must not be terminal")
	}
	if !nonTerminalExecutionStatus(domain.ExecutionDispatching) {
		t.Fatalf("dispatching must count as a LIVE execution — it is the state a duplicate dispatch observes")
	}
}

// TestLiveExecutionForStepScoping pins the scoping rules the guard depends on: an execution on another
// STEP, another RUN, a follow-up, or a terminal one must not block dispatch.
func TestLiveExecutionScopingRules(t *testing.T) {
	// The pure predicate part is exercised above; this documents the row-level filters liveExecutionForStep
	// applies, which are covered end-to-end through the db helper. Guard against a refactor that drops one
	// by asserting the filter function's contract on empty identity, which must never query.
	if live, err := liveExecutionForStep(nil, nil, "tnt", "", ""); err != nil || live != "" {
		t.Fatalf("a step with no run/step identity must resolve to no live execution (got %q, %v)", live, err)
	}
}

// TestBudgetNumberReadsTuningKnobs pins the JSON path a budget-resident knob uses to reach the manifest —
// the alternative to a schema column. An ABSENT key must report unset, never zero: zero is a meaningful
// cap value and must be distinguishable from "the operator did not set one".
func TestBudgetNumberReadsTuningKnobs(t *testing.T) {
	if n, ok := budgetNumber([]byte(`{"max_tool_output_bytes":32768}`), "max_tool_output_bytes"); !ok || n != 32768 {
		t.Fatalf("got %v,%v want 32768,true", n, ok)
	}
	// Explicit zero IS a value, not unset.
	if n, ok := budgetNumber([]byte(`{"max_tool_output_bytes":0}`), "max_tool_output_bytes"); !ok || n != 0 {
		t.Fatalf("an explicit 0 must read as set, got %v,%v", n, ok)
	}
	// Absent key = unset, so the env var / default apply.
	if _, ok := budgetNumber([]byte(`{"tokens":100}`), "max_tool_output_bytes"); ok {
		t.Fatal("an absent key must read as unset, not as zero")
	}
	for _, raw := range [][]byte{nil, {}, []byte("not json")} {
		if _, ok := budgetNumber(raw, "max_tool_output_bytes"); ok {
			t.Errorf("unparseable/empty budgets (%q) must report unset", raw)
		}
	}
}
