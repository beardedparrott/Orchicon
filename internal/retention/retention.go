package retention

// retention.go — the scheduled execution-history prune, and the VACUUM that makes it actually give
// disk back.
//
// WHY THIS PACKAGE EXISTS. `db.PruneExecutionHistory` was already correct, and `orchicon
// prune-executions` already exposed it. Neither was ever scheduled, and neither VACUUMed. Measured on a
// real long-lived deployment before this change:
//
//	execution_session_parts   3154 MB   30474 rows   98.3% DEAD SPACE (pgstattuple free_percent)
//	outbox                    2102 MB       1 row    99.59% DEAD SPACE
//
// The rows had been deleted. The FILES never shrank, because a Postgres DELETE marks space reusable
// inside the relation and returns nothing to the OS. So the data directory grew monotonically no matter
// how diligently the operator pruned — which is the whole complaint this addresses.
//
// THE PASS IS DELIBERATELY BORING. It reads one tenant's retention settings, plans the prune, refuses to
// run when the plan would touch live work, deletes through the existing code path, and then VACUUMs the
// relations that shrank. Everything it does is already idempotent, so a crash mid-pass costs the next
// pass nothing.

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// DefaultKeepExecutions is how many of the newest executions survive a pass when the tenant has not said.
// It matches the `orchicon prune-executions --keep` default, so the scheduled and manual paths agree.
const DefaultKeepExecutions = 150

// DefaultTimeout bounds one pass. A prune that runs long enough to matter is a prune holding locks the
// control plane needs, so it gets a budget rather than an open-ended lock window.
const DefaultTimeout = 300 * time.Second

// vacuumTables are the relations this package vacuums after a prune, in the order it vacuums them.
//
// THESE ARE NOT A GUESS. Each one is a table the prune deletes or de-links from AND the ones measured
// bloated in the field. VACUUM (not VACUUM FULL) is used deliberately — see vacuumAfterPrune — so this
// pass is safe to run against a live plane; it never takes an ACCESS EXCLUSIVE lock.
var vacuumTables = []string{
	"execution_session_parts",
	"worker_executions",
	"checkpoints",
	"recovery_step_runs",
	"recovery_executions",
	"continuation_plans",
	"workflow_step_runs",
	"outbox",
	"usage_records",
}

// Result describes one pass in the terms an operator reasons in: how much was deleted, and whether the
// disk was actually given back.
type Result struct {
	TenantID string

	// Skipped is the reason no prune ran, when it didn't. Empty when a prune ran.
	Skipped string

	Plan       db.ExecutionRetentionPlan
	Vacuumed   []string
	VacuumSkip string
	// ReclaimedBytes is the difference in summed relation size before and after the pass. It is an
	// ESTIMATE of what the OS got back, measured the same way twice, so it is comparable across passes.
	ReclaimedBytes int64
	Duration       time.Duration
}

// Runner executes retention passes. Construct it once and call RunForTenant on a ticker.
type Runner struct {
	pool *db.Pool
	log  *slog.Logger

	// KeepExecutions overrides the tenant's keep count when non-nil. Used by the CLI so a single
	// invocation can ask "what would keeping 40 do?" against live settings.
	KeepExecutions *int
	// Apply false makes a pass report only — the CLI's default, and the reason the scheduled path is
	// the only caller that passes true without an explicit operator gesture.
	Apply bool
	// Vacuum forces the vacuum decision off when false. When nil the tenant's setting decides.
	Vacuum *bool
	// Timeout bounds one pass; zero means DefaultTimeout.
	Timeout time.Duration
}

// NewRunner builds a Runner over a pool.
func NewRunner(pool *db.Pool, log *slog.Logger) *Runner {
	return &Runner{pool: pool, log: log, Apply: true}
}

