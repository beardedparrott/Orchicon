// Package scheduler implements the TaskReconciler — the control loop that reconciles desired
// work-item state with observed runtime state (docs/03_Scheduler_and_Runtime_Design.md §2–4).
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
	"github.com/beardedparrott/orchicon/internal/verify"
	"github.com/beardedparrott/orchicon/internal/workflow"
	"github.com/jackc/pgx/v5"
)

// heartbeatTTL is how long an adapter heartbeat remains valid for
// selection (docs/03 §5: heartbeat age > 60s → unhealthy).
const heartbeatTTL = 60 * time.Second

// scanBatchSize caps how many work items the TaskReconciler's empty-key scan pass processes per
// tick (docs/03 §4: "Limited to a batch per tick so one scan doesn't monopolize the reconciler.
const scanBatchSize = 16

// defaultDispatchConcurrency is the default bound on how many ready work items the scan pass
// dispatches CONCURRENTLY in one pass (docs/03 §4, parallel dispatch).
const defaultDispatchConcurrency = 4

// ConcurrencyLimiter reports the maximum number of concurrent dispatches allowed for a project.
type ConcurrencyLimiter interface {
	// Limit returns the maximum concurrent dispatches allowed for a project.
	Limit(ctx context.Context, projectID string) int
}

// DispatchLimiter resolves the configured max-concurrent-runs caps for a project
// (concurrency-guards work item, architecture-notes per-project-dispatch-limits.md).
type DispatchLimiter interface {
	// Limit returns the effective max-concurrent-runs cap for the project:
	// min(tenant.max_concurrent_runs, project.max_concurrent_runs).
	Limit(ctx context.Context, tx pgx.Tx, tenantID, projectID string) (int, error)
	// InPlaceLimit returns the non-repo in-place serialization limit for the project (default 1 =
	// serialize, unless the project explicitly opts into concurrency with max_concurrent_runs > 1).
	InPlaceLimit(ctx context.Context, tx pgx.Tx, tenantID, projectID string) (int, error)
}

// dbDispatchLimiter is the production DispatchLimiter, backed by the
// tenant_settings + projects tables (db.GetDispatchLimitValues).
type dbDispatchLimiter struct{}

// DBDispatchLimiter is the exported constructor for the production DispatchLimiter.
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

// TaskReconciler implements the reconciler.Reconciler interface for the "task" kind.
type TaskReconciler struct {
	pool       *db.Pool
	log        *slog.Logger
	dispatcher *Dispatcher
	eventPub   eventbus.Publisher // direct NATS publisher for low-latency streaming (bypasses outbox relay)
	// eventSeq is a monotonic per-process suffix that makes every direct publish's JetStream MsgID
	// unique.
	eventSeq         atomic.Uint64
	workflowNotifier func(ctx context.Context, runID string) // enqueues run for WorkflowReconciler on task completion

	// pendingWrittenFiles holds the file paths a running execution's session actually wrote
	// (OnWrittenFiles), keyed by execution ID.
	writtenMu           sync.Mutex
	pendingWrittenFiles map[string][]string

	// blockedMu guards the blocked re-evaluation rotation cursor below.
	blockedMu     sync.Mutex
	blockedCursor int

	// fileEditReconciler is the diff-pipeline completion hook (optional): OnResult runs it after the
	// terminal status writeback so the ledger reconciles against the worktree's git state.
	fileEditReconciler FileEditReconciler

	// dispatchConcurrency bounds how many reconcileOne calls the scan pass runs concurrently (in flight
	// at once) when fanning out its candidate batch.
	dispatchConcurrency int
	// concurrencyLimiter, when set, further bounds the pass by the per-project limits of the candidate
	// items (concurrency-guards seam).
	concurrencyLimiter ConcurrencyLimiter
	// dispatchLimiter, when set, applies the per-project/tenant max-concurrent-runs admission gate
	// inside reconcileOne (concurrency guards D2).
	dispatchLimiter DispatchLimiter
	// dispatchOverlap is a test-only hook invoked with the current number of reconcileOne calls in
	// flight whenever the scan fan-out acquires a semaphore slot.
	dispatchOverlap func(inFlight int)
}

// NewTaskReconciler creates a TaskReconciler.
func NewTaskReconciler(pool *db.Pool, log *slog.Logger, dispatcher *Dispatcher) *TaskReconciler {
	return &TaskReconciler{pool: pool, log: log, dispatcher: dispatcher}
}

// FileEditReconciler is the diff-pipeline completion hook (optional): when set, OnResult reconciles
// the execution's file-edit ledger against the run worktree's git state on every terminal.
type FileEditReconciler interface {
	ReconcileExecutionGit(ctx context.Context, tenantID, execID string)
}

// SetFileEditReconciler injects the completion-time ledger reconciler.
func (r *TaskReconciler) SetFileEditReconciler(fe FileEditReconciler) { r.fileEditReconciler = fe }

// SetDispatchConcurrency sets the per-pass concurrency bound for the scan pass
// (ORCHICON_DISPATCH_CONCURRENCY).
func (r *TaskReconciler) SetDispatchConcurrency(n int) {
	if n < 1 {
		n = 1
	}
	if n > scanBatchSize {
		n = scanBatchSize
	}
	r.dispatchConcurrency = n
}

// SetConcurrencyLimiter installs the per-project concurrency guard seam (concurrency-guards work
// item).
func (r *TaskReconciler) SetConcurrencyLimiter(l ConcurrencyLimiter) {
	r.concurrencyLimiter = l
}

// SetDispatchLimiter installs the per-project/tenant max-concurrent-runs admission seam
// (concurrency-guards work item D2).
func (r *TaskReconciler) SetDispatchLimiter(l DispatchLimiter) {
	r.dispatchLimiter = l
}

// SetRecoveryTrigger is deprecated. Recovery is triggered exclusively by
// explicit `recover` steps on the workflow canvas (docs/06 §1).
func (r *TaskReconciler) SetRecoveryTrigger(rt RecoveryTrigger) {}

// SetEventPublisher injects a direct NATS publisher for streaming execution events.
func (r *TaskReconciler) SetEventPublisher(pub eventbus.Publisher) { r.eventPub = pub }

// SetWorkflowNotifier injects a callback that is called when a work item transitions to a terminal
// state (succeeded/failed).
func (r *TaskReconciler) SetWorkflowNotifier(fn func(ctx context.Context, runID string)) {
	r.workflowNotifier = fn
}

// Kind returns the reconciler kind (docs/03 §2.1).
func (r *TaskReconciler) Kind() string { return "task" }

// DispatchTask implements scheduler.TaskDispatcher.
func (r *TaskReconciler) DispatchTask(ctx context.Context, taskID, stepRunID string) error {
	return r.reconcileOne(ctx, taskID, stepRunID)
}

// Reconcile processes a single task key.
func (r *TaskReconciler) Reconcile(ctx context.Context, key string) reconciler.Result {
	if key == "" {
		return r.scan(ctx)
	}
	if err := r.reconcileOne(ctx, key, ""); err != nil {
		return reconciler.Result{Error: err}
	}
	return reconciler.Result{}
}

