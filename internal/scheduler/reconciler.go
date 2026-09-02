// Package scheduler implements the TaskReconciler — the control loop
// that reconciles desired work-item state with observed runtime state
// (docs/03_Scheduler_and_Runtime_Design.md §2–4). It is the only
// component permitted to create WorkerExecutions and call
// adapter.Start (docs/03 §8 invariant #1).
//
// The dispatch flow (docs/03 §4):
//  1. Filter tasks in "ready" state.
//  2. For each, check dependencies are satisfied (docs/02 §4 #1).
//  3. Select a Worker (rule-based: runtime/model compatibility, health,
//     concurrency — docs/03 §4.1).
//  4. Select an Adapter (matching kind, healthy heartbeat, free capacity
//     — docs/03 §4.2).
//  5. Create a WorkerExecution (status=dispatching).
//  6. Call the adapter bridge to start the execution.
//  7. Transition the task to "assigned" and requeue.
package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/eventbus"
	"github.com/beardedparrott/orchicon/internal/reconciler"
	"github.com/beardedparrott/orchicon/internal/workflow"
	"github.com/jackc/pgx/v5"
)

// heartbeatTTL is how long an adapter heartbeat remains valid for
// selection (docs/03 §5: heartbeat age > 60s → unhealthy).
const heartbeatTTL = 60 * time.Second

// scanBatchSize caps how many work items the TaskReconciler's empty-key
// scan pass processes per tick (docs/03 §4: "Limited to a batch per tick
// so one scan doesn't monopolize the reconciler goroutine"). It is the
// candidate BATCH for one pass (how many items the pass examines), which
// is deliberately DECOUPLED from the concurrency bound below.
const scanBatchSize = 16

// defaultDispatchConcurrency is the default bound on how many ready work
// items the scan pass dispatches CONCURRENTLY in one pass (docs/03 §4,
// parallel dispatch). Conservative: each in-flight dispatch holds one pgx
// pool connection for its short transaction (pool default max conns =
// max(4, NumCPU)) and the daemon has a per-execution container budget.
// Overridable via Config.DispatchConcurrency (ORCHICON_DISPATCH_CONCURRENCY).
const defaultDispatchConcurrency = 4

// ConcurrencyLimiter reports the maximum number of concurrent dispatches
// allowed for a project. It is the seam for the per-project concurrency
// guard (sibling concurrency-guards work item): when set on the
// TaskReconciler, the effective per-pass dispatch bound is the minimum of
// the global DispatchConcurrency and the per-project limits of the
// candidate items. Nil (the default) applies only the global bound.
type ConcurrencyLimiter interface {
	// Limit returns the maximum concurrent dispatches allowed for a
	// project. A value <= 0 means the project imposes no additional
	// restriction (the global bound alone applies).
	Limit(ctx context.Context, projectID string) int
}

// DispatchLimiter resolves the configured max-concurrent-runs caps for a
// project (concurrency-guards work item, architecture-notes
// per-project-dispatch-limits.md). It is the seam for:
//
//   - D2 — the admission gate in TaskReconciler.reconcileOne, which holds
//     a dispatch (returns without creating an execution, leaving the item
//     'ready') when the project's effective cap is reached;
//   - D3 — the WorktreeReconciler's in-place (non-repo) serialization
//     gate, which atomically admits non-repo runs so two never share the
//     mutable project_dir.
//
// Both methods read in the caller's transaction so the limit value is
// consistent with the count queries that follow.
type DispatchLimiter interface {
	// Limit returns the effective max-concurrent-runs cap for the project:
	// min(tenant.max_concurrent_runs, project.max_concurrent_runs), where 0
	// on either side means "no additional restriction from that side".
	Limit(ctx context.Context, tx pgx.Tx, tenantID, projectID string) (int, error)
	// InPlaceLimit returns the non-repo in-place serialization limit for
	// the project (default 1 = serialize, unless the project explicitly
	// opts into concurrency with max_concurrent_runs > 1).
	InPlaceLimit(ctx context.Context, tx pgx.Tx, tenantID, projectID string) (int, error)
}

// dbDispatchLimiter is the production DispatchLimiter, backed by the
// tenant_settings + projects tables (db.GetDispatchLimitValues).
type dbDispatchLimiter struct{}

// DBDispatchLimiter is the exported constructor for the production
// DispatchLimiter. It is stateless — the resolver reads live from the
// database via the reconciler's transaction.
func DBDispatchLimiter() DispatchLimiter { return dbDispatchLimiter{} }

func (dbDispatchLimiter) Limit(ctx context.Context, tx pgx.Tx, tenantID, projectID string) (int, error) {
	return db.GetEffectiveDispatchLimit(ctx, tx, tenantID, projectID)
}

func (dbDispatchLimiter) InPlaceLimit(ctx context.Context, tx pgx.Tx, tenantID, projectID string) (int, error) {
	tenantLimit, projectLimit, err := db.GetDispatchLimitValues(ctx, tx, tenantID, projectID)
	if err != nil {
		return 0, err
	}
	return db.InPlaceLimit(tenantLimit, projectLimit), nil
}

// TaskReconciler implements the reconciler.Reconciler interface for the
// "task" kind. It polls the work_items table for ready tasks and
// dispatches them via the AdapterBridge resolved from the Dispatcher by
// the execution's adapter kind (adapter.AdapterKind(manifest.ModelRef)
// .Adapter).
type TaskReconciler struct {
	pool             *db.Pool
	log              *slog.Logger
	dispatcher       *Dispatcher
	eventPub         eventbus.Publisher                      // direct NATS publisher for low-latency streaming (bypasses outbox relay)
	workflowNotifier func(ctx context.Context, runID string) // enqueues run for WorkflowReconciler on task completion

	// pendingWrittenFiles holds the file paths a running execution's
	// session actually wrote (OnWrittenFiles), keyed by execution ID. They
	// are folded into the execution's _touched_files when it terminates so
	// the next worker is told to read exactly what was produced. Guarded
	// by writtenMu.
	writtenMu           sync.Mutex
	pendingWrittenFiles map[string][]string

	// blockedMu guards the blocked re-evaluation rotation cursor below.
	// The scan pass re-checks a rotating window of blocked tasks so every
	// blocked item is eventually re-evaluated (not just the same oldest
	// rows) when the backlog exceeds scanBatchSize.
	blockedMu     sync.Mutex
	blockedCursor int

	// dispatchConcurrency bounds how many reconcileOne calls the scan pass
	// runs concurrently (in flight at once) when fanning out its candidate
	// batch. Zero means defaultDispatchConcurrency. Set via
	// SetDispatchConcurrency.
	dispatchConcurrency int
	// concurrencyLimiter, when set, further bounds the pass by the
	// per-project limits of the candidate items (concurrency-guards seam).
	// Set via SetConcurrencyLimiter.
	concurrencyLimiter ConcurrencyLimiter
	// dispatchLimiter, when set, applies the per-project/tenant
	// max-concurrent-runs admission gate inside reconcileOne (concurrency
	// guards D2). Set via SetDispatchLimiter.
	dispatchLimiter DispatchLimiter
	// dispatchOverlap is a test-only hook invoked with the current number
	// of reconcileOne calls in flight whenever the scan fan-out acquires a
	// semaphore slot, so tests can assert the concurrency bound without
	// coupling to the adapter bridge. Nil in production.
	dispatchOverlap func(inFlight int)
}

// NewTaskReconciler creates a TaskReconciler. The dispatcher routes each
// dispatch to the AdapterBridge registered for the execution's adapter
// kind (parsed from the worker's model_ref at dispatch time) — never a
// hardcoded singleton bridge.
func NewTaskReconciler(pool *db.Pool, log *slog.Logger, dispatcher *Dispatcher) *TaskReconciler {
	return &TaskReconciler{pool: pool, log: log, dispatcher: dispatcher}
}

// SetDispatchConcurrency sets the per-pass concurrency bound for the scan
// pass (ORCHICON_DISPATCH_CONCURRENCY). Values are clamped to
// [1, scanBatchSize]; zero/negative falls back to the default (4) at scan
// time. Setters are called at startup, before the reconciler loop runs.
func (r *TaskReconciler) SetDispatchConcurrency(n int) {
	if n < 1 {
		n = 1
	}
	if n > scanBatchSize {
		n = scanBatchSize
	}
	r.dispatchConcurrency = n
}

// SetConcurrencyLimiter installs the per-project concurrency guard seam
// (concurrency-guards work item). When set, the effective per-pass limit
// is the minimum of the global DispatchConcurrency and the per-project
// limits reported for the candidate items. Nil keeps the global bound.
func (r *TaskReconciler) SetConcurrencyLimiter(l ConcurrencyLimiter) {
	r.concurrencyLimiter = l
}

// SetDispatchLimiter installs the per-project/tenant max-concurrent-runs
// admission seam (concurrency-guards work item D2). When set, reconcileOne
// holds a dispatch — returns without creating an execution, leaving the
// item 'ready' for the next scan pass — whenever the project is at its
// effective cap. Nil (the default) keeps today's unbounded dispatch.
func (r *TaskReconciler) SetDispatchLimiter(l DispatchLimiter) {
	r.dispatchLimiter = l
}

// SetRecoveryTrigger is deprecated. Recovery is triggered exclusively by
// explicit `recover` steps on the workflow canvas (docs/06 §1).
func (r *TaskReconciler) SetRecoveryTrigger(rt RecoveryTrigger) {}

// SetEventPublisher injects a direct NATS publisher for streaming
// execution events. When set, the reconciler publishes events directly
// to NATS after each callback commits, bypassing the outbox relay's
// 500ms poll interval. The outbox continues to be written for durability
// and catch-up on reconnect; the direct publish provides near-zero
// latency for the live frontend event stream.
func (r *TaskReconciler) SetEventPublisher(pub eventbus.Publisher) { r.eventPub = pub }

// SetWorkflowNotifier injects a callback that is called when a work
// item transitions to a terminal state (succeeded/failed). The callback
// should enqueue the workflow run ID so the WorkflowReconciler picks it
// up immediately instead of waiting for the next scan pass.
func (r *TaskReconciler) SetWorkflowNotifier(fn func(ctx context.Context, runID string)) {
	r.workflowNotifier = fn
}

// Kind returns the reconciler kind (docs/03 §2.1).
func (r *TaskReconciler) Kind() string { return "task" }

// DispatchTask implements scheduler.TaskDispatcher. It dispatches a
// single ready task synchronously. The WorkflowReconciler calls this
// after its own transaction commits so the step run + prompt are visible
// to the TaskReconciler's internal dispatch transaction (docs/03 §8
// invariant #1: only the TaskReconciler creates WorkerExecutions).
//
// stepRunID scopes the dispatch to a workflow step run ("" for
// standalone dispatch). For a workflow step the work item is a shared
// input reference — the step's execution is keyed by its step run, the
// composite prompt is read from the step run, and the ticket is never
// mutated (no status gate, no assigned/ready flip).
func (r *TaskReconciler) DispatchTask(ctx context.Context, taskID, stepRunID string) error {
	return r.reconcileOne(ctx, taskID, stepRunID)
}

// Reconcile processes a single task key. The key is the task (work item)
// ID. It is idempotent: re-running a pass for a task converges to the
// same state (docs/03 §1).
//
// The reconciler is driven by the manager's work queue, which enqueues
// ready tasks. When called with an empty key, it scans for ready tasks
// and dispatches them (docs/03 §4) — this is the scan pass the manager
// invokes when the queue is empty, which lets workflow task steps (and
// any other ready task) get dispatched without an explicit enqueue path.
func (r *TaskReconciler) Reconcile(ctx context.Context, key string) reconciler.Result {
	if key == "" {
		return r.scan(ctx)
	}
	if err := r.reconcileOne(ctx, key, ""); err != nil {
		return reconciler.Result{Error: err}
	}
	return reconciler.Result{}
}

// scan is the empty-key scan pass (docs/03 §4): find ready + blocked tasks
// and dispatch them with a BOUNDED IN-PASS FAN-OUT. Independent,
// dependency-satisfied items dispatch CONCURRENTLY (up to
// dispatchConcurrency in flight) instead of one-at-a-time, so a tick's
// wall time becomes MAX(item time) instead of SUM(item time).
//
// The candidate set is built exactly as before — ready tasks
// (priority-ordered) first, then a rotating window of blocked tasks filling
// the rest of the per-tick batch (scanBatchSize) — but the batch cap is
// DECOUPLED from the concurrency bound: every candidate in the batch goes
// through the unchanged reconcileOne in one pass, with at most
// dispatchConcurrency in flight at any moment. Keeping the batch at 16
// preserves the blocked re-evaluation rotation cadence under load — capping
// candidates at the concurrency limit (4) would starve the blocked window
// whenever ≥4 ready items exist, stalling dependency clears indefinitely.
//
// The fan-out waits for all in-flight items before returning, so the
// manager stays single-scan-per-tick and the blocked rotation cursor stays
// owned by the scan goroutine (no new races on blockedMu). Per-item errors
// are logged as warnings and never fail the pass (same as the serial scan).
func (r *TaskReconciler) scan(ctx context.Context) reconciler.Result {
	// v0.1: single dev tenant.
	tenantID := "tnt_dev"
	ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return reconciler.Result{Error: err}
	}
	ready, err := db.ListReadyTasks(ctx, ttx.Tx, tenantID)
	if err != nil {
		ttx.Rollback(ctx)
		return reconciler.Result{Error: fmt.Errorf("scan ready tasks: %w", err)}
	}
	// Blocked tasks are re-evaluated every pass so a newly satisfied
	// dependency gate flips them back to ready (and dispatches) without
	// waiting on a notifier. The re-evaluation window ROTATES: with more
	// blocked tasks than the per-tick batch, a fixed oldest-first window
	// would re-scan the same rows forever and a blocker that turned
	// terminal past the window would never clear.
	blocked, err := db.ListBlockedTasks(ctx, ttx.Tx, tenantID)
	ttx.Rollback(ctx)
	if err != nil {
		return reconciler.Result{Error: fmt.Errorf("scan blocked tasks: %w", err)}
	}
	// Ready tasks are dispatch candidates and must not be starved by a
	// blocked backlog; blocked re-evaluation fills the rest of the batch.
	candidates, examined := r.scanCandidates(ready, blocked)
	r.advanceBlockedCursor(len(blocked), examined)
	if len(candidates) == 0 {
		return reconciler.Result{}
	}
	r.dispatchCandidates(ctx, candidates)
	return reconciler.Result{}
}

// scanCandidates builds this pass's candidate set: ready tasks
// (priority-ordered) first, then the rotating blocked window up to the
// remaining batch. Ready items get priority; blocked re-evaluation fills
// the rest — the same budget semantics as the serial scan. It returns the
// candidates and the number of blocked items examined (used to advance
// the rotation cursor). The candidate batch is scanBatchSize regardless of
// the concurrency bound (see scan).
func (r *TaskReconciler) scanCandidates(ready, blocked []db.WorkItemRow) ([]db.WorkItemRow, int) {
	budget := scanBatchSize
	var candidates []db.WorkItemRow
	for _, task := range ready {
		if budget == 0 {
			break
		}
		candidates = append(candidates, task)
		budget--
	}
	examined := 0
	if budget > 0 && len(blocked) > 0 {
		n := len(blocked)
		start := r.blockedWindowStart(n)
		for k := 0; k < n && examined < budget; k++ {
			candidates = append(candidates, blocked[(start+k)%n])
			examined++
		}
	}
	return candidates, examined
}

