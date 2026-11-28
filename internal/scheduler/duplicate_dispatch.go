package scheduler

import (
	"context"
	"encoding/json"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/jackc/pgx/v5"
)

// duplicate_dispatch.go — the single-live-execution invariant for a task step.
//
// THE DEFECT THIS CLOSES. Two executions ran concurrently on workflow step `step-sse` of run
// 01M8FE5CWEZ9WP7QXQREDJDM9D:
//
//	01M8FE6PFRQE6XRPXS355XRG9M   15:22:25 → 16:29:57   33.5M tokens
//	01M8FE85YG3T4WHG7SPEGV133C   15:23:14 → 15:59:45    3.7M tokens
//
// They overlapped for 66 MINUTES on the same step, in the same worktree
// (.orchicon-worktrees/01M8FE5CWEZ9WP7QXQREDJDM9D) and on the same branch. Two sessions editing one
// worktree is a correctness hazard independent of any stall: each one's checkpoint commits land on the
// other's HEAD, and the "checkpoint: work in progress preserved" commit that was supposed to save the
// first worker's work was made by whichever process happened to run git.
//
// HOW THE GAP OPENS. Dispatch is TWO WRITES IN TWO TRANSACTIONS. dispatchStep marks the step run
// `running` and sets `worker_execution_id = ""` (workflow_reconciler.go), committing that; only later,
// in the TaskReconciler's own transaction, is the real id linked (reconciler.go). For the window
// between them this invariant is UNKNOWABLE from the step run alone: "running and unlinked" is
// indistinguishable from "dispatch lost the execution". pollTaskStep resolves that ambiguity with a
// 15-second timer (dispatchLinkGrace) and, past it, returns the step to `ready` — and dispatch happily
// creates a second execution.
//
// WHY THE FIX IS NOT A LONGER GRACE. A bigger number only narrows the hole and silently re-dispatches
// later; the ambiguity is still being resolved by a stopwatch. The EXECUTION ROW is the thing that
// actually knows, so the invariant is asked of the executions table instead: a step with a
// non-terminal execution on it is already being worked, full stop, however long the link takes.

// nonTerminalExecutionStatus reports whether an execution on this status is STILL WORKING.
//
// This is terminalExecutionStatus' complement and it is derived from the lifecycle documented in
// internal/domain/domain.go:
//
//	dispatching → running → healthy|stalled|unhealthy → terminating → succeeded|failed
//
// Anything in the left/middle part of that chain is live. In particular `dispatching` — which
// terminalExecutionStatus does NOT call terminal — is emphatically live: it is exactly the state a
// duplicate dispatch would observe, because the row is created as `dispatching` and only advanced once
// the adapter starts. A guard that used terminalExecutionStatus as "live" would treat the very window
// it exists to protect as "not live" and dispatch straight into it.
func nonTerminalExecutionStatus(status string) bool {
	switch status {
	case domain.ExecutionSucceeded, domain.ExecutionFailed, domain.ExecutionFailedToStart,
		domain.ExecutionTerminated:
		return false
	}
	return true
}

// liveExecutionForStep returns the id of the non-terminal execution already running on this workflow
// step, or "" when the step is free to dispatch.
//
// It is scoped by workflow_run_id + workflow_step_id, which is the step's own identity — not by task,
// because a retry/recovery re-dispatch deliberately runs on a FRESH ticket id, and a task-scoped check
// would miss exactly that case.
func liveExecutionForStep(ctx context.Context, tx pgx.Tx, tenantID, runID, stepID string) (string, error) {
	if runID == "" || stepID == "" {
		return "", nil
	}
	execs, err := db.ListExecutions(ctx, tx, db.ListExecutionsFilter{
		TenantID:      tenantID,
		WorkflowRunID: runID,
		PageSize:      100,
		SortBy:        "created_at",
		SortOrder:     "desc",
	})
	if err != nil {
		return "", err
	}
	for _, e := range execs {
		if e.WorkflowStepID != stepID {
			continue
		}
		// A FOLLOW-UP is a deliberate second session on a finished execution's context, never a
		// concurrent worker on the step.
		if e.IsFollowUp {
			continue
		}
		// The step run names its own execution; when it does, that row is the one that matters and any
		// OLDER terminal sibling must not block it.
		if nonTerminalExecutionStatus(e.Status) {
			return e.ID, nil
		}
	}
	return "", nil
}

// budgetNumber reads one numeric key out of a merged budget document.
//
// It exists so a knob that lives in the budget JSON can reach the manifest without a column: the budget
// document is a flat bag of settings (tokens, cost_usd, tool_call_count, wall_clock_seconds,
// compact_max_turns, ...) and a per-tool-result cap belongs with those, not in a table of its own.
// An absent or unparseable key reports ok=false, which callers read as "unset" — never as zero.
func budgetNumber(budgets []byte, key string) (float64, bool) {
	if len(budgets) == 0 {
		return 0, false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(budgets, &m); err != nil {
		return 0, false
	}
	raw, ok := m[key]
	if !ok {
		return 0, false
	}
	var n float64
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, false
	}
	return n, true
}
