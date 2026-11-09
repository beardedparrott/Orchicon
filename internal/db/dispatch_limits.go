package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// GetDispatchLimitValues reads the two configured max-concurrent-runs values for a project — the
// tenant-wide ceiling (tenant_settings) and the per-project override (projects).
func GetDispatchLimitValues(ctx context.Context, tx pgx.Tx, tenantID, projectID string) (tenantLimit, projectLimit int, err error) {
	// The project row must exist (an execution is only ever created for a
	// real project).
	err = tx.QueryRow(ctx,
		`SELECT max_concurrent_runs FROM projects WHERE tenant_id = $1 AND id = $2`,
		tenantID, projectID,
	).Scan(&projectLimit)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, fmt.Errorf("db: dispatch limits: project %s not found", projectID)
	}
	if err != nil {
		return 0, 0, fmt.Errorf("db: dispatch limits: read project: %w", err)
	}
	s, err := GetTenantSettings(ctx, tx, tenantID)
	if err != nil {
		return 0, 0, fmt.Errorf("db: dispatch limits: read tenant settings: %w", err)
	}
	return s.MaxConcurrentRuns, projectLimit, nil
}

// GetEffectiveDispatchLimit resolves the effective max-concurrent-runs cap for a project:
// min(tenant.max_concurrent_runs, project.max_concurrent_runs).
func GetEffectiveDispatchLimit(ctx context.Context, tx pgx.Tx, tenantID, projectID string) (int, error) {
	tenantLimit, projectLimit, err := GetDispatchLimitValues(ctx, tx, tenantID, projectID)
	if err != nil {
		return 0, err
	}
	return EffectiveDispatchLimit(tenantLimit, projectLimit), nil
}

// EffectiveDispatchLimit is the pure effective-limit formula: 0 on either side means "no
// restriction from that side", so the other side wins; otherwise the minimum applies.
func EffectiveDispatchLimit(tenantLimit, projectLimit int) int {
	if projectLimit == 0 {
		return tenantLimit
	}
	if tenantLimit == 0 {
		return projectLimit
	}
	if tenantLimit < projectLimit {
		return tenantLimit
	}
	return projectLimit
}

// InPlaceLimit is the pure non-repo (in-place fallback) serialization limit.
func InPlaceLimit(tenantLimit, projectLimit int) int {
	if projectLimit < 2 {
		// Not explicitly opted in: default to serialized in-place runs.
		return 1
	}
	eff := EffectiveDispatchLimit(tenantLimit, projectLimit)
	if eff < 1 {
		eff = 1
	}
	return eff
}

// AdmitInPlaceRun is the WorktreeReconciler's ATOMIC admission gate for a non-repo (in-place) run,
// invoked from within the same transaction that will mark the run's worktree_status 'skipped'.
func AdmitInPlaceRun(ctx context.Context, tx pgx.Tx, tenantID, projectID string, limit int) (bool, error) {
	if limit < 1 {
		// No cap configured (0) or a broken negative value: admit
		// unconditionally — the guard only ever kicks in above zero.
		return true, nil
	}
	// Serialize admissions for this project on the project row lock.
	var one int
	err := tx.QueryRow(ctx,
		`SELECT 1 FROM projects WHERE tenant_id = $1 AND id = $2 FOR UPDATE`,
		tenantID, projectID,
	).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("db: admit in-place run: project %s not found", projectID)
	}
	if err != nil {
		return false, fmt.Errorf("db: admit in-place run: lock project: %w", err)
	}
	// Count currently-admitted in-place runs: non-terminal workflow runs of the project whose
	// worktree_status='skipped' (the run-level token that says "this run executes in the shared.
	var active int
	err = tx.QueryRow(ctx,
		`SELECT count(*) FROM workflow_runs
		WHERE tenant_id = $1 AND project_id = $2
		  AND status IN ('pending','running','paused')
		  AND worktree_status = 'skipped'`,
		tenantID, projectID,
	).Scan(&active)
	if err != nil {
		return false, fmt.Errorf("db: admit in-place run: count active: %w", err)
	}
	return active < limit, nil
}