// dispatchCandidates fans out reconcileOne across the pass's candidate
// items with bounded concurrency (effectiveDispatchLimit), waiting for
// every in-flight item before returning. Each candidate still goes through
// the UNCHANGED reconcileOne — the ready→assigned version CAS prevents
// double dispatch, and each dispatch creates its own WorkerExecution /
// execution container, so independent items share no mutable state.
func (r *TaskReconciler) dispatchCandidates(ctx context.Context, candidates []db.WorkItemRow) {
	limit := r.effectiveDispatchLimit(ctx, candidates)
	if limit > len(candidates) {
		limit = len(candidates)
	}
	if limit < 1 {
		limit = 1
	}
	r.log.Info("dispatch pass", "candidates", len(candidates), "concurrency", limit)
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	var inFlight atomic.Int32
	for _, task := range candidates {
		wg.Add(1)
		go func(task db.WorkItemRow) {
			defer wg.Done()
			sem <- struct{}{}
			cur := inFlight.Add(1)
			if r.dispatchOverlap != nil {
				r.dispatchOverlap(int(cur))
			}
			defer func() {
				inFlight.Add(-1)
				<-sem
			}()
			if err := r.reconcileOne(ctx, task.ID, ""); err != nil {
				r.log.Warn("scan: dispatch task failed", "task", task.ID, "error", err)
			}
		}(task)
	}
	wg.Wait()
}

// effectiveDispatchLimit returns the concurrency bound for this pass: the
// configured global dispatchConcurrency (default 4), further restricted to
// the minimum POSITIVE per-project limit reported by the ConcurrencyLimiter
// when one is set (concurrency-guards seam). Clamped to [1, scanBatchSize].
func (r *TaskReconciler) effectiveDispatchLimit(ctx context.Context, candidates []db.WorkItemRow) int {
	limit := r.dispatchConcurrency
	if limit < 1 {
		limit = defaultDispatchConcurrency
	}
	if limit > scanBatchSize {
		limit = scanBatchSize
	}
	if r.concurrencyLimiter != nil {
		for _, c := range candidates {
			if pl := r.concurrencyLimiter.Limit(ctx, c.ProjectID); pl > 0 && pl < limit {
				limit = pl
			}
		}
	}
	return limit
}

// blockedWindowStart returns the rotation offset for this pass's blocked
// re-evaluation window. It is an in-memory cursor (not persisted): a
// reconciler restart simply resumes rotation from the start, and the
// guarantee is only that every blocked item is eventually rechecked.
func (r *TaskReconciler) blockedWindowStart(n int) int {
	r.blockedMu.Lock()
	defer r.blockedMu.Unlock()
	if n <= 0 {
		return 0
	}
	return r.blockedCursor % n
}

// advanceBlockedCursor records how many blocked tasks this pass examined so
// the next pass picks up further along the list.
func (r *TaskReconciler) advanceBlockedCursor(n, examined int) {
	if n <= 0 || examined <= 0 {
		return
	}
	r.blockedMu.Lock()
	defer r.blockedMu.Unlock()
	r.blockedCursor = (r.blockedCursor + examined) % n
}

// reconcileOne dispatches a single task. When stepRunID is set it is a
// workflow-step dispatch, scoped to that step run: the ticket is a shared
// input reference, so there is no "ready" gate and no status mutation on
// the ticket, and the worker comes from the step (stored on the step run)
// rather than the ticket's assigned_worker_ref. With an empty stepRunID it
// is a standalone dispatch (docs/03 §4).
func (r *TaskReconciler) reconcileOne(ctx context.Context, taskID, stepRunID string) error {
	// We need the tenant to scope the transaction. The task carries it.
	// First, read the task without a tenant tx (RLS will block us), so
	// we resolve the tenant from the work item row via a query that
	// sets a temporary tenant context. In practice, the poll loop that
	// enqueues tasks knows the tenant; for v0.1 we scan all tenants
	// via the dev tenant. This is acceptable because v0.1 has a single
	// dev tenant; multi-tenant scheduling arrives with auth (Phase 9).
	tenantID := "tnt_dev"
	ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer ttx.Rollback(ctx)

	task, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, taskID)
	if err != nil {
		if err == db.ErrNotFound {
			return nil // task deleted; nothing to reconcile
		}
		return fmt.Errorf("get task: %w", err)
	}

	// Workflow-step dispatch: keyed by the step run, not the ticket.
	// The step run carries the worker + composite prompt written by the
	// WorkflowReconciler; the ticket is never gated on "ready" or
	// mutated here (parallel steps bound to the same ticket each get
	// their own execution).
	var stepRun *db.WorkflowStepRunRow
	if stepRunID != "" {
		sr, err := db.GetWorkflowStepRun(ctx, ttx.Tx, tenantID, stepRunID)
		if err != nil {
			if err == db.ErrNotFound {
				return nil // step run gone; nothing to dispatch
			}
			return fmt.Errorf("get step run: %w", err)
		}
		// Idempotency: already linked to an execution (a prior inline
		// dispatch won) — don't double-dispatch this step.
		if sr.WorkerExecutionID != "" {
			return nil
		}
		stepRun = &sr
	} else {
		// Standalone dispatch. Only "ready" (docs/03 §4) and "blocked"
		// tasks are processed:
		//
		//   - ready + unsat deps → flip to blocked (persisted) so the stall
		//     is SURFACED instead of silently requeued;
		//   - blocked + satisfied deps → flip to ready, then fall through
		//     to dispatch in this same pass;
		//   - blocked + unsat deps → stay blocked (return nil).
		//
		// A blocked item must clear back to ready before any dispatch —
		// dispatch only ever proceeds from ready here, so the "no dispatch
		// for blocked" invariant holds by construction.
		switch task.Status {
		case domain.WorkItemReady, domain.WorkItemBlocked:
		default:
			return nil
		}
		// Skip items that belong to a WORKFLOW RUN: they are dispatched
		// exclusively by the WorkflowReconciler's inline (step-run)
		// dispatch, never by the standalone scan. This covers both the
		// shared ticket and per-step artifacts like the worker-backed
		// approval ticket (created "ready" with an assigned worker). Two
		// failure modes without it:
		//   (a) during the run — the scan dispatches the approval ticket
		//       in parallel with the inline dispatch → TWO executions for
		//       one step;
		//   (b) after the run reaches terminal — the orphaned "ready"
		//       approval ticket is no longer bound to an *active* run, so
		//       a boundToActiveRun guard wouldn't catch it, and the scan
		//       dispatches a ghost execution into the void.
		if task.WorkflowRunID != "" {
			return nil
		}
		// Check dependencies satisfied (docs/02 §4 #1, docs/03 §4).
		satisfied, err := db.CheckDependenciesSatisfied(ctx, ttx.Tx, tenantID, task.ID)
		if err != nil {
			return fmt.Errorf("check deps: %w", err)
		}
		if !satisfied {
			// Dependency stall. A ready task parks as blocked (persisted) —
			// surfaced, never silently requeued. A blocked task stays
			// blocked; the scan re-evaluates it on the next pass.
			if task.Status == domain.WorkItemReady {
				status := domain.WorkItemBlocked
				if _, err := db.UpdateWorkItem(ctx, ttx.Tx, tenantID, task.ID, task.Version, db.UpdateWorkItemFields{
					Status: &status,
				}); err != nil {
					return fmt.Errorf("park task blocked: %w", err)
				}
				// Commit the park so the surfaced status is durable — the
				// dispatch path below is skipped for a parked task (the
				// deferred Rollback is then a no-op).
				if err := ttx.Commit(ctx); err != nil {
					return fmt.Errorf("commit park blocked: %w", err)
				}
			}
			return nil
		}
		// Gate satisfied: a blocked task flips back to ready and dispatches
		// in this same pass (carry the fresh version forward so the
		// assigned transition below still CAS-matches).
		if task.Status == domain.WorkItemBlocked {
			status := domain.WorkItemReady
			updated, err := db.UpdateWorkItem(ctx, ttx.Tx, tenantID, task.ID, task.Version, db.UpdateWorkItemFields{
				Status: &status,
			})
			if err != nil {
				return fmt.Errorf("clear task blocked: %w", err)
			}
			task = updated
		}
	}

	// Runtime-serve readiness belt-and-suspenders: never dispatch a
	// workflow-run execution before the run's runtime opencode serve is
	// PROVEN usable (runtime_ready=true). The WorkflowReconciler holds
	// step progression until the gate flips, but a race — e.g. the inline
	// DispatchTask firing while the async probe is still flipping the gate
	// — must not create an execution that immediately hits a cold serve.
	runID := task.WorkflowRunID
	if stepRun != nil {
		runID = stepRun.WorkflowRunID
	}
	if runID != "" {
		if rtx, err := r.pool.BeginTenantTx(context.Background(), tenantID); err == nil {
			if run, gerr := db.GetWorkflowRun(context.Background(), rtx.Tx, tenantID, runID); gerr == nil {
				if run.Status == domain.WorkflowRunRunning && !run.RuntimeReady {
					_ = rtx.Rollback(context.Background())
					r.log.Debug("deferring dispatch: workflow runtime serve not ready", "task", task.ID, "run", runID)
					return nil
				}
			}
			_ = rtx.Rollback(context.Background())
		}
	}

	// Select a Worker (docs/03 §4.1: rule-based). For a workflow step the
	// worker is pinned by the STEP (stored on the step run by the
	// WorkflowReconciler), not the ticket.
	var version db.WorkerVersionRow
	if stepRun != nil {
		version, err = r.workerVersionForStepRun(ctx, ttx.Tx, tenantID, *stepRun)
		if err != nil {
			r.log.Warn("no suitable worker for step run", "task", task.ID, "step_run", stepRun.ID, "error", err)
			return nil
		}
	} else {
		_, version, err = r.selectWorker(ctx, ttx.Tx, tenantID, task)
		if err != nil {
			// No suitable worker — requeue with backoff.
			r.log.Warn("no suitable worker for task", "task", task.ID, "error", err)
			return nil
		}
	}

	// Select an Adapter (docs/03 §4.2). ADR-0005 D6: the row-selection kind
	// comes from the version's runtime_ref when set (all pre-existing
	// behavior — a divergent runtime_ref keeps its terminal
	// failed_to_start semantics rather than being silently repointed); an
	// EMPTY runtime_ref used to query kind "" — matching zero rows and
	// requeueing forever (the dispatch black hole) — so it falls back to
	// the model_ref's parsed adapter kind (the same single source of truth
	// the bridge Resolve at dispatch uses), then the legacy default kind.
	adapter, err := r.selectAdapter(ctx, ttx.Tx, tenantID, resolveAdapterRowKind(version.RuntimeRef, version.ModelRef))
	if err != nil {
		r.log.Warn("no suitable adapter for task", "task", task.ID, "worker", version.WorkerID, "error", err)
		return nil
	}

	// Concurrency guard (D2, architecture-notes/per-project-dispatch-limits
	// .md): never dispatch beyond the project's effective
	// max-concurrent-runs cap. Count-check-create has a TOCTOU window, so a
	// transient overshoot is possible — that is a resource spike, not a
	// correctness break, because the working-tree invariant is enforced
	// STRUCTURALLY by the WorktreeReconciler (D3). The item (or step run)
	// stays 'ready' and the next scan pass re-evaluates it, so it "waits
	// until a slot frees" by construction.
	if r.dispatchLimiter != nil {
		limit, lerr := r.dispatchLimiter.Limit(ctx, ttx.Tx, tenantID, task.ProjectID)
		if lerr != nil {
			return fmt.Errorf("resolve dispatch limit: %w", lerr)
		}
		if limit > 0 {
			active, aerr := db.CountActiveExecutionsForProject(ctx, ttx.Tx, tenantID, task.ProjectID)
			if aerr != nil {
				return fmt.Errorf("count active executions: %w", aerr)
			}
			if active >= limit {
				r.log.Info("dispatch deferred: project at max concurrent runs",
					"task", task.ID, "project", task.ProjectID, "active", active, "limit", limit)
				return nil
			}
		}
	}

	// Create WorkerExecution (docs/03 §4: createWorkerExecution).
	// Check if the work item's results indicate this is a follow-up
	// execution (created by CreateFollowUpExecution).
	var isFollowUp bool
	if len(task.Results) > 0 {
		var taskResults map[string]any
		if err := json.Unmarshal(task.Results, &taskResults); err == nil {
			if v, ok := taskResults["_is_follow_up"].(string); ok && v == "true" {
				isFollowUp = true
			}
		}
	}
	now := time.Now().UTC()
	// Look up the workflow step run's iteration (loop number) so the
	// execution can display "Work Item Name - Worker Name - Loop #" in
	// the frontend. Defaults to 0 for direct-dispatch (no workflow).
	var iteration int
	workflowRunID := task.WorkflowRunID
	workflowStepID := task.WorkflowStepID
	if stepRun != nil {
		workflowRunID = stepRun.WorkflowRunID
		workflowStepID = stepRun.StepID
		iteration = stepRun.Iteration
	} else if workflowRunID != "" && workflowStepID != "" {
		if sr, err := db.GetWorkflowStepRunByStep(ctx, ttx.Tx, tenantID, workflowRunID, workflowStepID); err == nil {
			iteration = sr.Iteration
		}
	}
	// Carry the worktree state onto the execution row so the execution
	// detail view can render worktree status/branch/path. D2: a
	// parallel-branch child's execution runs in the STEP RUN's own branch
	// worktree, so its state is copied from the step run when the step run
	// carries a ready worktree; every other step run keeps the per-run
	// worktree (provisioned at run arm). The WorktreeReconciler writes the
	// run/step-run rows; without this copy the worker_executions columns
	// stay NULL and the UI shows nothing.
	var worktreeStatus, worktreePath, worktreeBranch *string
	if stepRun != nil && stepRun.WorktreeStatus == domain.WorktreeReady && stepRun.WorktreePath != "" {
		worktreeStatus = strPtr(stepRun.WorktreeStatus)
		worktreePath = strPtr(stepRun.WorktreePath)
		worktreeBranch = strPtr(stepRun.WorktreeBranch)
	} else if workflowRunID != "" {
		if run, gerr := db.GetWorkflowRun(ctx, ttx.Tx, tenantID, workflowRunID); gerr == nil {
			if run.WorktreeStatus != "" {
				worktreeStatus = strPtr(run.WorktreeStatus)
			}
			if run.WorktreePath != "" {
				worktreePath = strPtr(run.WorktreePath)
			}
			if run.WorktreeBranch != "" {
				worktreeBranch = strPtr(run.WorktreeBranch)
			}
		}
	}
	// Carry the run's PR surface onto the execution row so the executions
	// list/detail can link out to the run's PR (same seam as the worktree
	// columns). The authoritative pr_url/pr_state live in the run's
	// run_context (worker-authored); when empty the UI falls back to a
	// deterministic `pull/new/{branch}` link from the project's repo_slug.
	var prURL, prState *string
	if workflowRunID != "" {
		if run, gerr := db.GetWorkflowRun(ctx, ttx.Tx, tenantID, workflowRunID); gerr == nil {
			u, s := db.PrFromRunContext(run.RunContext)
			if u != "" {
				prURL = strPtr(u)
			}
			if s != "" {
				prState = strPtr(s)
			}
		}
	}
	// Guard: workflow-bound dispatches must have a ready or skipped worktree.
	// A pending/pruned/failed worktree must be re-provisioned — never create
	// an execution that would run in the project dir (silent fallback).
	if workflowRunID != "" && worktreeStatus != nil {
		switch *worktreeStatus {
		case domain.WorktreePending, domain.WorktreePruned:
			r.log.Info("holding dispatch: worktree not ready, will re-provision", "task", task.ID, "run", workflowRunID, "status", *worktreeStatus)
			return nil
		case domain.WorktreeFailed:
			r.log.Warn("worktree provisioning failed — holding dispatch", "task", task.ID, "run", workflowRunID, "status", *worktreeStatus)
			return nil
		}
	}
	execRow := db.ExecutionRow{
		ID:             db.NewID(),
		TenantID:       tenantID,
		ProjectID:      task.ProjectID,
		TaskID:         task.ID,
		WorkerID:       version.WorkerID,
		WorkerVersion:  version.Version,
		AdapterID:      &adapter.ID,
		Status:         domain.ExecutionDispatching,
		HealthState:    domain.HealthHealthy,
		StartedAt:      &now,
		WorkflowRunID:  workflowRunID,
		WorkflowStepID: workflowStepID,
		IsFollowUp:     isFollowUp,
		Iteration:      iteration,
		WorktreeStatus: worktreeStatus,
		WorktreePath:   worktreePath,
		WorktreeBranch: worktreeBranch,
		PrURL:          prURL,
		PrState:        prState,
	}
	created, err := db.CreateExecution(ctx, ttx.Tx, execRow)
	if err != nil {
		return fmt.Errorf("create execution: %w", err)
	}

	// Transition task to "assigned" (docs/03 §6: ready → assigned) —
	// standalone dispatch only. A workflow-bound ticket stays "running"
	// for the whole run (the workflow reconciler sets its terminal status
	// at run end).
	if stepRun == nil {
		if _, err := db.UpdateWorkItem(ctx, ttx.Tx, tenantID, task.ID, task.Version, db.UpdateWorkItemFields{
			Status: strPtr(domain.WorkItemAssigned),
		}); err != nil {
			return fmt.Errorf("update task status: %w", err)
		}
	}

	// Link the workflow step run to the new execution so the run-view
	// UI can show "click step → open execution". Without this link,
	// the run view falls back to "waiting for dispatch…" placeholders
	// even after the dispatch succeeded, and the step run rows aren't
	// clickable (no worker_execution_id to navigate to).
	//
	// Done inside the same transaction so a worker-step run never
	// points at an execution that doesn't exist.
	if stepRun != nil {
		if _, err := db.UpdateWorkflowStepRun(ctx, ttx.Tx, tenantID, stepRun.ID, stepRun.Version, db.UpdateWorkflowStepRunFields{
			WorkerExecutionID: &created.ID,
			Status:            strPtr(domain.StepRunRunning),
			StartedAt:         &now,
		}); err != nil {
			return fmt.Errorf("link step run to execution: %w", err)
		}
	} else if workflowStepID != "" {
		if stepRunRow, err := db.GetWorkflowStepRunByStep(ctx, ttx.Tx, tenantID, workflowRunID, workflowStepID); err == nil {
			if _, err := db.UpdateWorkflowStepRun(ctx, ttx.Tx, tenantID, stepRunRow.ID, stepRunRow.Version, db.UpdateWorkflowStepRunFields{
				WorkerExecutionID: &created.ID,
				Status:            strPtr(domain.StepRunRunning),
				StartedAt:         &now,
			}); err != nil {
				return fmt.Errorf("link step run to execution: %w", err)
			}
		} else if !errors.Is(err, db.ErrNotFound) {
			return fmt.Errorf("get step run for link: %w", err)
		}
	}

	// Enqueue outbox events for the execution + task.
	if err := enqueueExecEvent(ctx, ttx.Tx, "execution.created", created, nil); err != nil {
		return fmt.Errorf("enqueue exec event: %w", err)
	}
	if err := enqueueWorkItemEvent(ctx, ttx.Tx, "work_item.assigned", task); err != nil {
		return fmt.Errorf("enqueue task event: %w", err)
	}

	if err := ttx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	// Start the execution via the adapter bridge. This happens AFTER
	// the tx commits so the execution row is durable; if the adapter
	// start fails, a later reconcile pass marks the execution
	// failed_to_start (docs/03 §8: adapter unreachable mid-dispatch).
	go r.startExecution(ctx, created, task, version, adapter)

	r.log.Info("task dispatched",
		"task", task.ID, "step_run", stepRunID, "execution", created.ID,
		"worker", version.WorkerID, "worker_version", version.Version,
		"adapter", adapter.ID)
	return nil
}

