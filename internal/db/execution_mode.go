package db

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"
)

// DefaultExecutionMode is the TENANT-LEVEL default: the value used when neither the work item nor
// its project states a mode.
const DefaultExecutionMode = ExecutionModeLocal

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
// only inherit representation). A project row that cannot be read — or an
// execution with no project at all — falls through to the TENANT DEFAULT, which
// is local (in-process) since the local-default decision; containers are the
// opt-in. That degradation is bounded by the local-mode DSN fence in
// internal/scheduler, which refuses a live plane DSN and fails closed, so a
// read failure cannot silently reach the live database from in-process. The
// mode is total — it always returns "runtime" or "local", so there is no
// "found" flag to thread through callers.
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

// effectiveExecutionModeValue is the pure precedence decision behind EffectiveExecutionMode: an
// explicit ITEM mode wins over the project, an explicit project mode wins over the tenant default.
func effectiveExecutionModeValue(itemMode, projectMode string) string {
	switch strings.TrimSpace(itemMode) {
	case ExecutionModeLocal:
		return ExecutionModeLocal
	case ExecutionModeRuntime:
		return ExecutionModeRuntime
	}
	// The PROJECT leg returns whatever the project says — BOTH values, not just local.
	switch strings.TrimSpace(projectMode) {
	case ExecutionModeLocal:
		return ExecutionModeLocal
	case ExecutionModeRuntime:
		return ExecutionModeRuntime
	}
	return DefaultExecutionMode
}

// EphemeralExecutionMode is the execution mode for machine-managed ephemeral items (Ask Orchicon
// Quick Work).
func EphemeralExecutionMode() string { return ExecutionModeLocal }