// executionModeFor returns the EFFECTIVE execution_mode for a work item's executions ("runtime" |
// "local"), resolved in exactly one place: db.EffectiveExecutionMode.
func executionModeFor(ctx context.Context, tx pgx.Tx, tenantID, workItemID, projectID string) string {
	var itemMode *string
	if strings.TrimSpace(workItemID) != "" {
		if w, err := db.GetWorkItem(ctx, tx, tenantID, workItemID); err == nil {
			itemMode = w.ExecutionMode
		}
	}
	return db.EffectiveExecutionMode(ctx, tx, tenantID, itemMode, projectID)
}

// liveDSNHosts are the loopback/bridge addresses that reach the LIVE plane from an in-process
// worker.
var liveDSNHosts = []string{"127.0.0.1:5432", "localhost:5432", "172.17.0.1:8080"}

// isLiveDSN reports whether a Postgres (or plane-URL) DSN points at the live plane: host ∈
// {127.0.0.1:5432, localhost:5432} for DB DSNs.
func isLiveDSN(dsn string) bool {
	lower := strings.ToLower(strings.TrimSpace(dsn))
	if strings.Contains(lower, "172.17.0.1:8080") {
		return true
	}
	host := lower
	// Strip scheme://userinfo@ prefix.
	if i := strings.LastIndex(host, "@"); i >= 0 {
		host = host[i+1:]
	} else if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	// Strip path/query.
	if i := strings.IndexAny(host, "/?"); i >= 0 {
		host = host[:i]
	}
	for _, live := range []string{"127.0.0.1:5432", "localhost:5432"} {
		if host == live {
			return true
		}
	}
	return false
}

// redactDSNHost returns the DSN's host portion for error messages (never
// the credentials).
func redactDSNHost(dsn string) string {
	lower := strings.TrimSpace(dsn)
	host := lower
	if i := strings.LastIndex(host, "@"); i >= 0 {
		host = host[i+1:]
	} else if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	if i := strings.IndexAny(host, "/?"); i >= 0 {
		host = host[:i]
	}
	if host == "" {
		return "(unparseable DSN)"
	}
	return host
}

// scan is the empty-key scan pass (docs/03 §4): find ready + blocked tasks and dispatch them with a
// BOUNDED IN-PASS FAN-OUT.
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
	// Blocked tasks are re-evaluated every pass so a newly satisfied dependency gate flips them back to
	// ready (and dispatches) without waiting on a notifier.
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

// scanCandidates builds this pass's candidate set: ready tasks (priority-ordered) first, then the
// rotating blocked window up to the remaining batch.
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

// dispatchCandidates fans out reconcileOne across the pass's candidate items with bounded
// concurrency (effectiveDispatchLimit), waiting for every in-flight item before returning.
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

// effectiveDispatchLimit returns the concurrency bound for this pass: the configured global
// dispatchConcurrency (default 4).
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

// blockedWindowStart returns the rotation offset for this pass's blocked re-evaluation window.
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