// workerVersionForStepRun resolves the worker version pinned by a
// workflow step. The WorkflowReconciler stores _worker_id +
// _worker_version on the step run's result when it dispatches the step;
// it falls back to the worker assigned on the ticket (worker-backed
// approval steps) when the step run carries none.
func (r *TaskReconciler) workerVersionForStepRun(ctx context.Context, tx pgx.Tx, tenantID string, sr db.WorkflowStepRunRow) (db.WorkerVersionRow, error) {
	var meta struct {
		WorkerID  string `json:"_worker_id"`
		WorkerVer int    `json:"_worker_version"`
	}
	_ = json.Unmarshal(sr.Result, &meta)
	if meta.WorkerID != "" {
		if meta.WorkerVer > 0 {
			if v, err := db.GetWorkerVersionByID(ctx, tx, tenantID, meta.WorkerID, fmt.Sprintf("v%d", meta.WorkerVer)); err == nil {
				return v, nil
			}
		}
		if v, err := db.GetLatestWorkerVersion(ctx, tx, tenantID, meta.WorkerID, true); err == nil {
			return v, nil
		}
		return db.WorkerVersionRow{}, fmt.Errorf("no dispatchable version for step worker %s", meta.WorkerID)
	}
	// No worker pinned on the step run (e.g. worker-backed approval):
	// fall back to the ticket's assigned worker.
	task, err := db.GetWorkItem(ctx, tx, tenantID, mustStepWorkItemID(sr))
	if err != nil {
		return db.WorkerVersionRow{}, fmt.Errorf("get task for worker resolution: %w", err)
	}
	_, version, err := r.selectWorker(ctx, tx, tenantID, task)
	if err != nil {
		return db.WorkerVersionRow{}, err
	}
	return version, nil
}

// mustStepWorkItemID reads _work_item_id from a step run's result. It
// must exist for any dispatched step; empty means the step run is not in
// a dispatchable state.
func mustStepWorkItemID(sr db.WorkflowStepRunRow) string {
	var meta struct {
		WorkItemID string `json:"_work_item_id"`
	}
	_ = json.Unmarshal(sr.Result, &meta)
	return meta.WorkItemID
}

// startExecution calls the adapter bridge to start the execution. It
// runs in a goroutine so the reconcile loop is not blocked by the
// adapter call (docs/03 §8: no SELECT FOR UPDATE held across external
// calls). The bridge updates the execution status as telemetry arrives.
func (r *TaskReconciler) startExecution(ctx context.Context, exec db.ExecutionRow, task db.WorkItemRow, version db.WorkerVersionRow, adp db.AdapterRow) {
	// Resolve the project directory so the adapter runs in the correct
	// working directory (avoids picking up Orchicon's own AGENTS.md etc.).
	// Use a background context (the reconciler's ctx may expire before
	// this goroutine gets a chance to query).
	var projectDir string
	{
		var p db.ProjectRow
		qCtx := context.Background()
		if err := r.pool.QueryRow(qCtx,
			`SELECT project_dir FROM projects WHERE id = $1 AND tenant_id = $2`,
			exec.ProjectID, exec.TenantID,
		).Scan(&p.ProjectDir); err == nil {
			projectDir = p.ProjectDir
		}
	}
	// Resolve the workflow run's runtime container image (the adapter's
	// self-heal recreates the container with the identical image the
	// WorkflowReconciler used at run start) AND the run's provisioned
	// worktree path, which becomes the execution working directory. This
	// fetch runs BEFORE the recovery-seed gate: the seed file lands in the
	// execution cwd (the worktree for worktree runs), so the cwd must be
	// resolved first.
	runtimeImage := ""
	worktreePath := ""
	worktreeStatus := ""
	worktreeBranch := ""
	if task.WorkflowRunID != "" {
		if rtx, err := r.pool.BeginTenantTx(context.Background(), exec.TenantID); err == nil {
			if run, gerr := db.GetWorkflowRun(context.Background(), rtx.Tx, exec.TenantID, task.WorkflowRunID); gerr == nil {
				runtimeImage = run.RuntimeImage
				worktreeStatus = run.WorktreeStatus
				worktreeBranch = run.WorktreeBranch
				if run.WorktreeStatus == domain.WorktreeReady && run.WorktreePath != "" {
					worktreePath = run.WorktreePath
				}
			}
			// D2: a parallel-branch child execution runs in the STEP RUN's
			// OWN branch worktree — its cwd must be the branch worktree,
			// not the run worktree. Resolve via the step run LINKED to this
			// execution (GetWorkflowStepRunByExecution), because
			// exec.WorkflowStepID alone is not unique across loop
			// iterations. Non-branch steps carry no ready branch worktree,
			// so they keep the run worktree (today's behavior).
			if exec.WorkflowStepID != "" {
				if sr, gerr := db.GetWorkflowStepRunByExecution(context.Background(), rtx.Tx, exec.TenantID, exec.ID); gerr == nil &&
					sr.WorktreeStatus == domain.WorktreeReady && sr.WorktreePath != "" {
					worktreeStatus = sr.WorktreeStatus
					worktreePath = sr.WorktreePath
					worktreeBranch = sr.WorktreeBranch
				}
			}
			_ = rtx.Rollback(context.Background())
		}
	}
	// The execution working directory is the provisioned worktree path when
	// the run has one (the worker starts already checked out on its branch
	// inside the worktree), else the project dir. Only `ready` and `skipped`
	// (intentionally in-place) are allowed to dispatch without a worktree
	// path; `pending`/`pruned`/`failed` must be re-provisioned and never
	// silently fall back to the project dir (the observed project-dir
	// fallback broke branch/PR invariants).
	execCwd := projectDir
	if worktreePath != "" {
		execCwd = worktreePath
	} else if exec.WorkflowRunID != "" {
		// Workflow-bound dispatch without a worktree path.
		switch worktreeStatus {
		case domain.WorktreePending, domain.WorktreePruned:
			r.log.Info("holding dispatch: worktree not ready, will re-provision", "task", task.ID, "run", exec.WorkflowRunID, "status", worktreeStatus)
			return
		case domain.WorktreeFailed:
			r.log.Warn("worktree provisioning failed — failing dispatch", "task", task.ID, "run", exec.WorkflowRunID, "status", worktreeStatus)
			return
		case domain.WorktreeSkipped, "":
			// intentionally in-place (non-repo) or undetermined — allow projectDir
		default:
			r.log.Warn("holding dispatch: unexpected worktree status without path", "task", task.ID, "run", exec.WorkflowRunID, "status", worktreeStatus)
			return
		}
	}
	// The system prompt is the full context the model sees on every
	// turn. The WorkflowReconciler builds the composite per step and
	// stores it on the STEP RUN's result (_prompt) — the ticket is a
	// shared input reference and is no longer mutated with per-step
	// context. For worker-backed approval steps (and legacy direct
	// dispatch) the composite still lives on the work item's
	// prompt_context. The composite carries the worker identity, the
	// project directory + context files, the task, the ancestor chain,
	// the recovery narrative, and the worker's contract (ORCHICON
	// WORKER SUMMARY marker). The opencode adapter delivers this as the
	// agent's `prompt` via OPENCODE_CONFIG_CONTENT so every
	// conversation turn carries the same context.
	composite, _ := extractComposite(task.PromptContext)
	var promptFP string      // context-file fingerprint (ADR-0009 D5)
	var stepRunResult []byte // step run's full result — carries the _recovery_* seed keys
	if exec.WorkflowRunID != "" && exec.WorkflowStepID != "" {
		if stx, err := r.pool.BeginTenantTx(context.Background(), exec.TenantID); err == nil {
			if sr, err := db.GetWorkflowStepRunByStep(context.Background(), stx.Tx, exec.TenantID, exec.WorkflowRunID, exec.WorkflowStepID); err == nil {
				stepRunResult = sr.Result
				var srMeta struct {
					Prompt      string `json:"_prompt"`
					Fingerprint string `json:"_prompt_fingerprint"`
				}
				_ = json.Unmarshal(sr.Result, &srMeta)
				if srMeta.Prompt != "" {
					composite = srMeta.Prompt
				}
				if srMeta.Fingerprint != "" {
					promptFP = srMeta.Fingerprint
				}
			}
			_ = stx.Rollback(context.Background())
		}
	}
	systemPrompt := composite
	// Fall back to a minimal worker-prompt if no composite was set
	// (legacy direct-dispatch path: work item dispatched outside a
	// workflow, so the workflow reconciler never built a composite).
	// The fallback now builds the SAME shared composite the workflow
	// path produces (worker identity + task + project context +
	// work-item context + instructions) so standalone dispatches see
	// project/work-item context "just like projects" (F5).
	if systemPrompt == "" {
		var fp string
		systemPrompt, fp = buildStandaloneComposite(r.pool, exec, task, version, worktreeStatus, worktreeBranch)
		if fp != "" {
			promptFP = fp
		}
		if strings.TrimSpace(systemPrompt) == "" {
			systemPrompt = "You are a worker in the Orchicon orchestration system. " +
				"Complete the work item described in the user message and report back."
		}
	}
	// Recovery seeding — a HARD gate: if this is a recovery-resumed dispatch
	// for the SAME worker that died, .orchicon/worker.recovery must exist,
	// be non-empty, and carry this recovery's footer BEFORE the session may
	// start. A different worker or a fresh dispatch never sees the file or
	// the reference (and any existing file is left alone — it may belong to
	// another in-flight recovery). A failure to write/verify the seed file
	// fails the dispatch (failed_to_start) instead of launching cold — the
	// recovery-resume invariant.
	if projectDir != "" {
		updatedPrompt, err := r.seedRecoveryFile(context.Background(), exec, task, version, execCwd, stepRunResult, systemPrompt)
		if err != nil {
			r.log.Error("recovery seed gate failed — failing dispatch instead of starting cold",
				"execution", exec.ID, "error", err)
			recoverySeedMetricsSingleton.recordBlocked()
			r.markFailedToStart(context.Background(), exec, "recovery seed file could not be written: "+err.Error())
			return
		}
		systemPrompt = updatedPrompt
	}
	// User message (Goal): just the work item title. The composite
	// (with the full task + project + recovery context) is the
	// system message, not the user message, so the worker
	// instruction to end with ORCHICON WORKER SUMMARY is consistent
	// across the first turn and every subsequent turn.
	// Fetch tenant settings for default model and stall thresholds.
	var defaultModelRef string
	var stallNoProgress, stallNoFileDiff, stallTextLoop int64
	var stallRepCount int32
	var stallRepWindow int64
	var stallNudgeMax int32
	var stallNudgeReplyWindow, stallNudgeCooldown int64
	var stallToolHang int64
	var defaultBudgetOverrides []byte
	{
		settingsCtx := context.Background()
		stx, err := r.pool.BeginTenantTx(settingsCtx, exec.TenantID)
		if err == nil {
			s, err := db.GetTenantSettings(settingsCtx, stx.Tx, exec.TenantID)
			if err == nil {
				defaultModelRef = s.DefaultWorkerModel
				stallNoProgress = s.StallNoProgressWindowSeconds
				stallNoFileDiff = s.StallNoFileDiffWindowSeconds
				stallTextLoop = s.StallTextLoopWindowSeconds
				stallRepCount = s.StallRepetitionCount
				stallRepWindow = s.StallRepetitionWindowSeconds
				stallNudgeMax = s.StallNudgeMax
				stallNudgeReplyWindow = s.StallNudgeReplyWindowSeconds
				stallNudgeCooldown = s.StallNudgeCooldownSeconds
				stallToolHang = s.StallToolHangSeconds
				defaultBudgetOverrides = s.BudgetJSON()
			}
			stx.Rollback(settingsCtx)
		}
	}

	// Merge the tenant-level default budget (base) with the worker's own
	// budget_overrides (override) so a worker's explicit field always wins
	// per-field over the tenant default. The merged JSON is what the
	// adapter enforces (e.g. wall_clock_seconds -> hard deadline).
	budgetsJSON := mergeBudgets(defaultBudgetOverrides, version.BudgetOverrides)

	manifest := ExecutionManifest{
		ExecutionID:                  exec.ID,
		TaskID:                       exec.TaskID,
		ProjectID:                    exec.ProjectID,
		WorkerID:                     version.WorkerID,
		WorkerVersion:                version.Version,
		SystemPrompt:                 systemPrompt,
		PromptFingerprint:            promptFP,
		Goal:                         task.Title,
		AcceptanceCriteria:           task.AcceptanceCriteria,
		ModelRef:                     version.ModelRef,
		DefaultModelRef:              defaultModelRef,
		ContextSources:               version.ContextSources,
		Budgets:                      budgetsJSON,
		Permissions:                  version.Permissions,
		ProjectDir:                   projectDir,
		WorktreePath:                 worktreePath,
		RuntimeWorkflowID:            task.WorkflowRunID,
		RuntimeImage:                 runtimeImage,
		StallNoProgressWindowSeconds: stallNoProgress,
		StallNoFileDiffWindowSeconds: stallNoFileDiff,
		StallTextLoopWindowSeconds:   stallTextLoop,
		StallRepetitionCount:         stallRepCount,
		StallRepetitionWindowSeconds: stallRepWindow,
		StallNudgeMax:                stallNudgeMax,
		StallNudgeReplyWindowSeconds: stallNudgeReplyWindow,
		StallNudgeCooldownSeconds:    stallNudgeCooldown,
		StallToolHangSeconds:         stallToolHang,
	}
	if r.dispatcher == nil {
		err := fmt.Errorf("no adapter dispatcher configured — the server must register at least one adapter bridge")
		r.log.Error("adapter dispatch failed", "execution", exec.ID, "error", err)
		r.markFailedToStart(context.Background(), exec, err.Error())
		return
	}
	// Resolve the adapter bridge from the execution's adapter kind — the
	// model_ref grammar (adapter.AdapterKind) is the SINGLE source of
	// truth (ADR-0003). A 2-segment legacy ref (opencode/<model>) resolves
	// to the default kind "opencode"; an unknown kind fails the execution
	// with an actionable message (never a panic).
	modelRef := manifest.ModelRef
	if modelRef == "" {
		modelRef = manifest.DefaultModelRef
	}
	kind := adapter.AdapterKind(modelRef)
	if kind == "" {
		// Empty/malformed model_ref: fall back to the default adapter
		// kind ("opencode") so legacy workers with single-segment refs
		// keep dispatching exactly as they did before the dispatcher
		// (previously the bridge was a hardcoded singleton).
		kind = adapter.DefaultAdapterKind
	}
	// Sequence continuation (opt-in, DEFAULT OFF): consecutive
	// same-worker tasks in a sequence chain may resume the prior task's
	// session transcript instead of starting fresh (tightly-coupled
	// chains where retained context beats isolation). Resolution happens
	// ONLY for the native engine (kind "orchicon" — the in-process
	// session engine); opencode/other adapters never resume (their
	// transcripts are subprocess-bound, not in-process). The flag is set
	// only when the task has a terminal-success predecessor sibling bound
	// to the same worker, so identity isolation holds by construction.
	if kind == "orchicon" && task.ParentID != nil && *task.ParentID != "" {
		if priorID, ok := r.continuationSessionID(context.Background(), task, version); ok {
			manifest.SequenceContinue = true
			manifest.ContinueFromSessionID = priorID
			r.log.Info("sequence continuation armed",
				"execution", exec.ID, "prior", priorID, "task", task.ID)
		}
	}
	bridge, err := r.dispatcher.Resolve(kind)
	if err != nil {
		r.log.Error("adapter dispatch failed", "execution", exec.ID, "kind", kind, "error", err)
		// The Resolve error names the missing kind and the registered
		// kinds — surface it verbatim so the operator can act on it.
		r.markFailedToStart(context.Background(), exec, err.Error())
		return
	}
	if err := bridge.Start(ctx, exec, manifest, r); err != nil {
		r.log.Error("adapter start failed", "execution", exec.ID, "error", err)
		// Mark the execution as failed_to_start.
		r.markFailedToStart(context.Background(), exec, err.Error())
	}
}

