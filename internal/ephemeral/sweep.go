// Package ephemeral sweeps machine-managed transient records that their owner
// never got to delete.
//
// Ask Orchicon's Quick Work mode creates a worker, a workflow and a work item
// per job, hides them from every human view, and HARD-DELETES them when the job
// ends. The deletion is performed by the agent, so it is the one step that
// cannot be guaranteed: a killed process, a crashed plane or an OOM leaves the
// records behind — invisible and still present, which is exactly the state the
// hard-delete rule exists to prevent.
//
// This package is the backstop for that single failure mode. It is not the
// primary cleanup path and must not be treated as one: a job that completes
// normally is cleaned up by its agent, immediately.
package ephemeral

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
)

// DefaultTTL is how long an ephemeral record may exist before the sweep
// considers it abandoned.
//
// SIX HOURS, which is two orders of magnitude longer than any real Quick Work
// job — those are conversational turns, i.e. minutes. The gap is deliberate:
// the sweep CANNOT tell "abandoned" from "long-running", so the only safe
// instrument it has is time, and it has to be generous enough that no
// legitimate job is ever near it. A too-short TTL would delete a live job's
// records out from under it, which is far worse than leaving a stale row for a
// few more hours.
const DefaultTTL = 6 * time.Hour

// SweepInterval is how often the sweep runs. Cheap by design: the query is
// gated by a partial index (created_at WHERE ephemeral), so on a plane with no
// transients it is an index probe on an empty predicate.
const SweepInterval = 10 * time.Minute

// Sweeper periodically hard-deletes abandoned ephemeral records.
type Sweeper struct {
	pool *db.Pool
	log  *slog.Logger
	ttl  time.Duration
	// now is injectable so the cutoff is testable without sleeping for hours.
	now func() time.Time
}

// NewSweeper builds the sweeper, taking the TTL from the environment when set.
func NewSweeper(pool *db.Pool, log *slog.Logger) *Sweeper {
	return &Sweeper{pool: pool, log: log, ttl: TTLFromEnv(), now: time.Now}
}

// TTLFromEnv resolves the abandonment window: ORCHICON_EPHEMERAL_SWEEP_TTL
// (a Go duration such as "30m", or a bare number of seconds) overriding
// DefaultTTL. A malformed or non-positive value is ignored rather than
// honoured — a typo in an env var must never collapse the window to zero and
// start deleting live records.
func TTLFromEnv() time.Duration {
	raw := os.Getenv("ORCHICON_EPHEMERAL_SWEEP_TTL")
	if raw == "" {
		return DefaultTTL
	}
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return d
	}
	if secs, err := strconv.Atoi(raw); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	return DefaultTTL
}

// TTL reports the configured abandonment window.
func (s *Sweeper) TTL() time.Duration { return s.ttl }

// Sweep runs one pass over every tenant and returns how many records were
// removed in total.
//
// TENANTS ARE ENUMERATED, not assumed. The other in-plane sweeps read tenant
// settings through a single hardcoded tenant, which is wrong for a delete: a
// sweep that only tidied one tenant would leave every other tenant's abandoned
// transients in place forever. The tenants table is not RLS-enabled (it IS the
// tenant), so it can be listed directly, and each tenant's deletes then run
// inside that tenant's own transaction so the RLS policy still applies.
//
// A tenant whose sweep fails does not stop the others; its error is logged and
// the pass continues, because one unhealthy tenant must not deny cleanup to
// every other one.
func (s *Sweeper) Sweep(ctx context.Context) (int, error) {
	cutoff := s.now().Add(-s.ttl)
	total := 0
	var firstErr error

	const pageSize = 200
	afterID := ""
	for {
		tenants, err := db.ListTenants(ctx, s.pool, pageSize, afterID)
		if err != nil {
			return total, fmt.Errorf("ephemeral sweep: list tenants: %w", err)
		}
		if len(tenants) == 0 {
			break
		}
		for _, t := range tenants {
			n, err := s.sweepTenant(ctx, t.ID, cutoff)
			total += n
			if err != nil {
				s.log.Warn("ephemeral sweep failed for tenant",
					"tenant", t.ID, "error", err)
				if firstErr == nil {
					firstErr = err
				}
			}
		}
		if len(tenants) < pageSize {
			break
		}
		afterID = tenants[len(tenants)-1].ID
	}

	if total > 0 {
		s.log.Info("ephemeral sweep removed abandoned records",
			"records", total, "ttl", s.ttl.String())
	}
	return total, firstErr
}

// sweepTenant removes one tenant's abandoned ephemeral records.
func (s *Sweeper) sweepTenant(ctx context.Context, tenantID string, cutoff time.Time) (int, error) {
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return 0, err
	}
	defer ttx.Rollback(ctx)
	res, err := db.SweepAbandonedEphemeral(ctx, ttx.Tx, tenantID, cutoff)
	if err != nil {
		return res.Total(), err
	}
	if err := ttx.Commit(ctx); err != nil {
		return 0, err
	}
	return res.Total(), nil
}
