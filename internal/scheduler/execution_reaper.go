package scheduler

import (
	"context"
	"log/slog"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
)

// execAliveChecker reports whether an exec is still running inside a
// workflow's runtime container. Satisfied by runtime.Client.
type execAliveChecker interface {
	ExecAlive(ctx context.Context, workflowID, execID string) (bool, error)
}

// ExecutionReaper finds executions that are stuck `running` with no live
// process — orphaned by a control-plane restart (in-flight subprocesses
// die with the plane) or by a lost/crashed runtime container — and fails
// them so recovery re-dispatches the step.
//
// Without this, a `container.sh rebuild` mid-workflow left the workflow
// run, step, and execution `running` forever (the stall monitor and
// wall-clock deadline were in-process state, lost on restart, and no
// reconciler re-checks running executions).
type ExecutionReaper struct {
	pool   *db.Pool
	rt     execAliveChecker   // nil = no runtime daemon (in-process only)
	active func(execID string) bool // adapter.IsExecutionActive
	fail   func(ctx context.Context, execID, errorMessage string)
	grace  time.Duration // skip executions started within this window (fresh dispatch race)
	log    *slog.Logger
}

// NewExecutionReaper creates the liveness reaper. rt may be nil (headless
// serve): workflow-run executions are then skipped (undeterminable) and
// only in-process executions are reaped.
func NewExecutionReaper(pool *db.Pool, rt execAliveChecker, active func(string) bool, fail func(context.Context, string, string), log *slog.Logger) *ExecutionReaper {
	return &ExecutionReaper{pool: pool, rt: rt, active: active, fail: fail, grace: 60 * time.Second, log: log}
}

// Reap scans running executions and fails the ones whose process is gone.
func (r *ExecutionReaper) Reap(ctx context.Context) error {
	ttx, err := r.pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		return err
	}
	execs, err := db.ListRunningExecutions(ctx, ttx.Tx, "tnt_dev")
	_ = ttx.Rollback(ctx)
	if err != nil {
		return err
	}
	for _, exec := range execs {
		if exec.StartedAt != nil && time.Since(*exec.StartedAt) < r.grace {
			// Too fresh to have an authoritative liveness answer — the
			// exec may not be registered with the runtime yet.
			continue
		}
		dead := false
		switch {
		case exec.WorkflowRunID != "":
			// Workflow-run execution: ask the daemon whether the exec is
			// alive inside the runtime container. Only a definitive
			// "not alive" (or missing container) counts as dead; a daemon
			// outage (error) is skipped so a temporary disconnect can't
			// mass-reap healthy executions.
			if r.rt == nil {
				continue
			}
			alive, err := r.rt.ExecAlive(ctx, exec.WorkflowRunID, exec.ID)
			if err != nil {
				r.log.Debug("exec liveness: undeterminable", "execution", exec.ID, "error", err)
				continue
			}
			dead = !alive
		default:
			// In-process execution (headless serve / no workflow run):
			// its subprocess lives in this plane, so if the adapter is not
			// tracking it, the process is gone.
			dead = r.active == nil || !r.active(exec.ID)
		}
		if !dead {
			continue
		}
		r.log.Warn("reaping lost execution", "execution", exec.ID, "run", exec.WorkflowRunID, "task", exec.TaskID, "status", exec.Status)
		r.fail(ctx, exec.ID, "execution lost: control plane restarted or runtime container gone")
	}
	return nil
}