// continuationSessionID resolves the prior session to continue for a
// sequence-chain task (sequence-continuation flag, opt-in DEFAULT OFF).
// It returns (sessionID, true) only when:
//   - the task is a direct child of a sequence parent (ParentID set),
//   - its immediate predecessor sibling is terminal-success (succeeded or
//     skipped — a failed/cancelled predecessor never continues),
//   - the predecessor's assigned worker is the SAME worker as the current
//     task's (identity isolation by construction: no worker ever resumes
//     another worker's transcript),
//   - the predecessor has a completed execution (session id == execution
//     id for the native engine).
//
// The session id for the native engine IS the execution id, so the prior
// execution's id is the continuation target.
func (r *TaskReconciler) continuationSessionID(ctx context.Context, task db.WorkItemRow, version db.WorkerVersionRow) (string, bool) {
	if task.ParentID == nil || *task.ParentID == "" {
		return "", false
	}
	ttx, err := r.pool.BeginTenantTx(context.Background(), task.TenantID)
	if err != nil {
		r.log.Warn("continuation: begin tx failed", "task", task.ID, "error", err)
		return "", false
	}
	defer ttx.Rollback(context.Background())

	children, err := db.ListDirectChildren(context.Background(), ttx.Tx, task.TenantID, *task.ParentID)
	if err != nil {
		r.log.Warn("continuation: list children failed", "task", task.ID, "error", err)
		return "", false
	}
	idx := -1
	for i, c := range children {
		if c.ID == task.ID {
			idx = i
			break
		}
	}
	if idx <= 0 {
		return "", false // no predecessor (first child or not found)
	}
	prev := children[idx-1]
	if !domain.WorkItemIsTerminalSuccess(prev.Status) {
		return "", false
	}
	// Same-worker requirement (identity isolation). The predecessor's
	// assigned worker is parsed from its AssignedWorkerRef.
	var ref struct {
		WorkerID string `json:"worker_id"`
		Version  int    `json:"version"`
	}
	if len(prev.AssignedWorkerRef) == 0 {
		return "", false
	}
	if err := json.Unmarshal(prev.AssignedWorkerRef, &ref); err != nil || ref.WorkerID == "" {
		return "", false
	}
	if ref.WorkerID != version.WorkerID {
		return "", false // cross-worker never continues
	}
	priorExec, err := db.GetLatestExecutionForTask(context.Background(), ttx.Tx, task.TenantID, prev.ID)
	if err != nil || priorExec.ID == "" {
		return "", false
	}
	// Only a succeeded prior execution is worth continuing from.
	if priorExec.Status != domain.ExecutionSucceeded {
		return "", false
	}
	return priorExec.ID, true
}

// markFailedToStart transitions an execution to failed_to_start
// (docs/03 §8: adapter unreachable mid-dispatch → failed_to_start, task
// requeues with backoff).
func (r *TaskReconciler) markFailedToStart(ctx context.Context, exec db.ExecutionRow, errorMessage string) {
	ttx, err := r.pool.BeginTenantTx(ctx, exec.TenantID)
	if err != nil {
		r.log.Error("begin tx for failed_to_start", "execution", exec.ID, "error", err)
		return
	}
	defer ttx.Rollback(ctx)
	now := time.Now().UTC()
	_, err = db.UpdateExecution(ctx, ttx.Tx, exec.TenantID, exec.ID, exec.Version, db.UpdateExecutionFields{
		Status:       strPtr(domain.ExecutionFailedToStart),
		EndedAt:      &now,
		ErrorMessage: &errorMessage,
	})
	if err != nil {
		r.log.Error("mark failed_to_start", "execution", exec.ID, "error", err)
		return
	}
	// Requeue the task: status back to ready (standalone only). A
	// workflow-bound ticket stays "running" for the whole run — its
	// status is set by the workflow reconciler at run end; the step run
	// is left to the workflow's poll/recovery path (dispatchLinkGrace →
	// recovery) instead of being requeued here.
	if exec.WorkflowRunID == "" {
		if _, err := db.UpdateWorkItem(ctx, ttx.Tx, exec.TenantID, exec.TaskID, 0, db.UpdateWorkItemFields{
			Status: strPtr(domain.WorkItemReady),
		}); err != nil {
			r.log.Error("requeue task after failed_to_start", "task", exec.TaskID, "error", err)
			return
		}
	}
	if err := ttx.Commit(ctx); err != nil {
		r.log.Error("commit failed_to_start", "execution", exec.ID, "error", err)
	}
}

// selectWorker selects a published Worker for the task using rule-based
// ranking (docs/03 §4.1): filter by health, rank by lowest utilization
// + LRU, deterministic tiebreak by worker id.
func (r *TaskReconciler) selectWorker(ctx context.Context, tx pgx.Tx, tenantID string, task db.WorkItemRow) (db.WorkerRow, db.WorkerVersionRow, error) {
	// v0.1: the task's assigned_worker_ref pins the worker. If empty,
	// there's no worker to dispatch to (the user must assign one).
	if len(task.AssignedWorkerRef) == 0 {
		return db.WorkerRow{}, db.WorkerVersionRow{}, fmt.Errorf("task has no assigned worker")
	}
	var ref struct {
		WorkerID string `json:"worker_id"`
		Version  int    `json:"version"`
	}
	if err := json.Unmarshal(task.AssignedWorkerRef, &ref); err != nil {
		return db.WorkerRow{}, db.WorkerVersionRow{}, fmt.Errorf("parse assigned_worker_ref: %w", err)
	}
	if ref.WorkerID == "" {
		return db.WorkerRow{}, db.WorkerVersionRow{}, fmt.Errorf("assigned_worker_ref has no worker_id")
	}
	worker, err := db.GetWorker(ctx, tx, tenantID, ref.WorkerID)
	if err != nil {
		return db.WorkerRow{}, db.WorkerVersionRow{}, fmt.Errorf("get worker: %w", err)
	}
	// Worker must be published or deprecated (dispatchable — docs/05 §4).
	if worker.Status != domain.WorkerPublished && worker.Status != domain.WorkerDeprecated {
		return db.WorkerRow{}, db.WorkerVersionRow{}, fmt.Errorf("worker %s is not dispatchable (status=%s)", ref.WorkerID, worker.Status)
	}
	// Resolve the version: specific or latest published.
	var version db.WorkerVersionRow
	if ref.Version > 0 {
		versions, err := db.ListWorkerVersions(ctx, tx, tenantID, ref.WorkerID)
		if err != nil {
			return db.WorkerRow{}, db.WorkerVersionRow{}, err
		}
		for _, v := range versions {
			if v.Version == ref.Version {
				version = v
				break
			}
		}
		if version.ID == "" {
			return db.WorkerRow{}, db.WorkerVersionRow{}, fmt.Errorf("worker version %d not found", ref.Version)
		}
	} else {
		version, err = db.GetLatestWorkerVersion(ctx, tx, tenantID, ref.WorkerID, true)
		if err != nil {
			return db.WorkerRow{}, db.WorkerVersionRow{}, fmt.Errorf("get latest published version: %w", err)
		}
	}
	return worker, version, nil
}

// selectAdapter selects a registered adapter of the matching kind with
// a recent heartbeat and free capacity (docs/03 §4.2).
// resolveAdapterRowKind resolves the kind used for adapter-ROW selection
// (ADR-0005 D6): the version's runtime_ref when set (pre-existing
// behavior; a divergent runtime_ref keeps its terminal failed_to_start
// semantics), else the model_ref's parsed adapter kind — the same single
// source of truth the bridge Resolve at dispatch uses — else the legacy
// default kind. The empty-runtime_ref fallback closes the dispatch black
// hole: kind "" matched zero runtime_adapters rows, so a worker created
// without a runtime_ref requeued forever instead of dispatching.
func resolveAdapterRowKind(runtimeRef, modelRef string) string {
	if runtimeRef != "" {
		return runtimeRef
	}
	if k := adapter.AdapterKind(modelRef); k != "" {
		return k
	}
	return adapter.DefaultAdapterKind
}

func (r *TaskReconciler) selectAdapter(ctx context.Context, tx pgx.Tx, tenantID, kind string) (db.AdapterRow, error) {
	adapters, err := db.ListReadyAdaptersByKind(ctx, tx, tenantID, kind, heartbeatTTL)
	if err != nil {
		return db.AdapterRow{}, fmt.Errorf("list adapters: %w", err)
	}
	if len(adapters) == 0 {
		return db.AdapterRow{}, fmt.Errorf("no ready adapters of kind %q", kind)
	}
	// Filter by free capacity (docs/03 §4.2: prefer adapters with
	// recent healthy heartbeats + free capacity).
	var candidates []db.AdapterRow
	for _, a := range adapters {
		count, err := db.CountActiveExecutionsForAdapter(ctx, tx, tenantID, a.ID)
		if err != nil {
			continue
		}
		if count < a.MaxConcurrentExecutions {
			candidates = append(candidates, a)
		}
	}
	if len(candidates) == 0 {
		return db.AdapterRow{}, fmt.Errorf("all adapters of kind %q at capacity", kind)
	}
	// Deterministic: sort by id (docs/03 §4.1: deterministic tiebreak).
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].ID < candidates[j].ID
	})
	return candidates[0], nil
}

// --- execution status callbacks (called by the adapter bridge) ---

// OnStarted is called by the adapter bridge when the adapter confirms
// execution has started (docs/03 §6: assigned → running).
func (r *TaskReconciler) OnStarted(ctx context.Context, execID string) {
	r.updateExecStatus(ctx, execID, domain.ExecutionRunning, domain.HealthHealthy, "")
}

// OnWrittenFiles is called by the adapter when the worker's session reports
// files it wrote or edited (opencode file_diff telemetry). The paths are
// stashed keyed by execution and folded into _touched_files when the
// execution terminates, so the next worker is told exactly which files to
// read before starting.
func (r *TaskReconciler) OnWrittenFiles(ctx context.Context, execID string, files []string) {
	if execID == "" || len(files) == 0 {
		return
	}
	r.writtenMu.Lock()
	defer r.writtenMu.Unlock()
	if r.pendingWrittenFiles == nil {
		r.pendingWrittenFiles = make(map[string][]string)
	}
	seen := make(map[string]bool, len(r.pendingWrittenFiles[execID]))
	for _, f := range r.pendingWrittenFiles[execID] {
		seen[f] = true
	}
	for _, f := range files {
		if f != "" && !seen[f] {
			seen[f] = true
			r.pendingWrittenFiles[execID] = append(r.pendingWrittenFiles[execID], f)
		}
	}
}

// writtenFilesFor returns (and clears) the pending written files for an
// execution, so the terminal transition can fold them into _touched_files.
func (r *TaskReconciler) writtenFilesFor(execID string) []string {
	r.writtenMu.Lock()
	defer r.writtenMu.Unlock()
	files := r.pendingWrittenFiles[execID]
	delete(r.pendingWrittenFiles, execID)
	return files
}

// OnResult is called by the adapter bridge when the execution reaches a
// terminal state (docs/03 §6: running → succeeded|failed). It updates
// the execution status and transitions the linked WorkItem to
// succeeded/failed so downstream consumers (the WorkflowReconciler
// polling task steps) observe completion (docs/02 §2.4: tasks are
// reconciled as children of workflows).
//
// `output` is the worker's accumulated text from the adapter (PR B —
// context propagation). When the worker succeeded, the TaskReconciler
// extracts the ORCHICON WORKER SUMMARY block from `output`, persists
// it as the work item's _summary, and copies it onto the linked
// workflow step run so downstream stages can include it as upstream
// context. `output` may be empty for non-opencode adapters or when
// the worker errored before producing any text.
func (r *TaskReconciler) OnResult(ctx context.Context, execID string, succeeded bool, output string, errorMessage string) {
	status := domain.ExecutionSucceeded
	if !succeeded {
		status = domain.ExecutionFailed
	}
	r.updateExecStatus(ctx, execID, status, domain.HealthTerminating, output, errorMessage)
	// A succeeded execution must not carry a stale advisory stall notice
	// (observed: the SSE worker succeeded yet kept `stalled:no_file_progress`
	// in error_message). Only stall-prefixed reasons are cleared — a real
	// failure reason on a failed execution is never touched.
	if succeeded {
		r.clearStallNotice(ctx, execID)
	}
	r.transitionWorkItemOnResult(ctx, execID, succeeded, output)
}

