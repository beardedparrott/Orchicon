package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// GetDispatchLimitValues reads the two configured max-concurrent-runs
// values for a project — the tenant-wide ceiling (tenant_settings) and the
// per-project override (projects) — in the caller's transaction so the
// admission checks in the reconcilers observe a consistent snapshot with
// the count queries that follow.
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

// GetEffectiveDispatchLimit resolves the effective max-concurrent-runs cap
// for a project: min(tenant.max_concurrent_runs, project.max_concurrent_runs),
// where 0 on either side means "no additional restriction from that side"
// (architecture-notes/per-project-dispatch-limits.md D1). A project whose
// effective limit is 1 serializes its runs; 0 means no cap.
func GetEffectiveDispatchLimit(ctx context.Context, tx pgx.Tx, tenantID, projectID string) (int, error) {
	tenantLimit, projectLimit, err := GetDispatchLimitValues(ctx, tx, tenantID, projectID)
	if err != nil {
		return 0, err
	}
	return EffectiveDispatchLimit(tenantLimit, projectLimit), nil
}

// EffectiveDispatchLimit is the pure effective-limit formula: 0 on either
// side means "no restriction from that side", so the other side wins;
// otherwise the minimum applies.
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

// InPlaceLimit is the pure non-repo (in-place fallback) serialization
// limit. Non-repo runs execute in the SHARED project_dir, so they are
// serialized by default (limit 1) unless the project EXPLICITLY opts into
// concurrency by setting its own max_concurrent_runs > 1 AND the tenant
// permits it (architecture-notes/per-project-dispatch-limits.md D3). The
// effective limit still applies — a tenant cap of 1 overrides a project
// that sets 4.
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

// AdmitInPlaceRun is the WorktreeReconciler's ATOMIC admission gate for a
// non-repo (in-place) run, invoked from within the same transaction that
// will mark the run's worktree_status 'skipped'. It locks the project row
// FOR UPDATE so concurrent admissions for the same project serialize, then
// counts already-admitted non-terminal in-place runs for that project and
// reports whether the new run may proceed (count < limit).
//
// Without the project-row lock, count-then-mark would reintroduce the
// TOCTOU race: two reconciler passes could both observe count < limit and
// both mark their runs 'skipped', letting two runs execute in place at
// once. With the lock, the second transaction blocks until the first
// commits, then re-counts under the now-committed state and correctly
// defers. The caller leaves the run 'pending' when admission is denied; the
// next scan pass re-evaluates it once a slot frees.
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
	// Count currently-admitted in-place runs: non-terminal workflow runs of
	// the project whose worktree_status='skipped' (the run-level token that
	// says "this run executes in the shared project_dir").
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
