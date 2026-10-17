package scheduler

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
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
	active func(exec db.ExecutionRow) bool // liveness probe (adapter.IsExecutionActive via LivenessReporter type-assert)
	fail   func(ctx context.Context, execID, errorMessage string)
	log    *slog.Logger

	// notAlive tracks consecutive not-alive liveness probes per execution.
	// A single false negative must not kill a healthy execution.
	notAliveMu sync.Mutex
	notAlive   map[string]int
}

// NewExecutionReaper creates the liveness reaper. The active probe
// receives the full execution row so it can resolve the execution's
// adapter kind (worker model_ref → Dispatcher) and type-assert the
// LivenessReporter capability — a bridge without the capability reports
// inactive (fail-closed: the reaper treats it as lost, matching the
// pre-dispatcher behavior for kinds that never supported liveness).
func NewExecutionReaper(pool *db.Pool, active func(exec db.ExecutionRow) bool, fail func(context.Context, string, string), log *slog.Logger) *ExecutionReaper {
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

// recordedNotWellStatus reports whether an execution's row ALREADY records a
// not-well liveness verdict. The monitoring pipeline writes these; the reaper's
// own probe is the live re-check.
func recordedNotWellStatus(status string) bool {
	switch status {
	case domain.ExecutionUnhealthy, domain.ExecutionStalled, domain.ExecutionTerminating:
		return true
	}
	return false
}

// reapOne applies the reap rule to a single execution and reports whether it
// was released. ONE rule, two entry points — the periodic tick and the
// operator-invoked project release — so the two can never drift.
//
// allowRecordedVerdict waives the consecutive-probe requirement for an
// execution whose row already carries a not-well status. It is set ONLY on the
// manual path, and it is not a weakening: the streak exists so an unattended
// reaper never acts on a single transient probe BEFORE any verdict is recorded,
// and an 'unhealthy'/'stalled' row IS a verdict the system already recorded. A
// `running`/`healthy` execution never gets this waiver on either path — there
// the streak still applies, so no button-click can kill healthy work on one
// bad probe.
func (r *ExecutionReaper) reapOne(ctx context.Context, exec db.ExecutionRow, grace time.Duration, threshold int, allowRecordedVerdict bool) bool {
	// AN UN-AGEABLE ROW IS NOT REAPED. The grace window is the protection
	// against acting on an execution the runtime has not registered yet, and it
	// works by AGE. With no started_at there is no age, so the protection
	// cannot be applied — and the honest response to "I cannot tell whether this
	// is one second old or three weeks old" is to wait for a human, not to
	// guess on the axis that decides whether live work is killed.
	//
	// This was a hole in the ORIGINAL predicate: `StartedAt != nil &&` made a
	// NULL start skip the grace entirely and fall straight through to the
	// probe, so a brand-new row could be failed on one not-alive answer. It is
	// closed here rather than in the attended path alone, because the tick
	// acting unattended on no evidence is the worse of the two.
	if exec.StartedAt == nil {
		r.forget(exec.ID)
		r.log.Debug("execution liveness: no start time — cannot age, not reaping", "execution", exec.ID)
		return false
	}
	if time.Since(*exec.StartedAt) < grace {
		// Too fresh to have an authoritative liveness answer — the
		// exec may not be registered with the runtime yet. Reset any
		// streak so a transient miss inside the grace window never
		// counts toward reaping.
		r.forget(exec.ID)
		return false
	}
	// Every execution (workflow-run or standalone) is a session run
	// tracked in the adapter's in-plane registry. If the adapter is no
	// longer tracking it, the session runner is gone (plane restart /
	// runtime container lost) — dead.
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
	r.log.Warn("reaping lost execution", "execution", exec.ID, "run", exec.WorkflowRunID, "task", exec.TaskID, "status", exec.Status)
	r.fail(ctx, exec.ID, "execution lost: control plane restarted or runtime container gone")
	r.forget(exec.ID)
	return true
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
		r.reapOne(ctx, exec, grace, threshold, false)
	}
	// Drop counters for executions that are no longer running.
	r.prune(seen)
	return nil
}

// ReapProject runs the reap rule NOW, scoped to one project, and reports what
// it released. It is the attended counterpart to Reap, backing the run view's
// "release stuck executions" action.
//
// WHY AN ATTENDED VARIANT EXISTS. A project pinned at its max-concurrent-runs
// cap by unreaped executions cannot dispatch anything, at all, for any run —
// so waiting for the next tick is not neutral, it is a whole project standing
// still while the operator watches it. Field incident: ten such executions,
// the oldest three weeks old, every dispatch deferred.
//
// The judgement is NOT re-implemented here: it is reapOne, the same rule the
// tick applies, with the one documented waiver for rows that already carry a
// not-well verdict. Nothing about this call can release an execution the
// liveness probe reports alive.
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
