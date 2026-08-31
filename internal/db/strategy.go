package db

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// DefaultGitStrategy is the effective git strategy used when neither the
// workflow nor the project specifies one. `local` means the run's branch is
// pushed to origin and reclaimed by the prune gate; `pr` adds a PR; `none`
// ("ephemeral") is a detached-HEAD run that must never create a branch ref
// locally or remotely.
const DefaultGitStrategy = "local"

// EffectiveGitStrategy resolves the effective git strategy for a workflow
// run: workflow wins, else project, else DefaultGitStrategy. It is the
// single source of truth for the strategy so the scheduler's PR gate, the
// worktree reconciler's detached-HEAD decision, and the runtime daemon's
// credential gate never disagree about whether a run is `local`, `pr`, or
// `none`.
//
// The precedence is delegated to effectiveGitStrategyValue, which is bit
// identical to the two pre-existing inline resolution sites (reconciler.go
// skipPRMarkerStamp and workflow_reconciler.go's step-success gate), so
// centralizing it here cannot change the effective strategy for
// `pr`/`local` runs. A `none`-strategy run consistently resolves to "none"
// everywhere, which is the enforcement guarantee.
func EffectiveGitStrategy(ctx context.Context, tx pgx.Tx, tenantID, workflowID, projectID string) string {
	workflowStrategy, projectStrategy := "", ""
	if workflowID != "" {
		if wf, err := GetWorkflow(ctx, tx, tenantID, workflowID); err == nil && wf.GitStrategy != nil {
			workflowStrategy = *wf.GitStrategy
		}
	}
	if projectID != "" {
		if proj, err := GetProject(ctx, tx, tenantID, projectID); err == nil {
			projectStrategy = proj.GitStrategy
		}
	}
	return effectiveGitStrategyValue(workflowStrategy, projectStrategy)
}

// effectiveGitStrategyValue is the pure precedence decision behind
// EffectiveGitStrategy: an explicit workflow strategy wins, else an explicit
// project strategy, else the default ("local"). It is separated so the
// precedence can be unit-tested without a database and proven bit-identical
// to the two pre-existing inline resolution sites.
func effectiveGitStrategyValue(workflowStrategy, projectStrategy string) string {
	if workflowStrategy != "" {
		return workflowStrategy
	}
	if projectStrategy != "" {
		return projectStrategy
	}
	return DefaultGitStrategy
}
