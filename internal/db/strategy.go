package db

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// DefaultGitStrategy is the effective git strategy used when neither the workflow nor the project
// specifies one.
const DefaultGitStrategy = "local"

// GitStrategyValues is the closed set a project may be set to.
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
func RequiresRepository(strategy string) bool {
	return strategy == "local" || strategy == "pr"
}

// EffectiveGitStrategy resolves the effective git strategy for a workflow run: workflow wins, else
// project, else DefaultGitStrategy.
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

// effectiveGitStrategyValue is the pure precedence decision behind EffectiveGitStrategy: an
// explicit workflow strategy wins, else an explicit project strategy, else the default ("local").
func effectiveGitStrategyValue(workflowStrategy, projectStrategy string) string {
	if workflowStrategy != "" {
		return workflowStrategy
	}
	if projectStrategy != "" {
		return projectStrategy
	}
	return DefaultGitStrategy
}
