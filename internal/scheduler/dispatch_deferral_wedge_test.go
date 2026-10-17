package scheduler

// The dispatch-deferral wedge: a step whose dispatch was DEFERRED for capacity
// must wait, not fail. DB-backed; skipped without ORCHICON_TEST_DSN.
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@postgres:5432/orchicon?sslmode=disable'
//	go test ./internal/scheduler/ -run 'TestProjectCapDeferral|TestDeferredStep|TestUnresumableRecovering' -v
//
// THE FIELD FAILURE THIS PINS (run 01M53DXZSF5G2T2Z592F7G5ETE, 2026-10-16). The
// run was created, its worktree provisioned, its runtime container started —
// and then it sat "running" for 20+ minutes with CurrentStep empty and no
// execution anywhere in the tenant. The UI said "waiting for dispatch".
// The log told the whole story in four lines:
//
//	17:39:42.659 workflow worker dispatched   step=step-q4xlbg6v ...
//	17:39:42.669 dispatch deferred: project at max concurrent runs
//	             {"task":"01M50TMA...","active":10,"limit":10}
//	17:39:57.853 task step failed, running recovery strategy  attempt=1 max_attempts=6
//	17:39:57.861 WARN post-commit recovery trigger failed
//	             error="recovery trigger: get execution: db: not found"
//
// The step WAS dispatched, but the TaskReconciler deferred the real work — the
// project was pinned at its max-concurrent-runs cap by ten unreaped `unhealthy`
// executions. No WorkerExecution was created. Fifteen seconds later the
// dispatchLinkGrace expired, the WorkflowReconciler could not tell a capacity
// deferral from a LOST dispatch, and it failed the step into the
// execution-keyed recovery engine — which then failed to find the execution
// that had never existed, leaving the step `recovering` and
// recoveryDispatchReady holding it forever, silently.
//
// Three separate assumptions had to be wrong for that to happen, and each is
// asserted here:
//
//  1. Only the PER-WORKER gate stamped a `_dispatch_wait` note; the PROJECT
//     gate did not — so a project-cap deferral left no evidence at all.
//  2. pollTaskStep treated "no execution link after the grace" as a failure,
//     with no way to ask whether the dispatch had merely been deferred.
//  3. recoveryDispatchReady held forever, silently, when a summarize_restart
//     step had no dead-execution identity — on the assumption that such a state
//     is "transient in-pass only". It is not transient when no execution was
//     ever created.

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
)