// reconcileOne dispatches a single task.
func (r *TaskReconciler) reconcileOne(ctx context.Context, taskID, stepRunID string) error {
	// We need the tenant to scope the transaction.
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
		// Standalone dispatch.
		switch task.Status {
		case domain.WorkItemReady, domain.WorkItemBlocked:
		default:
			return nil
		}
		// Skip items that belong to a WORKFLOW RUN: they are dispatched exclusively by the
		// WorkflowReconciler's inline (step-run) dispatch, never by the standalone scan.
		if task.WorkflowRunID != "" {
			return nil
		}
		// Workflow-first enforcement: standalone dispatch is retired, so an item with no workflow binding
		// can never execute.
		if task.WorkflowID == nil || *task.WorkflowID == "" {
			r.failNoWorkflow(ctx, tenantID, task)
			return nil
		}
		// Check dependencies satisfied (docs/02 §4 #1, docs/03 §4).
		satisfied, err := db.CheckDependenciesSatisfied(ctx, ttx.Tx, tenantID, task.ID)
		if err != nil {
			return fmt.Errorf("check deps: %w", err)
		}
		if !satisfied {
			// Dependency stall.
			if task.Status == domain.WorkItemReady {
				status := domain.WorkItemBlocked
				if _, err := db.UpdateWorkItem(ctx, ttx.Tx, tenantID, task.ID, task.Version, db.UpdateWorkItemFields{
					Status: &status,
				}); err != nil {
					return fmt.Errorf("park task blocked: %w", err)
				}
				// Commit the park so the surfaced status is durable — the dispatch path below is skipped for a
				// parked task (the deferred Rollback is then a no-op).
				if err := ttx.Commit(ctx); err != nil {
					return fmt.Errorf("commit park blocked: %w", err)
				}
			}
			return nil
		}
		// Gate satisfied: a blocked task flips back to ready and dispatches in this same pass (carry the
		// fresh version forward so the assigned transition below still CAS-matches).
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

	// Runtime-serve readiness belt-and-suspenders: never dispatch a workflow-run execution before the
	// run's runtime opencode serve is PROVEN usable (runtime_ready=true).
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

	// Local-mode DSN fence: in-process executions must never touch the LIVE plane.
	if task.ProjectID != "" || task.ID != "" {
		if mtx, merr := r.pool.BeginTenantTx(context.Background(), tenantID); merr == nil {
			mode := executionModeFor(context.Background(), mtx.Tx, tenantID, task.ID, task.ProjectID)
			_ = mtx.Rollback(context.Background())
			if mode == db.ExecutionModeLocal {
				if dsn := strings.TrimSpace(os.Getenv("ORCHICON_TEST_DSN")); dsn != "" && isLiveDSN(dsn) {
					r.log.Error("local-mode DSN fence: refusing live-plane DSN", "task", task.ID, "project", task.ProjectID)
					return fmt.Errorf("local-mode DSN fence: ORCHICON_TEST_DSN points at the LIVE plane (%q) — use a disposable database or unset it", redactDSNHost(dsn))
				}
			}
		}
	}

	// Select a Worker (docs/03 §4.1: rule-based).
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

	// Select an Adapter (docs/03 §4.2).
	adapter, err := r.selectAdapter(ctx, ttx.Tx, tenantID, resolveAdapterRowKind(version.ModelRef))
	if err != nil {
		r.log.Warn("no suitable adapter for task", "task", task.ID, "worker", version.WorkerID, "error", err)
		return nil
	}

	// Concurrency guard (D2, architecture-notes/per-project-dispatch-limits .md): never dispatch beyond
	// the project's effective max-concurrent-runs cap.
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
				// Stamp the deferral onto the step run, exactly as the per-worker gate below does.
				if stepRun != nil {
					if err := writeDispatchWaitNoteForProject(ctx, ttx.Tx, tenantID, *stepRun, active, limit); err != nil {
						r.log.Warn("write dispatch wait note", "step_run", stepRun.ID, "error", err)
					}
					if err := ttx.Commit(ctx); err != nil {
						return fmt.Errorf("commit dispatch wait note: %w", err)
					}
				}
				return nil
			}
		}
	}

	// Per-worker concurrency guard.
	if version.ConcurrencyLimit > 0 {
		activeWorker, werr := db.CountActiveExecutionsForWorker(ctx, ttx.Tx, tenantID, version.WorkerID)
		if werr != nil {
			return fmt.Errorf("count active worker executions: %w", werr)
		}
		if activeWorker >= version.ConcurrencyLimit {
			r.log.Info("dispatch deferred: worker at concurrency limit",
				"task", task.ID, "worker", version.WorkerID, "active", activeWorker, "limit", version.ConcurrencyLimit)
			workerDispatchMetrics.recordDeferred(version.WorkerID)
			if stepRun != nil {
				if err := writeDispatchWaitNote(ctx, ttx.Tx, tenantID, *stepRun, version.WorkerID, activeWorker, version.ConcurrencyLimit); err != nil {
					r.log.Warn("write dispatch wait note", "step_run", stepRun.ID, "error", err)
				}
				if err := ttx.Commit(ctx); err != nil {
					return fmt.Errorf("commit dispatch wait note: %w", err)
				}
			}
			return nil
		}
	}

	// Create WorkerExecution (docs/03 §4: createWorkerExecution).
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
	// Look up the workflow step run's iteration (loop number) so the execution can display "Work Item
	// Name - Worker Name - Loop #" in the frontend.
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
	// Carry the worktree state onto the execution row so the execution detail view can render worktree
	// status/branch/path.
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
	// Carry the run's PR surface onto the execution row so the executions list/detail can link out to
	// the run's PR (same seam as the worktree columns).
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

	// BASELINE CAPTURE — BEFORE THE WORK RUNS, BECAUSE AFTERWARDS IS TOO LATE.
	if projectDir := r.lookupProjectDir(ctx, task.ProjectID); projectDir != "" {
		wtPath, wtBranch := "", ""
		if worktreePath != nil {
			wtPath = *worktreePath
		}
		if worktreeBranch != nil {
			wtBranch = *worktreeBranch
		}
		r.captureBaselineForDispatch(ctx, projectDir, wtPath, wtBranch)
	}

	// Transition task to "assigned" (docs/03 §6: ready → assigned) — standalone dispatch only.
	if stepRun == nil {
		if _, err := db.UpdateWorkItem(ctx, ttx.Tx, tenantID, task.ID, task.Version, db.UpdateWorkItemFields{
			Status: strPtr(domain.WorkItemAssigned),
		}); err != nil {
			return fmt.Errorf("update task status: %w", err)
		}
	}

	// Link the workflow step run to the new execution so the run-view UI can show "click step → open
	// execution".
	if stepRun != nil {
		fields := db.UpdateWorkflowStepRunFields{
			WorkerExecutionID: &created.ID,
			Status:            strPtr(domain.StepRunRunning),
			StartedAt:         &now,
		}
		if cleared, changed := stripDispatchWait(stepRun.Result); changed {
			fields.Result = &cleared
		}
		if _, err := db.UpdateWorkflowStepRun(ctx, ttx.Tx, tenantID, stepRun.ID, stepRun.Version, fields); err != nil {
			return fmt.Errorf("link step run to execution: %w", err)
		}
	} else if workflowStepID != "" {
		if stepRunRow, err := db.GetWorkflowStepRunByStep(ctx, ttx.Tx, tenantID, workflowRunID, workflowStepID); err == nil {
			fields := db.UpdateWorkflowStepRunFields{
				WorkerExecutionID: &created.ID,
				Status:            strPtr(domain.StepRunRunning),
				StartedAt:         &now,
			}
			if cleared, changed := stripDispatchWait(stepRunRow.Result); changed {
				fields.Result = &cleared
			}
			if _, err := db.UpdateWorkflowStepRun(ctx, ttx.Tx, tenantID, stepRunRow.ID, stepRunRow.Version, fields); err != nil {
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

	// Start the execution via the adapter bridge.
	go r.startExecution(ctx, created, task, version, adapter)

	r.log.Info("task dispatched",
		"task", task.ID, "step_run", stepRunID, "execution", created.ID,
		"worker", version.WorkerID, "worker_version", version.Version,
		"adapter", adapter.ID)
	return nil
}

// workerVersionForStepRun resolves the worker version pinned by a workflow step.
func (r *TaskReconciler) workerVersionForStepRun(ctx context.Context, tx pgx.Tx, tenantID string, sr db.WorkflowStepRunRow) (db.WorkerVersionRow, error) {
	var meta struct {
		WorkerID  string `json:"_worker_id"`
		WorkerVer int    `json:"_worker_version"`
	}
	_ = json.Unmarshal(sr.Result, &meta)
	if meta.WorkerID != "" {
		if meta.WorkerVer > 0 {
			// By NUMBER, not by id: _worker_version carries the version ordinal and worker_versions.id is a
			// ULID, so a "v%d" pseudo-id could never match (see GetWorkerVersionByNumber).
			if v, err := db.GetWorkerVersionByNumber(ctx, tx, tenantID, meta.WorkerID, meta.WorkerVer); err == nil {
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

// mustStepWorkItemID reads _work_item_id from a step run's result.
func mustStepWorkItemID(sr db.WorkflowStepRunRow) string {
	var meta struct {
		WorkItemID string `json:"_work_item_id"`
	}
	_ = json.Unmarshal(sr.Result, &meta)
	return meta.WorkItemID
}

// startExecution calls the adapter bridge to start the execution.
func (r *TaskReconciler) startExecution(ctx context.Context, exec db.ExecutionRow, task db.WorkItemRow, version db.WorkerVersionRow, adp db.AdapterRow) {
	// Resolve the project directory so the adapter runs in the correct working directory (avoids
	// picking up Orchicon's own AGENTS.md etc.).
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
	// Resolve the workflow run's runtime container image (the adapter's self-heal recreates the
	// container with the identical image the WorkflowReconciler used at run start) AND the run's.
	runtimeImage := ""
	worktreePath := ""
	worktreeStatus := ""
	// executionMode is the run's EFFECTIVE execution_mode (runtime|local), resolved from the work item
	// override, else the project.
	executionMode := db.ExecutionModeRuntime // replaced by the resolver below; its own fail-open owns the default
	if rtx, err := r.pool.BeginTenantTx(context.Background(), exec.TenantID); err == nil {
		executionMode = executionModeFor(context.Background(), rtx.Tx, exec.TenantID, exec.TaskID, exec.ProjectID)
		if task.WorkflowRunID != "" {
			if run, gerr := db.GetWorkflowRun(context.Background(), rtx.Tx, exec.TenantID, task.WorkflowRunID); gerr == nil {
				runtimeImage = run.RuntimeImage
				worktreeStatus = run.WorktreeStatus
				if run.WorktreeStatus == domain.WorktreeReady && run.WorktreePath != "" {
					worktreePath = run.WorktreePath
				}
				// NOTE: the run-bound composite prompt (step-run _prompt) already carries the run-effective
				// image+mode — baked by buildCompositePrompt at dispatch.
			}
			// D2: a parallel-branch child execution runs in the STEP RUN's OWN branch worktree — its cwd must
			// be the branch worktree, not the run worktree.
			if exec.WorkflowStepID != "" {
				if sr, gerr := db.GetWorkflowStepRunByExecution(context.Background(), rtx.Tx, exec.TenantID, exec.ID); gerr == nil &&
					sr.WorktreeStatus == domain.WorktreeReady && sr.WorktreePath != "" {
					worktreeStatus = sr.WorktreeStatus
					worktreePath = sr.WorktreePath
				}
			}
		}
		_ = rtx.Rollback(context.Background())
	}
	// The execution working directory is the provisioned worktree path when the run has one (the worker
	// starts already checked out on its branch inside the worktree), else the project dir.
	execCwd := projectDir
	// THE STATUS IS CHECKED BEFORE THE PATH, and the order is the fix.
	if exec.WorkflowRunID != "" && worktreeStatus == domain.WorktreeFailed {
		r.log.Warn("worktree provisioning failed — failing dispatch", "task", task.ID, "run", exec.WorkflowRunID, "status", worktreeStatus)
		return
	}
	if worktreePath != "" {
		execCwd = worktreePath
	} else if exec.WorkflowRunID != "" {
		// Workflow-bound dispatch without a worktree path.
		switch worktreeStatus {
		case domain.WorktreePending, domain.WorktreePruned:
			r.log.Info("holding dispatch: worktree not ready, will re-provision", "task", task.ID, "run", exec.WorkflowRunID, "status", worktreeStatus)
			return
		case domain.WorktreeSkipped, "":
			// intentionally in-place (non-repo) or undetermined — allow projectDir
		default:
			r.log.Warn("holding dispatch: unexpected worktree status without path", "task", task.ID, "run", exec.WorkflowRunID, "status", worktreeStatus)
			return
		}
	}
	// The system prompt is the full context the model sees on every turn.
	composite, _ := extractComposite(task.PromptContext)
	var promptFP string      // context-file fingerprint (ADR-0009 D5)
	var agentPromptLen int   // run-level prefix length, for the 2.x message trim
	var stepRunResult []byte // step run's full result — carries the _recovery_* seed keys
	if exec.WorkflowRunID != "" && exec.WorkflowStepID != "" {
		if stx, err := r.pool.BeginTenantTx(context.Background(), exec.TenantID); err == nil {
			if sr, err := db.GetWorkflowStepRunByStep(context.Background(), stx.Tx, exec.TenantID, exec.WorkflowRunID, exec.WorkflowStepID); err == nil {
				stepRunResult = sr.Result
				var srMeta struct {
					Prompt      string `json:"_prompt"`
					Fingerprint string `json:"_prompt_fingerprint"`
					// The run-level prefix length, recorded by the composite writers.
					AgentPromptLen int `json:"_agent_prompt_len"`
				}
				_ = json.Unmarshal(sr.Result, &srMeta)
				if srMeta.Prompt != "" {
					composite = srMeta.Prompt
				}
				if srMeta.Fingerprint != "" {
					promptFP = srMeta.Fingerprint
				}
				agentPromptLen = srMeta.AgentPromptLen
			}
			_ = stx.Rollback(context.Background())
		}
	}
	systemPrompt := composite
	// Last-resort bare worker prompt.
	if strings.TrimSpace(systemPrompt) == "" {
		systemPrompt = "You are a worker in the Orchicon orchestration system. " +
			"Complete the work item described in the user message and report back."
	}
	// Recovery seeding — a HARD gate: if this is a recovery-resumed dispatch for the SAME worker that
	// died, .orchicon/worker.recovery must exist, be non-empty.
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
	// User message (Goal): just the work item title.
	var defaultModelRef string
	var stallNoProgress, stallNoFileDiff, stallTextLoop *int64
	var stallRepCount *int32
	var stallRepWindow *int64
	var stallNudgeMax *int32
	var stallNudgeReplyWindow, stallNudgeCooldown *int64
	var stallToolHang *int64
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

	// Merge the tenant-level default budget (base) with the worker's own budget_overrides (override) so
	// a worker's explicit field always wins per-field over the tenant default.
	budgetsJSON := mergeBudgets(defaultBudgetOverrides, version.BudgetOverrides)

	manifest := ExecutionManifest{
		ExecutionID:                  exec.ID,
		TaskID:                       exec.TaskID,
		ProjectID:                    exec.ProjectID,
		WorkerID:                     version.WorkerID,
		WorkerVersion:                version.Version,
		SystemPrompt:                 systemPrompt,
		AgentPromptLen:               agentPromptLen,
		PromptFingerprint:            promptFP,
		Goal:                         task.Title,
		AcceptanceCriteria:           task.AcceptanceCriteria,
		ModelRef:                     version.ModelRef,
		DefaultModelRef:              defaultModelRef,
		ContextWindow:                task.ContextWindow,
		ContextSources:               version.ContextSources,
		Budgets:                      budgetsJSON,
		Permissions:                  version.Permissions,
		ProjectDir:                   projectDir,
		WorktreePath:                 worktreePath,
		RuntimeWorkflowID:            task.WorkflowRunID,
		RuntimeImage:                 runtimeImage,
		ExecutionMode:                executionMode,
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
	// Resolve the adapter bridge from the execution's adapter kind — the model_ref grammar
	// (adapter.AdapterKind) is the SINGLE source of truth (ADR-0003).
	modelRef := manifest.ModelRef
	if modelRef == "" {
		modelRef = manifest.DefaultModelRef
	}
	kind := adapter.AdapterKind(modelRef)
	if kind == "" {
		// Empty/malformed model_ref: fall back to the default adapter kind ("opencode") so legacy workers
		// with single-segment refs keep dispatching exactly as they did before the dispatcher (previously.
		kind = adapter.DefaultAdapterKind
	}
	// Resolution happens ONLY for the native engine (kind "orchicon" — the in-process session engine);
	// opencode/other adapters never resume (their transcripts are subprocess-bound, not in-process).
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
		// The Resolve error names the missing kind and the registered kinds — surface it verbatim so the
		// operator can act on it.
		if permanentDispatchFailure(err) {
			r.markFailedToStartPermanent(context.Background(), exec, err.Error())
			return
		}
		r.markFailedToStart(context.Background(), exec, err.Error())
		return
	}
	if err := bridge.Start(ctx, exec, manifest, r); err != nil {
		r.log.Error("adapter start failed", "execution", exec.ID, "error", err)
		// Mark the execution as failed_to_start.
		r.markFailedToStart(context.Background(), exec, err.Error())
	}
}

// continuationSessionID resolves the prior session to continue for a sequence-chain task
// (sequence-continuation flag, opt-in DEFAULT OFF).
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

// mergeWorkItemError merges an actionable error message into a work item's Results JSON under
// "work_item_error", preserving every existing key (the Results blob also carries _output/_summary.
func mergeWorkItemError(results []byte, reason string) []byte {
	if len(results) == 0 {
		results = []byte("{}")
	}
	var m map[string]any
	_ = json.Unmarshal(results, &m)
	if m == nil {
		m = map[string]any{}
	}
	m["work_item_error"] = reason
	b, _ := json.Marshal(m)
	return b
}

// failNoWorkflowItemTx fails a work item that can never execute: standalone dispatch is retired, so
// an item with no workflow binding has nothing to run inside.
func failNoWorkflowItemTx(ctx context.Context, pool *db.Pool, log *slog.Logger, tenantID, itemID, reason string) {
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		log.Error("fail no-workflow item: begin tx", "work_item", itemID, "error", err)
		return
	}
	defer ttx.Rollback(ctx)
	item, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, itemID)
	if err != nil {
		log.Error("fail no-workflow item: read item", "work_item", itemID, "error", err)
		return
	}
	results := mergeWorkItemError(item.Results, reason)
	status := domain.WorkItemFailed
	updated, err := db.UpdateWorkItem(ctx, ttx.Tx, tenantID, itemID, item.Version, db.UpdateWorkItemFields{
		Status:  &status,
		Results: &results,
	})
	if err != nil {
		log.Error("fail no-workflow item: update", "work_item", itemID, "error", err)
		return
	}
	if err := enqueueWorkItemEvent(ctx, ttx.Tx, "work_item.updated", updated); err != nil {
		log.Error("fail no-workflow item: enqueue event", "work_item", itemID, "error", err)
		return
	}
	if err := ttx.Commit(ctx); err != nil {
		log.Error("fail no-workflow item: commit", "work_item", itemID, "error", err)
	}
}

// failNoWorkflow is the TaskReconciler's backstop for the retired standalone
// dispatch path: a "ready" item with no workflow binding is failed loudly.
func (r *TaskReconciler) failNoWorkflow(ctx context.Context, tenantID string, task db.WorkItemRow) {
	reason := fmt.Sprintf("Cannot dispatch %q: no workflow is set, so there is nothing to run. Bind a workflow to run this item.", task.Title)
	r.log.Error("task: no-workflow item cannot be dispatched — failing it loudly",
		"work_item", task.ID, "status", task.Status)
	failNoWorkflowItemTx(ctx, r.pool, r.log, tenantID, task.ID, reason)
}

// markFailedToStart transitions an execution to failed_to_start
// (docs/03 §8: adapter unreachable mid-dispatch → failed_to_start, task
// requeues with backoff).
// markExecutionFailedToStart writes the failed_to_start terminal state, RE-READING the row for its current
// version rather than trusting the snapshot the caller holds.
//
// THE BUG THIS FIXES. The call used `exec.Version` as the optimistic-concurrency key, taken before
// `bridge.Start` ran. Start marks the execution RUNNING (and the health probe touches it), so by the time the
// failure is recorded the version has moved — the UPDATE matches zero rows and returns
// `db: not found`:
//
//	01:12:06.939 ERROR adapter start failed   execution=…
//	01:12:06.940 ERROR mark failed_to_start   error="db: not found"
//
// REPORTED on every failure of a run, six times. The consequence is a PARTIAL write: the record carries the
// start failure but never reaches failed_to_start, so the run's state is built on a row that disagrees with
// what happened. `db: not found` is also the string that once caused a 22-minute hot loop in a different call
// site, which is why it read as a regression.
//
// NOT AN UPSERT, deliberately: the row certainly EXISTS (this is a real execution), so treating a version
// mismatch as "insert it" would be wrong. Re-read the current version and retry — bounded — and if the row has
// already reached a TERMINAL state, leave it alone: something else recorded the outcome, and overwriting it
// with failed_to_start would lose the real one.
func (r *TaskReconciler) markExecutionFailedToStart(ctx context.Context, ttx *db.TenantTx, exec db.ExecutionRow, errorMessage string) error {
	now := time.Now().UTC()
	fields := db.UpdateExecutionFields{
		Status:       strPtr(domain.ExecutionFailedToStart),
		EndedAt:      &now,
		ErrorMessage: &errorMessage,
	}
	version := exec.Version
	for attempt := 0; attempt < 3; attempt++ {
		cur, err := db.GetExecution(ctx, ttx.Tx, exec.TenantID, exec.ID)
		if err != nil {
			return err
		}
		if terminalExecutionStatus(cur.Status) {
			// ALREADY DECIDED. Recording failed_to_start over a terminal row would rewrite history: the
			// execution may have failed for a different, later-reported reason.
			return nil
		}
		version = cur.Version
		if _, uerr := db.UpdateExecution(ctx, ttx.Tx, exec.TenantID, exec.ID, version, fields); uerr != nil {
			if errors.Is(uerr, db.ErrNotFound) {
				// Lost a race with a concurrent writer: re-read and try again with the fresh version.
				continue
			}
			return uerr
		}
		return nil
	}
	return fmt.Errorf("mark failed_to_start: version kept moving for execution %s (last seen %d)", exec.ID, version)
}

// terminalExecutionStatus reports whether an execution has already reached a terminal state.
func terminalExecutionStatus(status string) bool {
	// The terminal set this codebase actually writes (internal/domain/execution constants — there is no
	// "cancelled"/"aborted" execution status, so naming one would not compile and asserting it would be wrong).
	switch status {
	case domain.ExecutionSucceeded, domain.ExecutionFailed, domain.ExecutionFailedToStart,
		domain.ExecutionTerminated:
		return true
	}
	return false
}

func (r *TaskReconciler) markFailedToStart(ctx context.Context, exec db.ExecutionRow, errorMessage string) {
	ttx, err := r.pool.BeginTenantTx(ctx, exec.TenantID)
	if err != nil {
		r.log.Error("begin tx for failed_to_start", "execution", exec.ID, "error", err)
		return
	}
	defer ttx.Rollback(ctx)
	if err := r.markExecutionFailedToStart(ctx, ttx, exec, errorMessage); err != nil {
		r.log.Error("mark failed_to_start", "execution", exec.ID, "error", err)
		return
	}
	// No task requeue: the standalone path that re-armed a failed dispatch back to "ready" is retired
	// (only a workflow-bound item can be dispatched.
	if err := ttx.Commit(ctx); err != nil {
		r.log.Error("commit failed_to_start", "execution", exec.ID, "error", err)
	}
}

// permanentDispatchFailure reports whether a dispatcher Resolve error is a PERMANENT
// routing/configuration failure.
func permanentDispatchFailure(err error) bool {
	return errors.Is(err, ErrAdapterDisabled) ||
		errors.Is(err, ErrAdapterKindUnregistered) ||
		errors.Is(err, ErrRuntimeImageAbsent)
}

// ErrRuntimeImageAbsent classifies a dispatch that cannot proceed because the run's runtime image
// is NOT downloaded on this host.
var ErrRuntimeImageAbsent = errors.New("runtime image not downloaded")

// runtimeImageAbsentError builds that classification error, naming the exact ref and the single
// action that fixes it.
func runtimeImageAbsentError(ref string) error {
	return fmt.Errorf("%w: %q — fetch it from Runtime Images (the Fetch action on that row) before dispatch; `podman pull %s` does the same thing from a shell. The item becomes dispatchable as soon as the image is present",
		ErrRuntimeImageAbsent, ref, ref)
}

// markFailedToStartPermanent is markFailedToStart for a PERMANENT dispatch failure: the execution's
// adapter kind can never resolve on this plane (administratively disabled.
func (r *TaskReconciler) markFailedToStartPermanent(ctx context.Context, exec db.ExecutionRow, errorMessage string) {
	ttx, err := r.pool.BeginTenantTx(ctx, exec.TenantID)
	if err != nil {
		r.log.Error("begin tx for permanent failed_to_start", "execution", exec.ID, "error", err)
		return
	}
	defer ttx.Rollback(ctx)
	if err := r.markExecutionFailedToStart(ctx, ttx, exec, errorMessage); err != nil {
		r.log.Error("mark permanent failed_to_start", "execution", exec.ID, "error", err)
		return
	}
	if exec.WorkflowRunID == "" {
		if _, err := db.UpdateWorkItem(ctx, ttx.Tx, exec.TenantID, exec.TaskID, 0, db.UpdateWorkItemFields{
			Status: strPtr(domain.WorkItemBlocked),
		}); err != nil {
			r.log.Error("block task after permanent failed_to_start", "task", exec.TaskID, "error", err)
			return
		}
	}
	if err := ttx.Commit(ctx); err != nil {
		r.log.Error("commit permanent failed_to_start", "execution", exec.ID, "error", err)
	}
}

// selectWorker selects a published Worker for the task using rule-based ranking (docs/03 §4.1):
// filter by health, rank by lowest utilization + LRU, deterministic tiebreak by worker id.
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

// selectAdapter selects a registered adapter of the matching kind with a recent heartbeat and free
// capacity (docs/03 §4.2).
func resolveAdapterRowKind(modelRef string) string {
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

// OnWrittenFiles is called by the adapter when the worker's session reports files it wrote or
// edited (opencode file_diff telemetry).
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

// OnResult is called by the adapter bridge when the execution reaches a terminal state (docs/03 §6:
// running → succeeded|failed).
func (r *TaskReconciler) OnResult(ctx context.Context, execID string, succeeded bool, output string, errorMessage string) {
	// THE WORKER'S OWN VERDICT DECIDES THE OUTCOME, not the process's exit code. The asymmetry is
	// deliberate and is the whole rule: - DOWNGRADE ONLY.
	if succeeded {
		if d := extractSummaryDecision(output); d == "failure" {
			r.log.Warn("worker reported FAILURE in its own summary while the process exited cleanly — "+
				"recording the execution as failed, because the worker's verdict is the outcome",
				"execution", execID, "decision", d,
				"summary", truncateForLog(extractWorkerSummary(output), 300))
			succeeded = false
			if strings.TrimSpace(errorMessage) == "" {
				// Give the row a reason, so this is legible wherever failures are listed rather than
				// appearing as a failure with no cause.
				errorMessage = "worker reported failure in its own ORCHICON WORKER SUMMARY: " +
					truncateForLog(extractWorkerSummary(output), 300)
			}
		}
	}
	// EVIDENCE GATE — ASK THE REPOSITORY, NOT THE REPORT.
	if succeeded {
		if exec, lerr := r.loadExecutionRow(ctx, execID); lerr == nil {
			if res, ok := r.verdictForOutcome(ctx, exec, output); ok {
				// THE VERDICT IS A RECORD, NOT A STATUS.
				var notice string
				succeeded, notice = applyVerdict(succeeded, res)
				if notice != "" && strings.TrimSpace(errorMessage) == "" {
					// The notice rides the same slot the worktree reconciler uses for its advisories, so it
					// appears where an operator already looks.
					errorMessage = notice
				}
				if res.Verdict == verify.VerdictUnsubstantiated {
					r.log.Warn("verify: the worker's claim is not supported by the repository (advisory only)",
						"execution", execID, "verdict", res.Verdict.String(), "reason", res.Reason)
				}
				// Record the evidence on the step run so the acceptance review and the UI can show what
				// was and was not confirmed. Best-effort; it cannot change the outcome above.
				r.recordVerification(ctx, exec, res)
				if res.Verdict == verify.VerdictVerified {
					r.log.Info("verify: step verified", "execution", execID, "checks", len(res.Checks))
				}
			}
		} else {
			// The row could not be read.
			r.log.Warn("verify: execution row unreadable — outcome left as reported", "execution", execID, "error", lerr)
		}
	}
	status := domain.ExecutionSucceeded
	if !succeeded {
		status = domain.ExecutionFailed
	}
	r.updateExecStatus(ctx, execID, status, domain.HealthTerminating, output, errorMessage)
	// A succeeded execution must not carry a stale advisory stall notice (observed: the SSE worker
	// succeeded yet kept `stalled:no_file_progress` in error_message).
	if succeeded {
		r.clearStallNotice(ctx, execID)
	}
	// Diff pipeline (AC 4): reconcile the file-edit ledger against the run worktree's git state once,
	// at the terminal transition.
	if r.fileEditReconciler != nil {
		r.fileEditReconciler.ReconcileExecutionGit(ctx, "tnt_dev", execID)
	}
	r.transitionWorkItemOnResult(ctx, execID, succeeded, output)
}

// FailLostExecution marks an execution failed with a clear reason and transitions its work item to
// failed, so the WorkflowReconciler's task-step poll observes the terminal state.
func (r *TaskReconciler) FailLostExecution(ctx context.Context, execID, errorMessage string) {
	r.updateExecStatus(ctx, execID, domain.ExecutionFailed, domain.HealthUnhealthy, "", errorMessage)
	r.transitionWorkItemOnResult(ctx, execID, false, "")
}

// transitionWorkItemOnResult moves the WorkItem linked to the execution to succeeded/failed when
// the execution terminates.
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
	// PR B: extract summary from worker output.
	var summary string
	if succeeded && output != "" {
		summary = extractWorkerSummary(output)
	}
	// Cap the narrative portion of the summary so a bloated summary doesn't tax every later step that
	// re-embeds it (Execution history + the .orchicon/<run>/summary + issues fallback files).
	summary = capSummaryNarrative(summary, maxSummaryTokens)
	// Check if this work item is a follow-up with a parent execution to write back to.
	var parentExecID string
	if len(wi.Results) > 0 {
		var rawResults map[string]any
		if err := json.Unmarshal(wi.Results, &rawResults); err == nil {
			if pid, ok := rawResults["_parent_execution_id"].(string); ok {
				parentExecID = pid
			}
		}
	}

	// Persist output + summary on the work item's results JSON so the audit trail shows what the worker
	// produced.
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
	// The decision signal is a SINGLE source of truth: the first word after the ORCHICON WORKER
	// SUMMARY: marker.
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
	// Fold in the files the session itself reported as written (opencode file_diff telemetry).
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
	// Write authored PR URL/state into the run's run_context (for the run detail page) and the
	// execution row columns (for the work item card and execution detail page).
	var prURL, prState string
	if r.skipPRMarkerStamp(ctx, ttx.Tx, exec, succeeded) {
		// Change 2: for a succeeded PR-requiring git_strategy=pr workflow step, the WorkflowReconciler's
		// step-success path is the single authoritative writer of pr_url/pr_state (the deterministic gh.
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
	// A work item bound to an ACTIVE workflow run tracks the RUN, not any single step execution: the
	// ticket is a shared input reference and stays "running" for the whole run.
	runActive := r.boundToActiveRun(ctx, ttx.Tx, wi)
	// A terminal run has ALREADY decided this item's fate — the WorkflowReconciler owns that write
	// (workflow_reconciler.go.
	if runTerminal, terminal := r.boundRunTerminalStatus(ctx, ttx.Tx, wi); terminal {
		if contradicts := lateVerdictContradictsRun(runTerminal, succeeded); contradicts {
			// Warn ONLY when the dropped verdict contradicts the run — a failure verdict over a completed run,
			// or a success verdict over a failed one.
			r.log.Warn("ignoring late execution verdict: its run already reached a terminal state",
				"execution", exec.ID, "work_item", wi.ID, "run", wi.WorkflowRunID,
				"run_status", runTerminal, "verdict_succeeded", succeeded,
				"item_status", wi.Status)
		}
		return
	}
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
		// Copy the execution results onto the linked workflow step run so the run view can display
		// decision/summary/issues/files without opening each execution.
		r.propagateStepRunResults(ctx, ttx.Tx, exec.ID, results)
	} else {
		// Failure: transition to failed so the step run transitions to terminal-failed, allowing a
		// downstream `recover` step to activate and trigger recovery (docs/06 §1).
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

	// System-side recovery-seed cleanup: a successful recovery-resumed execution removes
	// .orchicon/worker.recovery when the file's footer matches this execution (the worker-side `rm`.
	r.removeRecoveryFileForSuccess(ctx, exec, results, succeeded)

	// Follow-up write-back: if this work item has a parent execution (created via
	// CreateFollowUpExecution).
	if parentExecID != "" && succeeded && output != "" {
		r.appendToParentConversation(context.Background(), "tnt_dev", parentExecID, output)
	}

	// Notify the WorkflowReconciler that this task completed so it can progress the step DAG
	// immediately (docs/03 §2).
	if r.workflowNotifier != nil && wi.WorkflowRunID != "" {
		r.workflowNotifier(context.Background(), wi.WorkflowRunID)
	}

	// Recovery is NOT triggered automatically — explicit `recover`
	// steps on the workflow canvas handle this (docs/06 §1).
}

// boundToActiveRun reports whether the work item is linked to a workflow run that is still in
// flight (not yet terminal).
func (r *TaskReconciler) skipPRMarkerStamp(ctx context.Context, tx pgx.Tx, exec db.ExecutionRow, succeeded bool) bool {
	if !succeeded || exec.WorkflowRunID == "" || exec.WorkflowStepID == "" {
		return false
	}
	run, err := db.GetWorkflowRun(ctx, tx, "tnt_dev", exec.WorkflowRunID)
	if err != nil {
		return false // fail-open: keep extraction
	}
	// Effective git strategy: workflow-level wins, else project-level, else "local" — single source of
	// truth (db.EffectiveGitStrategy) so PR-path semantics never drift from the worktree/runtime.
	if db.EffectiveGitStrategy(ctx, tx, "tnt_dev", run.WorkflowID, run.ProjectID) != "pr" {
		return false
	}
	// Find the step's config in the published workflow version so requiresPRStep sees the same
	// requires_pr flag the WorkflowReconciler uses at step-success.
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

// boundRunTerminalStatus reports the bound run's status when that run has
// reached a TERMINAL state, so a per-execution verdict can tell "the run already
// decided this item's fate" apart from "there is no run".
//
// WHY THIS EXISTS. boundToActiveRun answers "is the run still going?", and the
// caller used a bare false to mean BOTH "no run" and "run finished". Those are
// opposite facts, and conflating them let a LATE failure verdict overwrite a
// work item whose run had already COMPLETED:
//
//	run 01M54CJA… finished at 03:10:22 with its PR merged (the item became succeeded)
//	03:11:33 the liveness reaper failed a SUPERSEDED execution of that same run
//	         → transitionWorkItemOnResult(…, false, …)
//	         → boundToActiveRun = false (run is "completed", not active)
//	         → the else branch wrote WorkItemFailed over succeeded
//
// The failed child then halted its sequence parent's whole chain in the failure
// pre-scan, so the NEXT work item never fired until an operator manually marked
// the item succeeded ~8 hours later. Nothing logged a word.
func (r *TaskReconciler) boundRunTerminalStatus(ctx context.Context, tx pgx.Tx, wi db.WorkItemRow) (string, bool) {
	if wi.WorkflowRunID == "" {
		return "", false
	}
	run, err := db.GetWorkflowRun(ctx, tx, wi.TenantID, wi.WorkflowRunID)
	if err != nil {
		return "", false
	}
	switch run.Status {
	case domain.WorkflowRunCompleted, domain.WorkflowRunFailed, domain.WorkflowRunAborted:
		return run.Status, true
	}
	return "", false
}

// lateVerdictContradictsRun reports whether a per-execution verdict DISAGREES with the terminal
// state its bound run already recorded.
func lateVerdictContradictsRun(runStatus string, verdictSucceeded bool) bool {
	runSucceeded := runStatus == domain.WorkflowRunCompleted
	return runSucceeded != verdictSucceeded
}

// writeOrchiconFiles writes the execution results to .orchicon/ files in the project directory so
// the next worker can read previous step results from disk instead of receiving them inline.
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
		// Facts ledger: persist the FACTS LEARNED lines this step recorded so later steps can read them
		// from disk (the composite prompt points the next worker at .orchicon/<run>/facts_learned first.
		facts := extractFactsLearned(summary)
		if len(facts) > 0 {
			stepName := r.stepNameForExecution(ctx, exec)
			if err := appendFactsToOrchiconFile(orchDir, stepName, facts); err != nil {
				r.log.Warn("write facts_learned (from summary)", "error", err)
			}
		}
	}
	// Deterministic terminal-time backstop: at EVERY execution terminal
	// (succeeded/failed/stalled/fail-lost.
	stepName := r.stepNameForExecution(ctx, exec)
	if tFacts := r.extractTranscriptFacts(ctx, exec); len(tFacts) > 0 {
		if err := appendFactsToOrchiconFile(orchDir, stepName, tFacts); err != nil {
			r.log.Warn("append facts_learned (from transcript)", "execution", exec.ID, "error", err)
		}
	}
	// The `issues` file is the feedback channel the composite prompt points the next worker at ("read
	// .orchicon/<run>/issues").
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

	// Per-step archive: persist this execution's results to a per-step file so the delta handoff
	// (execution-history index) can point workers at a stable, complete per-step record on disk.
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

// extractTranscriptFacts reads the persisted session-parts transcript tail for an execution (both
// the opencode and native adapters persist to execution_session_parts) and mechanically extracts.
func (r *TaskReconciler) extractTranscriptFacts(ctx context.Context, exec db.ExecutionRow) []string {
	if exec.TenantID == "" || exec.ID == "" {
		return nil
	}
	ttx, err := r.pool.BeginTenantTx(ctx, exec.TenantID)
	if err != nil {
		r.log.Warn("transcript facts: begin tx", "execution", exec.ID, "error", err)
		return nil
	}
	defer ttx.Rollback(ctx)
	parts, err := db.ListExecutionSessionPartsTail(ctx, ttx.Tx, exec.TenantID, exec.ID, factsTranscriptMaxParts)
	if err != nil {
		r.log.Warn("transcript facts: read tail", "execution", exec.ID, "error", err)
		return nil
	}
	_ = ttx.Commit(ctx)
	return extractFactsFromTranscript(parts)
}

// stepNameForExecution resolves the workflow step name that dispatched an execution, so facts
// written to .orchicon/<run>/facts_learned can carry per-step attribution (the embedded "## Facts.
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

// OnStall is called by the adapter bridge's progress monitor when a stall signal trips (docs/06 §2:
// "stalled health state | no progress within stall window"; docs/03 §5).
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

// OnRecovered is called by the adapter bridge's progress monitor when an advisory stall clears.
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

// clearStallNotice removes a "stalled:" reason from error_message.
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
	_ = ttx.Commit(ctx)
	// Per-token text deltas are NOT outboxed (AGENTS.md invariant #3 is satisfied by the low-frequency
	// state events below).
	r.publishExecEvent(ctx, "execution.text", current, map[string]any{
		"text": text,
	})
}

// OnArtifact publishes an artifact execution event so the frontend live session pane can show model
// output files inline (docs/10 §11).
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

// appendToParentConversation appends the assistant's output to the parent execution's conversation
// field so follow-up messages and responses appear as one continuous thread on the original.
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

// publishExecEvent builds the same event payload as enqueueExecEvent and publishes it directly to
// NATS via the reconciler's direct publisher.
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
	// The MsgID MUST be unique per publish.
	dedupID := fmt.Sprintf("direct:%s:%s:%d", e.ID, eventType, r.eventSeq.Add(1))
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

// mergeBudgets merges the tenant-level default budget (base) with a worker's own budget_overrides
// (override) into a single JSON document.
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

// It is INFORMATIONAL ONLY — it never influences the workflow decision, which is the single
// ORCHICON WORKER SUMMARY: word.
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
	// Change 3: inline-marker fallback.
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

// extractComposite pulls the "composite" string out of a work item's prompt_context JSON (set by
// the WorkflowReconciler's buildCompositePrompt).
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

// composeSystemPrompt assembles the worker-only system prompt from the four structured fields
// (role, skills, behavior, agents_md).
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

// placeholderSummaryBody reports whether the text following an ORCHICON WORKER SUMMARY marker is a
// placeholder/template echo (the worker wrote the marker as an *example* inside a plan.
func placeholderSummaryBody(rest string) bool {
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return true
	}
	// Inline code (backtick-quoted) markers are seed/instruction echo, never a real sign-off.
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

// lastRealSummaryMarker returns the index of the LAST genuine ORCHICON WORKER SUMMARY marker in
// output — one whose body is real content, skipping earlier placeholder/template echoes.
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

// extractWorkerSummary parses the ORCHICON WORKER SUMMARY block from the worker's text.
func extractWorkerSummary(output string) string {
	idx := lastRealSummaryMarker(output)
	if idx < 0 {
		return strings.TrimSpace(output)
	}
	rest := strings.TrimSpace(output[idx+len(summaryMarker):])
	return trimSummaryDecision(rest)
}

// extractSummaryDecision reads the first word of the summary block (the text after ORCHICON WORKER
// SUMMARY:) and returns "success", "failure", any other verbatim first word.
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

// firstWordAsDecision returns the normalized decision from the first whitespace-delimited word of
// s: "success"/"failure" for words that start with those prefixes.
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

// maxSummaryTokens caps the narrative portion of a worker summary persisted as `_summary` and
// re-embedded into every later step's prompt.
const maxSummaryTokens = 500

// capSummaryNarrative truncates the NARRATIVE portion of a worker summary to ~maxTokens tokens
// (lenient ~4 chars/token heuristic) while preserving the structural content verbatim:
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

// extractTouchedFiles parses `diff --git` lines from the worker's output text to determine which
// files were modified.
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

// propagateSummaryToStepRun copies the worker's summary onto the workflow step run that is awaiting
// this task (PR B — context propagation).
func stripDispatchWait(raw []byte) ([]byte, bool) {
	if len(raw) == 0 {
		return raw, false
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw, false
	}
	if _, ok := m["_dispatch_wait"]; !ok {
		return raw, false
	}
	delete(m, "_dispatch_wait")
	updated, err := json.Marshal(m)
	if err != nil {
		return raw, false
	}
	return updated, true
}

// dispatchWaitReason returns the recorded deferral note from a step run's result.
func dispatchWaitReason(raw []byte) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", false
	}
	note, ok := m["_dispatch_wait"].(string)
	if !ok || note == "" {
		return "", false
	}
	return note, true
}

// writeDispatchWaitNote records why a step run's dispatch was deferred by the per-worker
// concurrency gate.
func writeDispatchWaitNote(ctx context.Context, tx pgx.Tx, tenantID string, stepRun db.WorkflowStepRunRow, workerID string, active, limit int) error {
	return writeDispatchWait(ctx, tx, tenantID, stepRun,
		fmt.Sprintf("worker capacity %d/%d — waiting for a slot", active, limit), workerID)
}

// writeDispatchWaitNoteForProject is writeDispatchWaitNote's counterpart for the PROJECT
// max-concurrent-runs gate.
func writeDispatchWaitNoteForProject(ctx context.Context, tx pgx.Tx, tenantID string, stepRun db.WorkflowStepRunRow, active, limit int) error {
	return writeDispatchWait(ctx, tx, tenantID, stepRun,
		fmt.Sprintf("project capacity %d/%d — waiting for a slot", active, limit), "")
}

// writeDispatchWait stamps the deferral note (and, for the worker gate, the
// saturated worker) onto a step run's result. The caller commits.
func writeDispatchWait(ctx context.Context, tx pgx.Tx, tenantID string, stepRun db.WorkflowStepRunRow, note, workerID string) error {
	m := map[string]any{}
	if len(stepRun.Result) > 0 {
		_ = json.Unmarshal(stepRun.Result, &m)
	}
	m["_dispatch_wait"] = note
	if workerID != "" {
		m["_dispatch_wait_worker"] = workerID
	}
	updated, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal dispatch wait note: %w", err)
	}
	_, err = db.UpdateWorkflowStepRun(ctx, tx, tenantID, stepRun.ID, stepRun.Version, db.UpdateWorkflowStepRunFields{
		Result: &updated,
	})
	return err
}

// propagateStepRunResults copies execution fields onto the workflow step run that dispatched THIS
// execution, so the run-view UI can show them without opening each execution.
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
	// A dispatched execution means the step is no longer waiting on a worker slot — clear any stale
	// deferral note so it can never linger on a running step.
	delete(merged, "_dispatch_wait")
	updated, _ := json.Marshal(merged)
	if _, err := db.UpdateWorkflowStepRun(ctx, tx, "tnt_dev", stepRunID, version, db.UpdateWorkflowStepRunFields{
		Result: &updated,
	}); err != nil {
		r.log.Warn("propagate step run results: update", "execution", execID, "error", err)
	}
}

// truncateForLog bounds a worker-authored string for a log field or an error message. A runaway summary
// must not be able to blow up a row or a log line.
func truncateForLog(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