// FailLostExecution marks an execution failed with a clear reason and
// transitions its work item to failed, so the WorkflowReconciler's
// task-step poll observes the terminal state and the workflow's recover
// step re-dispatches in a fresh runtime. Used by the execution-liveness
// reaper (docs/03 §6) after a control-plane restart or a lost runtime
// container; it mirrors the failure path of a normal OnResult(false).
func (r *TaskReconciler) FailLostExecution(ctx context.Context, execID, errorMessage string) {
	r.updateExecStatus(ctx, execID, domain.ExecutionFailed, domain.HealthUnhealthy, "", errorMessage)
	r.transitionWorkItemOnResult(ctx, execID, false, "")
}

// transitionWorkItemOnResult moves the WorkItem linked to the execution
// to succeeded/failed when the execution terminates. This closes the
// loop so the WorkflowReconciler's task-step polling observes the
// terminal state (docs/02 §2.4, docs/03 §6). On failure, the
// TaskReconciler triggers recovery (Phase 7, docs/06 §2) — recovery is
// opt-out, not opt-in (docs/06 §1).
//
// `output` is the worker's accumulated text (PR B). On success, the
// function extracts the ORCHICON WORKER SUMMARY block, persists the
// full output + extracted summary onto the work item's results, and
// copies the summary onto the linked workflow step run so downstream
// stages can read it as upstream context.
func (r *TaskReconciler) transitionWorkItemOnResult(ctx context.Context, execID string, succeeded bool, output string) {
	ttx, err := r.pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		r.log.Error("transition work item: begin tx", "execution", execID, "error", err)
		return
	}
	defer ttx.Rollback(ctx)
	exec, err := db.GetExecution(ctx, ttx.Tx, "tnt_dev", execID)
	if err != nil {
		r.log.Error("transition work item: get execution", "execution", execID, "error", err)
		return
	}
	// Fetch the work item to use its current version for optimistic
	// concurrency (docs/09 §5). Passing 0 would never match.
	wi, err := db.GetWorkItem(ctx, ttx.Tx, "tnt_dev", exec.TaskID)
	if err != nil {
		r.log.Error("transition work item: get work item", "task", exec.TaskID, "error", err)
		return
	}
	// PR B: extract summary from worker output. If the marker is
	// absent, the entire output is treated as the summary (the
	// worker's prompt instructs it to end with the marker; lenient
	// workers that don't follow the contract still get their full
	// output propagated downstream).
	var summary string
	if succeeded && output != "" {
		summary = extractWorkerSummary(output)
	}
	// Cap the narrative portion of the summary so a bloated summary doesn't
	// tax every later step that re-embeds it (Execution history + the
	// .orchicon/<run>/summary + issues fallback files). The cap preserves the
	// routing word and every FACTS LEARNED: line verbatim (see
	// capSummaryNarrative); the full worker output remains in _output for
	// audit.
	summary = capSummaryNarrative(summary, maxSummaryTokens)
	// Check if this work item is a follow-up with a parent execution
	// to write back to. Read _parent_execution_id from the raw results
	// before we overwrite them with the worker output.
	var parentExecID string
	if len(wi.Results) > 0 {
		var rawResults map[string]any
		if err := json.Unmarshal(wi.Results, &rawResults); err == nil {
			if pid, ok := rawResults["_parent_execution_id"].(string); ok {
				parentExecID = pid
			}
		}
	}

	// Persist output + summary on the work item's results JSON so the
	// audit trail shows what the worker produced. The summary is the
	// canonical downstream input.
	//
	// We start fresh — previous results from an earlier execution
	// (e.g. SSE) carry stale fields that must NOT survive into this
	// execution. _parent_execution_id and the _recovery_* keys are
	// carried forward for traceability and so the dispatch path can
	// gate the .orchicon/worker.recovery file on a same-worker
	// recovery. _issues is NOT preserved — the execution history in
	// the prompt already shows prior issues.
	results := map[string]any{}
	if len(wi.Results) > 0 {
		var existing map[string]any
		if err := json.Unmarshal(wi.Results, &existing); err == nil {
			for _, k := range []string{"_parent_execution_id", "_recovery_summary", "_recovery_execution_id", "_recovery_worker_id"} {
				if v, ok := existing[k]; ok {
					results[k] = v
				}
			}
		}
	}
	if output != "" {
		results["_output"] = output
	}
	if summary != "" {
		results["_summary"] = summary
	}
	results["_worker"] = exec.WorkerID
	if exec.WorkerName != "" {
		results["_worker_name"] = exec.WorkerName
	}
	// The decision signal is a SINGLE source of truth: the first word
	// after the ORCHICON WORKER SUMMARY: marker. There is deliberately
	// no separate `_decision:` or `_issues:` channel that can contradict
	// it — a standalone `_decision:` line or an `_issues:` block must
	// never override the summary word. `_issues` is still captured for
	// the run view and .orchicon/issues (informational only). The first
	// word may be any signal: "success"/"failure" normalize, and custom
	// words pass through verbatim.
	// The decision key is only set when a marker was found, so a step run
	// that starts with a placeholder (e.g. worker-backed approval's
	// `_decision: "pending"`) keeps it until a real signal lands.
	if d := extractSummaryDecision(output); d != "" {
		results["_decision"] = d
	}
	extractIssuesLine(output, results)
	// Extract list of modified files from diff markers in the output.
	if output != "" {
		if files := extractTouchedFiles(output); len(files) > 0 {
			results["_touched_files"] = files
		}
	}
	// Fold in the files the session itself reported as written (opencode
	// file_diff telemetry). This is the authoritative set — it catches
	// files the model wrote without echoing a diff (e.g. .orchicon/ review
	// notes). Merge with any diff-marker files, deduped.
	if written := r.writtenFilesFor(execID); len(written) > 0 {
		merged := make([]any, 0, len(written))
		seen := make(map[string]bool, len(written))
		for _, f := range written {
			if f == "" || seen[f] {
				continue
			}
			seen[f] = true
			merged = append(merged, f)
		}
		if existing, ok := results["_touched_files"].([]any); ok {
			for _, e := range existing {
				if s, ok := e.(string); ok && !seen[s] {
					seen[s] = true
					merged = append(merged, s)
				}
			}
		}
		if len(merged) > 0 {
			results["_touched_files"] = merged
		}
	}
	resultsJSON, _ := json.Marshal(results)
	// Write authored PR URL/state into the run's run_context (for the run
	// detail page) and the execution row columns (for the work item card
	// and execution detail page). The PR is a fact — extracted regardless
	// of execution success, so a PR that was created but not merged still
	// surfaces (the worker may have failed after creating it).
	var prURL, prState string
	if r.skipPRMarkerStamp(ctx, ttx.Tx, exec, succeeded) {
		// Change 2: for a succeeded PR-requiring git_strategy=pr workflow
		// step, the WorkflowReconciler's step-success path is the single
		// authoritative writer of pr_url/pr_state (the deterministic gh
		// check). Demote the worker-marker parse to a fallback here so it
		// does not race the verified write. Keep extraction for failed PR
		// steps (a created-but-unmerged PR is still a fact), standalone
		// executions, and local/none-strategy steps.
	} else {
		prURL, prState = extractPRFields(output)
	}
	if exec.WorkflowRunID != "" && (prURL != "" || prState != "") {

		run, err := db.GetWorkflowRun(ctx, ttx.Tx, "tnt_dev", exec.WorkflowRunID)
		if err != nil {
			if err == db.ErrNotFound {
				r.log.Warn("transition: workflow run not found for PR capture", "run", exec.WorkflowRunID, "execution", exec.ID)
			} else {
				r.log.Error("transition: get workflow run for PR capture", "run", exec.WorkflowRunID, "execution", exec.ID, "error", err)
			}
		} else {
			ctxBytes, ok := mergeRunContext(run.RunContext, map[string]any{
				"pr_url":   prURL,
				"pr_state": prState,
			})
			if ok {
				_, err := db.UpdateWorkflowRun(ctx, ttx.Tx, "tnt_dev", exec.WorkflowRunID, run.Version, db.UpdateWorkflowRunFields{RunContext: &ctxBytes})
				if err != nil {
					// One retry on CAS conflict: re-read the run and try once more.
					reRead, reErr := db.GetWorkflowRun(ctx, ttx.Tx, "tnt_dev", exec.WorkflowRunID)
					if reErr == nil {
						ctxBytes2, ok2 := mergeRunContext(reRead.RunContext, map[string]any{
							"pr_url":   prURL,
							"pr_state": prState,
						})
						if ok2 {
							_, err = db.UpdateWorkflowRun(ctx, ttx.Tx, "tnt_dev", exec.WorkflowRunID, reRead.Version, db.UpdateWorkflowRunFields{RunContext: &ctxBytes2})
						}
					}
					if err != nil {
						r.log.Warn("transition: update run_context for PR capture (best-effort)", "run", exec.WorkflowRunID, "execution", exec.ID, "error", err)
					}
				}
			}
		}
	}
	// Update the execution row columns for PR URL/state (non-empty only,
	// so partial reports don't clobber existing values).
	if prURL != "" || prState != "" {
		fields := db.UpdateExecutionFields{}
		if prURL != "" {
			fields.PrURL = &prURL
		}
		if prState != "" {
			fields.PrState = &prState
		}
		_, err := db.UpdateExecution(ctx, ttx.Tx, "tnt_dev", exec.ID, exec.Version, fields)
		if err != nil {
			r.log.Warn("transition: update execution PR fields", "execution", exec.ID, "error", err)
		}
	}
	// A work item bound to an ACTIVE workflow run tracks the RUN, not any
	// single step execution: the ticket is a shared input reference and
	// stays "running" for the whole run — its per-step results/status are
	// NOT written here. The workflow reconciler aggregates the run-level
	// narrative (step outputs + recovery episodes) and sets the terminal
	// status when the run completes/fails. Standalone (unbound) items keep
	// the old per-execution terminal transition + results write.
	runActive := r.boundToActiveRun(ctx, ttx.Tx, wi)
	if runActive {
		// Step outputs go on the step run (propagateStepRunResults) so
		// the run view, loop decisions, and composite prompts see them.
		r.propagateStepRunResults(ctx, ttx.Tx, exec.ID, results)
	} else if succeeded {
		status := domain.WorkItemSucceeded
		fields := db.UpdateWorkItemFields{
			Status: strPtr(status),
		}
		if resultsJSON != nil {
			fields.Results = &resultsJSON
		}
		if _, err := db.UpdateWorkItem(ctx, ttx.Tx, "tnt_dev", exec.TaskID, wi.Version, fields); err != nil {
			r.log.Error("transition work item: update", "task", exec.TaskID, "error", err)
			return
		}
		// Copy the execution results onto the linked workflow step run
		// so the run view can display decision/summary/issues/files
		// without opening each execution. Best-effort.
		r.propagateStepRunResults(ctx, ttx.Tx, exec.ID, results)
	} else {
		// Failure: transition to failed so the step run transitions to
		// terminal-failed, allowing a downstream `recover` step to
		// activate and trigger recovery (docs/06 §1).
		status := domain.WorkItemFailed
		fields := db.UpdateWorkItemFields{
			Status: strPtr(status),
		}
		if resultsJSON != nil {
			fields.Results = &resultsJSON
		}
		if _, err := db.UpdateWorkItem(ctx, ttx.Tx, "tnt_dev", exec.TaskID, wi.Version, fields); err != nil {
			r.log.Error("transition work item: update", "task", exec.TaskID, "error", err)
			return
		}
	}
	if err := ttx.Commit(ctx); err != nil {
		r.log.Error("transition work item: commit", "execution", execID, "error", err)
		return
	}

	// Write .orchicon/ files for the next worker to read.
	r.writeOrchiconFiles(ctx, exec, wi, succeeded, results)

	// System-side recovery-seed cleanup: a successful recovery-resumed
	// execution removes .orchicon/worker.recovery when the file's footer
	// matches this execution (the worker-side `rm` in the file's own
	// directive is the primary mechanism; this is the backstop so the file
	// never lingers across workflows/projects and never deletes a newer
	// recovery's file).
	r.removeRecoveryFileForSuccess(ctx, exec, results, succeeded)

	// Follow-up write-back: if this work item has a parent execution
	// (created via CreateFollowUpExecution), append the assistant's
	// output to the parent execution's conversation so the follow-up
	// feels like a continuation of the same conversation.
	if parentExecID != "" && succeeded && output != "" {
		r.appendToParentConversation(context.Background(), "tnt_dev", parentExecID, output)
	}

	// Notify the WorkflowReconciler that this task completed so it
	// can progress the step DAG immediately (docs/03 §2). Done after
	// commit so the work item status is visible. No-op when the work
	// item has no workflow link (direct dispatch, not from a workflow).
	if r.workflowNotifier != nil && wi.WorkflowRunID != "" {
		r.workflowNotifier(context.Background(), wi.WorkflowRunID)
	}

	// Recovery is NOT triggered automatically — explicit `recover`
	// steps on the workflow canvas handle this (docs/06 §1).
}

// boundToActiveRun reports whether the work item is linked to a workflow
// run that is still in flight (not yet terminal). A bound item's status
// tracks the run as a whole — individual step executions succeed/fail
// while the run continues, so they must not flip the item to a terminal
// state until the run itself ends.
// skipPRMarkerStamp reports whether the worker-marker PR capture
// (extractPRFields) should be SKIPPED for this execution. It is true only
// for a succeeded WORKFLOW-BOUND execution whose step is PR-requiring
// (requires_pr) and whose run's effective git strategy is "pr": for that
// case the WorkflowReconciler's step-success path is the single
// authoritative writer of pr_url/pr_state (the deterministic gh check), so
// the marker parse is demoted to a fallback and must not race it.
// Determination is fail-open: any read/parse error or missing config falls
// back to extraction (never regress standalone or failed-step capture).
func (r *TaskReconciler) skipPRMarkerStamp(ctx context.Context, tx pgx.Tx, exec db.ExecutionRow, succeeded bool) bool {
	if !succeeded || exec.WorkflowRunID == "" || exec.WorkflowStepID == "" {
		return false
	}
	run, err := db.GetWorkflowRun(ctx, tx, "tnt_dev", exec.WorkflowRunID)
	if err != nil {
		return false // fail-open: keep extraction
	}
	// Effective git strategy: workflow-level wins, else project-level,
	// else "local" — single source of truth (db.EffectiveGitStrategy) so
	// PR-path semantics never drift from the worktree/runtime layers.
	if db.EffectiveGitStrategy(ctx, tx, "tnt_dev", run.WorkflowID, run.ProjectID) != "pr" {
		return false
	}
	// Find the step's config in the published workflow version so
	// requiresPRStep sees the same requires_pr flag the WorkflowReconciler
	// uses at step-success.
	config := ""
	if run.WorkflowID != "" {
		if ver, verr := db.GetWorkflowVersion(ctx, tx, "tnt_dev", run.WorkflowID, run.WorkflowVersion); verr == nil {
			if steps, perr := workflow.ParseSteps(ver.Steps); perr == nil {
				for _, s := range steps {
					if s.ID == exec.WorkflowStepID {
						config = s.Config
						break
					}
				}
			}
		}
	}
	return requiresPRStep(exec.WorkflowStepID, config)
}