// seedWedgeRunAndStep seeds a running, worktree-skipped workflow run plus one
// task step run carrying an arbitrary status/result — the minimum pollTaskStep
// and recoveryDispatchReady need to be driven directly.
func seedWedgeRunAndStep(t *testing.T, pool *db.Pool, projectID, status string, result map[string]any) (db.WorkflowRunRow, db.WorkflowStepRunRow, string) {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)

	ticket, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: approvalTestTenant, ProjectID: projectID,
		Kind: domain.WorkItemKindTask, Title: "Deferral Wedge Ticket " + db.NewID()[:6],
		Status: domain.WorkItemRunning,
	})
	if err != nil {
		t.Fatal(err)
	}
	wf, err := db.CreateWorkflow(ctx, ttx.Tx, db.WorkflowRow{
		ID: db.NewID(), TenantID: approvalTestTenant, ProjectID: projectID,
		Name:           "Deferral Wedge Workflow " + db.NewID()[:6],
		CurrentVersion: 1, Status: "published", Type: "template",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateWorkflowVersion(ctx, ttx.Tx, db.WorkflowVersionRow{
		ID: db.NewID(), TenantID: approvalTestTenant, WorkflowID: wf.ID,
		Version: 1, Status: "published", Steps: []byte("[]"),
		Inputs: []byte("{}"), Outputs: []byte("{}"),
	}); err != nil {
		t.Fatal(err)
	}
	run, err := db.CreateWorkflowRun(ctx, ttx.Tx, db.WorkflowRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant, WorkflowID: wf.ID,
		WorkflowVersion: 1, ProjectID: projectID, Status: domain.WorkflowRunRunning,
		RunContext: []byte("{}"), WorkItemID: ticket.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err = db.UpdateWorkflowRun(ctx, ttx.Tx, approvalTestTenant, run.ID, run.Version, db.UpdateWorkflowRunFields{
		RuntimeReady:   boolPtr(true),
		WorktreeStatus: strPtr(domain.WorktreeSkipped),
	})
	if err != nil {
		t.Fatal(err)
	}
	// The step's `_work_item_id` is how pollTaskStep (and recoveryDispatchReady)
	// resolve the ticket; the rest of `result` is the case being exercised.
	result["_work_item_id"] = ticket.ID
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	step, err := db.CreateWorkflowStepRun(ctx, ttx.Tx, db.WorkflowStepRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant, WorkflowRunID: run.ID,
		StepID: "step-wedge-" + db.NewID()[:6], StepName: "Wedge Step", StepKind: "task",
		Status: status, Result: raw,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Start it well past dispatchLinkGrace: past that point the reconciler has
	// always concluded "the dispatch was lost" — which is the misjudgement.
	started := time.Now().Add(-10 * time.Minute)
	step, err = db.UpdateWorkflowStepRun(ctx, ttx.Tx, approvalTestTenant, step.ID, step.Version, db.UpdateWorkflowStepRunFields{
		StartedAt: &started,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return run, step, ticket.ID
}

// TestProjectCapDeferralStampsWaitNote: the PROJECT max-concurrent-runs gate
// must leave the same evidence the per-WORKER gate leaves. Without it the
// deferral is indistinguishable from a lost dispatch downstream — the first of
// the three wrong assumptions, and the one that made the other two reachable.
func TestProjectCapDeferralStampsWaitNote(t *testing.T) {
	purgeScanTenant(t, approvalTestPool(t))
	env := newSequenceTestEnv(t)
	seedReadyAdapter(t, env.pool)
	// concurrency_limit 0 = unlimited, so ONLY the project gate can fire here.
	workerID := seedGateWorker(t, env.pool, 0)
	ctx := context.Background()

	rec := NewTaskReconciler(env.pool, slog.Default(), testDispatcher(&manifestCaptureBridge{}))
	rec.SetDispatchLimiter(stubDispatchLimiter{effLimit: func(string) int { return 1 }})

	// Saturate the PROJECT with one active (non-terminal) execution.
	first := seedGateTask(t, env.pool, env.proj.ID, workerID)
	if err := rec.reconcileOne(ctx, first.ID, ""); err != nil {
		t.Fatalf("dispatch saturating task: %v", err)
	}

	run, stepA, _, ticketID := seedGateWorkflowRunWithTwoSteps(t, env.pool, env.proj.ID, workerID)
	if err := rec.reconcileOne(ctx, ticketID, stepA.ID); err != nil {
		t.Fatalf("dispatch step A: %v", err)
	}

	deferred := mustGetStepRun(t, env.pool, run.ID, stepA.StepID)
	if deferred.WorkerExecutionID != "" {
		t.Fatalf("step A produced an execution despite the project being at its cap")
	}
	reason, ok := dispatchWaitReason(deferred.Result)
	if !ok {
		t.Fatalf("project-capped step run carries no _dispatch_wait note: %s\n"+
			"Without it the WorkflowReconciler cannot tell this wait apart from a lost dispatch.", deferred.Result)
	}
	if !strings.Contains(reason, "project capacity 1/1") {
		t.Fatalf("_dispatch_wait = %q, want a note naming the PROJECT capacity 1/1", reason)
	}
}

// TestDeferredStepReturnsToReadyInsteadOfRecovering: a task step with no
// execution link is a DEFERRED dispatch, not a failure. It must go back to
// `ready` (the state the dispatch section re-attempts) and must NOT be routed to
// the execution-keyed recovery engine — which is what turned a healthy waiting
// run into a permanent wedge.
func TestDeferredStepReturnsToReadyInsteadOfRecovering(t *testing.T) {
	purgeScanTenant(t, approvalTestPool(t))
	env := newSequenceTestEnv(t)
	ctx := context.Background()

	run, step, _ := seedWedgeRunAndStep(t, env.pool, env.proj.ID, domain.StepRunRunning, map[string]any{
		// Exactly what the project gate now stamps.
		"_dispatch_wait": "project capacity 10/10 — waiting for a slot",
	})

	rc := NewWorkflowReconciler(env.pool, slog.Default(), nil, nil, nil, nil)
	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)

	var triggers []recoveryTriggerReq
	terminal, failed, err := rc.pollTaskStep(ctx, ttx.Tx, approvalTestTenant, &run, step, "", map[string]db.WorkflowStepRunRow{}, &triggers)
	if err != nil {
		t.Fatalf("pollTaskStep on a deferred step: %v", err)
	}
	if terminal || failed {
		t.Fatalf("a capacity-deferred step was judged terminal=%v failed=%v — the deferral was misread as a lost dispatch", terminal, failed)
	}
	if len(triggers) != 0 {
		t.Fatalf("a capacity-deferred step enqueued %d recovery trigger(s).\n"+
			"Recovery is keyed to the FAILED EXECUTION and there is none, so the trigger can only fail\n"+
			"with \"get execution: db: not found\" and leave the step recovering forever.", len(triggers))
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	after := mustGetStepRun(t, env.pool, run.ID, step.StepID)
	if after.Status != domain.StepRunReady {
		t.Fatalf("deferred step status = %q, want %q — it must be dispatchable again so it resumes when a slot frees",
			after.Status, domain.StepRunReady)
	}
}

// TestUnresumableRecoveringStepFailsLoudPastTheStallWindow: the safety valve.
// A summarize_restart step with no dead-execution identity can never be
// resumed, so recoveryDispatchReady's hold must be BOUNDED. It used to hold
// silently and forever, which is precisely how the run sat "running" with the
// UI reading "waiting for dispatch" and nothing in the log.
func TestUnresumableRecoveringStepFailsLoudPastTheStallWindow(t *testing.T) {
	purgeScanTenant(t, approvalTestPool(t))
	env := newSequenceTestEnv(t)
	ctx := context.Background()

	run, step, _ := seedWedgeRunAndStep(t, env.pool, env.proj.ID, domain.StepRunRecovering, map[string]any{
		// summarize_restart + NO _failed_execution_id: the state a capacity
		// deferral misrouted into recovery leaves behind. There is no dead
		// execution to find, so no recovery can ever materialize.
		"_recovery_strategy": "summarize_restart",
		"_recovering_since":  time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano),
	})

	rc := NewWorkflowReconciler(env.pool, slog.Default(), nil, nil, nil, nil)
	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)

	// Inside the stall window: hold, quietly. A recovery trigger deferred to
	// post-commit can still land, so holding here is correct. (The window must
	// comfortably exceed the age of `_recovering_since` below — the valve keys
	// off the step's OWN recovering age, not the run's.)
	t.Setenv("ORCHICON_RECOVERING_STALL_TIMEOUT", "24h")
	ready, err := rc.recoveryDispatchReady(ctx, ttx.Tx, approvalTestTenant, run, step)
	if ready {
		t.Fatalf("recoveryDispatchReady = true for a step whose recovery cannot exist — it must hold, not dispatch cold")
	}
	if err != nil {
		t.Fatalf("within the stall window recoveryDispatchReady must hold silently, got error: %v", err)
	}

	// Past the window: fail LOUD, so the run terminalizes with a Retry button
	// instead of holding forever with no explanation.
	t.Setenv("ORCHICON_RECOVERING_STALL_TIMEOUT", "1ms")
	ready, err = rc.recoveryDispatchReady(ctx, ttx.Tx, approvalTestTenant, run, step)
	if ready {
		t.Fatalf("recoveryDispatchReady = true past the stall window; want a loud failure")
	}
	if err == nil {
		t.Fatal("an un-resumable recovering step held silently past the stall window — this is the wedge: no\n" +
			"log line, no terminal step, no Retry button, the run \"running\" forever.")
	}
	if !strings.Contains(err.Error(), "stuck recovering") {
		t.Fatalf("past-window error = %v, want the stalled-recovering failure the valve produces", err)
	}
}

// TestReaperListingCoversEverySlotHoldingStatus pins the invariant that made
// the wedge PERMANENT rather than merely slow.
//
// The dispatch guard counts every execution in activeExecutionStatuses —
// dispatching/running/healthy/stalled/unhealthy/terminating — so each of them
// HOLDS A DISPATCH SLOT. The reaper, the thing that releases slots, listed only
// ('running','healthy','dispatching'). An execution that a liveness probe moved
// to `unhealthy` or `stalled` therefore held a slot while being INVISIBLE to
// the reaper: nothing could ever release it. Field evidence: ten such
// executions, the oldest 2026-09-06, pinning a project at 10/10 — and once the
// cap is pinned, every dispatch is deferred, which is how the run in this file's
// header ended up "waiting for dispatch" forever.
//
// The assertion is the two definitions agreeing, because that is the property
// that was violated — not the enumeration of any particular status.
func TestReaperListingCoversEverySlotHoldingStatus(t *testing.T) {
	purgeScanTenant(t, approvalTestPool(t))
	env := newSequenceTestEnv(t)
	ctx := context.Background()

	slotHolding := []string{
		domain.ExecutionDispatching,
		domain.ExecutionRunning,
		domain.ExecutionHealthy,
		domain.ExecutionStalled,
		domain.ExecutionUnhealthy,
		domain.ExecutionTerminating,
	}
	// Terminal statuses have released their slot and must NOT be listed: the
	// reaper would otherwise "reap" finished work.
	terminal := []string{domain.ExecutionSucceeded, domain.ExecutionFailed}

	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)

	now := time.Now().UTC()
	seed := func(status string) string {
		t.Helper()
		id := db.NewID()
		if _, err := db.CreateExecution(ctx, ttx.Tx, db.ExecutionRow{
			ID: id, TenantID: approvalTestTenant, ProjectID: env.proj.ID,
			WorkerID: "w-gate-reaper", WorkerVersion: 1,
			Status: status, HealthState: status, StartedAt: &now,
		}); err != nil {
			t.Fatalf("create execution in status %q: %v", status, err)
		}
		return id
	}
	byStatus := map[string]string{}
	for _, s := range slotHolding {
		byStatus[s] = seed(s)
	}
	for _, s := range terminal {
		byStatus[s] = seed(s)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	ttx2, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx2.Rollback(ctx)
	listed, err := db.ListRunningExecutions(ctx, ttx2.Tx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range listed {
		got[e.ID] = true
	}

	for _, s := range slotHolding {
		if !got[byStatus[s]] {
			t.Errorf("an execution in status %q is NOT returned by ListRunningExecutions.\n"+
				"It holds a dispatch slot (the cap counts it) but the reaper cannot see it, so nothing can ever\n"+
				"release that slot — which is how a project reaches its cap and stays there forever.", s)
		}
	}
	for _, s := range terminal {
		if got[byStatus[s]] {
			t.Errorf("a TERMINAL execution (%q) is returned by ListRunningExecutions; the reaper must not\n"+
				"examine work that has already released its slot.", s)
		}
	}
}