// RunForTenant performs one retention pass for a tenant.
//
// IT IS SAFE TO CALL ON A TICKER WITH NO GUARD: the underlying prune is idempotent, rejects a plan that
// would delete live work, and deletes in bounded batches. The only stateful thing it touches is the
// VACUUM, which is a maintenance operation and also idempotent.
func (r *Runner) RunForTenant(ctx context.Context, tenantID string) (Result, error) {
	start := time.Now()
	res := Result{TenantID: tenantID}

	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// The policy comes from the tenant's own settings row.
	cfg, err := r.policy(ctx, tenantID)
	if err != nil {
		return res, err
	}
	res.Skipped = cfg.skipped
	if cfg.skipped != "" {
		res.Duration = time.Since(start)
		return res, nil
	}

	keep := cfg.keep
	if r.KeepExecutions != nil {
		keep = *r.KeepExecutions
	}

	if !r.Apply {
		plan, err := r.pool.PlanExecutionRetention(ctx, tenantID, keep)
		if err != nil {
			return res, fmt.Errorf("retention: plan: %w", err)
		}
		res.Plan = plan
		res.Duration = time.Since(start)
		return res, nil
	}

	before := r.relationBytes(ctx, tenantID)
	plan, err := r.pool.PruneExecutionHistory(ctx, tenantID, keep, 500)
	res.Plan = plan
	if err != nil {
		return res, fmt.Errorf("retention: prune: %w", err)
	}

	if plan.DeleteExecutions > 0 && cfg.vacuum {
		vacuumed, skip, verr := r.vacuumAfterPrune(ctx)
		res.Vacuumed = vacuumed
		res.VacuumSkip = skip
		if verr != nil {
			// A failed VACUUM is REPORTABLE, not fatal: the rows are already gone and the space is
			// reusable, so the next pass will try again. Failing the whole pass would just mean the
			// prune's success is invisible.
			r.log.Warn("retention: vacuum after prune failed", "tenant", tenantID, "error", verr)
		}
		res.ReclaimedBytes = before - r.relationBytes(ctx, tenantID)
	} else if plan.DeleteExecutions > 0 {
		res.VacuumSkip = "disabled by tenant settings"
	}

	res.Duration = time.Since(start)
	return res, nil
}

// policy is the tenant's effective retention decision.
type policy struct {
	keep    int
	vacuum  bool
	skipped string
}