func (r *TaskReconciler) boundToActiveRun(ctx context.Context, tx pgx.Tx, wi db.WorkItemRow) bool {
	if wi.WorkflowRunID == "" {
		return false
	}
	run, err := db.GetWorkflowRun(ctx, tx, wi.TenantID, wi.WorkflowRunID)
	if err != nil {
		return false
	}
	switch run.Status {
	case domain.WorkflowRunPending, domain.WorkflowRunRunning, domain.WorkflowRunPaused:
		return true
	}
	return false
}

// writeOrchiconFiles writes the execution results to .orchicon/ files
// in the project directory so the next worker can read previous step
// results from disk instead of receiving them inline in the prompt.
// Files are overwritten on every execution so disk always reflects
// the latest state. Best-effort — failures are logged but not fatal.
func (r *TaskReconciler) writeOrchiconFiles(ctx context.Context, exec db.ExecutionRow, wi db.WorkItemRow, succeeded bool, results map[string]any) {
	projectDir := r.lookupProjectDir(ctx, wi.ProjectID)
	if projectDir == "" {
		return
	}
	orchDir := filepath.Join(projectDir, ".orchicon", exec.WorkflowRunID)
	if err := os.MkdirAll(orchDir, 0755); err != nil {
		r.log.Warn("write .orchicon files: mkdir", "dir", orchDir, "error", err)
		return
	}

	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(orchDir, name), []byte(content), 0644); err != nil {
			r.log.Warn("write .orchicon file", "file", name, "error", err)
		}
	}

	write("status", map[bool]string{true: "success", false: "failure"}[succeeded])
	write("worker", exec.WorkerID)

	if summary, ok := results["_summary"].(string); ok && summary != "" {
		write("summary", summary)
		// Facts ledger: persist the FACTS LEARNED lines this step recorded
		// so later steps can read them from disk (the composite prompt
		// points the next worker at .orchicon/<run>/facts_learned first —
		// it is the single authoritative source of established facts since
		// the embedded "## Facts learned (this run)" prompt block was
		// removed). Append to any facts earlier steps already recorded.
		// Each line carries the originating step name so downstream workers
		// keep the same per-step attribution the embedded block used to give.
		facts := extractFactsLearned(summary)
		if len(facts) > 0 {
			stepName := r.stepNameForExecution(ctx, exec)
			existing := ""
			if b, err := os.ReadFile(filepath.Join(orchDir, "facts_learned")); err == nil {
				existing = string(b)
			}
			var sb strings.Builder
			if existing != "" {
				sb.WriteString(existing)
				if !strings.HasSuffix(existing, "\n") {
					sb.WriteString("\n")
				}
			}
			for _, f := range facts {
				if stepName != "" {
					sb.WriteString("FACTS LEARNED (from ")
					sb.WriteString(stepName)
					sb.WriteString("): ")
				} else {
					sb.WriteString("FACTS LEARNED: ")
				}
				sb.WriteString(f)
				sb.WriteString("\n")
			}
			write("facts_learned", strings.TrimSpace(sb.String()))
		}
	}
	// The `issues` file is the feedback channel the composite prompt points
	// the next worker at ("read .orchicon/<run>/issues"). It must ALWAYS be
	// written when there is anything to communicate — a reviewer that
	// reports problems via _summary (rather than a separate _issues block)
	// would otherwise leave the file missing and the next worker blind.
	// Prefer _issues; fall back to _summary.
	if issues, ok := results["_issues"].(string); ok && issues != "" {
		write("issues", issues)
	} else if summary, ok := results["_summary"].(string); ok && summary != "" {
		write("issues", summary)
	}
	// The `files` file lists every path the session wrote (opencode
	// file_diff telemetry) so the next worker knows exactly what to read.
	if files, ok := results["_touched_files"].([]any); ok && len(files) > 0 {
		var sb strings.Builder
		for _, f := range files {
			if s, ok := f.(string); ok {
				sb.WriteString(s)
				sb.WriteString("\n")
			}
		}
		write("touched_files", strings.TrimSpace(sb.String()))
	}

	// Per-step archive: persist this execution's results to a per-step file
	// so the delta handoff (execution-history index) can point workers at a
	// stable, complete per-step record on disk. The run-level summary/status
	// files are overwritten by each step; these per-step files are never
	// overwritten, so a later step can `read`/`grep` the exact output of ANY
	// earlier step (the "read on demand from the archive" contract instead of
	// re-embedding every past summary into the prompt).
	if exec.WorkflowStepID != "" {
		stepDir := filepath.Join(orchDir, "steps")
		if err := os.MkdirAll(stepDir, 0755); err == nil {
			stepFile := filepath.Join(stepDir, exec.WorkflowStepID+".md")
			var b strings.Builder
			fmt.Fprintf(&b, "# Step archive: %s\n\n", exec.WorkflowStepID)
			fmt.Fprintf(&b, "Status: %s\n", map[bool]string{true: "success", false: "failure"}[succeeded])
			b.WriteString("Worker: " + exec.WorkerID + "\n")
			if summary, ok := results["_summary"].(string); ok && summary != "" {
				b.WriteString("\n## Summary\n\n" + summary + "\n")
			}
			if issues, ok := results["_issues"].(string); ok && issues != "" {
				b.WriteString("\n## Issues\n\n" + issues + "\n")
			}
			if files, ok := results["_touched_files"].([]any); ok && len(files) > 0 {
				b.WriteString("\n## Touched files\n\n")
				for _, f := range files {
					if s, ok := f.(string); ok {
						b.WriteString("- " + s + "\n")
					}
				}
			}
			if err := os.WriteFile(stepFile, []byte(b.String()), 0644); err != nil {
				r.log.Warn("write per-step .orchicon file", "file", stepFile, "error", err)
			}
		}
	}
}

// stepNameForExecution resolves the workflow step name that dispatched an
// execution, so facts written to .orchicon/<run>/facts_learned can carry
// per-step attribution (the embedded "## Facts learned (this run)" prompt
// block that used to supply that attribution is gone). Looks up the step run
// that owns the execution via worker_execution_id. Best-effort: returns ""
// when the execution isn't tied to a step run (direct dispatch) or the lookup
// fails — the facts then fall back to the plain marker.
func (r *TaskReconciler) stepNameForExecution(ctx context.Context, exec db.ExecutionRow) string {
	if exec.TenantID == "" || exec.ID == "" {
		return ""
	}
	ttx, err := r.pool.BeginTenantTx(ctx, exec.TenantID)
	if err != nil {
		r.log.Warn("step name lookup: begin tx", "error", err)
		return ""
	}
	defer ttx.Rollback(ctx)
	var name string
	err = ttx.Tx.QueryRow(ctx,
		`SELECT step_name FROM workflow_step_runs
		  WHERE tenant_id = $1 AND worker_execution_id = $2 LIMIT 1`,
		exec.TenantID, exec.ID,
	).Scan(&name)
	if err != nil {
		return ""
	}
	_ = ttx.Commit(ctx)
	return name
}

// lookupProjectDir returns the project directory for a project id.
// Returns "" if the project is not found.
func (r *TaskReconciler) lookupProjectDir(ctx context.Context, projectID string) string {
	if projectID == "" {
		return ""
	}
	ttx, err := r.pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		r.log.Warn("lookup project dir: begin tx", "error", err)
		return ""
	}
	defer ttx.Rollback(ctx)
	var dir string
	err = ttx.Tx.QueryRow(ctx,
		`SELECT project_dir FROM projects WHERE id = $1 AND tenant_id = $2`,
		projectID, "tnt_dev",
	).Scan(&dir)
	if err != nil || dir == "" {
		return ""
	}
	ttx.Commit(ctx)
	return dir
}

// OnHealth is called by the adapter bridge to update the execution's
// health_state (docs/03 §5: HealthMonitor recomputes from signals).
func (r *TaskReconciler) OnHealth(ctx context.Context, execID, healthState string) {
	ttx, err := r.pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		return
	}
	defer ttx.Rollback(ctx)
	current, err := db.GetExecution(ctx, ttx.Tx, "tnt_dev", execID)
	if err != nil {
		return
	}
	_, _ = db.UpdateExecution(ctx, ttx.Tx, "tnt_dev", execID, current.Version, db.UpdateExecutionFields{
		HealthState: &healthState,
	})
	_ = ttx.Commit(ctx)
}

// OnStall is called by the adapter bridge's progress monitor when a stall
// signal trips (docs/06 §2: "stalled health state | no progress within
// stall window"; docs/03 §5). The reason carries which signal fired:
// stalled:no_progress | stalled:no_file_progress | stalled:repetition:<sig>.
//
// `fatal` distinguishes the two cases:
//
//   - FATAL (no_progress only): total silence — there is no responsive
//     surface to nudge. The adapter has already hard-killed the subprocess,
//     so the execution is marked unhealthy and the work item transitions to
//     failed — the downstream recover step (if any) activates on the next
//     reconcile pass.
//   - ADVISORY (text_loop / repetition / no_file_progress): the worker is
//     generating text / issuing tool calls, so a nudge can reach it. The
//     subprocess is NOT killed and the execution is NOT failed. The
//     execution gets a non-terminal `stalled` health notice + reason so the
//     operator sees the flag, stays `running`, and is either revived to
//     healthy when progress resumes (OnRecovered) or — after the nudge
//     budget is spent without the worker breaking the pattern — escalated
//     to a fatal kill + recovery by the session's onStall.
func (r *TaskReconciler) OnStall(ctx context.Context, execID, reason string, fatal bool) {
	r.log.Warn("execution stalled",
		"execution", execID, "reason", reason, "fatal", fatal)
	// Surface the stall: health_state → stalled (non-terminal) and persist
	// the reason so the UI shows why the execution was flagged.
	r.OnHealth(ctx, execID, domain.HealthStalled)
	ttx, txErr := r.pool.BeginTenantTx(ctx, "tnt_dev")
	if txErr == nil {
		current, getErr := db.GetExecution(ctx, ttx.Tx, "tnt_dev", execID)
		if getErr == nil {
			_, _ = db.UpdateExecution(ctx, ttx.Tx, "tnt_dev", execID, current.Version, db.UpdateExecutionFields{
				ErrorMessage: &reason,
			})
		}
		_ = ttx.Commit(ctx)
	}
	if !fatal {
		return
	}
	// Terminate the execution and fail the work item so the downstream
	// recover step (if any) activates on the next reconcile pass.
	r.updateExecStatus(ctx, execID, domain.ExecutionUnhealthy, domain.HealthUnhealthy, "", reason)
	r.transitionWorkItemOnResult(ctx, execID, false, reason)
}

// OnRecovered is called by the adapter bridge's progress monitor when an
// advisory stall clears — the worker resumed the missing progress signal
// (a file_diff after a no_file_progress trip) before the execution
// terminated. The execution is revived back to healthy and the stall
// notice is cleared. This is non-terminal: status stays `running` and the
// terminal OnResult still decides success/failure.
func (r *TaskReconciler) OnRecovered(ctx context.Context, execID, recovered string) {
	r.log.Info("execution recovered from advisory stall",
		"execution", execID, "recovered", recovered)
	ttx, err := r.pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		return
	}
	defer ttx.Rollback(ctx)
	current, err := db.GetExecution(ctx, ttx.Tx, "tnt_dev", execID)
	if err != nil {
		return
	}
	// Only revive an execution still in flight — a terminal execution
	// (OnResult already landed) must not be resurrected.
	if current.Status != domain.ExecutionRunning {
		return
	}
	healthy := domain.HealthHealthy
	fields := db.UpdateExecutionFields{HealthState: &healthy}
	if strings.HasPrefix(current.ErrorMessage, "stalled:") {
		clear := ""
		fields.ErrorMessage = &clear
	}
	_, _ = db.UpdateExecution(ctx, ttx.Tx, "tnt_dev", execID, current.Version, fields)
	_ = ttx.Commit(ctx)
}

// clearStallNotice removes a "stalled:" reason from error_message. Called
// when an execution that was flagged by the advisory stall monitor reaches
// success, so a succeeded execution doesn't carry a stale stall error
// (observed: the SSE worker completed successfully yet kept
// `error_message = stalled:no_file_progress`).
func (r *TaskReconciler) clearStallNotice(ctx context.Context, execID string) {
	ttx, err := r.pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		return
	}
	defer ttx.Rollback(ctx)
	current, err := db.GetExecution(ctx, ttx.Tx, "tnt_dev", execID)
	if err != nil {
		return
	}
	if !strings.HasPrefix(current.ErrorMessage, "stalled:") {
		return
	}
	clear := ""
	_, _ = db.UpdateExecution(ctx, ttx.Tx, "tnt_dev", execID, current.Version, db.UpdateExecutionFields{
		ErrorMessage: &clear,
	})
	_ = ttx.Commit(ctx)
}

// OnToolCall publishes a tool_call execution event so the frontend live
// session pane can show the tool invocation in real-time.
func (r *TaskReconciler) OnToolCall(ctx context.Context, execID, toolName string, input, output []byte) {
	ttx, err := r.pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		return
	}
	defer ttx.Rollback(ctx)
	current, err := db.GetExecution(ctx, ttx.Tx, "tnt_dev", execID)
	if err != nil {
		r.log.Error("on tool call: get execution", "execution", execID, "error", err)
		return
	}
	_ = enqueueExecEvent(ctx, ttx.Tx, "execution.tool_call", current, map[string]any{
		"tool_name": toolName,
		"input":     string(input),
		"output":    string(output),
	})
	_ = ttx.Commit(ctx)
	r.publishExecEvent(ctx, "execution.tool_call", current, map[string]any{
		"tool_name": toolName,
		"input":     string(input),
		"output":    string(output),
	})
}

// OnText publishes a text execution event so the frontend live session
// pane can show model output in real-time.
func (r *TaskReconciler) OnText(ctx context.Context, execID string, text string) {
	ttx, err := r.pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		return
	}
	defer ttx.Rollback(ctx)
	current, err := db.GetExecution(ctx, ttx.Tx, "tnt_dev", execID)
	if err != nil {
		r.log.Error("on text: get execution", "execution", execID, "error", err)
		return
	}
	_ = enqueueExecEvent(ctx, ttx.Tx, "execution.text", current, map[string]any{
		"text": text,
	})
	_ = ttx.Commit(ctx)
	r.publishExecEvent(ctx, "execution.text", current, map[string]any{
		"text": text,
	})
}

// OnArtifact publishes an artifact execution event so the frontend live
// session pane can show model output files inline (docs/10 §11). Called
// by the adapter when the model uses the `write` tool (opencode built-in
// file writer). The name is the file path, artifactType is "markdown" /
// "json" / "text", and content is the full artifact body.
func (r *TaskReconciler) OnArtifact(ctx context.Context, execID, name, artifactType, content string) {
	ttx, err := r.pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		return
	}
	defer ttx.Rollback(ctx)
	current, err := db.GetExecution(ctx, ttx.Tx, "tnt_dev", execID)
	if err != nil {
		r.log.Error("on artifact: get execution", "execution", execID, "error", err)
		return
	}
	_ = enqueueExecEvent(ctx, ttx.Tx, "execution.artifact", current, map[string]any{
		"artifact_name": name,
		"artifact_type": artifactType,
		"content":       content,
	})
	_ = ttx.Commit(ctx)
	r.publishExecEvent(ctx, "execution.artifact", current, map[string]any{
		"artifact_name": name,
		"artifact_type": artifactType,
		"content":       content,
	})
}

