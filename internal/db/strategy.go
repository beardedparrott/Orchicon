package db

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// DefaultGitStrategy is the effective git strategy used when neither the
// workflow nor the project specifies one. `local` means the run's branch is
// pushed to origin and reclaimed by the prune gate; `pr` adds a PR; `none`
// ("ephemeral") is a detached-HEAD run that must never create a branch ref
// locally or remotely.
const DefaultGitStrategy = "local"

// GitStrategyValues is the closed set a project may be set to. It exists so validation has ONE definition:
// the UI's proto enum, the Ask tools' string parameters and any future caller must agree about what a valid
// strategy is, and a strategy that is not in this set must be REFUSED rather than silently defaulted — a
// project whose strategy was quietly rewritten runs differently from the one the operator chose.
var GitStrategyValues = []string{"local", "pr", "none"}

// ParseGitStrategy validates a strategy, returning fallback for the empty string and an error for anything
// outside GitStrategyValues. Modelled on ParseExecutionMode so the two project fields validate alike.
func ParseGitStrategy(v, fallback string) (string, error) {
	v = strings.TrimSpace(strings.ToLower(v))
	if v == "" {
		return fallback, nil
	}
	for _, ok := range GitStrategyValues {
		if v == ok {
			return v, nil
		}
	}
	return "", fmt.Errorf("git_strategy %q is not valid — use one of %s", v, strings.Join(GitStrategyValues, ", "))
}

// RequiresRepository reports whether a strategy expects a git repository on disk.
//
// `local` and `pr` both do: they cut a run branch in a worktree, which is meaningless without a repo. `none`
// deliberately does not — it is the ephemeral, no-branch mode. This is the predicate the run-time check uses to
// decide whether a missing repository is a problem to FIX (initialise it) or simply how the project works.
func RequiresRepository(strategy string) bool {
	return strategy == "local" || strategy == "pr"
}

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
