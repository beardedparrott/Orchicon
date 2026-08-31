package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"

	"github.com/beardedparrott/orchicon/internal/config"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/jackc/pgx/v5"
)

// runBackfillPR is a one-shot, idempotent data fix: resolve each git-backed
// terminal workflow run's branch to its real PR URL/state (via `gh`) and write
// them into BOTH the run's run_context JSONB (for the run detail page) and the
// worker_executions.pr_url/pr_state columns (for the work item card and
// execution detail page).
//
// It runs out-of-band (not from the request path) and is safe to re-run: it
// only fills runs that have no pr_url yet, so already-populated runs are
// untouched. Runs whose branch resolves to no PR are left without a PR link.
//
// Usage: orchicon backfill-pr [--dry-run]
func runBackfillPR(args []string, log *slog.Logger) int {
	dryRun := false
	repoOverride := ""
	for _, a := range args {
		switch {
		case a == "--dry-run":
			dryRun = true
		case strings.HasPrefix(a, "--repo="):
			repoOverride = strings.TrimPrefix(a, "--repo=")
		}
	}

	cfg := config.Default()
	if err := cfg.Validate(); err != nil {
		log.Error("invalid configuration", "error", err)
		return 1
	}

	ctx := context.Background()
	pool, err := db.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		log.Error("db open", "error", err)
		return 1
	}
	defer pool.Close()

	ttx, err := pool.BeginTenantTx(ctx, cfg.DeploymentTenantID)
	if err != nil {
		log.Error("begin tenant tx", "error", err)
		return 1
	}
	defer ttx.Rollback(ctx)

	runs, err := db.ListBackfillPRRuns(ctx, ttx.Tx, cfg.DeploymentTenantID)
	if err != nil {
		log.Error("list backfill runs", "error", err)
		return 1
	}

	if len(runs) == 0 {
		log.Info("backfill-pr: no runs need a PR backfill")
		return 0
	}
	log.Info("backfill-pr: found git-backed terminal runs to resolve", "count", len(runs))

	updated, unresolved := 0, 0
	for _, run := range runs {
		repoSlug := repoOverride
		if repoSlug == "" && run.ProjectID != "" {
			if p, gerr := db.GetProject(ctx, ttx.Tx, cfg.DeploymentTenantID, run.ProjectID); gerr == nil && p.RepoSlug != nil && *p.RepoSlug != "" {
				repoSlug = *p.RepoSlug
			}
		}
		if repoSlug == "" {
			log.Warn("backfill-pr: run has no resolvable repo slug; leaving PR-less", "run", run.ID, "branch", run.WorktreeBranch)
			unresolved++
			continue
		}

		prURL, prState, err := resolvePR(ctx, repoSlug, run.WorktreeBranch)
		if err != nil {
			log.Warn("backfill-pr: gh resolve failed; leaving run PR-less", "run", run.ID, "branch", run.WorktreeBranch, "error", err)
			unresolved++
			continue
		}
		if prURL == "" {
			log.Info("backfill-pr: no PR found for branch; leaving run PR-less", "run", run.ID, "branch", run.WorktreeBranch)
			unresolved++
			continue
		}

		if dryRun {
			log.Info("backfill-pr: [dry-run] would write", "run", run.ID, "pr_url", prURL, "pr_state", prState)
			updated++
			continue
		}

		execs, err := db.ListExecutions(ctx, ttx.Tx, db.ListExecutionsFilter{TenantID: cfg.DeploymentTenantID, WorkflowRunID: run.ID, PageSize: 1000})
		if err != nil {
			log.Warn("backfill-pr: list executions for run", "run", run.ID, "error", err)
		}

		// 1) run_context JSONB (the PR / run-detail surface). Merge
		//    additively with a single CAS retry on version conflict.
		if err := writeRunContextPR(ctx, ttx.Tx, cfg.DeploymentTenantID, run.ID, run, prURL, prState); err != nil {
			log.Warn("backfill-pr: update run_context", "run", run.ID, "error", err)
		}

		// 2) execution columns (the work item card + execution detail).
		for _, e := range execs {
			prURL, prState := prURL, prState
			_, uerr := db.UpdateExecution(ctx, ttx.Tx, cfg.DeploymentTenantID, e.ID, e.Version, db.UpdateExecutionFields{PrURL: &prURL, PrState: &prState})
			if uerr != nil && !errors.Is(uerr, db.ErrNotFound) {
				log.Warn("backfill-pr: update execution PR fields", "execution", e.ID, "error", uerr)
			}
		}

		log.Info("backfill-pr: wrote", "run", run.ID, "pr_url", prURL, "pr_state", prState, "executions", len(execs))
		updated++
	}

	if err := ttx.Commit(ctx); err != nil {
		log.Error("commit", "error", err)
		return 1
	}

	log.Info("backfill-pr: done", "updated", updated, "unresolved", unresolved, "dry_run", dryRun)
	return 0
}