func (r *TaskReconciler) updateExecStatus(ctx context.Context, execID, status, health string, output string, errorMessage ...string) {
	ttx, err := r.pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		r.log.Error("begin tx for status update", "execution", execID, "error", err)
		return
	}
	defer ttx.Rollback(ctx)
	current, err := db.GetExecution(ctx, ttx.Tx, "tnt_dev", execID)
	if err != nil {
		r.log.Error("get execution for status update", "execution", execID, "error", err)
		return
	}
	var endedAt *time.Time
	if status == domain.ExecutionSucceeded || status == domain.ExecutionFailed || status == domain.ExecutionTerminated || status == domain.ExecutionUnhealthy {
		now := time.Now().UTC()
		endedAt = &now
	}
	fields := db.UpdateExecutionFields{
		Status:      &status,
		HealthState: &health,
		EndedAt:     endedAt,
	}
	if len(errorMessage) > 0 && errorMessage[0] != "" {
		fields.ErrorMessage = &errorMessage[0]
	}
	if output != "" {
		fields.Output = &output
	}
	updated, err := db.UpdateExecution(ctx, ttx.Tx, "tnt_dev", execID, current.Version, fields)
	if err != nil {
		r.log.Error("update execution status", "execution", execID, "error", err)
		return
	}
	// Enqueue event.
	eventType := "execution." + status
	_ = enqueueExecEvent(ctx, ttx.Tx, eventType, updated, nil)
	if err := ttx.Commit(ctx); err != nil {
		r.log.Error("commit status update", "execution", execID, "error", err)
		return
	}
	r.publishExecEvent(ctx, eventType, updated, nil)
}

// appendToParentConversation appends the assistant's output to the parent
// execution's conversation field so follow-up messages and responses appear
// as one continuous thread on the original execution detail page.
func (r *TaskReconciler) appendToParentConversation(ctx context.Context, tenantID, parentExecID, output string) {
	ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		r.log.Error("append to parent conversation: begin tx", "parent_execution", parentExecID, "error", err)
		return
	}
	defer ttx.Rollback(ctx)

	parent, err := db.GetExecution(ctx, ttx.Tx, tenantID, parentExecID)
	if err != nil {
		r.log.Error("append to parent conversation: get parent", "parent_execution", parentExecID, "error", err)
		return
	}
	conv := parent.Conversation
	if len(conv) == 0 {
		conv = []byte("[]")
	}
	var entries []map[string]any
	if err := json.Unmarshal(conv, &entries); err != nil {
		entries = []map[string]any{}
	}
	truncated := output
	if len(truncated) > 32000 {
		truncated = truncated[:32000]
	}
	entries = append(entries, map[string]any{
		"role":       "assistant",
		"content":    truncated,
		"type":       "follow_up_response",
		"created_at": time.Now().UTC().Format(time.RFC3339),
	})
	updatedConv, err := json.Marshal(entries)
	if err != nil {
		r.log.Error("append to parent conversation: marshal", "parent_execution", parentExecID, "error", err)
		return
	}
	if _, err := db.UpdateExecution(ctx, ttx.Tx, tenantID, parentExecID, parent.Version, db.UpdateExecutionFields{
		Conversation: &updatedConv,
	}); err != nil {
		r.log.Error("append to parent conversation: update", "parent_execution", parentExecID, "error", err)
		return
	}
	if err := ttx.Commit(ctx); err != nil {
		r.log.Error("append to parent conversation: commit", "parent_execution", parentExecID, "error", err)
		return
	}
	r.log.Info("follow-up response appended to parent conversation",
		"parent_execution", parentExecID, "output_len", len(truncated))
}

// --- helpers ---------------------------------------------------------------

// publishExecEvent builds the same event payload as enqueueExecEvent and
// publishes it directly to NATS via the reconciler's direct publisher.
// This bypasses the outbox relay's 500ms poll interval so the frontend
// event stream receives events in near-real-time (~1ms vs ~500ms).
// Must be called AFTER the outbox transaction commits so the event is
// committed to the DB before being published (the outbox serves as the
// durable fallback for catch-up on reconnect).
func (r *TaskReconciler) publishExecEvent(ctx context.Context, eventType string, e db.ExecutionRow, extra map[string]any) {
	if r.eventPub == nil {
		return
	}
	evt := map[string]any{
		"event_type":     eventType,
		"tenant_id":      e.TenantID,
		"execution_id":   e.ID,
		"task_id":        e.TaskID,
		"project_id":     e.ProjectID,
		"worker_id":      e.WorkerID,
		"worker_version": e.WorkerVersion,
		"status":         e.Status,
		"health_state":   e.HealthState,
		"aggregate_type": "execution",
		"aggregate_id":   e.ID,
		"occurred_at":    time.Now().UTC().Format(time.RFC3339Nano),
	}
	for k, v := range extra {
		evt[k] = v
	}
	payload, err := json.Marshal(evt)
	if err != nil {
		r.log.Error("publish exec event marshal", "execution", e.ID, "error", err)
		return
	}
	subject := eventbus.SubjectFor("execution", eventType)
	// Use the execution ID + event type as the dedup key so the outbox
	// relay's eventual publish with its own MsgID (the outbox row ULID)
	// is a distinct message — the frontend's seenIds dedup catches the
	// duplicate. This is intentional: the direct publish arrives fast,
	// the outbox relay provides the durable fallback.
	dedupID := fmt.Sprintf("direct:%s:%s", e.ID, eventType)
	if err := r.eventPub.Publish(ctx, subject, dedupID, payload); err != nil {
		r.log.Warn("publish exec event", "execution", e.ID, "subject", subject, "error", err)
	}
}

func enqueueExecEvent(ctx context.Context, tx pgx.Tx, eventType string, e db.ExecutionRow, extra map[string]any) error {
	evt := map[string]any{
		"event_type":     eventType,
		"tenant_id":      e.TenantID,
		"execution_id":   e.ID,
		"task_id":        e.TaskID,
		"project_id":     e.ProjectID,
		"worker_id":      e.WorkerID,
		"worker_version": e.WorkerVersion,
		"status":         e.Status,
		"health_state":   e.HealthState,
		"aggregate_type": "execution",
		"aggregate_id":   e.ID,
		"occurred_at":    time.Now().UTC().Format(time.RFC3339Nano),
	}
	for k, v := range extra {
		evt[k] = v
	}
	payload, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	return db.EnqueueOutbox(ctx, tx, db.OutboxRow{
		TenantID:      e.TenantID,
		EventType:     eventType,
		AggregateType: "execution",
		AggregateID:   e.ID,
		Payload:       payload,
		OccurredAt:    time.Now().UTC(),
	})
}

func enqueueWorkItemEvent(ctx context.Context, tx pgx.Tx, eventType string, w db.WorkItemRow) error {
	evt := map[string]any{
		"event_type":   eventType,
		"tenant_id":    w.TenantID,
		"work_item_id": w.ID,
		"project_id":   w.ProjectID,
		"status":       w.Status,
		"kind":         w.Kind,
		"title":        w.Title,
		"occurred_at":  time.Now().UTC().Format(time.RFC3339Nano),
	}
	payload, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	return db.EnqueueOutbox(ctx, tx, db.OutboxRow{
		TenantID:      w.TenantID,
		EventType:     eventType,
		AggregateType: "work_item",
		AggregateID:   w.ID,
		Payload:       payload,
		OccurredAt:    time.Now().UTC(),
	})
}

// mergeBudgets merges the tenant-level default budget (base) with a worker's
// own budget_overrides (override) into a single JSON document, so a worker's
// explicit field always wins per-key over the tenant default. Fields absent
// from both are omitted (the adapter's built-in defaults apply). Either input
// may be empty.
func mergeBudgets(tenantDefault, workerOverride []byte) []byte {
	out := map[string]any{}
	mu := func(b []byte) {
		if len(b) == 0 {
			return
		}
		var m map[string]any
		if json.Unmarshal(b, &m) == nil {
			for k, v := range m {
				out[k] = v
			}
		}
	}
	// Tenant default is the base; worker override is layered on top.
	mu(tenantDefault)
	mu(workerOverride)
	b, err := json.Marshal(out)
	if err != nil {
		return workerOverride
	}
	return b
}

func strPtr(s string) *string { return &s }

func boolPtr(b bool) *bool { return &b }

// extractIssuesLine captures an `_issues:` block from the worker's
// output for the run view and .orchicon/issues. It is INFORMATIONAL
// ONLY — it never influences the workflow decision, which is the single
// ORCHICON WORKER SUMMARY: word. It matches only lines that START with
// `_issues:` (after optional markdown list bullets / code-fence markers)
// so the literal substring `_issues:` in prose like "(not `_issues:`)"
// can never be misread as an issues block.
func extractIssuesLine(output string, results map[string]any) {
	if output == "" {
		return
	}
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		// Strip a leading markdown list bullet ("- " / "* ").
		trimmed = strings.TrimPrefix(trimmed, "- ")
		trimmed = strings.TrimPrefix(trimmed, "* ")
		trimmed = strings.TrimSpace(trimmed)
		// Strip markdown code-fence markers.
		trimmed = strings.TrimPrefix(trimmed, "```")
		trimmed = strings.TrimSuffix(trimmed, "```")
		trimmed = strings.TrimSpace(trimmed)
		if i := strings.Index(trimmed, "_issues:"); i == 0 {
			if _, ok := results["_issues"]; !ok {
				issues := strings.TrimSpace(trimmed[len("_issues:"):])
				if issues != "" {
					results["_issues"] = issues
				}
			}
		}
	}
}

// extractPRFields parses PR_URL: and PR_STATE: lines from the worker's
// final output and returns the extracted (prURL, prState). The contract:
//
// Workers emit these lines anywhere in their final output, line-anchored:
//
//	PR_URL: https://github.com/OWNER/REPO/pull/42
//	PR_STATE: merged
//
// Rules:
//
//   - Last occurrence of each line wins (final state is authoritative).
//   - Leading markdown bullets ("- " / "* ") and code-fence markers (```),
//     stripped before matching — exactly as extractIssuesLine does.
//   - PR_URL: value after the colon, trimmed. Accepted only if non-empty and
//     an absolute http:// or https:// URL (net/url parse + scheme check).
//   - PR_STATE: value after the colon, trimmed, lowercased. Accepted set:
//     open, merged, draft, closed, none. Unknown values are ignored.
//   - Lines are case-sensitive uppercase (consistent with
//     ORCHICON WORKER SUMMARY: and FACTS LEARNED:).
//   - Extraction is not gated on execution success: a PR URL is a fact.
//   - Only non-empty fields are written, per-key: a URL-only report must
//     not clobber a previously recorded state, and vice versa.
//
// Returns ("", "") when no valid lines are found.
func extractPRFields(output string) (prURL, prState string) {
	if output == "" {
		return "", ""
	}
	validStates := map[string]bool{
		"open":   true,
		"merged": true,
		"draft":  true,
		"closed": true,
		"none":   true,
	}
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		// Strip a leading markdown list bullet ("- " / "* ").
		trimmed = strings.TrimPrefix(trimmed, "- ")
		trimmed = strings.TrimPrefix(trimmed, "* ")
		trimmed = strings.TrimSpace(trimmed)
		// Strip markdown code-fence markers.
		trimmed = strings.TrimPrefix(trimmed, "```")
		trimmed = strings.TrimSuffix(trimmed, "```")
		trimmed = strings.TrimSpace(trimmed)

		if strings.HasPrefix(trimmed, "PR_URL:") {
			raw := strings.TrimSpace(trimmed[len("PR_URL:"):])
			if raw == "" {
				continue
			}
			if u, err := url.Parse(raw); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
				if u.Host != "" {
					prURL = raw
				}
			}
		}
		if strings.HasPrefix(trimmed, "PR_STATE:") {
			raw := strings.TrimSpace(trimmed[len("PR_STATE:"):])
			state := strings.ToLower(raw)
			if validStates[state] {
				prState = state
			}
		}
	}
	// Change 3: inline-marker fallback. The line-anchored pass above is the
	// primary path; when it finds nothing, scan the WHOLE output with a
	// non-anchored regex so a PR_URL:/PR_STATE: marker glued mid-sentence
	// (the 4.2 case) is still captured. Last match wins.
	if prURL == "" {
		if ms := prURLRe.FindAllStringSubmatch(output, -1); len(ms) > 0 {
			raw := ms[len(ms)-1][1]
			if u, err := url.Parse(raw); err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
				prURL = raw
			}
		}
	}
	if prState == "" {
		if ms := prStateMarkerRe.FindAllStringSubmatch(output, -1); len(ms) > 0 {
			if st := strings.ToLower(ms[len(ms)-1][1]); validStates[st] {
				prState = st
			}
		}
	}
	return prURL, prState
}

// extractComposite pulls the "composite" string out of a work item's
// prompt_context JSON (set by the WorkflowReconciler's buildCompositePrompt).
// Returns "" if the field is absent or unparseable.
func extractComposite(pc []byte) (string, error) {
	if len(pc) == 0 {
		return "", nil
	}
	var parsed struct {
		Composite string `json:"composite"`
	}
	if err := json.Unmarshal(pc, &parsed); err != nil {
		return "", err
	}
	return parsed.Composite, nil
}

// composeSystemPrompt assembles the worker-only system prompt from the
// four structured fields (role, skills, behavior, agents_md). Used as
// a fallback when the WorkflowReconciler did not build a composite
// (i.e. a work item dispatched outside a workflow, on the legacy
// direct path). The full system prompt the model sees on every turn
// is the composite — which itself contains composeSystemPrompt's
// output prepended under a "# Worker" section.
// workerIdentityPreamble is prepended to every worker system prompt so
// the model knows it is an autonomous Orchicon worker, not a human
// operator or an interactive session. It is the identity statement that
// distinguishes an in-Orchicon worker (operates autonomously, reports
// via the ORCHICON WORKER SUMMARY contract) from a human-facing session
// (must ask before PRing/merging). Both composite builders
// (buildStandaloneComposite and the workflow buildCompositePrompt) emit
// it so every dispatch carries the same self-definition.
//
// The canonical text lives in internal/db (db.WorkerIdentityPreamble) so
// the stable prompt prefix (db.StablePromptPrefix) can be built from a
// single shared constant. This alias keeps the scheduler's call sites
// terse.
const workerIdentityPreamble = db.WorkerIdentityPreamble

func composeSystemPrompt(v db.WorkerVersionRow) string {
	if v.Role == "" && v.Skills == "" && v.Behavior == "" && v.AgentsMD == "" {
		return v.SystemPrompt
	}
	var parts []string
	add := func(heading, content string) {
		c := strings.TrimSpace(content)
		if c == "" {
			return
		}
		parts = append(parts, "# "+heading+"\n\n"+c)
	}
	add("Role", v.Role)
	add("Skills", v.Skills)
	add("Behavior", v.Behavior)
	add("AGENTS.md", v.AgentsMD)
	return strings.Join(parts, "\n\n")
}

