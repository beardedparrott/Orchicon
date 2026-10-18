package db

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"
)

// DefaultExecutionMode is the TENANT-LEVEL default: the value used when neither
// the work item nor its project states a mode.
//
// It is a constant, not a column, because no tenant-level execution_mode exists
// today (see db/schema.hcl: the column lives on `projects` and, as of the
// work-item override, on `work_items`; `tenants` has none). Keeping the third
// level as one named constant means the local-default task flips the platform
// default in exactly one place, and every caller of EffectiveExecutionMode
// follows automatically.
const DefaultExecutionMode = ExecutionModeRuntime

// EffectiveExecutionMode resolves where a work item's executions actually run:
//
//	ITEM override -> PROJECT execution_mode -> DefaultExecutionMode (tenant default)
//
// It is the SINGLE SOURCE OF TRUTH for that decision, mirroring
// EffectiveGitStrategy (internal/db/strategy.go) for the same reason: the
// scheduler's arm gate, the local-mode DSN fence, the adapter's container
// predicate, and the honest local prompt block must never disagree about
// whether a run is `runtime` or `local`.
//
// itemMode nil means "inherit" (the column is nullable by design and NULL is the
// only inherit representation). A project row that cannot be read degrades to
// runtime, never to local: a read failure must never turn a runtime item into
// in-process host exec. The mode is total — it always returns "runtime" or
// "local", so there is no "found" flag to thread through callers.
func EffectiveExecutionMode(ctx context.Context, tx pgx.Tx, tenantID string, itemMode *string, projectID string) string {
	item := ""
	if itemMode != nil {
		item = strings.TrimSpace(*itemMode)
	}
	project := ""
	if strings.TrimSpace(projectID) != "" {
		if p, err := GetProject(ctx, tx, tenantID, projectID); err == nil {
			project = p.ExecutionMode
		}
	}
	return effectiveExecutionModeValue(item, project)
}

// effectiveExecutionModeValue is the pure precedence decision behind
// EffectiveExecutionMode: an explicit ITEM mode wins over the project, an
// explicit project mode wins over the tenant default, and anything unreadable
// or empty falls through to DefaultExecutionMode (runtime). An item "local"
// inside a "runtime" project stays local — that is the opt-out the lifecycle
// invariant depends on — and an item "runtime" inside a "local" project stays
// runtime, i.e. the item-level opt-IN provisions a container.
//
// Separated from the DB-touching resolver so the precedence is unit-testable
// with no database (internal/db/execution_mode_test.go), exactly like
// effectiveGitStrategyValue.
func effectiveExecutionModeValue(itemMode, projectMode string) string {
	switch strings.TrimSpace(itemMode) {
	case ExecutionModeLocal:
		return ExecutionModeLocal
	case ExecutionModeRuntime:
		return ExecutionModeRuntime
	}
	if strings.TrimSpace(projectMode) == ExecutionModeLocal {
		return ExecutionModeLocal
	}
	return DefaultExecutionMode
}

// EphemeralExecutionMode is the execution mode for machine-managed ephemeral
// items (Ask Orchicon Quick Work).
//
// It is STRICTLY local, and it is a decision, not a fallback: an ephemeral item
// is created by a machine and hidden from every human view, so nobody can notice
// a LOUD "runtime daemon not reachable" failure on it and flip the mode. The
// install path ships no runtime image at all (cmd/orchicon/install.go), so the
// engine-less host is the COMMON case and local is the only mode that always
// works there.
//
// Trade-off, stated so it can be revisited: Quick Work gets no container
// isolation. It is acceptable because the item exists only to carry one sandbox
// conversation on the operator's own machine; it is not acceptable if Quick Work
// ever starts executing untrusted third-party code.
//
// It is STAMPED onto the row at create (db.CreateWorkItem) rather than left
// NULL: the row is hard-deleted with its job, and "which mode did this run in"
// must never be a question about a row that no longer inherits anything
// meaningful. An explicit caller-supplied mode still wins over the stamp.
func EphemeralExecutionMode() string { return ExecutionModeLocal }