// resolvePR maps a branch to its most recent PR (across ALL states — merged
// PRs are excluded by gh's default open-only query) for the given repo slug.
// Returns the PR's html_url and a normalized pr_state, or ("", "") when the
// branch has no PR. `pr_state` is normalized to the lowercase values the
// frontend's prStateLabel maps (open/merged/draft/closed).
func resolvePR(ctx context.Context, repoSlug, branch string) (prURL, prState string, err error) {
	cmd := exec.CommandContext(ctx, "gh", "pr", "list",
		"--repo", repoSlug,
		"--head", branch,
		"--state", "all",
		"--json", "number,state,url",
	)
	out, cerr := cmd.CombinedOutput()
	if cerr != nil {
		return "", "", fmt.Errorf("gh pr list: %w: %s", cerr, strings.TrimSpace(string(out)))
	}

	var prs []struct {
		Number int    `json:"number"`
		State  string `json:"state"`
		URL    string `json:"url"`
	}
	if err := json.Unmarshal(out, &prs); err != nil {
		return "", "", fmt.Errorf("gh pr list: parse output: %w", err)
	}
	if len(prs) == 0 {
		return "", "", nil
	}
	// Prefer an exact headRefName match if multiple candidates: gh already
	// filters by --head, so the newest (largest number) is the authoritative
	// one for this branch.
	pick := prs[len(prs)-1]
	return pick.URL, normalizePRState(pick.State), nil
}

func normalizePRState(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "open":
		return "open"
	case "merged":
		return "merged"
	case "draft":
		return "draft"
	case "closed":
		return "closed"
	default:
		return "closed"
	}
}

// writeRunContextPR merges pr_url/pr_state into the run's run_context JSONB
// with optimistic concurrency (a single retry on version conflict, mirroring
// the reconciler's PR-capture write at internal/scheduler/reconciler.go).
func writeRunContextPR(ctx context.Context, tx pgx.Tx, tenantID, runID string, run db.WorkflowRunRow, prURL, prState string) error {
	ctxBytes, ok := mergeRunContext(run.RunContext, map[string]any{"pr_url": prURL, "pr_state": prState})
	if !ok {
		return errors.New("merge run_context")
	}
	_, err := db.UpdateWorkflowRun(ctx, tx, tenantID, runID, run.Version, db.UpdateWorkflowRunFields{RunContext: &ctxBytes})
	if err != nil {
		// One retry on CAS conflict: re-read the run and try once more.
		reRead, reErr := db.GetWorkflowRun(ctx, tx, tenantID, runID)
		if reErr != nil {
			return err
		}
		ctxBytes2, ok2 := mergeRunContext(reRead.RunContext, map[string]any{"pr_url": prURL, "pr_state": prState})
		if !ok2 {
			return err
		}
		_, err = db.UpdateWorkflowRun(ctx, tx, tenantID, runID, reRead.Version, db.UpdateWorkflowRunFields{RunContext: &ctxBytes2})
	}
	return err
}

// mergeRunContext folds the given key/values into an existing run_context
// JSONB map (additive; best-effort). Mirrors internal/scheduler's
// mergeRunContext so the CLI backfill and the reconciler stay in sync.
func mergeRunContext(existing []byte, add map[string]any) ([]byte, bool) {
	out := map[string]any{}
	if len(existing) > 0 {
		if err := json.Unmarshal(existing, &out); err != nil {
			out = map[string]any{}
		}
	}
	for k, v := range add {
		out[k] = v
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, false
	}
	return b, true
}
