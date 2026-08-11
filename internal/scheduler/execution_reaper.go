package scheduler

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
)

// ExecutionReaper finds executions that are stuck `running` with no live
// process — orphaned by a control-plane restart (in-flight session runs die
// with the plane) — and fails them so recovery re-dispatches the step.
//
// Without this, a `container.sh rebuild` mid-workflow left the workflow
// run, step, and execution `running` forever (the stall monitor and
// wall-clock deadline were in-process state, lost on restart, and no
// reconciler re-checks running executions).
//
// The reaper is deliberately cautious: the liveness probe can false-negative
// on a transient blip, so an execution is only reaped after it has been
// "not alive" for `consecutive_failures` consecutive probes AND is older
// than the `grace` window (fresh-dispatch race). Both are tenant settings
// (execution_reap_grace_seconds / _consecutive_failures) with env-var
// fallbacks (ORCHICON_REAP_GRACE_SECONDS / ORCHICON_REAP_CONSECUTIVE_FAILURES)
// for dev overrides.
type ExecutionReaper struct {
	pool   *db.Pool
	active func(execID string) bool // adapter.IsExecutionActive
	fail   func(ctx context.Context, execID, errorMessage string)
	log    *slog.Logger

	// notAlive tracks consecutive not-alive liveness probes per execution.
	// A single false negative must not kill a healthy execution.
	notAliveMu sync.Mutex
	notAlive   map[string]int
}

// NewExecutionReaper creates the liveness reaper.
func NewExecutionReaper(pool *db.Pool, active func(string) bool, fail func(context.Context, string, string), log *slog.Logger) *ExecutionReaper {
	return &ExecutionReaper{pool: pool, active: active, fail: fail, log: log, notAlive: make(map[string]int)}
}

// reapTuning resolves the grace window and consecutive-failure threshold
// from tenant settings, with env-var overrides (dev) and built-in defaults
// (60s / 3) as the fallback chain.
func (r *ExecutionReaper) reapTuning(ctx context.Context) (grace time.Duration, threshold int) {
	grace = 60 * time.Second
	threshold = 3
	ttx, err := r.pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		return grace, threshold
	}
	s, err := db.GetTenantSettings(ctx, ttx.Tx, "tnt_dev")
	_ = ttx.Rollback(ctx)
	if err != nil {
		return grace, threshold
	}
	if s.ExecutionReapGraceSeconds > 0 {
		grace = time.Duration(s.ExecutionReapGraceSeconds) * time.Second
	}
	if s.ExecutionReapConsecutiveFailures > 0 {
		threshold = int(s.ExecutionReapConsecutiveFailures)
	}
	// Env overrides (dev debugging) win over the DB value.
	if v, err := strconv.Atoi(os.Getenv("ORCHICON_REAP_GRACE_SECONDS")); err == nil && v > 0 {
		grace = time.Duration(v) * time.Second
	}
	if v, err := strconv.Atoi(os.Getenv("ORCHICON_REAP_CONSECUTIVE_FAILURES")); err == nil && v > 0 {
		threshold = v
	}
	return grace, threshold
}

// Reap scans running executions and fails the ones whose process is gone.
// An execution is only reaped once it is older than the grace window AND
// has returned "not alive" for `threshold` consecutive probes.
func (r *ExecutionReaper) Reap(ctx context.Context) error {
	grace, threshold := r.reapTuning(ctx)
	ttx, err := r.pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		return err
	}
	execs, err := db.ListRunningExecutions(ctx, ttx.Tx, "tnt_dev")
	_ = ttx.Rollback(ctx)
	if err != nil {
		return err
	}
	seen := make(map[string]bool, len(execs))
	for _, exec := range execs {
		seen[exec.ID] = true
		if exec.StartedAt != nil && time.Since(*exec.StartedAt) < grace {
			// Too fresh to have an authoritative liveness answer — the
			// exec may not be registered with the runtime yet. Reset any
			// streak so a transient miss inside the grace window never
			// counts toward reaping.
			r.forget(exec.ID)
			continue
		}
		dead := false
		// Every execution (workflow-run or standalone) is a session run
		// tracked in the adapter's in-plane registry. If the adapter is no
		// longer tracking it, the session runner is gone (plane restart /
		// runtime container lost) — dead.
		dead = r.active == nil || !r.active(exec.ID)
		if !dead {
			r.forget(exec.ID)
			continue
		}
		// Sustained not-alive. Only reap once the consecutive streak
		// crosses the threshold.
		count := r.bump(exec.ID)
		if count < threshold {
			r.log.Debug("execution liveness: not-alive below reap threshold",
				"execution", exec.ID, "run", exec.WorkflowRunID, "count", count, "threshold", threshold)
			continue
		}
		r.log.Warn("reaping lost execution", "execution", exec.ID, "run", exec.WorkflowRunID, "task", exec.TaskID, "status", exec.Status, "consecutive_not_alive", count)
		r.fail(ctx, exec.ID, "execution lost: control plane restarted or runtime container gone")
		r.forget(exec.ID)
	}
	// Drop counters for executions that are no longer running.
	r.prune(seen)
	return nil
}

func (r *ExecutionReaper) bump(execID string) int {
	r.notAliveMu.Lock()
	defer r.notAliveMu.Unlock()
	r.notAlive[execID]++
	return r.notAlive[execID]
}

func (r *ExecutionReaper) forget(execID string) {
	r.notAliveMu.Lock()
	defer r.notAliveMu.Unlock()
	delete(r.notAlive, execID)
}

func (r *ExecutionReaper) prune(keep map[string]bool) {
	r.notAliveMu.Lock()
	defer r.notAliveMu.Unlock()
	for id := range r.notAlive {
		if !keep[id] {
			delete(r.notAlive, id)
		}
	}
}