// buildStandaloneComposite assembles the full worker prompt for a work
// item dispatched OUTSIDE a workflow (the TaskReconciler's direct
// dispatch path, where the WorkflowReconciler never built a per-step
// composite). It produces the same shape the workflow path renders —
// worker identity, the task, the project directory + project
// context_files (files AND directories, via the shared renderer), the
// work item's own context_files, and the worker's contract — so
// standalone tasks see project + work-item context "just like projects".
//
// Best-effort: any DB read failure degrades to the subset that succeeded
// (the caller falls back to a bare worker prompt if the result is empty).
func buildStandaloneComposite(pool *db.Pool, exec db.ExecutionRow, task db.WorkItemRow, version db.WorkerVersionRow, worktreeStatus, worktreeBranch string) (string, string) {
	var sb strings.Builder
	// Context-file fingerprint (ADR-0009 D5): same sha256 over the
	// project + work-item context-file stamps the workflow path
	// computes, so both dispatch paths export the cache-correlation
	// value identically.
	contextFP := "none"
	// Stable prefix first: shared identity + safety + efficiency + runtime
	// environment. Same byte-identical block the workflow path prepends, so a
	// standalone dispatch and a workflow step share the llama.cpp KV-cache
	// prefix.
	sb.WriteString(db.StablePromptPrefix(task.RuntimeImage))
	if worker := composeSystemPrompt(version); worker != "" {
		fmt.Fprintf(&sb, "# Worker\n\n%s\n\n", worker)
	}

	// Task.
	sb.WriteString("# Task\n\n")
	fmt.Fprintf(&sb, "Original work item: \"%s\"\n\n", strings.TrimSpace(task.Title))
	if d := strings.TrimSpace(task.Description); d != "" {
		fmt.Fprintf(&sb, "Description:\n%s\n\n", d)
	}
	if ac := strings.TrimSpace(task.AcceptanceCriteria); ac != "" {
		fmt.Fprintf(&sb, "Acceptance criteria:\n%s\n\n", ac)
	}

	// Recovery context: same-worker recovery-resumed dispatch reads the
	// seed and points at .orchicon/worker.recovery (transcript tail +
	// already-done directive). A different worker / fresh dispatch gets
	// no block and never sees the file. buildStandaloneComposite parity
	// with the workflow path (the recovery engine writes the seed keys
	// into the work item's Results on resume).
	if seed := recoverySeedFor(nil, task.Results, version.WorkerID); seed != nil {
		sb.WriteString(recoveryFileReferenceBlock(seed))
	}

	// Project context — project_dir + project context_files.
	projectDir := ""
	if task.ProjectID != "" {
		var p db.ProjectRow
		ctx := context.Background()
		if ttx, err := pool.BeginTenantTx(ctx, exec.TenantID); err == nil {
			if proj, err := db.GetProject(ctx, ttx.Tx, exec.TenantID, task.ProjectID); err == nil {
				p = proj
			}
			_ = ttx.Rollback(ctx)
		}
		projectDir = p.ProjectDir
		if p.ProjectDir != "" || len(p.ContextFiles) > 0 {
			var ctxSB strings.Builder
			if p.ProjectDir != "" {
				fmt.Fprintf(&ctxSB, "Working directory: `%s`\n\n", p.ProjectDir)
			}
			var files []string
			_ = json.Unmarshal(p.ContextFiles, &files)
			section, fp := renderContextSectionCached(globalPromptCache, standalonePromptLog(), exec.TenantID, task.ProjectID, "# Project context", files, p.ProjectDir)
			ctxSB.WriteString(section)
			if fp != "" {
				contextFP = fp
			}
			if ctxSB.Len() > 0 {
				sb.WriteString(ctxSB.String())
			}
		}
	}

	// Work item context — the item's own context_files (same renderer).
	if len(task.ContextFiles) > 0 {
		var files []string
		_ = json.Unmarshal(task.ContextFiles, &files)
		section, fp := renderContextSectionCached(globalPromptCache, standalonePromptLog(), exec.TenantID, task.ProjectID, "# Work item context", files, projectDir)
		if section != "" {
			sb.WriteString(section)
		}
		if fp != "" {
			contextFP = contextFP + "." + fp
		}
	}

	// Worker's contract.
	sb.WriteString("# Instructions\n\n")
	// Git/branch guidance keyed on the run's worktree_status AND effective
	// git strategy: a non-repo (in-place) run is never told to work on a
	// branch, and a `none` (ephemeral) run is told the worktree is detached
	// HEAD with nothing pushed. Same block the workflow composite emits, so
	// the two dispatch paths agree.
	gitStrategy := db.DefaultGitStrategy
	if task.WorkflowID != nil || task.ProjectID != "" {
		tctx := context.Background()
		if ttx, err := pool.BeginTenantTx(tctx, exec.TenantID); err == nil {
			wfID := ""
			if task.WorkflowID != nil {
				wfID = *task.WorkflowID
			}
			gitStrategy = db.EffectiveGitStrategy(tctx, ttx.Tx, exec.TenantID, wfID, task.ProjectID)
			_ = ttx.Rollback(tctx)
		}
	}
	sb.WriteString(db.GitGuidanceBlock(worktreeStatus, worktreeBranch, projectDir, gitStrategy))
	sb.WriteString("The workflow routes on exactly one signal: the word after `ORCHICON WORKER SUMMARY:` — `success` or `failure`. There is no `_issues:` failure channel. If work genuinely cannot be accepted, end with `ORCHICON WORKER SUMMARY: failure` and say what needs fixing in the summary text. Non-blocking observations belong in the summary text only and never affect the routing.\n\n")
	sb.WriteString("Format:\n")
	sb.WriteString("```\nORCHICON WORKER SUMMARY: success — Implemented the feature.\n```\n")
	sb.WriteString("or\n")
	sb.WriteString("```\nORCHICON WORKER SUMMARY: failure — Found 3 bugs in the implementation.\n```\n\n")
	sb.WriteString("The first word (`success` or `failure`) is used to route the workflow. The text after `—` is passed to the next stage as the summary of your work.\n\n")
	sb.WriteString("Keep your final summary concise — under ~500 tokens (roughly 2000 characters). It is re-embedded into every later step's prompt and persisted to `.orchicon/<run>/summary`, so verbosity taxes all downstream steps. `FACTS LEARNED:` lines and blocking feedback are exempt from the cap.\n\n")

	return sb.String(), contextFP
}

// summaryMarker is the literal line the worker's prompt instructs it to
// end with. Everything from the marker (inclusive) to the end of the
// worker's output becomes the summary that flows downstream as upstream
// context. If absent, the entire output is treated as the summary so
// lenient workers that don't follow the contract still propagate.
//
// The marker is followed by an optional decision prefix (first word,
// "success" or "failure") and the summary text:
//
//	ORCHICON WORKER SUMMARY: success — Implemented the feature.
//	ORCHICON WORKER SUMMARY: failure — Found 3 bugs.
//
// The `—` separator is optional; anything after the first word is the
// summary. If no decision prefix is present, the full text is treated
// as the summary (backward compatible).
const summaryMarker = "ORCHICON WORKER SUMMARY:"

// placeholderSummaryBody reports whether the text following an
// ORCHICON WORKER SUMMARY marker is a placeholder/template echo (the worker
// wrote the marker as an *example* inside a plan — e.g. ending with
// `ORCHICON WORKER SUMMARY: success — <summary>`) rather than the real
// sign-off. A placeholder must not advance the workflow on a fake `success`;
// the lenient fallback (no real marker → full output as summary) already
// covers genuinely non-compliant workers, so this only filters out the
// hollow echo. Keep in sync with internal/opencode/session_run.go
// placeholderMarkerBody.
func placeholderSummaryBody(rest string) bool {
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return true
	}
	// Inline code (backtick-quoted) markers are seed/instruction echo, never a
	// real sign-off. The recovery seed writes `` `ORCHICON WORKER SUMMARY:
	// failure` reason `recovery seed file missing` `` and system prompts quote
	// `` `ORCHICON WORKER SUMMARY: success` `` as an example. The body right
	// after the marker is a backtick-wrapped word (e.g. "`failure`", or
	// "failure` reason ..."), so strip a leading backtick from the first word:
	// a bare `success`/`failure` in backticks is a placeholder, not a delivery.
	if first := firstWordAsDecision(rest); first != "" {
		words := strings.Fields(rest)
		if len(words) > 0 {
			raw := words[0]
			before, _ := strings.CutPrefix(raw, "`")
			after, afterBacktick := strings.CutSuffix(before, "`")
			if afterBacktick {
				lower := strings.ToLower(after)
				if lower == "success" || lower == "failure" {
					return true
				}
			}
		}
	}
	if strings.Contains(rest, "<summary>") || strings.Contains(rest, "<reason>") ||
		strings.Contains(rest, "<your summary>") || strings.Contains(rest, "<your-summary>") {
		return true
	}
	lower := strings.ToLower(rest)
	switch lower {
	case "", "success", "failure", "success —", "failure —", "success — <summary>", "failure — <reason>":
		return true
	}
	return false
}

// lastRealSummaryMarker returns the index of the LAST genuine
// ORCHICON WORKER SUMMARY marker in output — one whose body is real content,
// skipping earlier placeholder/template echoes. Returns -1 when the worker
// only ever wrote the marker as an example, never as an actual sign-off.
func lastRealSummaryMarker(output string) int {
	idx := strings.LastIndex(output, summaryMarker)
	for idx >= 0 {
		if !placeholderSummaryBody(output[idx+len(summaryMarker):]) {
			return idx
		}
		idx = strings.LastIndex(output[:idx], summaryMarker)
	}
	return -1
}

// extractWorkerSummary parses the ORCHICON WORKER SUMMARY block from
// the worker's text. It takes the LAST GENUINE occurrence of the marker
// (a marker used as a literal example inside a plan — `success — <summary>` —
// is treated as absent so a worker that never actually signed off has its
// full output propagated as the lenient fallback) and returns everything
// after it, trimmed, minus the decision prefix. If no real marker is
// present, the entire input is returned.
func extractWorkerSummary(output string) string {
	idx := lastRealSummaryMarker(output)
	if idx < 0 {
		return strings.TrimSpace(output)
	}
	rest := strings.TrimSpace(output[idx+len(summaryMarker):])
	return trimSummaryDecision(rest)
}

// extractSummaryDecision reads the first word of the summary block
// (the text after ORCHICON WORKER SUMMARY:) and returns "success",
// "failure", any other verbatim first word, or ""
// if no real marker is present (a placeholder echo like
// "success: <summary>" does not count).
func extractSummaryDecision(output string) string {
	idx := lastRealSummaryMarker(output)
	if idx < 0 {
		return ""
	}
	rest := strings.TrimSpace(output[idx+len(summaryMarker):])
	return firstWordAsDecision(rest)
}

// trimSummaryDecision removes the leading decision word (if present)
// and any separator from the summary text.
func trimSummaryDecision(s string) string {
	first := firstWordAsDecision(s)
	if first == "" {
		return s
	}
	// Remove the decision word and any separator that follows.
	rest := strings.TrimSpace(strings.TrimPrefix(s, first))
	rest = strings.TrimLeft(rest, "—:- ")
	return strings.TrimSpace(rest)
}

// firstWordAsDecision returns the normalized decision from the first
// whitespace-delimited word of s: "success"/"failure" for words that
// start with those prefixes. Any OTHER first word is passed through
// verbatim (lowercased). Returns "" only for an empty input.
func firstWordAsDecision(s string) string {
	parts := strings.Fields(s)
	if len(parts) == 0 {
		return ""
	}
	first := strings.ToLower(parts[0])
	switch {
	case strings.HasPrefix(first, "success"):
		return "success"
	case strings.HasPrefix(first, "failure"):
		return "failure"
	}
	return first
}

// maxSummaryTokens caps the narrative portion of a worker summary persisted as
// `_summary` and re-embedded into every later step's prompt. Local-model runs
// pay for context size, and a bloated summary taxes all downstream steps, so
// the cap is a hard guard on top of the soft "keep it under ~500 tokens"
// instruction in the summary contract.
const maxSummaryTokens = 500

// capSummaryNarrative truncates the NARRATIVE portion of a worker summary to
// ~maxTokens tokens (lenient ~4 chars/token heuristic) while preserving the
// structural content verbatim:
//
//   - the ORCHICON WORKER SUMMARY: <decision> routing line, if present in the
//     text (normally already stripped into `_decision`, kept defensively)
//   - every FACTS LEARNED: line — extractFactsLearned must still see them.
//     Both the plain `FACTS LEARNED: <fact>` form a worker writes and the
//     `FACTS LEARNED (from <step>): <fact>` form the handoff writer persists to
//     .orchicon/<run>/facts_learned (a downstream worker may quote file lines
//     back verbatim) are treated as structural and pass through untouched.
//
// Narrative lines are kept from the front of the summary until the budget is
// spent, in original order, so blocking feedback (stated up front) survives.
// A clear marker is appended so downstream workers know the summary was
// capped. When nothing was truncated (e.g. a summary that is entirely facts)
// the original text is returned unchanged.
func capSummaryNarrative(summary string, maxTokens int) string {
	if summary == "" {
		return summary
	}
	const charsPerToken = 4
	budget := maxTokens * charsPerToken
	if len(summary) <= budget {
		return summary
	}
	var out []string
	used := 0
	truncated := false
	for _, line := range strings.Split(summary, "\n") {
		trimmed := strings.TrimSpace(line)
		isFact := false
		if trimmed != "" {
			s := strings.ToLower(strings.TrimLeft(strings.TrimLeft(trimmed, "-"), " "))
			// Structural facts: both the plain marker a worker writes and the
			// step-attributed form the facts_learned file carries.
			if strings.HasPrefix(s, "facts learned:") || strings.HasPrefix(s, "facts learned (from") {
				isFact = true
			}
		}
		if isFact || strings.Contains(line, summaryMarker) {
			// Structural content always passes through untouched.
			out = append(out, line)
			continue
		}
		if used >= budget {
			truncated = true
			continue
		}
		out = append(out, line)
		used += len(line) + 1
	}
	if !truncated {
		return summary
	}
	out = append(out, "…[summary narrative truncated at ~"+strconv.Itoa(maxTokens)+
		" tokens — FACTS LEARNED lines and the ORCHICON WORKER SUMMARY routing are preserved verbatim; see the execution output for the full text]")
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// extractTouchedFiles parses `diff --git` lines from the worker's
// output text to determine which files were modified. Returns the
// target paths (the b/ side of the diff).
func extractTouchedFiles(output string) []string {
	var files []string
	seen := make(map[string]bool)
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "diff --git") {
			continue
		}
		// diff --git a/path b/path
		parts := strings.Fields(trimmed)
		if len(parts) < 4 {
			continue
		}
		p := strings.TrimPrefix(parts[3], "b/")
		if p != "" && !seen[p] {
			seen[p] = true
			files = append(files, p)
		}
	}
	return files
}

// propagateSummaryToStepRun copies the worker's summary onto the
// workflow step run that is awaiting this task (PR B — context
// propagation). The step run's _work_item_id (set when the run was
// dispatched) points at the work item; we look up the step run by
// that id and append _summary to its results JSON.
//
// Best-effort: a missing step run (e.g. dispatched without a
// workflow) is logged at debug and skipped. An error is returned only
// for genuine database errors.
// propagateStepRunResults copies execution fields onto the workflow step
// run that dispatched THIS execution, so the run-view UI can show them
// without opening each execution. The step run is located by its
// worker_execution_id — NOT by searching for the work item id in the result
// JSON: many steps share the same bound work item, and all step runs are
// seeded with the same created_at, so a `_work_item_id` LIKE search can
// return an arbitrary matching step run (e.g. a previous step's or a
// different step sharing the item) and mislabel it with this execution's
// results. worker_execution_id is set on the step run in the same
// transaction that creates the execution, so it is always unambiguous.
func (r *TaskReconciler) propagateStepRunResults(ctx context.Context, tx pgx.Tx, execID string, results map[string]any) {
	const q = `SELECT id, result, version FROM workflow_step_runs
		WHERE tenant_id = $1 AND worker_execution_id = $2 LIMIT 1`
	rows, err := tx.Query(ctx, q, "tnt_dev", execID)
	if err != nil {
		r.log.Warn("propagate step run results: query", "execution", execID, "error", err)
		return
	}
	defer rows.Close()
	if !rows.Next() {
		return
	}
	var stepRunID, rawResult string
	var version int
	if err := rows.Scan(&stepRunID, &rawResult, &version); err != nil {
		r.log.Warn("propagate step run results: scan", "execution", execID, "error", err)
		return
	}
	rows.Close()
	merged := map[string]any{}
	if rawResult != "" {
		_ = json.Unmarshal([]byte(rawResult), &merged)
	}
	// Propagate execution fields onto the step run so the run-view
	// UI can show them without opening each execution.
	for _, k := range []string{"_summary", "_decision", "_issues", "_touched_files", "_worker", "_worker_name"} {
		if v, ok := results[k]; ok {
			merged[k] = v
		}
	}
	updated, _ := json.Marshal(merged)
	if _, err := db.UpdateWorkflowStepRun(ctx, tx, "tnt_dev", stepRunID, version, db.UpdateWorkflowStepRunFields{
		Result: &updated,
	}); err != nil {
		r.log.Warn("propagate step run results: update", "execution", execID, "error", err)
	}
}
