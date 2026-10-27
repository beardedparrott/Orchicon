package scheduler

// runtime_reap_sweep.go — TERMINAL RUNS MUST NOT LEAK THEIR CONTAINER.
//
// THE DEFECT THIS CLOSES. A run terminalised OUTSIDE the reconciler kept its
// runtime container. The reconcile-path reaper (workflow_reconciler.go, the
// "terminal runs … have their runtime container reaped here" branch) is
// UNREACHABLE for exactly the case its comment names, because the scan that
// feeds it selects non-terminal runs only:
//
//	ListPendingWorkflowRuns: WHERE status IN ('pending','running','paused')
//
// so a run that reached `aborted` is never returned, and the branch never runs
// for it. AbortWorkflow does not reap either — it ends the run, terminates the
// executions and aborts the live sessions, but never touches the container.
// Observed on the host: the daemon logged "pool release: resetting run
// container" for a COMPLETED run and nothing at all for the ABORTED one beside
// it, whose container was still `running` long after.
//
// WHY A SWEEP RATHER THAN A HOOK IN AbortWorkflow. A hook fixes one caller. The
// invariant is about the STATE, not the route into it: a run can be terminalised
// by the reconciler, by AbortWorkflow, by the sequence engine's StopSequence, or
// by a future path nobody has written yet. Sweeping terminal runs covers all of
// them with one predicate, and it is self-tuning — a run that loses its container
// simply never appears again.
//
// WHY IT LIVES ON THE WORKFLOW RECONCILER. It already owns the runtime lifecycle
// (EnsureForRun/ReapForRun) and already has a scan pass, so this is an EXTENSION
// of an existing reconciler rather than a seventh one. The alternative,
// terminating containers in AbortWorkflow, was rejected: it would need the
// runtime client injected into the workflow service, which currently has no
// dependency on it, and it would still miss every non-abort route.

import (
	"context"

	"github.com/beardedparrott/orchicon/internal/db"
)

// runtimeReapSweepLimit bounds one pass. The sweep is idempotent and cheap
// (ReapForRun on a run with no lease is a no-op), so a small page that is
// revisited on the next 200ms scan drains any backlog without a long pass.
const runtimeReapSweepLimit = 32

// sweepTerminalRunContainers reaps the container of every TERMINAL run that
// still shows evidence of having had one.
//
// IT IS BEST-EFFORT PER RUN: one failure is logged and the pass continues, so a
// single wedged container cannot starve the rest of the sweep. A failure is
// retried naturally on the next pass because the run still matches the query.
func (r *WorkflowReconciler) sweepTerminalRunContainers(ctx context.Context, tenantID string) {
	if !r.runtimeEnabled() {
		return
	}
	ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		r.log.Warn("runtime reap sweep: begin tx failed", "error", err)
		return
	}
	runs, err := db.ListTerminalRunsWithRuntimeEvidence(ctx, ttx.Tx, tenantID, runtimeReapSweepLimit)
	ttx.Rollback(ctx)
	if err != nil {
		r.log.Warn("runtime reap sweep: list terminal runs failed", "error", err)
		return
	}
	for _, run := range runs {
		if err := r.runtime.ReapForRun(context.Background(), run.ID); err != nil {
			r.log.Warn("runtime reap sweep: reap failed",
				"run", run.ID, "status", run.Status, "error", err)
			continue
		}
		r.log.Info("runtime reap sweep: reaped a terminal run's container",
			"run", run.ID, "status", run.Status)
	}
}