// policy reads the tenant settings row and decides whether this tenant prunes at all.
//
// NO ROW, NO PRUNE. A tenant that has never opened Settings has never opted in, and a background
// destructive job must not be the way an operator discovers a feature exists. The feature ships off.
func (r *Runner) policy(ctx context.Context, tenantID string) (policy, error) {
	tx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return policy{}, fmt.Errorf("retention: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	row, err := db.GetTenantSettings(ctx, tx.Tx, tenantID)
	if err != nil {
		return policy{}, fmt.Errorf("retention: read settings: %w", err)
	}
	if !row.ExecutionRetentionEnabled {
		return policy{skipped: "execution retention is disabled for this tenant"}, nil
	}
	keep := DefaultKeepExecutions
	if row.ExecutionRetentionKeep != nil {
		keep = int(*row.ExecutionRetentionKeep)
	}
	if keep < 0 {
		// Constant-safe rather than a hard error: a negative keep is meaningless, and treating it as
		// 0 would silently discard the whole history.
		keep = DefaultKeepExecutions
	}
	vacuum := row.ExecutionRetentionVacuum
	if r.Vacuum != nil {
		vacuum = *r.Vacuum
	}
	return policy{keep: keep, vacuum: vacuum}, nil
}

// vacuumAfterPrune runs a plain VACUUM over the tables the prune touched, and reports which it managed.
//
// VACUUM RATHER THAN VACUUM FULL, ON PURPOSE. VACUUM FULL rewrites the relation under an ACCESS
// EXCLUSIVE lock — it is what returns space to the OS most completely, and it is exactly what must NOT
// run unattended against a live plane: it blocks every read for the duration. Plain VACUUM takes only a
// SHARE UPDATE EXCLUSIVE lock, updates the visibility map, and lets the next autovacuum return space.
//
// The operator who wants the aggressive form has `orchicon prune-executions` plus a manual VACUUM FULL;
// this scheduled pass is the one that must never surprise a live deployment.
//
// IT RUNS OUTSIDE A TRANSACTION, necessarily: VACUUM cannot run inside a transaction block. These tables
// are not RLS-protected (tenant_settings is; the perf tables are not), so no tenant session variable is
// needed — and none could be set here anyway.
func (r *Runner) vacuumAfterPrune(ctx context.Context) ([]string, string, error) {
	var done []string
	for _, table := range vacuumTables {
		if ctx.Err() != nil {
			return done, "pass deadline reached", ctx.Err()
		}
		// Identifiers cannot be parameterised, so the table name is interpolated — which is safe ONLY
		// because vacuumTables is a package-level constant list, never caller input.
		if _, err := r.pool.Exec(ctx, "VACUUM (ANALYZE) "+table); err != nil {
			return done, "", fmt.Errorf("vacuum %s: %w", table, err)
		}
		done = append(done, table)
	}
	return done, "", nil
}

// relationBytes sums the on-disk size of the tables this package prunes, for before/after comparison.
//
// pg_total_relation_size INCLUDES INDEXES because the indexes are where the bloat actually lived in the
// field: the outbox's 2102 MB was 1927 MB of index around 175 MB of heap. Measuring heap alone would have
// reported that reclaim as 175 MB and hidden the real win.
func (r *Runner) relationBytes(ctx context.Context, tenantID string) int64 {
	var total int64
	const q = `SELECT COALESCE(sum(pg_total_relation_size(c.oid)), 0)
	           FROM pg_class c
	           JOIN pg_namespace n ON n.oid = c.relnamespace
	           WHERE n.nspname = 'public' AND c.relkind = 'r'
	             AND c.relname = ANY($1)`
	if err := r.pool.QueryRow(ctx, q, vacuumTables).Scan(&total); err != nil {
		// Sizing is DIAGNOSTIC. A failure here must never fail the pass that already succeeded.
		if r.log != nil {
			r.log.Debug("retention: relation sizing failed", "tenant", tenantID, "error", err)
		}
		return 0
	}
	return total
}

// RunAll performs one pass for every active tenant on a plane. Empty slice means the plane has exactly
// one deployment tenant and no multi-tenancy, which is the normal case.
func (r *Runner) RunAll(ctx context.Context, tenantIDs []string, deploymentTenant string) []Result {
	targets := tenantIDs
	if len(targets) == 0 {
		if deploymentTenant == "" {
			return nil
		}
		targets = []string{deploymentTenant}
	}
	out := make([]Result, 0, len(targets))
	for _, tid := range targets {
		if ctx.Err() != nil {
			break
		}
		res, err := r.RunForTenant(ctx, tid)
		if err != nil {
			if r.log != nil {
				r.log.Warn("retention pass failed", "tenant", tid, "error", err)
			}
			continue
		}
		if res.Skipped != "" {
			if r.log != nil {
				r.log.Debug("retention pass skipped", "tenant", tid, "reason", res.Skipped)
			}
			continue
		}
		if r.log != nil {
			r.log.Info("retention pass complete",
				"tenant", tid,
				"executions_removed", res.Plan.DeleteExecutions,
				"session_parts_removed", res.Plan.SessionParts,
				"kept", res.Plan.KeepExecutions,
				"vacuumed", len(res.Vacuumed),
				"reclaimed_bytes", res.ReclaimedBytes,
				"duration", res.Duration.String())
		}
		out = append(out, res)
	}
	return out
}

// tenantIDs is kept as a tiny indirection so the caller's tenant resolution and this package's logging
// do not drift; the plane's context helper is the single source.
var _ = tenant.FromContext
