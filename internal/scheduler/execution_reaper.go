package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
)

// ExecutionReaper finds executions that are stuck `running` with no live process — orphaned by a
// control-plane restart (in-flight session runs die with the plane).
type ExecutionReaper struct {
	pool   *db.Pool
	active func(exec db.ExecutionRow) bool // liveness probe (adapter.IsExecutionActive via LivenessReporter type-assert)
	fail   func(ctx context.Context, execID, errorMessage string)
	log    *slog.Logger

	// notAlive tracks consecutive not-alive liveness probes per execution.
	// A single false negative must not kill a healthy execution.
	notAliveMu sync.Mutex
	notAlive   map[string]int
}

// NewExecutionReaper creates the liveness reaper.
func NewExecutionReaper(pool *db.Pool, active func(exec db.ExecutionRow) bool, fail func(context.Context, string, string), log *slog.Logger) *ExecutionReaper {
	return &ExecutionReaper{pool: pool, active: active, fail: fail, log: log, notAlive: make(map[string]int)}
}

// reapTuning resolves the grace window and consecutive-failure threshold from tenant settings, with
// env-var overrides (dev) and built-in defaults (60s / 3) as the fallback chain.
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

// recordedNotWellStatus reports whether an execution's row ALREADY records a not-well liveness
// verdict.
func recordedNotWellStatus(status string) bool {
	switch status {
	case domain.ExecutionUnhealthy, domain.ExecutionStalled, domain.ExecutionTerminating:
		return true
	}
	return false
}

// It is set ONLY on the manual path, and it is not a weakening: the streak exists so an unattended
// reaper never acts on a single transient probe BEFORE any verdict is recorded.
func (r *ExecutionReaper) reapOne(ctx context.Context, exec db.ExecutionRow, grace time.Duration, threshold int, allowRecordedVerdict bool) bool {
	// AN UN-AGEABLE ROW IS NOT REAPED.
	if exec.StartedAt == nil {
		r.forget(exec.ID)
		r.log.Debug("execution liveness: no start time — cannot age, not reaping", "execution", exec.ID)
		return false
	}
	if time.Since(*exec.StartedAt) < grace {
		// Too fresh to have an authoritative liveness answer — the exec may not be registered with the
		// runtime yet.
		r.forget(exec.ID)
		return false
	}
	// Every execution (workflow-run or standalone) is a session run tracked in the adapter's in-plane
	// registry.
	if r.active != nil && r.active(exec) {
		r.forget(exec.ID)
		return false
	}
	// Sustained not-alive, unless the verdict is already on the row and this
	// is the attended path.
	if !(allowRecordedVerdict && recordedNotWellStatus(exec.Status)) {
		count := r.bump(exec.ID)
		if count < threshold {
			r.log.Debug("execution liveness: not-alive below reap threshold",
				"execution", exec.ID, "run", exec.WorkflowRunID, "count", count, "threshold", threshold)
			return false
		}
	}
	msg := reapFailureMessage(exec, threshold)
	r.log.Warn("reaping lost execution",
		"execution", exec.ID, "run", exec.WorkflowRunID, "task", exec.TaskID,
		"status", exec.Status, "health", exec.HealthState, "adapter", adapterIDForReap(exec),
		"probes", threshold, "reason", msg)
	r.fail(ctx, exec.ID, msg)
	r.forget(exec.ID)
	return true
}

// reapFailureMessage names WHY the reaper concluded an execution was lost, from what it actually
// observed — never a fixed guess.
//
// THE OLD STRING WAS "execution lost: control plane restarted or runtime container gone", and it was
// emitted for every reap regardless of cause. It named a container for executions that never had one
// and a restart that had not happened, and it was the reason a misdirected liveness probe stayed
// undiagnosed for days: every reap looked like infrastructure, so nobody looked at the probe. An error
// message that states one possible cause is worse than no detail — it actively misdirects.
//
// It reports only what the reaper knows: the execution's own last recorded state, its adapter, and the
// number of consecutive failed liveness probes. It deliberately does NOT claim which of the possible
// causes applied, because the reaper cannot see that; the probe's own WARN lines carry the specifics.
func reapFailureMessage(exec db.ExecutionRow, probes int) string {
	adapter := adapterIDForReap(exec)
	if adapter == "" {
		adapter = "unknown adapter"
	}
	health := strings.TrimSpace(exec.HealthState)
	state := ""
	if health != "" && health != domain.HealthHealthy {
		state = "; last recorded health " + health
	}
	return fmt.Sprintf(
		"execution lost: no live session in its adapter (%s) after %d consecutive liveness probes — the "+
			"session process is gone (plane restarted, runtime container recycled, or the run never "+
			"registered)%s. The liveness probe's own WARN lines name which",
		adapter, probes, state)
}

// adapterIDForReap renders the execution's adapter id for a log field or message.
func adapterIDForReap(exec db.ExecutionRow) string {
	if exec.AdapterID == nil {
		return ""
	}
	return *exec.AdapterID
}

// Reap scans running executions and fails the ones whose process is gone.
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
		r.reapOne(ctx, exec, grace, threshold, false)
	}
	// Drop counters for executions that are no longer running.
	r.prune(seen)
	return nil
}

// ReapProject runs the reap rule NOW, scoped to one project, and reports what it released.
func (r *ExecutionReaper) ReapProject(ctx context.Context, projectID string) (released []string, examined int, err error) {
	grace, threshold := r.reapTuning(ctx)
	ttx, err := r.pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		return nil, 0, err
	}
	execs, err := db.ListRunningExecutionsForProject(ctx, ttx.Tx, "tnt_dev", projectID)
	_ = ttx.Rollback(ctx)
	if err != nil {
		return nil, 0, err
	}
	examined = len(execs)
	for _, exec := range execs {
		if r.reapOne(ctx, exec, grace, threshold, true) {
			released = append(released, exec.ID)
		}
	}
	return released, examined, nil
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
