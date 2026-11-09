// Package ephemeral sweeps machine-managed transient records that their owner never got to delete.
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

// The gap is deliberate: the sweep CANNOT tell "abandoned" from "long-running", so the only safe
// instrument it has is time, and it has to be generous enough that no legitimate job is ever near
// it.
const DefaultTTL = 6 * time.Hour

// SweepInterval is how often the sweep runs.
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

// TTLFromEnv resolves the abandonment window: ORCHICON_EPHEMERAL_SWEEP_TTL (a Go duration such as
// "30m", or a bare number of seconds) overriding DefaultTTL.
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

// Sweep runs one pass over every tenant and returns how many records were removed in total.
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
