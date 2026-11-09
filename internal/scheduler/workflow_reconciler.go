// WorkflowReconciler — the control loop that progresses workflow runs
// through their step DAG (docs/03_Scheduler_and_Runtime_Design.md §2,
// docs/02_Domain_Model.md §2.4).
//
// A workflow run is the top-level reconcilable object for execution;
// tasks are reconciled as children (docs/02 §2.4). The
// WorkflowReconciler:
//  1. Scans pending/running workflow runs.
//  2. Transitions a pending run to running.
//  3. Progresses the step DAG: for each step whose depends_on are all
//     satisfied (succeeded), mark it ready; for each ready step,
//     evaluate its gate (gate_policy_ref) then dispatch by kind:
//     - task: create a WorkItem (kind=task) with the step's Worker ref
//     and hand it to the TaskReconciler for dispatch (only the
//     TaskReconciler creates WorkerExecutions — docs/03 §8
//     invariant #1). After the workflow transaction commits,
//     the reconciler calls DispatchTask inline so the execution
//     appears immediately (no wait for the TaskReconciler
//     heartbeat). The step run polls the WorkItem to completion.
//     - decision: evaluate the branch (v0.1: default-true) and mark
//     succeeded; downstream branches that don't match are skipped.
//     - approval: block at approval_pending (human approval wiring
//     arrives with the Policy engine, Phase 7).
//     - parallel: mark succeeded; downstream fan-out steps become
//     ready as their deps complete.
//     - recover: invoke the recovery workflow (v0.1: mark succeeded;
//     full recovery arrives Phase 7).
//  4. When all steps are terminal-success, mark the run completed. If
//     any step failed with no recovery path, mark the run failed.
//
// Gate evaluation (docs/02 §2.5 Tier 1): the gate_policy_ref is
// evaluated before a ready step runs. The Rego Policy Engine arrives in
// Phase 7; for v0.1 the gate is a pass-through that logs the decision
// (allow) so the DAG progresses end-to-end for dev verification.
package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/contextfiles"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/reconciler"
	"github.com/beardedparrott/orchicon/internal/runtime"
	"github.com/beardedparrott/orchicon/internal/workflow"
	"github.com/jackc/pgx/v5"

	"github.com/beardedparrott/orchicon/internal/procwin"
)

// WorkflowReconciler implements the reconciler.Reconciler interface for the "workflow" kind.
type WorkflowReconciler struct {
	pool           *db.Pool
	log            *slog.Logger
	policy         PolicyEvaluator  // Phase 7: Rego gate evaluation (docs/02 §2.5)
	taskDispatcher TaskDispatcher   // inline dispatch so executions appear immediately
	recovery       RecoveryTrigger  // triggers recovery on explicit `recover` steps
	runtime        RuntimeLifecycle // per-workflow runtime container lifecycle (may be nil)
	// sequenceNotifier is called after a bound work item reaches a terminal state AND it has a parent:
	// the sequence engine advances the parent's chain immediately instead of waiting for its next scan.
	sequenceNotifier func(ctx context.Context, parentID string)
	// runFailed is called after a scan pass when reconciling ONE run failed, so a persistently broken
	// run gets the work queue's exponential backoff and its single "gave up" log line.
	runFailed func(runID string, err error)

	// worktreeNotifier is called right after a run is armed (pending→running) so the WorktreeReconciler
	// provisions the run's isolated working tree immediately instead of waiting for its next scan.
	worktreeNotifier func(ctx context.Context, runID string)

	// integrationNotifier is called right after a run reaches COMPLETED, so its project's local
	// integration runs NOW rather than on the next sweep.
	integrationNotifier func(ctx context.Context, tenantID, projectID string)

	// integrationEnqueuer writes the run's INTEGRATION OBLIGATION — one row in local_integrations — the
	// moment the run reaches COMPLETED.
	integrationEnqueuer func(ctx context.Context, tenantID, projectID, runID string)

	// warming tracks workflow runs whose runtime-serve readiness probe is in flight (the async
	// ensure-serving pass).
	warmingMu sync.Mutex
	warming   map[string]bool

	// dispatchConcurrency bounds how many inline TaskReconciler dispatches the post-commit fan-out runs
	// concurrently for the independent branch steps collected in one pass (D1.
	dispatchConcurrency int
	// dispatchOverlap is a test-only hook invoked with the current number of DispatchTask calls in
	// flight whenever the inline fan-out acquires a semaphore slot.
	dispatchOverlap func(inFlight int)

	// baseImage is the resolved fallback for a run whose image is not otherwise determined: the base
	// tag the runtime daemon on this host actually has (baseimage.go).
	baseImage string
}

// RuntimeLifecycle creates/reaps the per-workflow runtime container and proves its opencode serve
// usable before a run dispatches.
type RuntimeLifecycle interface {
	// ServeDependent reports whether the adapter kind named by a worker model_ref needs an in-container
	// serve to run (the opencode bridge does; the native "orchicon" kind does not.
	ServeDependent(kind string) bool
	// EnsureForRun creates the run's container (ALL runs in runtime mode,
	// regardless of kind). LOUD error when no daemon is reachable.
	EnsureForRun(ctx context.Context, run db.WorkflowRunRow) error
	// EnsureServing blocks until the run's opencode serve is PROVEN usable (L1: health + a real
	// session-create round-trip) when needsServe is true; returns nil immediately when false
	// (native-only.
	EnsureServing(ctx context.Context, run db.WorkflowRunRow, needsServe bool) error
	// ImagePresent reports whether a runtime image exists LOCALLY on the daemon host.
	ImagePresent(ctx context.Context, ref string) (bool, error)
	ReapForRun(ctx context.Context, runID string) error
}

// runtimeEnabled reports whether runtime-container lifecycle is wired.
func (r *WorkflowReconciler) runtimeEnabled() bool {
	if r.runtime == nil {
		return false
	}
	rv := reflect.ValueOf(r.runtime)
	return rv.Kind() != reflect.Ptr || !rv.IsNil()
}

// shouldEnsureRunFor is THE arm-site container gate ("should this run's
// container be provisioned?"). It is extracted from the arm path so the
// acceptance property is assertable directly:
//
//   - a run/execution whose EFFECTIVE execution_mode resolves to local NEVER
//     provisions a container, whatever its project says — an item-level opt-out
//     inside a runtime project keeps the lifecycle invariant at
//     internal/runtime/lifecycle.go ("local mode ... never reaches here");
//   - a runtime-resolving run provisions one whenever a daemon is wired, so an
//     item-level opt-IN inside a local project really does run in a container.
//
// runtimeEnabled() is false only for headless `orchicon serve` (no daemon
// socket), where there is nothing to provision — the LOUD failure then comes
// from runtime.Lifecycle.EnsureForRun itself rather than being masked here.
func (r *WorkflowReconciler) shouldEnsureRunFor(runLocalMode bool) bool {
	return !runLocalMode && r.runtimeEnabled()
}

// runNeedsServe reports whether any worker step of the run resolves to a serve-dependent adapter
// kind (opencode today).
func (r *WorkflowReconciler) runNeedsServe(ctx context.Context, tx pgx.Tx, tenantID string, run db.WorkflowRunRow, steps []workflow.StepWire) bool {
	if !r.runtimeEnabled() {
		return false
	}
	// Callers on the reconcile path pass the pass transaction.
	if tx == nil && r.pool != nil {
		ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
		if err != nil {
			return true
		}
		defer ttx.Rollback(ctx)
		tx = ttx.Tx
	}
	// ONE WALK, ONE PLACE (AC 2): the per-step worker versions are resolved by adapter.ResolveRunSteps,
	// the SAME walk the run-level MCP/skills union reads (mcpsettings' run-scope resolver).
	res := adapter.ResolveRunSteps(ctx, tx, tenantID, steps)
	return adapter.AdapterDemandSet(res.ModelRefs()...).NeedsServe(r.runtime.ServeDependent)
}

// NewWorkflowReconciler creates a WorkflowReconciler.
func NewWorkflowReconciler(pool *db.Pool, log *slog.Logger, pe PolicyEvaluator, td TaskDispatcher, rt RecoveryTrigger, rl RuntimeLifecycle) *WorkflowReconciler {
	// POINT THE RESOLVE CHAIN'S FALLBACK AT AN IMAGE THIS HOST ACTUALLY HAS, before this reconciler can
	// resolve anything.
	base := UseDaemonBaseImage()
	return &WorkflowReconciler{pool: pool, log: log, policy: pe, taskDispatcher: td, recovery: rt, runtime: rl,
		warming: make(map[string]bool), baseImage: base}
}

// SetRunFailedHook injects the scan-pass failure sink — normally the reconciler Manager's work queue, so a
// failing run backs off and reports its terminal failure once instead of once per heartbeat.
func (r *WorkflowReconciler) SetRunFailedHook(fn func(runID string, err error)) {
	r.runFailed = fn
}

// SetSequenceNotifier injects the callback that advances a sequence parent
// when one of its bound children reaches a terminal state. Optional.
func (r *WorkflowReconciler) SetSequenceNotifier(fn func(ctx context.Context, parentID string)) {
	r.sequenceNotifier = fn
}

// isSequenceParentActive reports whether a sequence parent is still participating in chain advance.
func (r *WorkflowReconciler) isSequenceParentActive(ctx context.Context, tenantID, parentID string) bool {
	if r.pool == nil {
		return true
	}
	ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return false
	}
	defer ttx.Rollback(ctx)
	p, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, parentID)
	if err != nil {
		return false
	}
	return p.Status == domain.WorkItemRunning || p.Status == domain.WorkItemFailed
}

// SetWorktreeNotifier injects the callback that enqueues a run (or, for a parallel-branch step run,
// a "<runID>:<stepRunID>" key) with the WorktreeReconciler so its isolated working tree.
func (r *WorkflowReconciler) SetWorktreeNotifier(fn func(ctx context.Context, runID string)) {
	r.worktreeNotifier = fn
}

// SetIntegrationNotifier injects the callback that advances LOCAL INTEGRATION immediately for a
// run's project when the run reaches COMPLETED.
func (r *WorkflowReconciler) SetIntegrationNotifier(fn func(ctx context.Context, tenantID, projectID string)) {
	r.integrationNotifier = fn
}

// SetIntegrationEnqueuer injects the hook that records a completed run's integration obligation.
func (r *WorkflowReconciler) SetIntegrationEnqueuer(fn func(ctx context.Context, tenantID, projectID, runID string)) {
	r.integrationEnqueuer = fn
}

// SetDispatchConcurrency sets the per-pass concurrency bound for the post-commit inline dispatch
// fan-out (ORCHICON_DISPATCH_CONCURRENCY).
func (r *WorkflowReconciler) SetDispatchConcurrency(n int) {
	if n < 1 {
		n = 1
	}
	if n > 64 {
		n = 64
	}
	r.dispatchConcurrency = n
}

// Kind returns the reconciler kind (docs/03 §2.1).
func (r *WorkflowReconciler) Kind() string { return "workflow" }

// Reconcile processes a single workflow run (key = run id), or scans all pending runs when key is
// empty.
func (r *WorkflowReconciler) Reconcile(ctx context.Context, key string) reconciler.Result {
	// v0.1: single dev tenant. Multi-tenant scheduling arrives with
	// auth (Phase 9). Same assumption as TaskReconciler.reconcileOne.
	tenantID := "tnt_dev"
	if key == "" {
		// Scan pass: progress all pending/running runs.
		if err := r.scanRuns(ctx, tenantID); err != nil {
			return reconciler.Result{Error: err}
		}
		return reconciler.Result{}
	}
	if err := r.reconcileRun(ctx, tenantID, key); err != nil {
		return reconciler.Result{Error: err}
	}
	return reconciler.Result{}
}

// scanRuns lists pending/running runs and reconciles each.
func (r *WorkflowReconciler) scanRuns(ctx context.Context, tenantID string) error {
	ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	runs, err := db.ListPendingWorkflowRuns(ctx, ttx.Tx, tenantID)
	ttx.Rollback(ctx)
	if err != nil {
		return fmt.Errorf("list pending runs: %w", err)
	}
	for _, run := range runs {
		if err := r.reconcileRun(ctx, tenantID, run.ID); err != nil {
			// THE QUEUE'S BACKOFF IF THERE IS ONE, A WARNING IF THERE IS NOT.
			if r.runFailed != nil {
				r.runFailed(run.ID, err)
				continue
			}
			r.log.Warn("workflow run reconcile failed", "run", run.ID, "error", err)
		}
	}
	// THEN THE TERMINAL RUNS, WHICH THE LOOP ABOVE CANNOT REACH.
	r.sweepTerminalRunContainers(ctx, tenantID)
	return nil
}

// parallelBranchChildIDs returns the set of step IDs that are parallel-branch children: steps whose
// depends_on references a step whose kind is `parallel`.
func requiresPRStep(stepID, config string) bool {
	if stepID == "step-devops-pr" {
		return true
	}
	if config == "" {
		return false
	}
	var cfg struct {
		RequiresPR *bool `json:"requires_pr"`
		RequiresPr *bool `json:"requiresPr"`
	}
	if err := json.Unmarshal([]byte(config), &cfg); err == nil {
		if cfg.RequiresPR != nil && *cfg.RequiresPR {
			return true
		}
		if cfg.RequiresPr != nil && *cfg.RequiresPr {
			return true
		}
	}
	return false
}

// effectivePRURL returns a PR URL for a run from platform-recorded and worker-authored sources, in
// order: run_context, the execution's typed PrURL, then the worker's stdout PR_URL marker.
var prURLRe = regexp.MustCompile(`PR_URL:\s*(https://github\.com/[^\s"]+/pull/\d+)`)
var prURLJSONRe = regexp.MustCompile(`"pr_url"\s*:\s*"(https://[^"]+)"`)
var prStateMarkerRe = regexp.MustCompile(`PR_STATE:\s*([A-Za-z]+)`)

// ghBin is the binary deterministicPRForBranch shells out to for the platform-verified PR lookup.
var ghBin = "gh"

func effectivePRURL(run db.WorkflowRunRow, execRow db.ExecutionRow) string {
	if u, _ := db.PrFromRunContext(run.RunContext); u != "" {
		return u
	}
	if execRow.PrURL != nil && *execRow.PrURL != "" {
		return *execRow.PrURL
	}
	if m := prURLRe.FindStringSubmatch(string(execRow.Output)); len(m) == 2 {
		return m[1]
	}
	if m := prURLJSONRe.FindStringSubmatch(string(execRow.Output)); len(m) == 2 {
		return m[1]
	}
	return ""
}

// markerPRState extracts a worker-reported PR_STATE from the execution output using a non-anchored
// whole-output scan, so a marker glued mid-sentence (the 4.2 case) still captures its state.
func markerPRState(output string) string {
	if m := prStateMarkerRe.FindStringSubmatch(output); len(m) == 2 {
		s := strings.ToLower(m[1])
		switch s {
		case "open", "merged", "draft", "closed", "none":
			return s
		}
	}
	return ""
}

// deterministicPRForBranch queries GitHub for a PR whose head is the given branch in the given repo
// ("owner/repo").
func deterministicPRForBranch(repoSlug, branch string) (prURL, prState string, verified bool) {
	if repoSlug == "" || branch == "" {
		return "", "", false
	}
	// gh pr list --head <branch> --state all --repo OWNER/REPO --json url,state The jq filter must not
	// error on an empty result set: `length > 0` selects the first PR only when one exists.
	if out, err := procwin.Command(ghBin, "pr", "list", "--head", branch, "--state", "all",
		"--repo", repoSlug, "--json", "url,state",
		"--jq", `if length > 0 then .[0] | [.url, .state] | @tsv else empty end`).Output(); err == nil {
		if url, state := parsePRLine(string(out)); url != "" {
			return url, state, true
		}
		// gh ran and returned no PR for the branch.
		return "", "", true
	}
	// Fallback: gh pr view <branch> --repo OWNER/REPO
	if out, err := procwin.Command(ghBin, "pr", "view", branch,
		"--repo", repoSlug, "--json", "url,state",
		"--jq", `[.url, .state] | @tsv`).Output(); err == nil {
		if url, state := parsePRLine(string(out)); url != "" {
			return url, state, true
		}
		return "", "", true
	}
	return "", "", false
}

// parsePRLine parses a tab-separated "url\tstate" line emitted by `gh ...
func parsePRLine(out string) (string, string) {
	fields := strings.Split(strings.TrimSpace(out), "\t")
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "https://") {
		return "", ""
	}
	state := "open"
	if len(fields) >= 2 && fields[1] != "" {
		state = strings.ToLower(fields[1])
	}
	return fields[0], state
}

func parallelBranchChildIDs(steps []workflow.StepWire) map[string]bool {
	parallel := make(map[string]bool, len(steps))
	for _, s := range steps {
		if s.Kind == domain.StepKindParallel {
			parallel[s.ID] = true
		}
	}
	out := make(map[string]bool)
	for _, s := range steps {
		for _, dep := range s.DependsOn {
			if parallel[dep] {
				out[s.ID] = true
				break
			}
		}
	}
	return out
}

// The hold engages ONLY when the cached detection is FRESH and non-repo: an undetermined
// (git_detected_at NULL) or stale cache means the WorktreeReconciler has not yet decided.
func (r *WorkflowReconciler) holdInPlaceDispatch(ctx context.Context, tx pgx.Tx, tenantID string, run db.WorkflowRunRow) bool {
	if run.WorktreeStatus != domain.WorktreePending {
		// 'skipped' (admitted in place), 'ready' (worktree), 'failed',
		// 'pruned' — all dispatch normally.
		return false
	}
	if run.ProjectID == "" {
		return false
	}
	var gitWorkTree bool
	var detectedAt *time.Time
	err := tx.QueryRow(ctx,
		`SELECT git_work_tree, git_detected_at FROM projects WHERE tenant_id = $1 AND id = $2`,
		tenantID, run.ProjectID,
	).Scan(&gitWorkTree, &detectedAt)
	if err != nil {
		// Missing project: nothing to serialize against — fail open.
		return false
	}
	// Git-backed pending runs must not dispatch in the shared project_dir (mandatory worktree for git
	// repos, ADR 2.1).
	if gitWorkTree {
		return true
	}
	if detectedAt == nil {
		return false
	}
	if time.Since(*detectedAt) >= gitDetectionTTL {
		// Stale cache: the WorktreeReconciler re-detects on its next pass.
		return false
	}
	return !gitWorkTree
}

func (r *WorkflowReconciler) reconcileRun(ctx context.Context, tenantID, runID string) error {
	ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer ttx.Rollback(ctx)

	run, err := db.GetWorkflowRun(ctx, ttx.Tx, tenantID, runID)
	if err != nil {
		if err == db.ErrNotFound {
			return nil
		}
		return fmt.Errorf("get run: %w", err)
	}
	// Only progress non-terminal runs.
	if run.Status == domain.WorkflowRunCompleted || run.Status == domain.WorkflowRunFailed || run.Status == domain.WorkflowRunAborted {
		if r.runtime != nil {
			if err := r.runtime.ReapForRun(context.Background(), run.ID); err != nil {
				r.log.Warn("reap terminal run runtime failed", "run", run.ID, "error", err)
			}
		}
		return nil
	}

	// Reaping a terminal run's runtime container is deferred until after the DB transaction commits:
	// the container is not part of run state.
	reapRuntime := false
	defer func() {
		if !r.runtimeEnabled() {
			return
		}
		bg := context.Background()
		if reapRuntime {
			if err := r.runtime.ReapForRun(bg, run.ID); err != nil {
				r.log.Warn("reap workflow runtime failed", "run", run.ID, "error", err)
			}
		}
	}()

	// Load the published version's steps to drive DAG progression.
	version, err := db.GetWorkflowVersion(ctx, ttx.Tx, tenantID, run.WorkflowID, run.WorkflowVersion)
	if err != nil {
		if err == db.ErrNotFound {
			// The workflow version the run references is gone (workflow deleted, or a raw-seeded run never had
			// one).
			if ferr := r.failRunAtStart(ctx, ttx.Tx, tenantID, run,
				fmt.Sprintf("workflow version v%d not found", run.WorkflowVersion)); ferr != nil {
				return ferr
			}
			// Commit the failure — the deferred rollback would otherwise
			// undo it on the early return.
			if cerr := ttx.Commit(ctx); cerr != nil {
				return fmt.Errorf("commit fail-at-start: %w", cerr)
			}
			reapRuntime = true
			return nil
		}
		return fmt.Errorf("get workflow version: %w", err)
	}
	steps, err := workflow.ParseSteps(version.Steps)
	if err != nil {
		return fmt.Errorf("parse steps: %w", err)
	}
	stepByID := make(map[string]workflow.StepWire, len(steps))
	for _, s := range steps {
		stepByID[s.ID] = s
	}
	// A run whose published version has an EMPTY step DAG can never progress: the terminal-state check
	// requires hasSteps && allSucceeded.
	if len(steps) == 0 {
		if ferr := r.failRunAtStart(ctx, ttx.Tx, tenantID, run,
			"workflow has no steps (empty step DAG)"); ferr != nil {
			return ferr
		}
		// Commit the failure — the deferred rollback would otherwise undo
		// it on the early return (the run would stay "running" forever).
		if cerr := ttx.Commit(ctx); cerr != nil {
			return fmt.Errorf("commit fail-at-start: %w", cerr)
		}
		reapRuntime = true
		return nil
	}

	// Transition pending → running (docs/02 §2.4). Resolve the runtime
	// container image from the work item(s) the run will use:
	//   - template runs: the bound work item's runtime_image;
	//   - one-shot runs: the WORK_ITEM canvas markers' work items —
	//     all must agree (or be empty → base), conflicting images fail
	//     the run at start since one container can't serve two images.
	// armed marks a run that left pending in this pass so the worktree
	// notifier fires post-commit (mirrors the sequence/workflow notifiers).
	var armed bool
	needsServe := r.runNeedsServe(ctx, ttx.Tx, tenantID, run, steps)
	// runLocalMode is the EFFECTIVE execution_mode gate (work-item override -> project -> tenant
	// default, resolved in db.EffectiveExecutionMode).
	runLocalMode := runExecutionMode(ctx, ttx.Tx, tenantID, run) == db.ExecutionModeLocal
	// resolvedImage is the run's effective image for this pass (the armed value, or "" for runs armed
	// before always-container — re-resolved from the run row below when needed).
	resolvedImage := run.RuntimeImage
	if run.Status == domain.WorkflowRunPending {
		resolved, rerr := r.resolveRuntimeImage(ctx, ttx.Tx, tenantID, run, steps)
		if rerr != nil {
			// Fail the run at start: an unresolvable image is a config
			// error, not a recoverable execution failure.
			if ferr := r.failRunAtStart(ctx, ttx.Tx, tenantID, run,
				fmt.Sprintf("runtime image: %v", rerr)); ferr != nil {
				return ferr
			}
			// Commit the failure — the deferred rollback would otherwise undo it on the early return (the run
			// would stay "pending" forever, re-attempting the image resolve every pass).
			if cerr := ttx.Commit(ctx); cerr != nil {
				return fmt.Errorf("commit fail-at-start: %w", cerr)
			}
			reapRuntime = true
			return nil
		}
		armImage, armReady := armRuntimeColumns(runLocalMode, resolved, needsServe, r.runtimeEnabled())
		updated, err := db.UpdateWorkflowRun(ctx, ttx.Tx, tenantID, runID, run.Version, db.UpdateWorkflowRunFields{
			Status:       strPtr(domain.WorkflowRunRunning),
			RuntimeReady: armReady,
			RuntimeImage: strPtr(armImage),
		})
		if err != nil {
			return fmt.Errorf("transition run to running: %w", err)
		}
		run = updated
		resolvedImage = run.RuntimeImage
		armed = true
		// Always-container: EVERY runtime-mode run gets its container synchronously at arm (idempotent —
		// EnsureForRun is a no-op when the run already holds a lease).
		if r.shouldEnsureRunFor(runLocalMode) {
			try := run
			try.RuntimeImage = resolvedImage
			if cerr := r.runtime.EnsureForRun(ctx, try); cerr != nil {
				// Arm-site ensure failed: roll back the arm (the run stays pending, the run_started event is
				// unwritten) and run the failover in its own tx.
				_ = ttx.Rollback(ctx)
				if err := r.ensureRunContainerFailover(ctx, tenantID, runID, resolvedImage); err != nil {
					return err
				}
				// Failover either retried into success (run still pending — next pass re-arms) or failed the run
				// LOUD at start and committed: stop the pass and reap post-commit.
				reapRuntime = true
				return nil
			}
		}
		if err := r.enqueueRunEvent(ctx, ttx.Tx, domain.WorkflowEventRunStarted, run, ""); err != nil {
			return fmt.Errorf("enqueue run_started: %w", err)
		}
	}

	// Runtime-serve readiness gate: no execution may start for a run until its runtime container's
	// opencode serve is PROVEN usable.
	if !run.RuntimeReady {
		// Always-container: EVERY runtime-mode run gets a container.
		needsServeGate := needsServe
		if r.runtimeEnabled() {
			needsServeGate = r.runNeedsServe(ctx, ttx.Tx, tenantID, run, steps)
		}
		// Local-mode runs clear the gate here unconditionally (no
		// container, no probe — in-process by explicit opt-out).
		if runLocalMode {
			needsServeGate = false
		}
		if !needsServeGate {
			updated, err := db.UpdateWorkflowRun(ctx, ttx.Tx, tenantID, runID, run.Version, db.UpdateWorkflowRunFields{
				RuntimeReady: boolPtr(true),
			})
			if err != nil {
				return fmt.Errorf("clear runtime gate (serve-less run): %w", err)
			}
			run.RuntimeReady = true
			// THE VERSION MUST MOVE WITH THE ROW.
			run.Version = updated.Version
		}
		if r.runtimeEnabled() && !run.RuntimeReady {
			// Local mode never enters the container path: no EnsureForRun, no probe — flip ready and progress
			// in-process.
			if !runLocalMode {
				// The container was ensured synchronously at arm above (every runtime-mode run, regardless of
				// kind).
				r.startEnsureServing(run, needsServeGate)
			}
			// Commit the transition (the deferred rollback would undo it on the early return) and hold
			// progression until the probe flips the gate.
			if cerr := ttx.Commit(ctx); cerr != nil {
				return fmt.Errorf("commit run start (warming runtime): %w", cerr)
			}
			if armed && r.worktreeNotifier != nil {
				r.worktreeNotifier(context.Background(), runID)
			}
			return nil
		}
		// No runtime configured (headless) OR local mode: the gate is moot — flip it so
		// progression proceeds immediately. Covers runs created before the
		// runtime_ready migration (default false).
		//
		// LOCAL MODE ARRIVES HERE TOO, AND THAT USED TO BE AN INFINITE HOT LOOP. The first
		// UpdateWorkflowRun above (the needsServeGate arm) fires for a local run when there is no
		// container engine — on Windows without one, r.runtimeEnabled() is false, so
		// needsServeGate collapses to false and the gate is flipped there. This second update then
		// ran with the SAME run.Version, but the first had already committed version+1. The
		// optimistic-concurrency WHERE clause therefore matched nothing:
		//
		//     error="clear runtime gate (headless): db: not found"
		//
		// — and ErrNotFound is a plain error, so the reconciler retried. MEASURED by an operator on
		// Windows: that message repeated every ~200ms for 22 minutes (5552 times), the run frozen at
		// pending with zero executions and NOTHING shown in the UI. A configuration state was
		// converted into an unbounded retry loop.
		//
		// TWO FIXES, and both matter: the stale version is a bug on its own (any code path that
		// updates a row twice in one pass would hit it), and the idempotent early return makes the
		// second write unnecessary rather than merely correct.
		// GUARD THE WRITE, DO NOT RETURN FROM THE PASS. An early `return nil` here looked tidier and was
		// WRONG: it skips the pass's Commit, so the deferred Rollback discards the gate clear that the branch
		// above just made — leaving runtime_ready false and the run parked, which is the very hang this
		// assembly exists to remove. The condition is only "is there anything left to write".
		if !run.RuntimeReady {
			updated, err := db.UpdateWorkflowRun(ctx, ttx.Tx, tenantID, runID, run.Version, db.UpdateWorkflowRunFields{
				RuntimeReady: boolPtr(true),
			})
			if err != nil {
				return fmt.Errorf("clear runtime gate (headless): %w", err)
			}
			// CARRY THE VERSION FORWARD. A later update in this same pass must use the version the
			// database now holds, and leaving the stale one in `run` is what made this assembly fail.
			run.RuntimeReady = true
			run.Version = updated.Version
		}
	}

	// stepRuns + runByID are built inside the outer-progress loop so newly-created step runs
	// (loop_decision iterations, loop-back re-entry runs) are visible on subsequent passes.
	var stepRuns []db.WorkflowStepRunRow
	runByID := map[string]db.WorkflowStepRunRow{}

	// Collect steps dispatched in this pass for inline TaskReconciler dispatch after the transaction
	// commits.
	var dispatchedSteps []dispatchReq

	// branchChild is the set of step IDs that are parallel-branch children (deps reference a `parallel`
	// step).
	branchChild := parallelBranchChildIDs(steps)
	// branchWorktreeTriggers collects parallel-branch step runs whose branch worktree was not yet
	// provisioned this pass; post-commit each is enqueued with the WorktreeReconciler (key.
	var branchWorktreeTriggers []string

	// Recovery triggers collected during this pass are invoked AFTER the transaction commits.
	var recoveryTriggers []recoveryTriggerReq

	// D3 rule 3 — scan-scoped create-once guard.
	loopIterationCreated := map[string]bool{}

	// DAG progression loop: repeat pending→ready, dispatch, and poll until no step makes progress in a
	// full pass.
	progressed := false
	passes := 0
	for {
		if passes >= maxDAGPasses() {
			// The pass made no forward progress across maxDAGPasses iterations — a pathological run that flips
			// a step's status every pass (e.g. a loop_decision runaway).
			r.log.Warn("workflow DAG pass limit reached — committing pass progress",
				"run", runID, "passes", passes)
			break
		}
		passes++
		madeProgress := false

		// Reload step runs on every outer iteration so newly-created step runs (e.g. loop_decision
		// iterations, loop-back re-entry runs) are visible to the terminal-state check and to the next
		// dispatch.
		stepRuns, err = db.ListWorkflowStepRuns(ctx, ttx.Tx, tenantID, runID)
		if err != nil {
			return fmt.Errorf("reload step runs: %w", err)
		}
		runByID = make(map[string]db.WorkflowStepRunRow, len(stepRuns))
		for _, sr := range stepRuns {
			// A SUPERSEDED run must never shadow the active run for its step_id: loop_decision iterations are
			// created in the same transaction as the superseding update, so several rows can share a
			// created_at.
			cur, exists := runByID[sr.StepID]
			if sr.SupersededBy != "" {
				// Superseded row: only use it if nothing exists yet; the
				// active row (when it appears) overwrites it.
				if exists {
					continue
				}
			} else if exists && cur.SupersededBy == "" {
				// Active row already won — never overwrite it.
				continue
			}
			runByID[sr.StepID] = sr
		}

		// Phase 0: Handle rejected approval steps.
		for _, sr := range runByID {
			if sr.SupersededBy != "" || sr.StepKind != domain.StepKindApproval {
				continue
			}
			if sr.Status != domain.StepRunSucceeded {
				continue
			}

			// Check step run result for _decision (human approval).
			var srResult struct {
				Decision   string `json:"_decision"`
				WorkItemID string `json:"_work_item_id"`
			}
			json.Unmarshal(sr.Result, &srResult)

			// Fall back to the work item's results ONLY when the step run carries no decision at all (legacy
			// rows) — a stale _decision left on a shared ticket by a prior run/step must never override.
			decision := srResult.Decision
			if decision == "" && srResult.WorkItemID != "" {
				if wi, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, srResult.WorkItemID); err == nil {
					decision = approvalDecisionFromSources("", wi.Results)
				}
			}

			// Map: success/approved → forward; failure/rejected → loop.
			if decision != "failure" && decision != "rejected" {
				continue
			}

			step, ok := stepByID[sr.StepID]
			if !ok {
				continue
			}
			cfg := parseApprovalConfig(step.Config)
			if cfg.LoopBranch == "" || cfg.MaxIterations < 1 {
				continue
			}
			currentIter := currentLoopIteration(runByID, cfg.LoopBranch)
			if currentIter >= cfg.MaxIterations {
				// Mark the step failed and CONTINUE the pass so the transaction commits (the completion check then
				// fails the run).
				if err := r.failStep(ctx, ttx.Tx, tenantID, run, sr, runByID,
					fmt.Errorf("approval step %q: max_iterations (%d) exhausted (rejected)", step.Name, cfg.MaxIterations)); err != nil {
					return err
				}
				madeProgress = true
				continue
			}
			if err := r.approvalReenter(ctx, ttx.Tx, tenantID, run, sr, step, runByID, cfg.LoopBranch, currentIter, time.Now().UTC(), steps); err != nil {
				return err
			}
			madeProgress = true
		}

		// Progress pending steps whose deps are satisfied → ready.
		for _, sr := range stepRuns {
			if sr.SupersededBy != "" {
				continue
			}
			// Skip if the active run for this StepID has already been moved past PENDING in a prior pass (avoid
			// version-mismatch on re-update).
			active := sr
			if cur, ok := runByID[sr.StepID]; ok {
				active = cur
			}
			if active.Status != domain.StepRunPending {
				continue
			}
			step, ok := stepByID[sr.StepID]
			if !ok {
				continue
			}
			// D3 rule 4 — hold-at-PENDING.
			if step.Kind == domain.StepKindLoopDecision && sr.Iteration > 0 &&
				r.loopDecisionUpstreamFailed(step, runByID) {
				continue
			}
			if r.depsSatisfied(step, runByID) {
				updated, err := db.UpdateWorkflowStepRun(ctx, ttx.Tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
					Status: strPtr(domain.StepRunReady),
				})
				if err != nil {
					return fmt.Errorf("mark step ready: %w", err)
				}
				runByID[sr.StepID] = updated
				madeProgress = true
				if err := r.enqueueStepEvent(ctx, ttx.Tx, domain.WorkflowEventStepReady, run, updated); err != nil {
					return fmt.Errorf("enqueue step_ready: %w", err)
				}
			}
		}

		// D3 — in-place serialization hold (concurrency guards).
		inPlaceHold := r.holdInPlaceDispatch(ctx, ttx.Tx, tenantID, run)
		if inPlaceHold {
			if r.worktreeNotifier != nil {
				r.worktreeNotifier(context.Background(), run.ID)
			}
			r.log.Info("workflow: holding steps — run not yet admitted to in-place execution",
				"run", run.ID, "project", run.ProjectID)
		}

		// Dispatch ready or recovering steps by kind, evaluating gates first.
		if !inPlaceHold {
			for _, sr := range stepRuns {
				if sr.SupersededBy != "" {
					continue
				}
				if sr.Status != domain.StepRunReady && sr.Status != domain.StepRunRecovering {
					if r2, ok := runByID[sr.StepID]; ok && (r2.Status == domain.StepRunReady || r2.Status == domain.StepRunRecovering) {
						sr = r2
					} else {
						continue
					}
				}
				// For recovering steps, hold the dispatch until the recovery is READY: terminal `resumed` AND a
				// seed resolvable for the exact dispatching worker (the file gate then writes/verifies.
				if sr.Status == domain.StepRunRecovering && (sr.StepKind == domain.StepKindTask || sr.StepKind == domain.StepKindApproval) {
					ready, stepFail := r.recoveryDispatchReady(ctx, ttx.Tx, tenantID, run, sr)
					if stepFail != nil {
						if ferr := r.failStep(ctx, ttx.Tx, tenantID, run, sr, runByID, stepFail); ferr != nil {
							return ferr
						}
						madeProgress = true
						continue
					}
					if !ready {
						// Recovery not ready — hold for the next pass.
						recoverySeedMetricsSingleton.recordHeld()
						continue
					}
				}
				step, ok := stepByID[sr.StepID]
				if !ok {
					continue
				}
				// D2 — branch-worktree readiness gate. A parallel-branch child
				// must execute in its OWN worktree (AC1), so it is held at
				// ready until the WorktreeReconciler provisions one — no
				// eventually-consistent fallback to the run worktree (that
				// would let branches share a filesystem and race .orchicon/).
				//   'pending' → hold + enqueue a branch-worktree notifier
				//               (fired post-commit); the 200ms scan pass is
				//               the safety net that re-checks next pass.
				//   'skipped' → non-git project / no run project: run in the
				//               run's cwd (there is no worktree concept).
				//   'failed'  → provisioning error: fail the step (mirrors
				//               the run-worktree failed path).
				//   'ready' → dispatch normally
				//   'pruned'/'pending' → hold + re-provision (retry after prune)
				//   'skipped' → non-git project: run in run's cwd
				//   'failed' → provisioning error: fail the step
				if branchChild[sr.StepID] {
					switch sr.WorktreeStatus {
					case domain.WorktreePending, domain.WorktreePruned, "":
						// Held + queued for provisioning: collected here, fired ONCE post-commit (below) — an in-loop fire
						// here would double-notify (pre-#393 this loop collected only.
						branchWorktreeTriggers = append(branchWorktreeTriggers, sr.ID)
						continue
					case domain.WorktreeFailed:
						if ferr := r.failStep(ctx, ttx.Tx, tenantID, run, sr, runByID,
							fmt.Errorf("branch worktree provisioning failed for step %q", step.Name)); ferr != nil {
							return ferr
						}
						madeProgress = true
						continue
					}
				}
				// Non-branch steps: gate on the RUN worktree (they execute in the run's worktree).
				if !branchChild[sr.StepID] {
					switch run.WorktreeStatus {
					case domain.WorktreeReady, domain.WorktreeSkipped:
						// ready to dispatch
					case domain.WorktreeFailed:
						if ferr := r.failStep(ctx, ttx.Tx, tenantID, run, sr, runByID,
							fmt.Errorf("worktree provisioning failed for run %s — step %q cannot dispatch", run.ID, step.Name)); ferr != nil {
							return ferr
						}
						madeProgress = true
						continue
					case domain.WorktreePending, domain.WorktreePruned, "":
						if run.ProjectID != "" {
							if r.worktreeNotifier != nil {
								r.worktreeNotifier(context.Background(), run.ID)
							}
						}
						continue
					}
				}
				allowed := r.evaluateGate(ctx, step, run)
				if !allowed {
					now := time.Now().UTC()
					updated, err := db.UpdateWorkflowStepRun(ctx, ttx.Tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
						Status:  strPtr(domain.StepRunBlocked),
						EndedAt: &now,
					})
					if err != nil {
						return fmt.Errorf("mark step blocked: %w", err)
					}
					runByID[sr.StepID] = updated
					madeProgress = true
					if err := r.enqueueStepEvent(ctx, ttx.Tx, domain.WorkflowEventStepBlocked, run, updated); err != nil {
						return fmt.Errorf("enqueue step_blocked: %w", err)
					}
					continue
				}
				var stepDispatches []dispatchReq
				if err := r.dispatchStep(ctx, ttx.Tx, tenantID, run, step, sr, runByID, steps, &stepDispatches, &recoveryTriggers, loopIterationCreated); err != nil {
					return err
				}
				madeProgress = true
				dispatchedSteps = append(dispatchedSteps, stepDispatches...)
			}
		}

		// Poll running task steps + worker-backed approval steps: check
		// their linked WorkItem status.
		for i, sr := range stepRuns {
			if sr.SupersededBy != "" {
				continue
			}
			if sr.Status != domain.StepRunRunning {
				continue
			}
			if sr.StepKind != domain.StepKindTask && sr.StepKind != domain.StepKindApproval {
				continue
			}
			stepCfg := ""
			if s, ok := stepByID[sr.StepID]; ok {
				stepCfg = s.Config
			}
			terminal, failed, err := r.pollTaskStep(ctx, ttx.Tx, tenantID, &run, sr, stepCfg, runByID, &recoveryTriggers)
			if err != nil {
				return err
			}
			if terminal {
				endNow := time.Now().UTC()
				finalStatus := domain.StepRunSucceeded
				if failed {
					finalStatus = domain.StepRunFailed
				}
				// CARRY THE VERDICT WITH THE TERMINAL MARK. The step run's
				// `_decision` is written by the TaskReconciler (propagateStepRunResults,
				// on the execution's completion) while THIS reconciler marks the step
				// terminal from the execution's STATUS, in its own pass. Two reconcilers,
				// two scan loops: there is a window where the step run is `succeeded` and
				// its Result still carries only the dispatch-time keys. A loop_decision
				// reading it in that window sees NO verdict, falls through to the
				// missing-decision branch, and re-asks the reviewer for a decision it had
				// already given. Measured on run 01M522422MB39XNM0R6819JGXM: every loop
				// iteration was created 47-108ms BEFORE the reviewer step run received its
				// `_decision`, so all three re-asks fired on verdicts that already existed,
				// burned max_reask (3), and failed the run.
				// THE TERMINAL MARK MUST NOT LOSE ITS VERSION. This write aborts the
				// whole pass on a stale version, and because the pass is ONE
				// transaction that rollback takes an UPSTREAM step's terminal mark
				// with it — the exact shape of the 2026-10-17 field incident:
				//
				//	03:10:22 WARN workflow run reconcile failed
				//	         error="mark task step terminal: db: not found"
				//
				// while the run itself went on to complete. Each attempt re-reads the
				// step run and re-derives the verdict merge from the FRESH row, so the
				// retry writes against the version that actually exists. An error is
				// still returned after the bounded attempts: a genuinely missing row
				// is not a race and must not be retried forever.
				updated, err := retryStaleVersion(ctx, r.log, "mark task step terminal", func() (db.WorkflowStepRunRow, error) {
					fresh := sr
					if cur, gerr := db.GetWorkflowStepRun(ctx, ttx.Tx, tenantID, sr.ID); gerr == nil {
						fresh = cur
					} else if !errors.Is(gerr, db.ErrNotFound) {
						return db.WorkflowStepRunRow{}, gerr
					}
					fields := db.UpdateWorkflowStepRunFields{
						Status:  strPtr(finalStatus),
						EndedAt: &endNow,
					}
					if merged, merr := r.stepRunResultWithVerdict(ctx, ttx.Tx, tenantID, fresh); merr == nil && merged != nil {
						fields.Result = &merged
					}
					return db.UpdateWorkflowStepRun(ctx, ttx.Tx, tenantID, fresh.ID, fresh.Version, fields)
				})
				if err != nil {
					return fmt.Errorf("mark task step terminal: %w", err)
				}
				stepRuns[i] = updated
				runByID[sr.StepID] = updated
				madeProgress = true
				evt := domain.WorkflowEventStepSucceeded
				if failed {
					evt = domain.WorkflowEventStepFailed
				}
				if err := r.enqueueStepEvent(ctx, ttx.Tx, evt, run, updated); err != nil {
					return fmt.Errorf("enqueue step result: %w", err)
				}
			} else if cur, ok := runByID[sr.StepID]; ok && cur.Status == domain.StepRunRecovering {
				// pollTaskStep initiated a retry — a new work_item was created and the step was set to recovering.
				stepRuns[i] = cur
				madeProgress = true
			}
		}

		// Poll running LOOP_DECISION steps: check if their re-entered chain (SSE → … → upstream reviewer)
		// has all completed.
		for i, sr := range stepRuns {
			if sr.SupersededBy != "" {
				continue
			}
			if sr.Status != domain.StepRunRunning || sr.StepKind != domain.StepKindLoopDecision {
				continue
			}
			if ok, _ := r.pollLoopDecisionChain(ctx, ttx.Tx, tenantID, sr, steps); ok {
				endNow := time.Now().UTC()
				// re-check the decision; if it's still failure, dispatchStep will re-enter again (bounded by
				// max_iterations).
				updated, err := db.UpdateWorkflowStepRun(ctx, ttx.Tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
					Status:  strPtr(domain.StepRunReady),
					EndedAt: &endNow,
				})
				if err != nil {
					return fmt.Errorf("mark loop_decision step ready: %w", err)
				}
				stepRuns[i] = updated
				runByID[sr.StepID] = updated
				madeProgress = true
				r.log.Info("loop_decision chain complete, re-evaluating",
					"run", run.ID, "step", sr.StepID)
			}
		}

		if !madeProgress {
			break
		}
		progressed = true
	}

	// Determine run terminal state: all steps succeeded → completed; any failed → failed.
	allSucceeded := true
	anyFailed := false
	anySkipped := false
	hasSteps := false
	// terminalParent, when set, is the parent of a bound work item that reached a terminal state in
	// this pass — the sequence engine is notified after commit so the chain advances immediately.
	var terminalParent string
	// completedProjectID/completedRunID carry a run that reached COMPLETED this pass to the post-commit
	// integration hooks. Empty when no run completed in this pass.
	var completedProjectID, completedRunID string
	// Collect every active non-terminal step run so that — once anyFailed is known — ALL of them are
	// skipped, not just the ones that happened to be iterated after the first failed step (created_at.
	var nonTerminal []db.WorkflowStepRunRow
	// anyRunning tracks whether any step run is still RUNNING.
	anyRunning := false
	for _, sr := range stepRuns {
		if sr.SupersededBy != "" {
			continue
		}
		hasSteps = true
		if latest, ok := runByID[sr.StepID]; ok {
			sr = latest
		}
		switch sr.Status {
		case domain.StepRunSucceeded:
		case domain.StepRunSkipped:
			// A skipped step is terminal-success; a run whose every active step is succeeded/skipped completes
			// to a skipped work item when at least one step was skipped (distinct from succeeded).
			anySkipped = true
		case domain.StepRunFailed, domain.StepRunBlocked:
			anyFailed = true
			allSucceeded = false
		case domain.StepRunRunning:
			allSucceeded = false
			anyRunning = true
		case domain.StepRunApprovalPending:
			allSucceeded = false
		case domain.StepRunRecovering:
			// Recovering is not terminal — the step is being retried.
			// Keep the run running (don't set anyFailed).
			allSucceeded = false
		default:
			allSucceeded = false
			nonTerminal = append(nonTerminal, sr)
		}
	}
	// If the run has failed, skip the not-yet-dispatched steps so the UI accurately reflects the run
	// state instead of showing them as "pending" forever.
	if anyFailed {
		for _, cur := range nonTerminal {
			if cur.Status == domain.StepRunRunning {
				continue
			}
			now2 := time.Now().UTC()
			updated, err := db.UpdateWorkflowStepRun(ctx, ttx.Tx, tenantID, cur.ID, cur.Version, db.UpdateWorkflowStepRunFields{
				Status:  strPtr(domain.StepRunSkipped),
				EndedAt: &now2,
			})
			if err != nil {
				return fmt.Errorf("skip pending step on failed run: %w", err)
			}
			runByID[cur.StepID] = updated
		}
	}
	if hasSteps && allSucceeded {
		now := time.Now().UTC()
		// The bound work item is now complete: it stayed "running" for the whole run (each step's execution
		// transitions it to running, not succeeded.
		if run.WorkItemID != "" {
			if wi, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, run.WorkItemID); err == nil {
				// Recurring items stay "recurring" after a successful run: the next_run_at was pre-computed by
				// RecurringFireReconciler and the item should be re-scanned on the next occurrence.
				status := domain.WorkItemSucceeded
				if anySkipped {
					status = domain.WorkItemSkipped
				}
				fields := db.UpdateWorkItemFields{}
				if len(wi.RecurringSchedule) > 0 {
					status = domain.WorkItemRecurring
					// If the cursor was cleared mid-run, recompute it so the
					// next occurrence still fires on schedule.
					fields.NextRunAt = ensureRecurringNextRun(wi.RecurringSchedule, wi.NextRunAt, time.Now().UTC())
				}
				fields.Status = &status
				if narrative, err := r.buildRunNarrative(ctx, ttx.Tx, tenantID, run, stepRuns, domain.WorkflowRunCompleted); err != nil {
					r.log.Warn("build run narrative", "run", runID, "error", err)
				} else if narrative != nil {
					fields.Results = narrative
				}
				review := r.buildAcceptanceReview(ctx, ttx.Tx, tenantID, run, stepRuns, domain.WorkflowRunCompleted)
				fields.AcceptanceReview = &review
				if _, err := db.UpdateWorkItem(ctx, ttx.Tx, tenantID, run.WorkItemID, wi.Version, fields); err != nil {
					return fmt.Errorf("mark bound work item terminal-success: %w", err)
				}
				// Sequence wiring: a bound child reaching terminal-success must advance its parent's chain.
				if wi.ParentID != nil && domain.WorkItemIsTerminalSuccess(status) {
					terminalParent = *wi.ParentID
				}
			}
		}
		// A COMPLETED RUN CARRIES NO FAILURE, and clearing it is not tidiness.
		emptyErr := ""
		updated, err := db.UpdateWorkflowRun(ctx, ttx.Tx, tenantID, runID, run.Version, db.UpdateWorkflowRunFields{
			Status:       strPtr(domain.WorkflowRunCompleted),
			EndedAt:      &now,
			ErrorMessage: &emptyErr,
			ErrorKind:    &emptyErr,
		})
		if err != nil {
			return fmt.Errorf("mark run completed: %w", err)
		}
		run = updated
		progressed = true
		reapRuntime = true
		// Mark the terminal transition for the post-commit notifier.
		completedProjectID = run.ProjectID
		completedRunID = run.ID
		if err := r.enqueueRunEvent(ctx, ttx.Tx, domain.WorkflowEventRunCompleted, run, ""); err != nil {
			return fmt.Errorf("enqueue run_completed: %w", err)
		}
	} else if anyFailed && !anyRunning {
		now := time.Now().UTC()
		// Update the linked work item to failed, carrying the run-level
		// narrative.
		if run.WorkItemID != "" {
			if wi, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, run.WorkItemID); err == nil {
				status := domain.WorkItemFailed
				fields := db.UpdateWorkItemFields{}
				if len(wi.RecurringSchedule) > 0 {
					// Failure semantics for recurring items: the failed occurrence is recoverable (the per-step
					// recovery flow already ran) but the cycle lives.
					status = domain.WorkItemRecurring
					// If the cursor was cleared mid-run, recompute it so the
					// next occurrence still fires on schedule.
					fields.NextRunAt = ensureRecurringNextRun(wi.RecurringSchedule, wi.NextRunAt, time.Now().UTC())
				}
				fields.Status = &status
				if narrative, err := r.buildRunNarrative(ctx, ttx.Tx, tenantID, run, stepRuns, domain.WorkflowRunFailed); err != nil {
					r.log.Warn("build run narrative", "run", runID, "error", err)
				} else if narrative != nil {
					fields.Results = narrative
				}
				review := r.buildAcceptanceReview(ctx, ttx.Tx, tenantID, run, stepRuns, domain.WorkflowRunFailed)
				fields.AcceptanceReview = &review
				_, _ = db.UpdateWorkItem(ctx, ttx.Tx, tenantID, run.WorkItemID, wi.Version, fields)
				// Sequence wiring: a bound child failing halts its parent's chain.
				if wi.ParentID != nil && status == domain.WorkItemFailed {
					terminalParent = *wi.ParentID
				}
			}
		}
		// Terminate any running worker executions linked to this run.
		for _, sr := range stepRuns {
			if sr.WorkerExecutionID != "" {
				if exec, err := db.GetExecution(ctx, ttx.Tx, tenantID, sr.WorkerExecutionID); err == nil {
					if exec.Status == domain.ExecutionRunning || exec.Status == domain.ExecutionDispatching {
						termStatus := domain.ExecutionTerminated
						termHealth := domain.HealthUnhealthy
						if _, err := db.UpdateExecution(ctx, ttx.Tx, tenantID, exec.ID, exec.Version, db.UpdateExecutionFields{
							Status:       &termStatus,
							HealthState:  &termHealth,
							EndedAt:      &now,
							ErrorMessage: strPtr("workflow run failed"),
						}); err != nil {
							return fmt.Errorf("terminate execution on run failure: %w", err)
						}
					}
				}
			}
		}
		updated, err := db.UpdateWorkflowRun(ctx, ttx.Tx, tenantID, runID, run.Version, db.UpdateWorkflowRunFields{
			Status:  strPtr(domain.WorkflowRunFailed),
			EndedAt: &now,
		})
		if err != nil {
			return fmt.Errorf("mark run failed: %w", err)
		}
		run = updated
		progressed = true
		reapRuntime = true
		if err := r.enqueueRunEvent(ctx, ttx.Tx, domain.WorkflowEventRunFailed, run, ""); err != nil {
			return fmt.Errorf("enqueue run_failed: %w", err)
		}
	}

	if err := ttx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	// Local integration for a run that just COMPLETED, post-commit so the run's terminal status is
	// visible to the integration pass it triggers.
	if completedRunID != "" && completedProjectID != "" {
		if r.integrationEnqueuer != nil {
			r.integrationEnqueuer(context.Background(), tenantID, completedProjectID, completedRunID)
		}
		if r.integrationNotifier != nil {
			r.integrationNotifier(context.Background(), tenantID, completedProjectID)
		}
	}
	// Worktree provisioning: a run armed in this pass is enqueued with the WorktreeReconciler
	// immediately so its isolated working tree is ready when the run's first step dispatches.
	if armed && r.worktreeNotifier != nil {
		r.worktreeNotifier(context.Background(), runID)
	}
	// Branch-worktree provisioning (D2): parallel-branch child step runs held at ready this pass are
	// enqueued with the WorktreeReconciler under their composite "<runID>:<stepRunID>" key so their
	// own.
	if r.worktreeNotifier != nil {
		for _, stepRunID := range branchWorktreeTriggers {
			r.worktreeNotifier(context.Background(), runID+":"+stepRunID)
		}
	}
	// Sequence advance: a bound child reached a terminal state and has a parent — notify the sequence
	// engine so the parent's chain advances (or halts) immediately rather than on the next scan tick.
	if terminalParent != "" && r.sequenceNotifier != nil {
		// Parked parent (pending) must not auto-advance: a single child fired standalone while the chain is
		// STOP-parked must run alone.
		if r.isSequenceParentActive(context.Background(), tenantID, terminalParent) {
			r.sequenceNotifier(context.Background(), terminalParent)
		}
	}
	// Recovery triggers run AFTER the transaction commits: TriggerOnFailure opens its own transaction
	// on a separate connection.
	if r.recovery != nil {
		for _, tr := range recoveryTriggers {
			// System path: no user actor, so no audit row (the reconciler
			// is system churn, out of the audit scope — design D8).
			if err := r.recovery.TriggerOnFailure(context.Background(), tr.tenantID, tr.workItemID, tr.failedExecID, tr.stepRunID, tr.reason, nil); err != nil {
				r.log.Warn("post-commit recovery trigger failed",
					"run", runID, "work_item", tr.workItemID, "error", err)
			}
		}
	}
	// Inline dispatch: hand dispatched steps to the TaskReconciler immediately so executions appear in
	// the UI without waiting for the next TaskReconciler heartbeat (~1s).
	if r.taskDispatcher != nil && len(dispatchedSteps) > 0 {
		r.dispatchInline(context.Background(), dispatchedSteps)
	}
	if progressed {
		r.log.Info("workflow run progressed", "run", runID, "status", run.Status)
	}
	return nil
}

// dispatchInline fans out the pass's collected inline TaskReconciler dispatches with bounded
// concurrency (dispatchConcurrency, default 4), waiting for every in-flight dispatch before
// returning.
func (r *WorkflowReconciler) dispatchInline(ctx context.Context, steps []dispatchReq) {
	limit := r.dispatchInlineLimit(len(steps))
	r.log.Info("inline dispatch pass", "dispatches", len(steps), "concurrency", limit)
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	var inFlight atomic.Int32
	for _, d := range steps {
		wg.Add(1)
		go func(d dispatchReq) {
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
			if err := r.taskDispatcher.DispatchTask(ctx, d.taskID, d.stepRunID); err != nil {
				r.log.Warn("inline dispatch failed", "work_item", d.taskID, "step_run", d.stepRunID, "error", err)
			}
		}(d)
	}
	wg.Wait()
}

// dispatchInlineLimit returns the effective concurrency bound for the inline dispatch fan-out: the
// configured dispatchConcurrency (default defaultDispatchConcurrency), clamped to the batch size.
func (r *WorkflowReconciler) dispatchInlineLimit(n int) int {
	limit := r.dispatchConcurrency
	if limit < 1 {
		limit = defaultDispatchConcurrency
	}
	if limit > n {
		limit = n
	}
	return limit
}

// containerFailureReason formats the diagnostic recorded when a run's container could not be
// created.
func containerFailureReason(img string, retryErr, baseErr error) string {
	const armNote = "the arm-site ensure attempt (autocommit) had already failed"
	return fmt.Sprintf("runtime container could not be created: image %q -> %v; base %q -> %v (%s)",
		img, retryErr, db.RuntimeBaseImage(), errorOrNone(baseErr), armNote)
}

// errorOrNone renders an error for a diagnostic string, or a plain word when there was none.
func errorOrNone(err error) string {
	if err == nil {
		return "not attempted"
	}
	return err.Error()
}

// failRunAtStart fails a workflow run before it can execute — a structural or configuration error
// (unresolvable runtime image, empty step DAG, missing workflow version).
func (r *WorkflowReconciler) failRunAtStart(ctx context.Context, tx pgx.Tx, tenantID string, run db.WorkflowRunRow, reason string) error {
	now := time.Now().UTC()

	// LOG THE REASON BEFORE ANYTHING THAT CAN FAIL, because THIS FUNCTION'S OWN FAILURE IS WHAT THE
	// OPERATOR SEES AND IT USED TO HIDE THE REASON COMPLETELY.
	//
	// The shape was: attempt the status update; if IT fails, return its error. The `reason` — the only
	// statement of WHY the run could not start — was discarded on that path, so the reconcile pass
	// logged nothing but a failure of the failure handler:
	//
	//	WARN workflow run reconcile failed ... error="fail run at start: db: not found"
	//
	// That is a sentence about the UPDATE, not about the run, and it cost the operator and me several
	// rounds of archaeology: the real cause (a container that could not be created, whose Docker error
	// had only just been made available) was computed on the line before and thrown away here. Reported
	// as "it failed immediately complaining about the runtime image" — with the image error never
	// reaching a log.
	//
	// ONE PLACE FIXES ALL FIVE CALLERS: the image resolve, an empty step DAG, a missing workflow
	// version, the container-create failover and the serve gate all report their reason through here.
	// The transient version-race that makes the update itself fail remains (a later pass re-fails the
	// run with a fresh snapshot, which is why the run does end up terminal) — but it can no longer
	// erase the cause.
	r.log.Warn("workflow run failed at start", "run", run.ID, "tenant", tenantID,
		"work_item", run.WorkItemID, "workflow", run.WorkflowID, "reason", reason)

	if _, err := db.UpdateWorkflowRun(ctx, tx, tenantID, run.ID, run.Version, db.UpdateWorkflowRunFields{
		Status:  strPtr(domain.WorkflowRunFailed),
		EndedAt: &now,
	}); err != nil {
		// The reason is carried in the error too, so a caller that propagates it (rather than logging its
		// own line) still states what happened.
		return fmt.Errorf("fail run at start (%s): %w", reason, err)
	}
	// Mark the bound work item failed too.
	if run.WorkItemID != "" {
		if wi, err := db.GetWorkItem(ctx, tx, tenantID, run.WorkItemID); err == nil {
			status := domain.WorkItemFailed
			fields := db.UpdateWorkItemFields{}
			// Same failure semantics as reconcileRun: a recurring item whose run fails at start stays
			// "recurring" — the cycle lives on and the RecurringFireReconciler fires the next occurrence.
			if len(wi.RecurringSchedule) > 0 {
				status = domain.WorkItemRecurring
				// If the cursor was cleared mid-cycle, recompute it so the
				// next occurrence still fires on schedule.
				fields.NextRunAt = ensureRecurringNextRun(wi.RecurringSchedule, wi.NextRunAt, time.Now().UTC())
			}
			narrative, _ := json.Marshal(map[string]any{"_run_narrative": map[string]any{
				"run_id": run.ID, "status": domain.WorkflowRunFailed, "error": reason,
			}})
			review := fmt.Sprintf("## Acceptance Review\n\n**Run:** `%s` · **Status:** failed\n\nNo work was delivered — the run failed to start: %s", run.ID, reason)
			fields.Status = &status
			fields.Results = &narrative
			fields.AcceptanceReview = &review
			_, _ = db.UpdateWorkItem(ctx, tx, tenantID, run.WorkItemID, wi.Version, fields)
		}
	}
	_ = r.enqueueRunEvent(ctx, tx, domain.WorkflowEventRunFailed, run, reason)
	return nil
}

// ensureRunContainerFailover runs the arm-site failover in its own tx: retry the resolved tag once
// (transient daemon/pool miss), then the base image once; if every attempt fails.
func (r *WorkflowReconciler) ensureRunContainerFailover(ctx context.Context, tenantID, runID, resolvedImage string) error {
	if !r.runtimeEnabled() {
		return nil
	}
	// THE FALLBACK IS THE DAEMON'S OWN BASE TAG (db.RuntimeBaseImage), not the historical placeholder:
	// falling back to a tag the daemon's allowlist refuses turned the base attempt into a guaranteed.
	baseImg := db.RuntimeBaseImage()
	img := strings.TrimSpace(resolvedImage)
	if img == "" || img == runtime.NoServeImage {
		img = baseImg
	}
	ftx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("failover begin tx: %w", err)
	}
	defer ftx.Rollback(ctx)
	cur, err := db.GetWorkflowRun(ctx, ftx.Tx, tenantID, runID)
	if err != nil {
		return fmt.Errorf("failover get run: %w", err)
	}
	try := cur
	try.RuntimeImage = img
	// FAIL FAST on a missing image, BEFORE any container-create attempt and BEFORE the base-image
	// fallback below.
	if reason, absent := r.runtimeImageAbsentReason(ctx, img); absent {
		r.log.Error("workflow runtime image is not downloaded — failing the run at start (no base fallback): fetch the image, then re-dispatch",
			"run", cur.ID, "image", img)
		if ferr := r.failRunAtStart(ctx, ftx.Tx, tenantID, cur, reason); ferr != nil {
			return ferr
		}
		if cerr := ftx.Commit(ctx); cerr != nil {
			return fmt.Errorf("commit fail-at-start (missing runtime image): %w", cerr)
		}
		return nil
	}
	// The arm site already attempted EnsureForRun once (in autocommit);
	// entry here means that attempt failed. Retry the resolved tag once
	// (transient daemon/pool miss), then fall back to the base image.
	// CAPTURE THE ACTUAL ERRORS, because the failure message is the operator's ONLY evidence and this
	// used to discard all of it.
	//
	// The old shape held a PLACEHOLDER here — fmt.Errorf("arm-site container ensure failed") — and
	// DISCARDED the errors returned by both EnsureForRun attempts below (they were bound inside the
	// `if` statements and went out of scope). So a run that could not get a container reported:
	//
	//	runtime container could not be created (image "X", then base "orchicon-runtime:base"):
	//	  arm-site container ensure failed
	//
	// which names neither the Docker error nor which attempt produced it. Reported by an operator as
	// "failed immediately complaining about the runtime image", and it is un-diagnosable from the
	// outside for exactly that reason: the sentence says a container could not be created and refuses
	// to say why.
	//
	// The arm-site attempt happened in AUTOCOMMIT before this function was entered, so its error is
	// genuinely unavailable here — the note stays, but it no longer stands in for the two that ARE
	// available, and both are now reported with the image each was tried with.
	retryErr := r.runtime.EnsureForRun(ctx, try)
	if retryErr == nil {
		return nil
	}
	// THE BASE-IMAGE FALLBACK IS NOT ATTEMPTED WHEN IT STRUCTURALLY CANNOT WORK.
	var baseErr error
	if img != baseImg {
		if kinds := r.runImageSuppliedKinds(ctx, ftx.Tx, tenantID, cur.ID); len(kinds) > 0 {
			r.log.Warn("workflow runtime: not falling back to the base image — it cannot carry the adapter "+
				"this run dispatches to, so the fallback would create a second container that cannot start its "+
				"adapter", "run", cur.ID, "resolved", img, "kinds", kinds, "error", retryErr)
		} else {
			try.RuntimeImage = baseImg
			baseErr = r.runtime.EnsureForRun(ctx, try)
			if baseErr == nil {
				r.log.Warn("workflow runtime fell back to base image", "run", cur.ID, "resolved", img, "error", retryErr)
				return nil
			}
		}
	}
	reason := containerFailureReason(img, retryErr, baseErr)
	if ferr := r.failRunAtStart(ctx, ftx.Tx, tenantID, cur, reason); ferr != nil {
		return ferr
	}
	if cerr := ftx.Commit(ctx); cerr != nil {
		return fmt.Errorf("commit fail-at-start: %w", cerr)
	}
	return nil
}

// runImageSuppliedKinds returns the adapter kinds this run dispatches to whose CLI must come FROM
// THE IMAGE.
func (r *WorkflowReconciler) runImageSuppliedKinds(ctx context.Context, tx pgx.Tx, tenantID, runID string) []string {
	cur, err := db.GetWorkflowRun(ctx, tx, tenantID, runID)
	if err != nil {
		return nil
	}
	ver, err := db.GetWorkflowVersion(ctx, tx, tenantID, cur.WorkflowID, cur.WorkflowVersion)
	if err != nil {
		// A read failure must not silently ENABLE the fallback it was meant to suppress, but nor may it fail a
		// run over a diagnostic: treat it as "unknown" and leave the fallback as it was before this change.
		return nil
	}
	steps, err := workflow.ParseSteps(ver.Steps)
	if err != nil {
		return nil
	}
	res := adapter.ResolveRunSteps(ctx, tx, tenantID, steps)
	var out []string
	for _, k := range adapter.AdapterDemandSet(res.ModelRefs()...).Kinds() {
		if k == adapter.NativeAdapterKind {
			continue
		}
		out = append(out, k)
	}
	return out
}

// runtimeImageAbsentReason reports the fail-fast reason when the run's runtime image is NOT present
// locally, and whether the caller must stop and fail the run at start.
func (r *WorkflowReconciler) runtimeImageAbsentReason(ctx context.Context, img string) (string, bool) {
	if !r.runtimeEnabled() {
		return "", false
	}
	present, err := r.runtime.ImagePresent(ctx, img)
	if err != nil || present {
		return "", false
	}
	return runtimeImageAbsentError(img).Error(), true
}

// startEnsureServing kicks off the ASYNC runtime-serve readiness probe for a run (idempotent — one
// goroutine per run; the in-flight map clears when it finishes).
func (r *WorkflowReconciler) startEnsureServing(run db.WorkflowRunRow, needsServe bool) {
	if !needsServe {
		return
	}
	r.warmingMu.Lock()
	if r.warming[run.ID] {
		r.warmingMu.Unlock()
		return
	}
	r.warming[run.ID] = true
	r.warmingMu.Unlock()

	go func() {
		defer func() {
			r.warmingMu.Lock()
			delete(r.warming, run.ID)
			r.warmingMu.Unlock()
		}()
		bg := context.Background()
		// startEnsureServing only launches this goroutine when needsServe is true (early return above), so
		// the probe always runs with needsServe=true here.
		if err := r.runtime.EnsureServing(bg, run, true); err != nil {
			r.log.Error("workflow runtime serve failed to become usable — failing run", "run", run.ID, "error", err)
			r.failRunServeGate(bg, run, err)
			return
		}
		if err := r.markRuntimeReady(bg, run); err != nil {
			r.log.Warn("mark workflow runtime ready failed", "run", run.ID, "error", err)
		}
	}()
}

// markRuntimeReady flips the run's runtime_ready gate to true (its serve
// passed the L1 probe). No-op if the run terminalized while warming.
func (r *WorkflowReconciler) markRuntimeReady(ctx context.Context, run db.WorkflowRunRow) error {
	ttx, err := r.pool.BeginTenantTx(ctx, run.TenantID)
	if err != nil {
		return err
	}
	defer ttx.Rollback(ctx)
	cur, err := db.GetWorkflowRun(ctx, ttx.Tx, run.TenantID, run.ID)
	if err != nil {
		return err
	}
	if cur.Status != domain.WorkflowRunRunning {
		return nil // run terminalized while warming — nothing to gate
	}
	if _, err := db.UpdateWorkflowRun(ctx, ttx.Tx, run.TenantID, run.ID, cur.Version, db.UpdateWorkflowRunFields{
		RuntimeReady: boolPtr(true),
	}); err != nil {
		return err
	}
	return ttx.Commit(ctx)
}

// failRunServeGate fails a run whose runtime serve could not be proven usable within the readiness
// window.
func (r *WorkflowReconciler) failRunServeGate(ctx context.Context, run db.WorkflowRunRow, serveErr error) {
	reason := fmt.Sprintf("runtime opencode serve failed to become usable: %v", serveErr)
	ttx, err := r.pool.BeginTenantTx(ctx, run.TenantID)
	if err != nil {
		r.log.Error("fail run serve gate: begin tx", "run", run.ID, "error", err)
		return
	}
	defer ttx.Rollback(ctx)
	cur, err := db.GetWorkflowRun(ctx, ttx.Tx, run.TenantID, run.ID)
	if err != nil {
		r.log.Error("fail run serve gate: get run", "run", run.ID, "error", err)
		return
	}
	if cur.Status != domain.WorkflowRunRunning {
		return // already terminal — nothing to fail
	}
	if err := r.failRunAtStart(ctx, ttx.Tx, run.TenantID, cur, reason); err != nil {
		r.log.Error("fail run serve gate", "run", run.ID, "error", err)
		return
	}
	if err := ttx.Commit(ctx); err != nil {
		r.log.Error("fail run serve gate: commit", "run", run.ID, "error", err)
		return
	}
	if r.runtime != nil {
		if err := r.runtime.ReapForRun(context.Background(), run.ID); err != nil {
			r.log.Warn("reap failed run runtime", "run", run.ID, "error", err)
		}
	}
}

// buildRunNarrative aggregates the run's step results + recovery episodes into the bound work
// item's results — the run-level narrative.
func (r *WorkflowReconciler) buildRunNarrative(ctx context.Context, tx pgx.Tx, tenantID string, run db.WorkflowRunRow, stepRuns []db.WorkflowStepRunRow, finalStatus string) (*[]byte, error) {
	if run.WorkItemID == "" {
		return nil, nil
	}
	wi, err := db.GetWorkItem(ctx, tx, tenantID, run.WorkItemID)
	if err != nil {
		return nil, fmt.Errorf("get bound work item for narrative: %w", err)
	}
	var steps []map[string]any
	for _, sr := range stepRuns {
		var meta map[string]any
		_ = json.Unmarshal(sr.Result, &meta)
		entry := map[string]any{
			"step_id":   sr.StepID,
			"step_name": sr.StepName,
			"status":    sr.Status,
		}
		for _, k := range []string{"_summary", "_decision", "_issues", "_worker", "_worker_name", "_recovery_summary", "_recovery_execution_id", "_recovery_worker_id"} {
			if v, ok := meta[k]; ok {
				entry[strings.TrimPrefix(k, "_")] = v
			}
		}
		steps = append(steps, entry)
	}
	var recoveries []map[string]any
	if recs, err := db.ListRecoveries(ctx, tx, db.ListRecoveriesFilter{TenantID: tenantID, TaskID: run.WorkItemID}); err == nil {
		for _, rec := range recs {
			recoveries = append(recoveries, map[string]any{
				"recovery_id": rec.ID,
				"summary":     rec.Summary,
				"reason":      rec.TriggerReason,
				"status":      rec.Status,
			})
		}
	}
	merged := map[string]any{}
	if len(wi.Results) > 0 {
		_ = json.Unmarshal(wi.Results, &merged)
	}
	merged["_run_narrative"] = map[string]any{
		"run_id":     run.ID,
		"status":     finalStatus,
		"steps":      steps,
		"recoveries": recoveries,
	}
	mergedJSON, _ := json.Marshal(merged)
	return &mergedJSON, nil
}

// buildAcceptanceReview assembles the deterministic, human-readable
// acceptance review markdown for a terminal run — the faithful projection
// of the step data buildRunNarrative reads (the per-step worker _summary /
// _decision / _issues and any recovery episodes ARE the final work done).
//
// Rules (architecture note — add-acceptance-review-field-to-work-items):
//   - Deterministic: step runs are sorted by step_id; no LLM call, no
//     wall-clock-dependent ordering. A completed run lists "What was
//     delivered"; a failed run additionally lists "Not delivered /
//     needs attention" drawn from failed steps' _summary/_issues.
//   - Only steps with a non-empty _summary (or, on failure, _issues) are
//     listed — no empty noise. Skipped steps are omitted (a skip delivered
//     nothing). Superseded iterations are omitted (superseded_by != "" —
//     replaced by a later loop iteration).
//   - Recovery episodes (when any) are listed once under "Recovery",
//     deduplicated by recovery id.
//   - The document is capped at maxDescLen (1 MiB) so a pathological run
//     cannot bloat the column; the caller still validates via the API
//     boundary when a human edits it later.
func (r *WorkflowReconciler) buildAcceptanceReview(ctx context.Context, tx pgx.Tx, tenantID string, run db.WorkflowRunRow, stepRuns []db.WorkflowStepRunRow, finalStatus string) string {
	// A REVIEW IS ABOUT THIS RUN.
	var recoveries []db.RecoveryExecutionRow
	if recs, err := db.ListRecoveries(ctx, tx, db.ListRecoveriesFilter{
		TenantID: tenantID, TaskID: run.WorkItemID, PageSize: 1000,
	}); err == nil {
		ownExecs := map[string]struct{}{}
		if execs, eerr := db.ListExecutions(ctx, tx, db.ListExecutionsFilter{
			TenantID: tenantID, WorkflowRunID: run.ID, PageSize: 1000,
		}); eerr == nil {
			for _, e := range execs {
				ownExecs[e.ID] = struct{}{}
			}
		}
		for _, rec := range recs {
			if _, mine := ownExecs[rec.FailedExecutionID]; mine {
				recoveries = append(recoveries, rec)
			}
		}
		if len(ownExecs) == 0 {
			// No executions found for this run (an early failure, or a read problem): showing NOTHING is the
			// honest answer. Showing another run's recoveries is what this fix removes.
			recoveries = nil
		}
	}
	return formatAcceptanceReview(run, stepRuns, recoveries, finalStatus)
}

// formatAcceptanceReview is the pure, deterministic formatter behind buildAcceptanceReview — split
// out so the aggregation rules are unit-testable without a database.
func formatAcceptanceReview(run db.WorkflowRunRow, stepRuns []db.WorkflowStepRunRow, recoveries []db.RecoveryExecutionRow, finalStatus string) string {
	sorted := append([]db.WorkflowStepRunRow(nil), stepRuns...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].StepID != sorted[j].StepID {
			return sorted[i].StepID < sorted[j].StepID
		}
		return sorted[i].ID < sorted[j].ID
	})

	var b strings.Builder
	statusLabel := "completed"
	if finalStatus == domain.WorkflowRunFailed {
		statusLabel = "failed"
	}
	fmt.Fprintf(&b, "## Acceptance Review\n\n**Run:** `%s` · **Status:** %s\n", run.ID, statusLabel)
	if run.EndedAt != nil {
		fmt.Fprintf(&b, "**Completed at:** %s\n", run.EndedAt.UTC().Format(time.RFC3339))
	}
	b.WriteString("\n")

	var delivered []string
	var notDelivered []string
	for _, sr := range sorted {
		if sr.SupersededBy != "" {
			// Replaced by a later loop iteration — not final work.
			continue
		}
		var meta map[string]any
		_ = json.Unmarshal(sr.Result, &meta)
		summary, _ := meta["_summary"].(string)
		issues, _ := meta["_issues"].(string)
		decision, _ := meta["_decision"].(string)
		switch sr.Status {
		case domain.StepRunSucceeded:
			summary = strings.TrimSpace(summary)
			if summary == "" {
				continue
			}
			label := sr.StepName
			if decision != "" {
				label += " — " + decision
			}
			delivered = append(delivered, fmt.Sprintf("- **%s** (succeeded): %s", label, summary))
		case domain.StepRunFailed, domain.StepRunBlocked:
			text := strings.TrimSpace(summary)
			if text == "" {
				text = strings.TrimSpace(issues)
			}
			if text == "" {
				text = "no summary recorded"
			}
			label := sr.StepName
			if decision != "" {
				label += " — " + decision
			}
			notDelivered = append(notDelivered, fmt.Sprintf("- **%s** (%s): %s", label, sr.Status, text))
		}
	}

	if len(delivered) > 0 {
		b.WriteString("### What was delivered\n\n")
		for _, d := range delivered {
			b.WriteString(d)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	if len(notDelivered) > 0 {
		b.WriteString("### Not delivered / needs attention\n\n")
		for _, d := range notDelivered {
			b.WriteString(d)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	// VERIFICATION EVIDENCE — the answer to "was any of this actually checked?".
	b.WriteString(formatVerificationSummary(sorted))

	if len(recoveries) > 0 {
		b.WriteString("### Recovery\n\n")
		for _, rec := range recoveries {
			b.WriteString(fmt.Sprintf("- **%s** (%s): %s\n", rec.Status, rec.TriggerReason, strings.TrimSpace(rec.Summary)))
		}
		b.WriteString("\n")
	}

	if len(delivered) == 0 && len(notDelivered) == 0 && len(recoveries) == 0 {
		// A run with no eligible step summaries still records the terminal outcome — an empty review field
		// would be indistinguishable from "no run yet".
		out := fmt.Sprintf("## Acceptance Review\n\n**Run:** `%s` · **Status:** %s\n\nNo step summaries were recorded.", run.ID, statusLabel)
		if len(out) > maxWorkItemDescLen {
			out = out[:maxWorkItemDescLen]
		}
		return out
	}

	out := strings.TrimSpace(b.String())
	if len(out) > maxWorkItemDescLen {
		out = out[:maxWorkItemDescLen]
	}
	return out
}

// maxWorkItemDescLen mirrors workitem's maxDescLen (1 MiB) so the
// reconciler's auto-generated review shares the API boundary's cap.
const maxWorkItemDescLen = 1 << 20

// depsSatisfied returns true if all depends_on steps of `step` are in a terminal-success state
// (succeeded or skipped).
func (r *WorkflowReconciler) depsSatisfied(step workflow.StepWire, runs map[string]db.WorkflowStepRunRow) bool {
	isLoopDecision := step.Kind == domain.StepKindLoopDecision
	for _, dep := range step.DependsOn {
		sr, ok := runs[dep]
		if !ok {
			return false
		}
		if sr.SupersededBy != "" {
			return false
		}
		if sr.Status == domain.StepRunSucceeded || sr.Status == domain.StepRunSkipped {
			continue
		}
		if isLoopDecision && sr.Status == domain.StepRunFailed {
			continue
		}
		return false
	}
	return true
}

// loopDecisionUpstreamFailed reports whether ANY upstream dependency of a loop_decision step is
// terminal-FAILED.
func (r *WorkflowReconciler) loopDecisionUpstreamFailed(step workflow.StepWire, runs map[string]db.WorkflowStepRunRow) bool {
	for _, dep := range step.DependsOn {
		if up, ok := runs[dep]; ok && up.SupersededBy == "" && up.Status == domain.StepRunFailed {
			return true
		}
	}
	return false
}

// evaluateGate evaluates the step's gate_policy_ref (docs/02 §2.5 Tier 1).
func (r *WorkflowReconciler) evaluateGate(ctx context.Context, step workflow.StepWire, run db.WorkflowRunRow) bool {
	if step.GatePolicyRef == "" {
		return true
	}
	if r.policy == nil {
		r.log.Info("workflow gate pass-through (no policy engine)",
			"run", run.ID, "step", step.ID, "gate_policy_ref", step.GatePolicyRef, "decision", "allow")
		return true
	}
	allowed, err := r.policy.EvaluateGate(ctx, run.TenantID, step.GatePolicyRef, "step_run", run.ID, map[string]any{
		"workflow_id": run.WorkflowID, "run_id": run.ID, "step_id": step.ID,
		"step_kind": step.Kind, "project_id": run.ProjectID,
	})
	if err != nil {
		r.log.Warn("workflow gate evaluation error (fail-open)",
			"run", run.ID, "step", step.ID, "error", err)
		return true
	}
	r.log.Info("workflow gate evaluated",
		"run", run.ID, "step", step.ID, "gate_policy_ref", step.GatePolicyRef, "allowed", allowed)
	return allowed
}

// dispatchStep dispatches a ready step by kind (docs/02 §2.4, docs/10 §5.1).
//
// The PR A model: the canvas holds three first-class node types —
// PROJECT (entry, sets the project context), WORK_ITEM (a passive
// marker that holds a work item's metadata as context for downstream
// workers), and TASK (a worker that processes the work item(s)
// connected to its input edge). Decision/Approval/Parallel/Recover
// remain for advanced control flow but are not required for the simple
// Work Item → Worker chain.
//
// TASK semantics under PR A:
//   - Find upstream WORK_ITEM steps in step.DependsOn.
//   - For each, load the referenced work item, set its
//     assigned_worker_ref to this step's worker, and dispatch it via
//     the existing TaskReconciler path (which keys on
//     assigned_worker_ref — docs/03 §8 invariant #1).
//   - The step run tracks the primary work item id under
//     _work_item_id in result JSON so pollTaskStep can poll.
func (r *WorkflowReconciler) dispatchStep(ctx context.Context, tx pgx.Tx, tenantID string, run db.WorkflowRunRow, step workflow.StepWire, sr db.WorkflowStepRunRow, runs map[string]db.WorkflowStepRunRow, allSteps []workflow.StepWire, dispatchedSteps *[]dispatchReq, recoveryTriggers *[]recoveryTriggerReq, loopIterationCreated map[string]bool) error {
	now := time.Now().UTC()
	switch step.Kind {
	case domain.StepKindProject:
		// Project marker step.
		pid := readConfigProjectID(step.Config)
		if pid == "" {
			return r.failStep(ctx, tx, tenantID, run, sr, runs,
				fmt.Errorf("project step %q has no config.project_id", step.Name))
		}
		if run.ProjectID != pid {
			updated, err := db.UpdateWorkflowRun(ctx, tx, tenantID, run.ID, run.Version, db.UpdateWorkflowRunFields{
				ProjectID: &pid,
			})
			if err != nil {
				return fmt.Errorf("set workflow project_id: %w", err)
			}
			run = updated
			r.log.Info("workflow project bound", "run", run.ID, "project", pid)
		}
		return r.succeedStep(ctx, tx, tenantID, run, sr, runs, now, "project bound")

	case domain.StepKindWorkItem:
		// Work item marker step.
		wid := readConfigWorkItemID(step.Config)
		if wid == "" {
			return r.failStep(ctx, tx, tenantID, run, sr, runs,
				fmt.Errorf("work_item step %q has no config.work_item_id", step.Name))
		}
		if _, err := db.GetWorkItem(ctx, tx, tenantID, wid); err != nil {
			if err == db.ErrNotFound {
				return r.failStep(ctx, tx, tenantID, run, sr, runs,
					fmt.Errorf("work item %s not found", wid))
			}
			return fmt.Errorf("load work item: %w", err)
		}
		return r.succeedStep(ctx, tx, tenantID, run, sr, runs, now, "work_item marker")

	case domain.StepKindTask:
		// The ticket is NEVER mutated here — no assigned_worker_ref / workflow_step_id / prompt_context /
		// status writes.
		var upstream []string
		if sr.Status == domain.StepRunRecovering {
			var parsed struct {
				WorkItemID string `json:"_work_item_id"`
			}
			if err := json.Unmarshal(sr.Result, &parsed); err == nil && parsed.WorkItemID != "" {
				upstream = []string{parsed.WorkItemID}
			}
		}
		if len(upstream) == 0 {
			// Normal path: look upstream for WORK_ITEM steps or bound item.
			upstream = upstreamWorkItemIDs(step, allSteps)
			if len(upstream) == 0 && run.WorkItemID != "" {
				upstream = []string{run.WorkItemID}
			}
		}
		if len(upstream) == 0 {
			return r.failStep(ctx, tx, tenantID, run, sr, runs,
				fmt.Errorf("worker step %q has no upstream work_item", step.Name))
		}
		if step.Ref == "" {
			return r.failStep(ctx, tx, tenantID, run, sr, runs,
				fmt.Errorf("worker step %q has no worker ref", step.Name))
		}
		// By NUMBER, not by id.
		workerVer, err := db.GetWorkerVersionByNumber(ctx, tx, tenantID, step.Ref, step.WorkerVersion)
		if err != nil {
			if err == db.ErrNotFound {
				// Fall back to latest published — supports workflows
				// that don't pin a specific version.
				workerVer, err = db.GetLatestWorkerVersion(ctx, tx, tenantID, step.Ref, true)
				if err != nil {
					// The worker has no usable version (deleted, no published version, or an index/catalog anomaly hid
					// it).
					return r.failStep(ctx, tx, tenantID, run, sr, runs,
						fmt.Errorf("load worker version for %s: %w", step.Ref, err))
				}
			} else {
				return fmt.Errorf("load worker version for %s: %w", step.Ref, err)
			}
		}
		// Build the composite prompt from the (read-only) work item + upstream step-run context.
		var primaryWID string
		var composite string
		var promptFP string // context-file fingerprint (ADR-0009 D5)
		var agentPromptLen int
		for _, wid := range upstream {
			wi, err := db.GetWorkItem(ctx, tx, tenantID, wid)
			if err != nil {
				if err == db.ErrNotFound {
					return r.failStep(ctx, tx, tenantID, run, sr, runs,
						fmt.Errorf("work item %s not found", wid))
				}
				return fmt.Errorf("load work item: %w", err)
			}
			wi.WorkflowStepID = sr.StepID
			var fp string
			composite, fp, err = r.buildCompositePrompt(ctx, tx, tenantID, wi, workerVer, allSteps, runs)
			if err != nil {
				return fmt.Errorf("build composite prompt for %s: %w", wid, err)
			}
			if fp != "" {
				promptFP = fp
			}
			primaryWID = wid
			agentPromptLen = len(r.agentPromptFor(ctx, tx, tenantID, wi, workerVer))
		}
		// Record the primary work item id + the composite prompt + the step's worker on the STEP RUN
		// (per-step, not on the shared ticket).
		stepResult, _ := json.Marshal(map[string]any{
			"_work_item_id":       primaryWID,
			"_prompt":             composite,
			"_prompt_fingerprint": promptFP,
			// The RUN-LEVEL prefix length: the adapter trims exactly this
			// many bytes off the message on 2.x, where the agent carries them.
			"_agent_prompt_len": agentPromptLen,
			"_worker_id":        step.Ref,
			"_worker_version":   step.WorkerVersion,
		})
		// Preserve the recovery narrative across a re-dispatch so the run view keeps showing it after the
		// step runs again.
		if sr.Status == domain.StepRunRecovering {
			var prev map[string]any
			_ = json.Unmarshal(sr.Result, &prev)
			if len(prev) > 0 {
				var newResult map[string]any
				_ = json.Unmarshal(stepResult, &newResult)
				for _, k := range recoveryResultKeys {
					if v, ok := prev[k]; ok {
						newResult[k] = v
					}
				}
				// Pin the re-dispatch to the dead execution's worker version: model_ref (hence adapter) is a
				// per-version property, so resolving the step's active version here could silently switch adapters
				// (v3.
				if rv, ok := prev["_recovery_worker_version"].(float64); ok && rv != 0 {
					newResult["_worker_version"] = rv
				}
				stepResult, _ = json.Marshal(newResult)
			}
		}
		// Clear any stale worker_execution_id (a recovering step re-dispatched here still references its
		// FAILED execution): pollTaskStep would otherwise see the old failed execution and re-trigger.
		updated, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
			Status:            strPtr(domain.StepRunRunning),
			Result:            &stepResult,
			WorkerExecutionID: strPtr(""),
			StartedAt:         &now,
		})
		if err != nil {
			return fmt.Errorf("mark task step running: %w", err)
		}
		runs[step.ID] = updated
		if err := r.enqueueStepEvent(ctx, tx, domain.WorkflowEventStepStarted, run, updated); err != nil {
			return fmt.Errorf("enqueue step_started: %w", err)
		}
		if dispatchedSteps != nil {
			*dispatchedSteps = append(*dispatchedSteps, dispatchReq{taskID: primaryWID, stepRunID: sr.ID})
		}
		r.log.Info("workflow worker dispatched",
			"run", run.ID, "step", step.ID,
			"work_items", upstream, "worker", step.Ref)

	case domain.StepKindDecision:
		// v0.1: default branch (true). Branch-condition evaluation from
		// step.config arrives with richer decision config.
		updated, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
			Status:    strPtr(domain.StepRunSucceeded),
			StartedAt: &now,
			EndedAt:   &now,
		})
		if err != nil {
			return fmt.Errorf("mark decision step succeeded: %w", err)
		}
		runs[step.ID] = updated
		if err := r.enqueueStepEvent(ctx, tx, domain.WorkflowEventStepSucceeded, run, updated); err != nil {
			return fmt.Errorf("enqueue decision step_succeeded: %w", err)
		}

	case domain.StepKindLoopDecision:
		// Loop decision step (docs/11 §3): inspect the upstream step's
		// result and the bound work item's decision signal.
		//
		//   Upstream failed       → loop back to loop_branch (if iterations
		//                            < max_iterations) or fail.
		//   Upstream succeeded +
		//     _decision: success  → proceed forward.
		//     _decision: failure  → loop back to loop_branch with full
		//                            context (same as upstream failed).
		//     no _decision field  → re-ask the reviewer (up to max_reask).
		cfg := parseLoopDecisionConfig(step.Config)
		if cfg.LoopBranch == "" || cfg.MaxIterations < 1 {
			return r.failStep(ctx, tx, tenantID, run, sr, runs,
				fmt.Errorf("loop_decision step %q: missing or invalid config (loop_branch=%q, max_iterations=%d)",
					step.Name, cfg.LoopBranch, cfg.MaxIterations))
		}

		// Fan-in: collect terminal runs for EVERY dependency.
		type upInfo struct {
			run      db.WorkflowStepRunRow
			wid      string
			decision string
		}
		var upInfos []upInfo
		for _, dep := range step.DependsOn {
			s, ok := runs[dep]
			if !ok {
				continue
			}
			var upResult struct {
				WorkItemID string `json:"_work_item_id"`
				Decision   string `json:"_decision"`
			}
			json.Unmarshal(s.Result, &upResult)
			upInfos = append(upInfos, upInfo{run: s, wid: upResult.WorkItemID, decision: upResult.Decision})
		}
		if len(upInfos) == 0 {
			return r.failStep(ctx, tx, tenantID, run, sr, runs,
				fmt.Errorf("loop_decision step %q: no upstream step result found", step.Name))
		}
		// Wait until every upstream reaches a terminal state before deciding.
		allTerminal := true
		for _, u := range upInfos {
			if u.run.Status != domain.StepRunSucceeded && u.run.Status != domain.StepRunFailed {
				allTerminal = false
				break
			}
		}
		if !allTerminal {
			break
		}

		// If any upstream failed (crash, stall, tool error), trigger recovery for each failed step and
		// create a new loop decision iteration so downstream steps block until the recovery cycle
		// completes.
		anyFailed := false
		for _, u := range upInfos {
			if u.run.Status == domain.StepRunSucceeded {
				continue
			}
			anyFailed = true
			if r.recovery != nil && u.wid != "" {
				// The failed execution + step run are the upstream step run's own (not GetLatestExecutionForTask —
				// on a shared work item that could resolve to a different step's run).
				failedExecID := u.run.WorkerExecutionID
				// Defer the trigger to post-commit (see reconcileRun): TriggerOnFailure opens its own transaction,
				// which would block on this pass's locks on the same work item.
				if recoveryTriggers != nil {
					*recoveryTriggers = append(*recoveryTriggers, recoveryTriggerReq{
						tenantID:     tenantID,
						workItemID:   u.wid,
						failedExecID: failedExecID,
						stepRunID:    u.run.ID,
						reason:       "loop_decision:upstream_failed",
					})
				}
			}
		}
		if anyFailed {
			// D3 — anti-flood guard. An upstream failed → loop back and
			// create a new loop_decision iteration so downstream steps block
			// until the recovery cycle completes and the re-dispatched
			// reviewer produces a fresh decision. Without a guard,
			// depsSatisfied treats the failed upstream as satisfied for THIS
			// loop decision, so the freshly-created pending iteration
			// re-dispatches on the next DAG pass and (upstream still failed)
			// spawns ANOTHER iteration — all within one reconcile
			// transaction (the observed runaway + maxDAGPasses rollback).
			// Three rules close it:
			//
			//   Rule 1 (iteration field): only the ORIGINAL run
			//     (Iteration == 0) may create the recovery iteration. A
			//     spawned iteration (Iteration > 0) never spawns again — it
			//     waits for the recovery to land and re-evaluate.
			//   Rule 2 (broadened in-flight): wait if ANY other
			//     non-superseded, non-terminal iteration exists for this
			//     step — not just `pending` (the status=pending-only guard
			//     was the bug: a concurrent class of rows in
			//     READY/RUNNING/RECOVERING was invisible to it).
			//   Rule 3 (scan-scoped create-once): loopIterationCreated
			//     records that THIS pass already spawned for this step, so a
			//     second evaluation of the same failed upstream within one
			//     transaction is a no-op.
			//
			// The hold-at-PENDING rule (pending→ready phase) keeps the
			// spawned iteration at PENDING while any upstream dep is still
			// terminal-failed, so madeProgress stays false and the scan
			// exits on !madeProgress instead of looping.
			if sr.Iteration > 0 || loopIterationCreated[step.ID] {
				r.log.Info("loop_decision: upstream failed, iteration already spawned this pass — waiting",
					"run", run.ID, "step", step.ID, "iteration", sr.Iteration)
				break
			}
			hasActiveIter := false
			if iterRuns, ierr := listStepRunsByStepID(ctx, tx, tenantID, run.ID, step.ID); ierr == nil {
				for _, prev := range iterRuns {
					if prev.SupersededBy == "" && prev.ID != sr.ID && prev.Iteration > 0 &&
						prev.Status != domain.StepRunSucceeded && prev.Status != domain.StepRunFailed &&
						prev.Status != domain.StepRunSkipped {
						hasActiveIter = true
						break
					}
				}
			}
			if hasActiveIter {
				r.log.Info("loop_decision: upstream failed, active non-terminal iteration exists — waiting",
					"run", run.ID, "step", step.ID)
				break
			}
			loopIterationCreated[step.ID] = true
			nextIter := currentLoopIteration(runs, step.ID) + 1
			if err := r.createLoopDecisionIteration(ctx, tx, tenantID, run, sr, step, runs, nextIter, now, `{"loop":"recovered"}`); err != nil {
				return err
			}
			r.log.Info("loop_decision: upstream failed, new iteration waiting", "run", run.ID, "step", step.ID)
			break
		}

		// All upstreams succeeded.
		decisions := make([]string, 0, len(upInfos))
		for _, u := range upInfos {
			d := u.decision
			if d == "" && u.wid != "" {
				wi, err := db.GetWorkItem(ctx, tx, tenantID, u.wid)
				if err == nil && len(wi.Results) > 0 {
					var wiResult map[string]any
					if json.Unmarshal(wi.Results, &wiResult) == nil {
						if v, ok := wiResult[cfg.DecisionField]; ok {
							d, _ = v.(string)
						}
					}
				}
			}
			decisions = append(decisions, d)
		}

		decision := aggregateLoopDecisions(decisions, cfg.FailureValue, cfg.SuccessValue)

		switch decision {
		case cfg.SuccessValue:
			// Decision is success → proceed forward.
			if err := r.loopDecisionAccept(ctx, tx, tenantID, run, sr, step, runs, now, "upstream success"); err != nil {
				return err
			}

		case cfg.FailureValue:
			// Decision is failure → loop back to loop_branch with full context.
			currentIter := currentLoopIteration(runs, cfg.LoopBranch)
			if currentIter >= cfg.MaxIterations {
				// Fail even though the step run succeeded — the reviewer
				// determined the work is not acceptable.
				return r.failStep(ctx, tx, tenantID, run, sr, runs,
					fmt.Errorf("loop_decision step %q: max_iterations (%d) exhausted (rejected by reviewer)", step.Name, cfg.MaxIterations))
			}
			r.log.Info("loop_decision: rejected, looping back",
				"run", run.ID, "step", step.ID, "loop_branch", cfg.LoopBranch, "iteration", currentIter)
			if err := r.loopDecisionReenter(ctx, tx, tenantID, run, sr, step, runs, cfg.LoopBranch, currentIter, now, allSteps); err != nil {
				return err
			}

		default:
			// No upstream supplied a value in cfg.DecisionField. WHAT THAT MEANS IS
			// THE WORKFLOW'S CALL, not the engine's — so it is config:
			//
			//   reask   (default, and what an ABSENT key means) — re-dispatch the
			//           reviewer and ask it to state a verdict.
			//   success — proceed forward. For a gate whose upstream has no verdict
			//           to give: re-asking it only re-runs the same step, and the
			//           loop target IS that same step, so the "re-ask" carries no new
			//           information. It burns the re-ask budget and then fails the
			//           node — the wedge that needed force_progress.
			//   fail    — a verdict is mandatory here; refuse immediately.
			//
			// This REPLACES a hardcode that recognised the terminal devops loop by
			// tenant step id (loop_branch "step-devops-pr" plus a success_branch of
			// "step-l32ezp4b" or "step-end" — the latter a fossil no live row ever
			// carried). That guard matched only ONE of the two loops it was written
			// for: SDLC (human approval)'s loop points at step-qonmbwyu, so it kept
			// re-asking and kept failing. Expressed as a policy it is data the
			// operator sets, and 20260924000000_backfill_loop_decision_missing_verdict
			// backfills the loops the hardcode actually rescued.
			if cfg.OnMissingDecision == MissingDecisionSuccess {
				if err := r.loopDecisionAccept(ctx, tx, tenantID, run, sr, step, runs, now,
					"on_missing_decision=success (no upstream verdict)"); err != nil {
					return err
				}
				break
			}
			if cfg.OnMissingDecision == MissingDecisionFail {
				return r.failStep(ctx, tx, tenantID, run, sr, runs,
					fmt.Errorf("loop_decision step %q: no decision signal in %q and on_missing_decision is fail",
						step.Name, cfg.DecisionField))
			}
			// Default: re-ask the reviewer.
			reviewerStepID := ""
			for _, dep := range step.DependsOn {
				reviewerStepID = dep
				break
			}
			if reviewerStepID == "" {
				return r.failStep(ctx, tx, tenantID, run, sr, runs,
					fmt.Errorf("loop_decision step %q: no upstream step to re-ask", step.Name))
			}

			// Count REAL re-asks — step runs created by reaskDecisionStep (StepName "Reviewer (re-ask)").
			reaskList, err := listStepRunsByStepID(ctx, tx, tenantID, run.ID, reviewerStepID)
			if err != nil {
				return r.failStep(ctx, tx, tenantID, run, sr, runs,
					fmt.Errorf("loop_decision step %q: list reviewer runs: %w", step.Name, err))
			}
			reaskCount := countReaskRuns(reaskList)
			if reaskCount >= cfg.MaxReask {
				// Re-ask exhausted — fail the loop node even though the step run succeeded, because the reviewer
				// never provided a decision.
				return r.failStep(ctx, tx, tenantID, run, sr, runs,
					fmt.Errorf("loop_decision step %q: reviewer did not provide decision signal after %d attempts"+
						" (set config.on_missing_decision to %q if this step has no verdict to give)",
						step.Name, cfg.MaxReask, MissingDecisionSuccess))
			}

			r.log.Info("loop_decision: re-asking reviewer",
				"run", run.ID, "step", step.ID, "reviewer_step", reviewerStepID, "attempt", reaskCount+1)
			if _, err := r.reaskDecisionStep(ctx, tx, tenantID, run, step, sr, runs, reviewerStepID, now); err != nil {
				return fmt.Errorf("loop_decision step %q: re-ask: %w", step.Name, err)
			}

			// Create a new loop decision iteration (supersedes the current one) so downstream steps (e.g. QA
			// Engineer) do NOT start until the re-asked reviewer finishes.
			nextIter := currentLoopIteration(runs, step.ID) + 1
			if err := r.createLoopDecisionIteration(ctx, tx, tenantID, run, sr, step, runs, nextIter, now, `{"loop":"re-ask"}`); err != nil {
				return err
			}
		}

	case domain.StepKindEnd:
		// End: terminal sink, always succeeds when deps satisfied. No dispatch.
		updated, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
			Status:    strPtr(domain.StepRunSucceeded),
			StartedAt: &now,
			EndedAt:   &now,
		})
		if err != nil {
			return fmt.Errorf("mark end step succeeded: %w", err)
		}
		runs[step.ID] = updated
		if err := r.enqueueStepEvent(ctx, tx, domain.WorkflowEventStepSucceeded, run, updated); err != nil {
			return fmt.Errorf("enqueue end step_succeeded: %w", err)
		}

	case domain.StepKindParallel:
		// Fan-out: mark succeeded; downstream steps that depend on this one become ready on the next pass
		// (their deps are now satisfied).
		updated, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
			Status:    strPtr(domain.StepRunSucceeded),
			StartedAt: &now,
			EndedAt:   &now,
		})
		if err != nil {
			return fmt.Errorf("mark parallel step succeeded: %w", err)
		}
		runs[step.ID] = updated
		if err := r.enqueueStepEvent(ctx, tx, domain.WorkflowEventStepSucceeded, run, updated); err != nil {
			return fmt.Errorf("enqueue parallel step_succeeded: %w", err)
		}

	case domain.StepKindApproval:
		cfg := parseApprovalConfig(step.Config)

		// Capture upstream context from preceding steps for the reviewer.
		upstreamWorker := ""
		upstreamSummary := ""
		var upstreamFiles []string
		ac := ""
		upstreamWorkItemID := ""
		for _, depID := range step.DependsOn {
			us, ok := runs[depID]
			if !ok || us.Status != domain.StepRunSucceeded {
				continue
			}
			var upResult struct {
				WorkItemID string `json:"_work_item_id"`
			}
			if err := json.Unmarshal(us.Result, &upResult); err != nil || upResult.WorkItemID == "" {
				continue
			}
			upstreamWorkItemID = upResult.WorkItemID
			wi, err := db.GetWorkItem(ctx, tx, tenantID, upResult.WorkItemID)
			if err != nil {
				continue
			}
			ac = wi.AcceptanceCriteria
			if len(wi.Results) > 0 {
				var wiResult map[string]any
				if json.Unmarshal(wi.Results, &wiResult) == nil {
					if v, ok := wiResult["_summary"].(string); ok {
						upstreamSummary = v
					}
					if files, ok := wiResult["_touched_files"].([]any); ok {
						for _, f := range files {
							if s, ok := f.(string); ok {
								upstreamFiles = append(upstreamFiles, s)
							}
						}
					}
				}
			}
			if exec, err := db.GetLatestExecutionForTask(ctx, tx, tenantID, upResult.WorkItemID); err == nil && exec.WorkerID != "" {
				if wrk, err := db.GetWorker(ctx, tx, tenantID, exec.WorkerID); err == nil {
					upstreamWorker = wrk.Name
				}
				if upstreamWorker == "" {
					upstreamWorker = exec.WorkerID
				}
			}
		}

		if cfg.Reviewer == "worker" {
			// Worker-backed approval: dispatch like a task step to the approver worker.
			workerRefStr := step.Ref
			if workerRefStr == "" {
				workerRefStr = cfg.WorkerRef
			}
			if workerRefStr == "" {
				// Missing worker ref is a workflow-definition error on THIS step — fail it (clear reason, run
				// terminalizes) rather than erroring out and rolling back the whole pass (which would leave the
				// run.
				return r.failStep(ctx, tx, tenantID, run, sr, runs,
					fmt.Errorf("worker-backed approval step %s has no worker ref", step.ID))
			}
			// The approval execution runs against the run's shared work item (the ticket) — NO per-step
			// approval work item is ever created; the step run IS the approval record.
			upstream := resolveApprovalWorkItems(sr, step, allSteps, run.WorkItemID)
			if len(upstream) == 0 {
				return r.failStep(ctx, tx, tenantID, run, sr, runs,
					fmt.Errorf("worker-backed approval step %q has no upstream work_item", step.Name))
			}
			workerVer, err := db.GetLatestWorkerVersion(ctx, tx, tenantID, workerRefStr, true)
			if err != nil {
				// The approver worker has no usable version (deleted, no published version, or an index/catalog
				// anomaly hid it).
				return r.failStep(ctx, tx, tenantID, run, sr, runs,
					fmt.Errorf("load approver worker version for %s: %w", workerRefStr, err))
			}
			var primaryWID string
			var composite string
			var promptFP string
			var agentPromptLen int
			for _, wid := range upstream {
				wi, err := db.GetWorkItem(ctx, tx, tenantID, wid)
				if err != nil {
					if err == db.ErrNotFound {
						return r.failStep(ctx, tx, tenantID, run, sr, runs,
							fmt.Errorf("work item %s not found", wid))
					}
					return fmt.Errorf("load work item: %w", err)
				}
				wi.WorkflowStepID = sr.StepID
				var fp string
				composite, fp, err = r.buildCompositePrompt(ctx, tx, tenantID, wi, workerVer, allSteps, runs)
				if err != nil {
					return fmt.Errorf("build composite prompt for approver: %w", err)
				}
				if fp != "" {
					promptFP = fp
				}
				primaryWID = wid
				agentPromptLen = len(r.agentPromptFor(ctx, tx, tenantID, wi, workerVer))
			}
			// Record the ticket + composite prompt + the approver worker pin on the STEP RUN (exactly the
			// TASK-step shape).
			stepResult := buildApprovalStepResult(primaryWID, composite, promptFP, workerRefStr, workerVer.Version,
				upstreamWorker, upstreamSummary, upstreamFiles, ac, sr.Result,
				agentPromptLen)
			// Clear any stale worker_execution_id (a recovering step re-dispatched here still references its
			// FAILED execution): pollTaskStep would otherwise see the old failed execution and re-trigger.
			updated, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
				Status:            strPtr(domain.StepRunRunning),
				Result:            &stepResult,
				WorkerExecutionID: strPtr(""),
				StartedAt:         &now,
			})
			if err != nil {
				return fmt.Errorf("mark approval step running: %w", err)
			}
			runs[step.ID] = updated
			if err := r.enqueueStepEvent(ctx, tx, domain.WorkflowEventStepStarted, run, updated); err != nil {
				return fmt.Errorf("enqueue approval step_started: %w", err)
			}
			if dispatchedSteps != nil {
				*dispatchedSteps = append(*dispatchedSteps, dispatchReq{taskID: primaryWID, stepRunID: sr.ID})
			}
			r.log.Info("workflow approver dispatched",
				"run", run.ID, "step", step.ID,
				"work_item", primaryWID, "worker", workerRefStr)
		} else {
			// Human approval: set to approval_pending and wait for
			// human review via the ApproveStep RPC.
			resultPayload, _ := json.Marshal(map[string]any{
				"_work_item_id": upstreamWorkItemID,
				"_review_context": map[string]any{
					"_upstream_worker":  upstreamWorker,
					"_upstream_summary": upstreamSummary,
					"_upstream_files":   upstreamFiles,
					"_ac":               ac,
				},
				"_decision": "pending",
			})
			updated, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
				Status:    strPtr(domain.StepRunApprovalPending),
				StartedAt: &now,
				Result:    &resultPayload,
			})
			if err != nil {
				return fmt.Errorf("mark approval step pending: %w", err)
			}
			runs[step.ID] = updated
			if err := r.enqueueStepEvent(ctx, tx, domain.WorkflowEventStepApproval, run, updated); err != nil {
				return fmt.Errorf("enqueue step_approval: %w", err)
			}
			r.writeApprovalInitFiles(ctx, tx, tenantID, run, updated, upstreamWorker, upstreamSummary, upstreamFiles)
		}

	default:
		// Unknown kind → fail the step rather than stall the run.
		updated, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
			Status:  strPtr(domain.StepRunFailed),
			EndedAt: &now,
		})
		if err != nil {
			return fmt.Errorf("mark unknown step failed: %w", err)
		}
		runs[step.ID] = updated
		if err := r.enqueueStepEvent(ctx, tx, domain.WorkflowEventStepFailed, run, updated); err != nil {
			return fmt.Errorf("enqueue step_failed: %w", err)
		}
	}
	return nil
}

// succeedStep marks a passive step (project, work_item) as succeeded and
// emits the success event. Used by dispatchStep for non-dispatching kinds.
func (r *WorkflowReconciler) succeedStep(ctx context.Context, tx pgx.Tx, tenantID string, run db.WorkflowRunRow, sr db.WorkflowStepRunRow, runs map[string]db.WorkflowStepRunRow, now time.Time, _ string) error {
	updated, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
		Status:    strPtr(domain.StepRunSucceeded),
		StartedAt: &now,
		EndedAt:   &now,
	})
	if err != nil {
		return fmt.Errorf("mark step succeeded: %w", err)
	}
	runs[sr.StepID] = updated
	return r.enqueueStepEvent(ctx, tx, domain.WorkflowEventStepSucceeded, run, updated)
}

// failStep marks a step as failed with the given reason and emits the failed event.
func (r *WorkflowReconciler) failStep(ctx context.Context, tx pgx.Tx, tenantID string, run db.WorkflowRunRow, sr db.WorkflowStepRunRow, runs map[string]db.WorkflowStepRunRow, reason error) error {
	now := time.Now().UTC()
	msg := reason.Error()
	result, _ := json.Marshal(map[string]string{"error": msg})
	updated, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
		Status:  strPtr(domain.StepRunFailed),
		Result:  &result,
		EndedAt: &now,
	})
	if err != nil {
		return fmt.Errorf("mark step failed: %w", err)
	}
	runs[sr.StepID] = updated
	r.log.Info("step failed", "run", run.ID, "step", sr.StepID, "reason", msg)
	if err := r.enqueueStepEvent(ctx, tx, domain.WorkflowEventStepFailed, run, updated); err != nil {
		return fmt.Errorf("enqueue step_failed: %w", err)
	}
	return nil
}

// buildCompositePrompt assembles the prompt text the worker should see
// when this work item is dispatched (PR B — context propagation). It
// has the following sections:
//
//  0. # Worker — the worker's identity (Role / Skills / Behavior /
//     AGENTS.md). Prepended so the visible prompt the operator
//     inspects in the execution detail page is the full context
//     the model actually sees. The runtime delivers the same
//     content as the system prompt via OPENCODE_CONFIG_CONTENT
//     (see the opencode adapter) so the worker identity lands on
//     every conversation turn, not just the first.
//  1. # Task — the work item itself: title, description, acceptance
//     criteria. This is THE task; everything else is context.
//  2. # Project context — the project directory (working dir) + the
//     project's `context_files`, rendered by the shared
//     internal/contextfiles renderer. A context path may be a file
//     (inlined, capped) or a directory (expanded into a bounded
//     listing with explicit "read every file, do NOT open the
//     directory as a file" instructions).
//  3. # Work item context — the work item's own `context_files`,
//     rendered exactly like the project's (same renderer, resolved
//     against the same project_dir).
//  4. # Instructions — the worker's contract: emit the
//     ORCHICON WORKER SUMMARY marker at the end of the response so
//     the next stage can read it as upstream context. Also carries
//     the workflow-aware role context, iteration/git-branch notes,
//     the per-step recovery summary, the `.orchicon/` files to read,
//     and the execution-history timeline.
//
// (The ancestor-chain and recovery sections historically described
// below are now part of the workflow timeline rendered inside the
// instructions block.)
//
// The composite is the opencode adapter's "message" (passed via the
// manifest Goal). Sections in order:
//
//  0. Role — the worker's purpose
//  1. Task — the work item
//  2. Project context — directory and file contents
//  3. Work item context — the item's own context files/directories
//  4. Instructions — read .orchicon/ for previous step results,
//     then output format including decision prefix
func (r *WorkflowReconciler) buildCompositePrompt(ctx context.Context, tx pgx.Tx, tenantID string, wi db.WorkItemRow, worker db.WorkerVersionRow, allSteps []workflow.StepWire, runs map[string]db.WorkflowStepRunRow) (string, string, error) {
	var sb strings.Builder
	// contextFP is the sha256 over the project + work-item context-file stamps (ADR-0009 D5): unchanged
	// files ⇒ unchanged bytes ⇒ the rendered section is reused verbatim from the prefix cache.
	contextFP := "none"
	// Stable prompt prefix first: shared identity + safety rules + efficiency directives + runtime
	// environment, built ONLY from shared constants so it is byte-identical across all workers and
	// steps.
	sb.WriteString(r.agentPromptFor(ctx, tx, tenantID, wi, worker))

	// 1. Task.
	sb.WriteString("# Task\n\n")
	fmt.Fprintf(&sb, "Original work item: \"%s\"\n\n", strings.TrimSpace(wi.Title))
	if d := strings.TrimSpace(wi.Description); d != "" {
		fmt.Fprintf(&sb, "Description:\n%s\n\n", d)
	}
	if ac := strings.TrimSpace(wi.AcceptanceCriteria); ac != "" {
		fmt.Fprintf(&sb, "Acceptance criteria:\n%s\n\n", ac)
	}

	// 2. Project context — directory + context files (files AND
	//    directories; directories are expanded into a bounded listing
	//    with explicit read instructions — internal/contextfiles).
	if wi.ProjectID != "" {
		var p db.ProjectRow
		if err := tx.QueryRow(ctx,
			`SELECT project_dir, context_files, skill_files FROM projects WHERE id = $1 AND tenant_id = $2`,
			wi.ProjectID, tenantID,
		).Scan(&p.ProjectDir, &p.ContextFiles, &p.SkillFiles); err == nil {
			var sb2 strings.Builder
			if p.ProjectDir != "" {
				fmt.Fprintf(&sb2, "Working directory: `%s`\n\n", p.ProjectDir)
			}
			var files []string
			_ = json.Unmarshal(p.ContextFiles, &files)
			section, fp := r.renderContextSectionCached(tenantID, wi.ProjectID, "# Project context", files, p.ProjectDir)
			sb2.WriteString(section)
			if fp != "" {
				contextFP = fp
			}
			if sb2.Len() > 0 {
				sb.WriteString(sb2.String())
			}

			// 2b. SKILLS — the SELECTABLE skill artifacts, rendered by the SAME
			//     renderer as every other context section (contextfiles.RenderManifest),
			//     over the UNION of the project's skill_files and this VERSION's
			//     skill_files. This is the worker half of the one shared platform render
			//     path; the Ask half calls the same renderer in
			//     internal/askorchicon/chat.go. NO adapter knows about skills: the
			//     rendered section is consumed verbatim as part of the composite prompt
			//     (ADR-0009), which is what makes the feature adapter-agnostic.
			//
			//     DISTINCT from worker.Skills, the free-text prompt prose rendered as
			//     `## Skills` above — this section is real on-disk paths.
			//
			//     CONTAINMENT IS ENFORCED HERE, at the render boundary, because a worker
			//     version is project-agnostic (structural Validate happened at save
			//     time); the union is filtered against THIS project's dir so a skill
			//     outside it can never render a dead "could not read" note. Sorted +
			//     deduped by contextfiles.Union, so two renders of one selection are
			//     byte-identical (the section sits inside the cached static prefix).
			var versionSkillFiles []string
			_ = json.Unmarshal(worker.SkillFiles, &versionSkillFiles)
			var projectSkillFiles []string
			_ = json.Unmarshal(p.SkillFiles, &projectSkillFiles)
			skillUnion := contextfiles.Union(projectSkillFiles, versionSkillFiles)
			if len(skillUnion) > 0 {
				if err := contextfiles.ValidateWithin(skillUnion, p.ProjectDir); err != nil {
					if r.log != nil {
						r.log.Info("dropping out-of-project skill files from the composite prompt",
							"project", wi.ProjectID, "error", err.Error())
					}
				} else {
					// ManifestPolicySkill: a worker must READ its instructions, not be handed a table of contents.
					skillSection, skillFP := r.renderSectionCachedWithPolicy(tenantID, wi.ProjectID, "# Skills", skillUnion, p.ProjectDir, contextfiles.ManifestPolicySkill)
					if skillSection != "" {
						sb.WriteString(skillSection)
					}
					if skillFP != "" {
						contextFP = contextFP + ".skills:" + skillFP
					}
				}
			}
		}
	}

	// 3. Work item context — the item's own context_files (files AND
	//    directories), rendered exactly like the project's, resolved
	//    against the same project_dir (backward-compat rule).
	if len(wi.ContextFiles) > 0 {
		var files []string
		_ = json.Unmarshal(wi.ContextFiles, &files)
		projectDir := ""
		if wi.ProjectID != "" {
			var pd string
			_ = tx.QueryRow(ctx,
				`SELECT project_dir FROM projects WHERE id = $1 AND tenant_id = $2`,
				wi.ProjectID, tenantID,
			).Scan(&pd)
			projectDir = pd
		}
		section, fp := r.renderContextSectionCached(tenantID, wi.ProjectID, "# Work item context", files, projectDir)
		if section != "" {
			sb.WriteString(section)
		}
		if fp != "" {
			contextFP = contextFP + "." + fp
		}
	}

	// 3b. Work item attachments — Channel B visibility (file-materialized + manifest line per ADR-4).
	if tx != nil {
		if atts, err := db.ListWorkItemAttachments(ctx, tx, tenantID, wi.ID); err == nil && len(atts) > 0 {
			sb.WriteString(db.WorkItemAttachmentsManifest(atts, wi.ID))
		}
	}

	// 4. Instructions.
	sb.WriteString("# Instructions\n\n")

	// The workflow decision comes from exactly ONE signal: the word after ORCHICON WORKER SUMMARY:.
	sb.WriteString("The workflow routes on exactly one signal: the word after `ORCHICON WORKER SUMMARY:` — `success` or `failure`. There is no `_issues:` failure channel. If work genuinely cannot be accepted, end with `ORCHICON WORKER SUMMARY: failure` and say what needs fixing in the summary text. Non-blocking observations belong in the summary text only and never affect the routing.\n\n")

	// Facts ledger: every step appends the facts it established so later steps inherit them instead of
	// re-deriving them (the observed over-verification and re-investigation pattern in review/QA.
	sb.WriteString("**Facts ledger.** When you establish a fact, root cause, environment gotcha, or decision that later steps should not have to re-derive, record it as a `FACTS LEARNED:` line inside your final summary — one fact per line, each starting with `FACTS LEARNED:`. Example: `FACTS LEARNED: the runtime container's supervisor runs the pre-feature daemon self-copy (old binary), so the sandbox plane does not auto-boot until the daemon rebuilds.` Do not record obvious or transient details. Never re-verify a fact already recorded by an earlier step — if you believe one is wrong, append a correcting `FACTS LEARNED:` line rather than silently re-deriving it.\n\n")

	// Workflow-aware role context: tell the worker where they fit in the overall workflow so they don't
	// perform work meant for other steps.
	type stepMeta struct {
		idx  int
		name string
		id   string
	}
	var allMeta []stepMeta    // all steps, used to build the dependency graph
	var activeMeta []stepMeta // only task + approval
	myPos := -1
	myName := ""
	stepIdx := make(map[string]int)
	for i, s := range allSteps {
		stepIdx[s.ID] = i
		allMeta = append(allMeta, stepMeta{i, s.Name, s.ID})
		if s.Kind == "task" || s.Kind == "approval" {
			activeMeta = append(activeMeta, stepMeta{0, s.Name, s.ID})
		}
	}
	// Kahn's algorithm for topological order over all steps.
	inDeg := make([]int, len(allSteps))
	adj := make([][]int, len(allSteps))
	for _, s := range allSteps {
		for _, dep := range s.DependsOn {
			if dIdx, ok := stepIdx[dep]; ok {
				adj[dIdx] = append(adj[dIdx], stepIdx[s.ID])
				inDeg[stepIdx[s.ID]]++
			}
		}
	}
	var topo []int
	q := make([]int, 0, len(allSteps))
	for i, d := range inDeg {
		if d == 0 {
			q = append(q, i)
		}
	}
	for len(q) > 0 {
		u := q[0]
		q = q[1:]
		topo = append(topo, u)
		for _, v := range adj[u] {
			inDeg[v]--
			if inDeg[v] == 0 {
				q = append(q, v)
			}
		}
	}
	// Build a position map: index in topological sort → step ID.
	topoPos := make(map[string]int)
	for i, idx := range topo {
		topoPos[allSteps[idx].ID] = i
	}
	// Sort active steps by their topological position in the full DAG.
	sort.SliceStable(activeMeta, func(i, j int) bool {
		return topoPos[activeMeta[i].id] < topoPos[activeMeta[j].id]
	})
	for i, s := range activeMeta {
		activeMeta[i].idx = i
		if s.id == wi.WorkflowStepID {
			myPos = i
			myName = s.name
		}
	}
	if myPos >= 0 && len(activeMeta) > 0 {
		// Render the live step lineup from the workflow's ACTUAL steps (names and topological order,
		// worker-facing kinds only).
		nameCount := make(map[string]int)
		for _, s := range activeMeta {
			nameCount[s.name]++
		}
		var parts []string
		for i, s := range activeMeta {
			label := s.name
			if nameCount[s.name] > 1 {
				label = fmt.Sprintf("%s (%s)", s.name, s.id)
			}
			marker := ""
			if i == myPos {
				marker = " — you"
			}
			parts = append(parts, fmt.Sprintf("%d. %s%s", s.idx+1, label, marker))
		}
		fmt.Fprintf(&sb, "This workflow has %d steps and you are step %d of %d — %s.\n", len(activeMeta), myPos+1, len(activeMeta), myName)
		sb.WriteString("The steps, in order:\n")
		for _, p := range parts {
			sb.WriteString("- " + p + "\n")
		}
		sb.WriteString("Follow your role for this step. Work that belongs to another step in this list is that step's job, not yours.\n\n")
	}

	// Iteration context: tell the worker if this is a re-do (loop-back).
	if currentRun, ok := runs[wi.WorkflowStepID]; ok && currentRun.Iteration > 0 {
		fmt.Fprintf(&sb, "This is iteration %d of this step. You may have done this work before — review your previous output and the feedback from downstream steps before repeating yourself.\n\n", currentRun.Iteration)
	}

	// Git/branch guidance: keyed on the run's worktree_status AND effective git strategy so a non-repo
	// (in-place) run is never told to work on a branch and a `none` (ephemeral) run is told.
	worktreeStatus, worktreeBranch, projectDir, gitStrategy := "", "", "", ""
	runWorkflowID, runProjectID := "", ""
	if wi.WorkflowID != nil {
		runWorkflowID = *wi.WorkflowID
	}
	if wi.ProjectID != "" {
		runProjectID = wi.ProjectID
	}
	if wi.WorkflowRunID != "" {
		_ = tx.QueryRow(ctx,
			`SELECT worktree_status, worktree_branch, COALESCE(workflow_id,''), COALESCE(project_id,'') FROM workflow_runs WHERE id = $1 AND tenant_id = $2`,
			wi.WorkflowRunID, tenantID,
		).Scan(&worktreeStatus, &worktreeBranch, &runWorkflowID, &runProjectID)
	}
	if wi.ProjectID != "" {
		_ = tx.QueryRow(ctx,
			`SELECT project_dir FROM projects WHERE id = $1 AND tenant_id = $2`,
			wi.ProjectID, tenantID,
		).Scan(&projectDir)
	}
	gitStrategy = db.EffectiveGitStrategy(ctx, tx, tenantID, runWorkflowID, runProjectID)
	sb.WriteString(db.GitGuidanceBlock(worktreeStatus, worktreeBranch, projectDir, gitStrategy))

	// Recovery context: if THIS step is being re-dispatched after a recovery (its step run is
	// recovering and carries a recovery seed).
	if currentRun, ok := runs[wi.WorkflowStepID]; ok {
		if seed := recoverySeedFor(currentRun.Result, nil, worker.WorkerID); seed != nil {
			sb.WriteString(recoveryFileReferenceBlock(seed))
		} else {
			var recMeta struct {
				RecoverySummary string `json:"_recovery_summary"`
			}
			_ = json.Unmarshal(currentRun.Result, &recMeta)
			if recMeta.RecoverySummary != "" {
				fmt.Fprintf(&sb, "## Recovery\n\nA previous execution of this step failed and was recovered. Recovery summary:\n%s\n\n", recMeta.RecoverySummary)
			}
		}
	}

	// Only instruct the worker to read .orchicon/ files when prior
	// steps have completed. On the first step there's nothing to read.
	hasPriorSteps := false
	for _, sr := range runs {
		if sr.Status == domain.StepRunSucceeded || sr.Status == domain.StepRunFailed {
			hasPriorSteps = true
			break
		}
	}
	// INDEPENDENT REVIEW: withhold the implementer's own account.
	reviewIndependently := isVerifierStep(wi.WorkflowStepID, "", worker.Role)
	if hasPriorSteps {
		fmt.Fprintf(&sb, "**Before you begin:** read this run's `.orchicon/` files (%s) to see what earlier steps produced and what they require of you. They are the authoritative feedback — do not start until you have read them:\n\n", orchiconLocationNote(projectDir))
		fmt.Fprintf(&sb, "- `.orchicon/%s/facts_learned` — a running ledger of facts, root causes, environment gotchas, and decisions established by earlier steps. **A fact already recorded here is established — do not re-verify or re-derive it.** If you establish something new, append a `FACTS LEARNED:` line (see below)\n", wi.WorkflowRunID)
		fmt.Fprintf(&sb, "- `.orchicon/%s/status` — `success` or `failure` from the previous step\n", wi.WorkflowRunID)
		if reviewIndependently {
			sb.WriteString("- `.orchicon/<run>/summary` — **withheld from you on purpose**: it is the previous worker's own account of what it did, which is the claim you are testing. Do not seek it out.\n")
		} else {
			fmt.Fprintf(&sb, "- `.orchicon/%s/summary` — what the previous worker did\n", wi.WorkflowRunID)
		}
		fmt.Fprintf(&sb, "- `.orchicon/%s/issues` — issues found by the previous reviewer (read these; they are blocking unless stated otherwise)\n", wi.WorkflowRunID)
		fmt.Fprintf(&sb, "- `.orchicon/%s/touched_files` — files the previous worker wrote or edited (read the ones relevant to your task)\n", wi.WorkflowRunID)
		fmt.Fprintf(&sb, "- `.orchicon/%s/worker` — which worker produced the previous results\n", wi.WorkflowRunID)
		fmt.Fprintf(&sb, "- `.orchicon/%s/attachments/` — files/screenshots the human attached to an approval decision (read them!)\n\n", wi.WorkflowRunID)
	}
	if reviewIndependently {
		sb.WriteString(verifierIsolationBlock)
	}
	sb.WriteString("Complete the work this step requires of you and bring it to a state that passes review before handing it off. Do not push your step's work downstream incomplete, and do not take on work that belongs to another step in this workflow — each step has its own role and deliverable, and doing another step's job for it breaks the division of labor. When you have finished, end your response with the literal line `ORCHICON WORKER SUMMARY:` followed by one word — either `success` or `failure` — and a short paragraph summarizing what you did.\n\n")
	sb.WriteString("Format:\n")
	sb.WriteString("```\nORCHICON WORKER SUMMARY: success — Implemented the feature.\n```\n")
	sb.WriteString("or\n")
	sb.WriteString("```\nORCHICON WORKER SUMMARY: failure — Found 3 bugs in the implementation.\n```\n\n")
	sb.WriteString("The first word (`success` or `failure`) is used to route the workflow. The text after `—` is passed to the next stage as the summary of your work.\n\n")
	sb.WriteString("**Important:** The workflow routes on the single word after `ORCHICON WORKER SUMMARY:` — `success` or `failure`. There is no `_issues:` failure channel: any `_issues:` block in your response is informational only and never changes the routing. If you find blocking problems, end with `failure` and explain them in the summary text. If you have only minor suggestions, keep the routing `success` and mention them in your summary text.\n\n")

	// Summary brevity: the summary is re-embedded into every later step's prompt (Execution history)
	// and persisted to .orchicon/<run>/summary, so a bloated summary taxes all downstream steps.
	sb.WriteString("Keep your final summary concise — under ~500 tokens (roughly 2000 characters). It is re-embedded into every later step's prompt and persisted to `.orchicon/<run>/summary`, so verbosity taxes all downstream steps. `FACTS LEARNED:` lines and blocking feedback are exempt from the cap.\n\n")

	// Prior execution timeline — DELTA handoff.
	sb.WriteString("## Execution history\n\n")
	// Build a step-ID→name lookup from allSteps.
	stepNameByID := make(map[string]string)
	for _, s := range allSteps {
		stepNameByID[s.ID] = s.Name
	}
	// Direct upstream: the steps THIS step depends on (DependsOn).
	directUpstream := map[string]bool{}
	for _, s := range allSteps {
		if s.ID == wi.WorkflowStepID {
			for _, d := range s.DependsOn {
				directUpstream[d] = true
			}
		}
	}

	var history []histEntryType
	seen := make(map[string]bool)
	for stepID, sr := range runs {
		sr := sr
		if _, ok := stepNameByID[stepID]; !ok {
			continue
		}
		if sr.Status != domain.StepRunSucceeded && sr.Status != domain.StepRunFailed {
			continue
		}
		var rData struct {
			Summary     string `json:"_summary"`
			Issues      string `json:"_issues"`
			Reason      string `json:"_reason"`
			Attachments []struct {
				Filename string `json:"filename"`
				Path     string `json:"path"`
			} `json:"_attachments"`
		}
		json.Unmarshal(sr.Result, &rData)
		iterLabel := "first"
		if sr.Iteration > 0 {
			iterLabel = fmt.Sprintf("iteration %d", sr.Iteration)
		}
		entry := histEntryType{
			stepID:    stepID,
			stepName:  stepNameByID[stepID],
			status:    sr.Status,
			summary:   rData.Summary,
			issues:    rData.Issues,
			reason:    rData.Reason,
			iteration: iterLabel,
			direct:    directUpstream[stepID],
		}
		for _, a := range rData.Attachments {
			if a.Filename != "" {
				entry.attachments = append(entry.attachments, a.Filename)
			}
		}
		if !seen[sr.ID] {
			history = append(history, entry)
			seen[sr.ID] = true
		}
	}
	// Sort by topological order.
	sort.SliceStable(history, func(i, j int) bool {
		var pi, pj int
		for _, s := range allSteps {
			if s.Name == history[i].stepName {
				pi = topoPos[s.ID]
			}
			if s.Name == history[j].stepName {
				pj = topoPos[s.ID]
			}
		}
		return pi < pj
	})
	if len(history) == 0 {
		sb.WriteString("No steps have completed yet. You are the first.\n\n")
	} else {
		// Full detail for direct upstreams.
		sb.WriteString("**Directly above you** (these steps' output is the input you must work with):\n\n")
		if !anyDirectCompleted(history) {
			// No direct upstream completed yet — e.g. this is a fan-in / early step whose DependsOn steps
			// haven't produced a full run, or the first step of a linear chain.
			sb.WriteString("(No directly-above step has completed yet — showing all completed steps below.)\n\n")
			for i := range history {
				history[i].direct = true
			}
		}
		full := 0
		for _, h := range history {
			if !h.direct {
				continue
			}
			full++
			statusSym := "✓"
			if h.status == domain.StepRunFailed {
				statusSym = "✗"
			}
			fmt.Fprintf(&sb, "- **%s** %s [%s] %s\n", h.stepName, statusSym, h.iteration, map[bool]string{true: "succeeded", false: "failed"}[h.status == domain.StepRunSucceeded])
			if h.summary != "" {
				fmt.Fprintf(&sb, "  - Summary: %s\n", h.summary)
			}
			if h.issues != "" {
				fmt.Fprintf(&sb, "  - Issues: %s\n", h.issues)
			}
			if h.reason != "" {
				fmt.Fprintf(&sb, "  - Human review: %s\n", h.reason)
			}
			if len(h.attachments) > 0 {
				fmt.Fprintf(&sb, "  - Human attachments (read them from the project dir): %s\n", strings.Join(h.attachments, ", "))
			}
		}
		// Compact index for all other completed steps — read on disk.
		sb.WriteString("\n**Earlier steps** (index — read the file for any you need):\n")
		for _, h := range history {
			if h.direct {
				continue
			}
			statusSym := "✓"
			if h.status == domain.StepRunFailed {
				statusSym = "✗"
			}
			fmt.Fprintf(&sb, "- **%s** %s [%s] %s", h.stepName, statusSym, h.iteration, map[bool]string{true: "succeeded", false: "failed"}[h.status == domain.StepRunSucceeded])
			if h.summary != "" {
				if len(h.summary) > 180 {
					fmt.Fprintf(&sb, " — %s…", h.summary[:180])
				} else {
					fmt.Fprintf(&sb, " — %s", h.summary)
				}
			}
			fmt.Fprintf(&sb, " (full detail: `.orchicon/%s/steps/%s.md`)\n", wi.WorkflowRunID, h.stepID)
		}
		sb.WriteString("\nIf you need the full detail of an earlier step listed above, read its file from the project directory; each is under `.orchicon/<run>/steps/`. Never reconstruct from memory — read the file.\n\n")
	}
	sb.WriteString("If you produce an output file, use the `write` tool (not `bash` with a heredoc). The `write` tool saves the file and orchicon captures it as an inline artifact.\n")

	return sb.String(), contextFP, nil
}

// runtimeEnvironmentBlock is kept as a thin alias for the shared db.RuntimeEnvironmentBlock so
// scheduler tests and callers stay terse.
func orchiconLocationNote(projectDir string) string {
	if projectDir != "" {
		return fmt.Sprintf("the `.orchicon/` directory lives at the **project root** (%s/.orchicon/) — **not inside the worktree**", projectDir)
	}
	return "the `.orchicon/` directory lives at the **project root** — **not inside the worktree**"
}

// histEntryType is one completed step-run in the delta execution-history renderer.
type histEntryType struct {
	stepID, stepName, status, summary, issues, reason, iteration string
	attachments                                                  []string
	direct                                                       bool
}

// anyDirectCompleted reports whether the history contains a completed step that is a direct
// upstream (DependsOn) of the dispatching step.
func anyDirectCompleted(history []histEntryType) bool {
	for _, h := range history {
		if h.direct {
			return true
		}
	}
	return false
}

// runExecutionMode returns the run's EFFECTIVE execution_mode (runtime|local) for arm routing,
// resolved by db.EffectiveExecutionMode.
func runExecutionMode(ctx context.Context, tx pgx.Tx, tenantID string, run db.WorkflowRunRow) string {
	var itemMode *string
	if strings.TrimSpace(run.WorkItemID) != "" {
		if w, err := db.GetWorkItem(ctx, tx, tenantID, run.WorkItemID); err == nil {
			itemMode = w.ExecutionMode
		}
	}
	return db.EffectiveExecutionMode(ctx, tx, tenantID, itemMode, run.ProjectID)
}

// agentPromptFor is the RUN-LEVEL part of a worker's prompt: the stable prefix plus the worker's
// identity block. IT EXISTS SO THE TWO CONSUMERS CANNOT DRIFT.
func (r *WorkflowReconciler) agentPromptFor(ctx context.Context, tx pgx.Tx, tenantID string, wi db.WorkItemRow, worker db.WorkerVersionRow) string {
	mode := projectExecutionModeForPrompt(ctx, tx, tenantID, wi)
	// A LOCAL RUN IS TOLD NOTHING ABOUT AN IMAGE, because it has no container.
	image := runEffectiveImage(ctx, tx, tenantID, wi)
	if mode == db.ExecutionModeLocal {
		image = ""
	}
	return db.AgentSystemPrompt(worker, image, mode)
}

// runEffectiveImage returns the run row's RuntimeImage for workflow-bound items (the armed value —
// the prompt must describe the container the run actually got, not the work-item request).
func runEffectiveImage(ctx context.Context, tx pgx.Tx, tenantID string, wi db.WorkItemRow) string {
	if wi.WorkflowRunID != "" {
		if run, err := db.GetWorkflowRun(ctx, tx, tenantID, wi.WorkflowRunID); err == nil && strings.TrimSpace(run.RuntimeImage) != "" {
			return strings.TrimSpace(run.RuntimeImage)
		}
	}
	if img := strings.TrimSpace(wi.RuntimeImage); img != "" {
		return img
	}
	// The LIVE fallback (the daemon's own base tag), never the placeholder — see
	// imageForRun for why the placeholder is not a resolvable image.
	return db.RuntimeBaseImage()
}

// projectExecutionModeForPrompt returns the work item's EFFECTIVE execution mode for prompt
// branching (the runtime vs local honest block).
func projectExecutionModeForPrompt(ctx context.Context, tx pgx.Tx, tenantID string, wi db.WorkItemRow) string {
	return db.EffectiveExecutionMode(ctx, tx, tenantID, wi.ExecutionMode, wi.ProjectID)
}

func runtimeEnvironmentBlock(image string) string {
	return db.RuntimeEnvironmentBlock(image, "")
}

// walkAncestors walks the parent_id chain from a work item up to the root (epic) and returns the
// ancestors in root-first order.
func walkAncestors(ctx context.Context, tx pgx.Tx, tenantID string, wi db.WorkItemRow) ([]db.WorkItemRow, error) {
	const maxAncestorDepth = 16
	var out []db.WorkItemRow
	cur := wi
	for i := 0; i < maxAncestorDepth; i++ {
		if cur.ParentID == nil || *cur.ParentID == "" {
			break
		}
		parent, err := db.GetWorkItem(ctx, tx, tenantID, *cur.ParentID)
		if err != nil {
			if err == db.ErrNotFound {
				break
			}
			return nil, err
		}
		out = append(out, parent)
		cur = parent
	}
	// Reverse so the oldest (epic) comes first.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// upstreamContext renders a compact "Workflow context" section for
// the worker. Each prior step is a short card: decision, summary,
// and list of touched files. No full output dumps. The current step
// is marked so the worker can orient itself.
//
// Per-step rendering (see renderUpstreamStep):
//
//   - TASK: _decision, _summary, _touched_files from the work item's
//     results. No full output dump.
//   - WORK_ITEM: linked work item title + a short description.
//   - PROJECT: project name only.
//   - DECISION / APPROVAL / PARALLEL: status only.
//
// Returns "" when the run has no completed steps yet. Walks the DAG
// by step-id order via allSteps.
func (r *WorkflowReconciler) upstreamContext(ctx context.Context, tx pgx.Tx, tenantID string, wi db.WorkItemRow, allSteps []workflow.StepWire, runs map[string]db.WorkflowStepRunRow) (string, error) {
	// Find the current step (the one being dispatched).
	currentStepID := ""
	if wi.WorkflowStepID != "" {
		currentStepID = wi.WorkflowStepID
	} else {
		// Fallback: scan for the step run that referenced this work item.
		for sid, sr := range runs {
			var parsed struct {
				WorkItemID string `json:"_work_item_id"`
			}
			if json.Unmarshal(sr.Result, &parsed) == nil && parsed.WorkItemID == wi.ID {
				currentStepID = sid
				break
			}
		}
	}

	// Count terminal (succeeded/failed/skipped) prior steps to know whether to render the section at
	// all.
	hasAnyTerminal := false
	for _, s := range allSteps {
		sr, ok := runs[s.ID]
		if !ok {
			continue
		}
		if sr.Status == domain.StepRunSucceeded || sr.Status == domain.StepRunFailed || sr.Status == domain.StepRunSkipped {
			hasAnyTerminal = true
			break
		}
	}
	if !hasAnyTerminal {
		return "", nil
	}

	var sb strings.Builder
	sb.WriteString("# Workflow context\n\n")
	sb.WriteString("This is a chronological view of every step in the workflow run, in DAG order. The current step is marked `→ you are here`. Use prior step results as input to your work and produce the output the next stage will read.\n\n")

	stage := 0
	for _, s := range allSteps {
		sr, ok := runs[s.ID]
		if !ok {
			// Step hasn't been visited yet — skip.
			continue
		}
		// Only show steps that have actually started. A pending
		// step is invisible to the worker.
		if sr.Status == domain.StepRunPending || sr.Status == domain.StepRunReady {
			continue
		}
		stage++
		isCurrent := s.ID == currentStepID
		if err := r.renderUpstreamStep(ctx, tx, tenantID, &sb, stage, s, sr, isCurrent); err != nil {
			return "", err
		}
	}

	// If a current step is known, append a brief "next task" reminder
	// so the worker can see at a glance what is expected of them.
	sb.WriteString("---\n\n")
	if currentStepID != "" {
		for _, s := range allSteps {
			if s.ID != currentStepID {
				continue
			}
			sb.WriteString("**→ Your task:** ")
			fmt.Fprintf(&sb, "You are executing **%s**", strings.TrimSpace(s.Name))
			if s.Kind == domain.StepKindTask && s.Ref != "" {
				fmt.Fprintf(&sb, " as `%s`", s.Ref)
			}
			sb.WriteString(". Complete the work in the *Task* section above, then end with the marker.\n\n")
			break
		}
	}

	return sb.String(), nil
}

// renderUpstreamStep writes one step's entry to the workflow context timeline.
func (r *WorkflowReconciler) renderUpstreamStep(ctx context.Context, tx pgx.Tx, tenantID string, sb *strings.Builder, stage int, s workflow.StepWire, sr db.WorkflowStepRunRow, isCurrent bool) error {
	marker := ""
	if isCurrent {
		marker = "  → you are here"
	}
	fmt.Fprintf(sb, "## Stage %d — %s (%s)%s\n", stage, strings.TrimSpace(s.Name), stepKindLabel(s.Kind), marker)

	// Step status. For everything but TASK, this is usually the only
	// information we have; render it inline.
	switch sr.Status {
	case domain.StepRunSucceeded:
		fmt.Fprintf(sb, "Status: succeeded\n")
	case domain.StepRunFailed:
		fmt.Fprintf(sb, "Status: **failed**\n")
	case domain.StepRunSkipped:
		fmt.Fprintf(sb, "Status: skipped\n")
	case domain.StepRunRunning:
		fmt.Fprintf(sb, "Status: running\n")
	case domain.StepRunBlocked:
		fmt.Fprintf(sb, "Status: blocked\n")
	case domain.StepRunApprovalPending:
		fmt.Fprintf(sb, "Status: awaiting approval\n")
	default:
		fmt.Fprintf(sb, "Status: %s\n", sr.Status)
	}

	// Per-kind body.
	switch s.Kind {
	case domain.StepKindTask:
		// The step run's result holds the execution's fields — _decision, _summary, _touched_files,
		// _recovery_summary (written by propagateStepRunResults on completion + the recovery engine on
		// resume).
		var parsed map[string]any
		if len(sr.Result) > 0 {
			_ = json.Unmarshal(sr.Result, &parsed)
		}
		if d, ok := parsed["_decision"].(string); ok && d != "" {
			fmt.Fprintf(sb, "Decision: %s\n", d)
		}
		if summary, ok := parsed["_summary"].(string); ok && summary != "" {
			fmt.Fprintf(sb, "Summary: %s\n", summary)
		}
		if files, ok := parsed["_touched_files"].([]any); ok && len(files) > 0 {
			paths := make([]string, 0, len(files))
			for _, f := range files {
				if s, ok := f.(string); ok {
					paths = append(paths, s)
				}
			}
			if len(paths) > 0 {
				fmt.Fprintf(sb, "Files: `%s`\n", strings.Join(paths, "`, `"))
			}
		}
		if recSummary, ok := parsed["_recovery_summary"].(string); ok && recSummary != "" {
			fmt.Fprintf(sb, "Recovery: %s\n", recSummary)
		}

	case domain.StepKindWorkItem:
		// Passive marker.
		wid := readConfigWorkItemID(s.Config)
		if wid == "" {
			break
		}
		wi, err := db.GetWorkItem(ctx, tx, tenantID, wid)
		if err != nil {
			if err == db.ErrNotFound {
				break
			}
			return fmt.Errorf("load work item for work_item step %s: %w", s.ID, err)
		}
		fmt.Fprintf(sb, "Linked work item: %s (%s)\n", strings.TrimSpace(wi.Title), workItemKindLabel(wi.Kind))
		if d := strings.TrimSpace(wi.Description); d != "" {
			r.writeCappedText(sb, "Description", d, upstreamDescriptionMaxChars)
		}

	case domain.StepKindProject:
		// Passive marker. The project id is in the step config.
		pid := readConfigProjectID(s.Config)
		if pid == "" {
			break
		}
		p, err := db.GetProject(ctx, tx, tenantID, pid)
		if err != nil {
			if err == db.ErrNotFound {
				break
			}
			return fmt.Errorf("load project for project step %s: %w", s.ID, err)
		}
		fmt.Fprintf(sb, "Project: %s\n", strings.TrimSpace(p.Name))
	}

	sb.WriteString("\n")
	return nil
}

// upstreamOutputMaxChars caps the worker's full output inline in the workflow context.
const upstreamOutputMaxChars = 16 * 1024

// upstreamDescriptionMaxChars caps descriptions from passive context markers (WORK_ITEM, PROJECT).
const upstreamDescriptionMaxChars = 1024

// writeCappedText writes a label + body to the buffer, truncating body to maxChars with a clear
// "truncated" marker if it would otherwise blow the budget.
func (r *WorkflowReconciler) writeCappedText(sb *strings.Builder, label, body string, maxChars int) {
	fmt.Fprintf(sb, "\n%s:\n", label)
	if len(body) <= maxChars {
		sb.WriteString(body)
		if !strings.HasSuffix(body, "\n") {
			sb.WriteString("\n")
		}
		return
	}
	// Truncate at a sensible boundary: find the last newline before
	// the cap so we don't slice a word in half.
	cut := maxChars
	if nl := strings.LastIndex(body[:cut], "\n"); nl > cut/2 {
		cut = nl
	}
	sb.WriteString(body[:cut])
	sb.WriteString("\n…[truncated — see the ORCHICON WORKER SUMMARY below for the canonical downstream input]\n")
}

// stepKindLabel returns a human-readable label for a workflow step's
// kind. Used in the workflow-context timeline header.
func stepKindLabel(kind string) string {
	switch kind {
	case domain.StepKindTask:
		return "task"
	case domain.StepKindDecision:
		return "decision"
	case domain.StepKindApproval:
		return "approval"
	case domain.StepKindParallel:
		return "parallel"
	case domain.StepKindRecover:
		return "recovery"
	case domain.StepKindWorkItem:
		return "work item"
	case domain.StepKindProject:
		return "project"
	default:
		return kind
	}
}

// readProjectContextFiles renders the project's context_files (files AND directories) as a "#
// Project context" prompt section via the shared contextfiles.Render.
func (r *WorkflowReconciler) readProjectContextFiles(ctx context.Context, tx pgx.Tx, tenantID, projectID string) (string, error) {
	p, err := db.GetProject(ctx, tx, tenantID, projectID)
	if err != nil {
		return "", fmt.Errorf("get project for context files: %w", err)
	}
	if len(p.ContextFiles) == 0 {
		return "", nil
	}
	var files []string
	if err := json.Unmarshal(p.ContextFiles, &files); err != nil {
		return "", fmt.Errorf("parse context_files JSON: %w", err)
	}
	if len(files) == 0 {
		return "", nil
	}
	return contextfiles.RenderManifest("# Project context", files, p.ProjectDir), nil
}

// workItemKindLabel returns a human-readable label for a work item's
// kind enum value (1=task, 2=feature, 3=epic, 4=subtask).
func workItemKindLabel(kind string) string {
	switch kind {
	case "task":
		return "task"
	case "feature":
		return "feature"
	case "epic":
		return "epic"
	case "subtask":
		return "subtask"
	default:
		return kind
	}
}

// readConfigWorkItemID extracts work_item_id from a step's config JSON.
// Returns "" for empty / malformed / missing config.
func readConfigWorkItemID(config string) string {
	if config == "" {
		return ""
	}
	var parsed struct {
		WorkItemID string `json:"work_item_id"`
	}
	if err := json.Unmarshal([]byte(config), &parsed); err != nil {
		return ""
	}
	return parsed.WorkItemID
}

// readConfigProjectID extracts project_id from a step's config JSON.
// Returns "" for empty / malformed / missing config.
func readConfigProjectID(config string) string {
	if config == "" {
		return ""
	}
	var parsed struct {
		ProjectID string `json:"project_id"`
	}
	if err := json.Unmarshal([]byte(config), &parsed); err != nil {
		return ""
	}
	return parsed.ProjectID
}

// readConfigStrategy extracts the recovery strategy from a step's
// config JSON. PR D — recovery palette tiles (stop, summarize_restart,
// human_escalation, retry_n) write this field. The reconciler logs it
// when a RECOVER step is reached so operators can see what the
// runtime would have done on a real failure. Returns "" for empty /
// missing.
// stepRecoveryConfig is the per-step recovery configuration embedded in
// a task step's config JSON under the "recovery" key. Controls what
// happens when the step's work item fails after blind-retry attempts
// are exhausted.
//
// Strategy options:
//   - "retry" (default): blind retry — create a fresh work item and
//     re-dispatch immediately.
//   - "summarize_restart": trigger the recovery engine to capture,
//     summarize, and resume the failed work item with context.
//   - "human_escalation": set the step to approval_pending; a human
//     must mark the task succeeded to continue.
//   - "stop": permanent failure — no retry, step is marked failed.
//
// There is deliberately NO delay field. live config carries `retry_delay_seconds` — it
// was written into the seeds and into stored step configs — but NOTHING ever waited on
// it: the value was parsed here and then read by no code, and the recovery row that
// carried it was never consulted when a retry was dispatched (there is no deferral
// mechanism for execution dispatch at all; retries go out immediately). A knob that
// silently does nothing is worse than no knob, so it is gone. A stored
// `retry_delay_seconds` key is now IGNORED here and PRESERVED verbatim by the step
// editors, which never modelled it.
type stepRecoveryConfig struct {
	Strategy    string `json:"strategy"`
	MaxAttempts int    `json:"max_attempts"`

	// strategySet records whether the config carried an EXPLICIT, non-empty strategy.
	strategySet bool
}

// Step-level recovery strategy names, as written in a step's config.recovery.strategy.
const (
	strategyRetry            = "retry"
	strategySummarizeRestart = "summarize_restart"
)

// readStepRecoveryConfig reads the "recovery" block from the step's config JSON.
func readStepRecoveryConfig(config string) stepRecoveryConfig {
	cfg := stepRecoveryConfig{
		Strategy:    strategyRetry,
		MaxAttempts: 3,
	}
	if config == "" {
		return cfg
	}
	var outer struct {
		Recovery stepRecoveryConfig `json:"recovery"`
	}
	if err := json.Unmarshal([]byte(config), &outer); err != nil {
		return cfg
	}
	if outer.Recovery.Strategy != "" {
		cfg.Strategy = outer.Recovery.Strategy
		cfg.strategySet = true
	}
	if outer.Recovery.MaxAttempts > 0 {
		cfg.MaxAttempts = outer.Recovery.MaxAttempts
	}
	return cfg
}

// For such a run the generic "retry" fallback is the wrong default, because retry is NOT the
// recovery flow: it clones the step's ticket and re-dispatches it blind.
func resolveStepRecoveryConfig(config string, ephemeral bool) stepRecoveryConfig {
	rc := readStepRecoveryConfig(config)
	if !ephemeral || rc.strategySet {
		return rc
	}
	rc.Strategy = strategySummarizeRestart
	return rc
}

// resolveRuntimeImage determines the runtime container image for a run
// at start (always-container resolution chain):
//   - template runs (run.WorkItemID set): the bound work item's stored
//     runtime_image when explicit;
//   - one-shot runs: the WORK_ITEM canvas markers' work items' stored
//     runtime_image values, which must all agree;
//   - project default: when the item/marker lookup yields "", the
//     run's (or marker item's) project default_runtime_image;
//   - base: empty everywhere resolves to orchicon-runtime:base.
//
// Never returns "" and never the no-serve sentinel (the sentinel survives
// only as an in-memory serve-skip signal, never as a persisted image).
//
// The resolved value is stored on the run row so the adapter's self-heal
// recreates the container with the identical image.
func (r *WorkflowReconciler) resolveRuntimeImage(ctx context.Context, tx pgx.Tx, tenantID string, run db.WorkflowRunRow, steps []workflow.StepWire) (string, error) {
	if run.WorkItemID != "" {
		wi, err := db.GetWorkItem(ctx, tx, tenantID, run.WorkItemID)
		if err != nil {
			return "", fmt.Errorf("get bound work item: %w", err)
		}
		if img := strings.TrimSpace(wi.RuntimeImage); img != "" {
			return img, nil
		}
		return resolveProjectDefaultImage(ctx, tx, tenantID, run.ProjectID), nil
	}
	// One-shot: collect WORK_ITEM markers' images; all must agree.
	values := []string{}
	for _, s := range steps {
		if s.Kind != domain.StepKindWorkItem {
			continue
		}
		wid := readConfigWorkItemID(s.Config)
		if wid == "" {
			continue
		}
		wi, err := db.GetWorkItem(ctx, tx, tenantID, wid)
		if err != nil {
			if err == db.ErrNotFound {
				continue
			}
			return "", fmt.Errorf("get marker work item %s: %w", wid, err)
		}
		if img := strings.TrimSpace(wi.RuntimeImage); img != "" {
			values = append(values, img)
		}
	}
	chosen, err := resolveImageFromValues(values)
	if err != nil {
		return "", err
	}
	if chosen != "" {
		return chosen, nil
	}
	// One-shot with no explicit image: fall back to the run's project
	// default, else the base image.
	return resolveProjectDefaultImage(ctx, tx, tenantID, run.ProjectID), nil
}

// resolveProjectDefaultImage returns the project's default_runtime_image when set, else the base
// image.
func resolveProjectDefaultImage(ctx context.Context, tx pgx.Tx, tenantID, projectID string) string {
	if strings.TrimSpace(projectID) == "" {
		return db.RuntimeBaseImage()
	}
	p, err := db.GetProject(ctx, tx, tenantID, projectID)
	if err != nil {
		return db.RuntimeBaseImage()
	}
	if p.DefaultRuntimeImage != nil && strings.TrimSpace(*p.DefaultRuntimeImage) != "" {
		return strings.TrimSpace(*p.DefaultRuntimeImage)
	}
	return db.RuntimeBaseImage()
}

// resolveImageFromValues applies the one-shot image agreement rule: all non-empty values must be
// identical, or a single container cannot serve the run.
func resolveImageFromValues(values []string) (string, error) {
	var chosen string
	for _, img := range values {
		if img == "" {
			continue
		}
		if chosen == "" {
			chosen = img
			continue
		}
		if img != chosen {
			return "", fmt.Errorf("conflicting runtime images on work items (%s vs %s) — a workflow run uses one container", chosen, img)
		}
	}
	return chosen, nil
}

// upstreamWorkItemIDs walks step.DependsOn looking for WORK_ITEM steps and returns the
// work_item_ids they reference.
func upstreamWorkItemIDs(step workflow.StepWire, allSteps []workflow.StepWire) []string {
	byID := make(map[string]workflow.StepWire, len(allSteps))
	for _, s := range allSteps {
		byID[s.ID] = s
	}
	var ids []string
	for _, dep := range step.DependsOn {
		ds, ok := byID[dep]
		if !ok {
			continue
		}
		if ds.Kind != domain.StepKindWorkItem {
			continue
		}
		if wid := readConfigWorkItemID(ds.Config); wid != "" {
			ids = append(ids, wid)
		}
	}
	return ids
}

// resolveApprovalWorkItems determines the work item(s) a worker-backed
// approval execution runs against. NO approval work item is ever
// created — the step run IS the approval record, so the execution is
// dispatched against the run's shared ticket, resolved exactly like a
// TASK step:
//   - a recovering step re-uses the ticket already recorded in its
//     result's _work_item_id;
//   - otherwise WORK_ITEM markers upstream of the approval step;
//   - otherwise the run's bound work item (run.WorkItemID).
//
// Returns nil when no ticket exists; the caller fails the step with a
// clear message (same contract as TASK steps).
func resolveApprovalWorkItems(sr db.WorkflowStepRunRow, step workflow.StepWire, allSteps []workflow.StepWire, runWorkItemID string) []string {
	var upstream []string
	if sr.Status == domain.StepRunRecovering {
		var parsed struct {
			WorkItemID string `json:"_work_item_id"`
		}
		if err := json.Unmarshal(sr.Result, &parsed); err == nil && parsed.WorkItemID != "" {
			upstream = []string{parsed.WorkItemID}
		}
	}
	if len(upstream) == 0 {
		upstream = upstreamWorkItemIDs(step, allSteps)
		if len(upstream) == 0 && runWorkItemID != "" {
			upstream = []string{runWorkItemID}
		}
	}
	return upstream
}

// buildApprovalStepResult builds the result JSON written to a worker-backed approval step run when
// it is dispatched.
func buildApprovalStepResult(primaryWID, composite, fingerprint, workerRefStr string, workerVersion int, upstreamWorker, upstreamSummary string, upstreamFiles []string, ac string, prevResult []byte, agentPromptLen int) []byte {
	stepResult, _ := json.Marshal(map[string]any{
		"_work_item_id":       primaryWID,
		"_prompt":             composite,
		"_prompt_fingerprint": fingerprint,
		// The RUN-LEVEL prefix length — see the task step's writer for why.
		"_agent_prompt_len": agentPromptLen,
		"_worker_id":        workerRefStr,
		"_worker_version":   workerVersion,
		"_upstream_worker":  upstreamWorker,
		"_upstream_summary": upstreamSummary,
		"_upstream_files":   upstreamFiles,
		"_ac":               ac,
		"_decision":         "pending",
	})
	var prev map[string]any
	_ = json.Unmarshal(prevResult, &prev)
	if len(prev) > 0 {
		var newResult map[string]any
		_ = json.Unmarshal(stepResult, &newResult)
		for _, k := range recoveryResultKeys {
			if v, ok := prev[k]; ok {
				newResult[k] = v
			}
		}
		stepResult, _ = json.Marshal(newResult)
	}
	return stepResult
}

// The work item's _decision is consulted ONLY as a legacy fallback when the step run carries none,
// so a stale decision left on a shared ticket by a prior run/step can never override the current.
func approvalDecisionFromSources(srDecision string, wiResults []byte) string {
	if srDecision != "" {
		return srDecision
	}
	if len(wiResults) == 0 {
		return ""
	}
	var wiResult map[string]any
	if json.Unmarshal(wiResults, &wiResult) != nil {
		return ""
	}
	if v, ok := wiResult["_decision"].(string); ok {
		return v
	}
	return ""
}

// dispatchReq is an inline TaskReconciler dispatch queued during a reconcile pass and invoked after
// the pass's transaction commits.
type dispatchReq struct {
	taskID    string
	stepRunID string
}

// recoveryTriggerReq is a deferred recovery trigger collected during a reconcile pass and invoked
// AFTER the pass's transaction commits (see reconcileRun.
type recoveryTriggerReq struct {
	tenantID     string
	workItemID   string
	failedExecID string
	stepRunID    string
	reason       string
}

// recoveryResultKeys are the result keys a re-dispatch preserves verbatim.
var recoveryResultKeys = []string{
	"_recovery_summary", "_recovery_execution_id", "_recovery_worker_id",
	"_recovery_worker_version", "_recovery_adapter",
	"_recovery_failure_reason",
	"_failed_execution_id", "_failed_worker_id", "_recovery_strategy",
}

// recoveringStepResult builds the result JSON written when a step run transitions to recovering.
func recoveringStepResult(ctx context.Context, tx pgx.Tx, tenantID, workItemID, failedExecID, strategy string, prevResult []byte) []byte {
	if strategy == "" {
		strategy = strategyRetry
	}
	res := map[string]any{
		"_work_item_id":        workItemID,
		"_failed_execution_id": failedExecID,
		"_recovery_strategy":   strategy,
		// RC2 — bound the stalled-recovering guard on WHEN this step entered recovering, never on run age.
		"_recovering_since": time.Now().UTC().Format(time.RFC3339Nano),
	}
	if len(prevResult) > 0 {
		var prev map[string]any
		if json.Unmarshal(prevResult, &prev) == nil {
			// _recovery_failure_reason IS CARRIED HERE, not only in the recoveryResultKeys sweeps.
			for _, k := range []string{"_worker_id", "_worker_version", "_recovery_failure_reason"} {
				if v, ok := prev[k]; ok {
					res[k] = v
				}
			}
		}
	}
	if failedExecID != "" && tx != nil {
		if fe, err := db.GetExecution(ctx, tx, tenantID, failedExecID); err == nil && fe.WorkerID != "" {
			res["_failed_worker_id"] = fe.WorkerID
		}
	}
	b, _ := json.Marshal(res)
	return b
}

// recoveryDispatchReady is the single predicate gating a recovering
// TASK/APPROVAL step run's re-dispatch (replaces the old gate that keyed
// off sr.WorkerExecutionID, which dispatchStep clears at re-dispatch). It
// enforces the recovery-resume invariant:
//
//	A recovery-resumed execution is only created after (1) its recovery is
//	terminal `resumed`, (2) the seed is resolvable for the exact
//	dispatching worker, and (3) the TaskReconciler's file gate will
//	write+verify .orchicon/worker.recovery before the session starts.
//
// Returns (ready=true, nil) to dispatch, (false, nil) to hold for the next
// pass, or (false, reason) to FAIL the step with `reason` (terminal
// recovery that never resumed — never dispatch cold).
func (r *WorkflowReconciler) recoveryDispatchReady(ctx context.Context, tx pgx.Tx, tenantID string, run db.WorkflowRunRow, sr db.WorkflowStepRunRow) (bool, error) {
	var meta struct {
		WorkItemID            string  `json:"_work_item_id"`
		FailedExecID          string  `json:"_failed_execution_id"`
		RecoveryExecID        string  `json:"_recovery_execution_id"`
		RecoveryStrategy      string  `json:"_recovery_strategy"`
		WorkerID              string  `json:"_worker_id"`
		WorkerVersion         float64 `json:"_worker_version"`
		FailedWorkerID        string  `json:"_failed_worker_id"`
		RecoveryWorkerVersion float64 `json:"_recovery_worker_version"`
		RecoveryAdapter       string  `json:"_recovery_adapter"`
	}
	if err := json.Unmarshal(sr.Result, &meta); err != nil || meta.WorkItemID == "" {
		// No ticket recorded — cannot gate; dispatchStep/failStep will
		// resolve the situation.
		return true, nil
	}
	failedExecID := meta.FailedExecID
	if failedExecID == "" {
		failedExecID = meta.RecoveryExecID
	}
	if failedExecID == "" {
		failedExecID = sr.WorkerExecutionID
	}

	// Legacy in-flight recovering row (pre-deploy — no _recovery_strategy):
	// current behavior — wait only while an ACTIVE recovery exists.
	if meta.RecoveryStrategy == "" {
		if failedExecID == "" {
			return true, nil
		}
		if _, err := db.GetActiveRecoveryForExecution(ctx, tx, tenantID, meta.WorkItemID, failedExecID); err == nil {
			return false, nil // active recovery — wait
		}
		return true, nil // no active recovery — dispatch (legacy behavior)
	}

	// Only the engine-driven summarize_restart strategy gates on the recovery lifecycle.
	if meta.RecoveryStrategy != "summarize_restart" {
		return true, nil
	}
	if failedExecID == "" {
		// A summarize_restart recovering step with no dead-execution identity must never fall back to a
		// cold dispatch.
		stalled, holdErr := r.holdRecoveringStep(run, sr, "no dead-execution identity — a summarize_restart recovery can never be resolved")
		if stalled {
			return false, holdErr
		}
		return false, nil
	}

	// 1. No ACTIVE recovery may remain for the failed execution.
	if _, err := db.GetActiveRecoveryForExecution(ctx, tx, tenantID, meta.WorkItemID, failedExecID); err == nil {
		return false, nil // still in flight — wait
	} else if err != db.ErrNotFound {
		return false, err
	}

	// 2. A TERMINAL `resumed` recovery must exist. A missing row means the
	//    post-commit trigger has not created it yet (the same-pass race) —
	//    hold. A terminal non-resumed recovery means the step can never be
	//    resumed — fail it loud instead of wedging or dispatching cold.
	rec, err := db.GetLatestRecoveryForExecution(ctx, tx, tenantID, meta.WorkItemID, failedExecID)
	if err == db.ErrNotFound {
		stalled, holdErr := r.holdRecoveringStep(run, sr, "recovery never materialized for the failed execution")
		if stalled {
			return false, holdErr
		}
		return false, nil
	}
	if err != nil {
		return false, err
	}
	switch rec.Status {
	case domain.RecoveryResumed:
		// proceed to the seed check below
	case domain.RecoveryFailed, domain.RecoveryCancelled:
		return false, fmt.Errorf("recovery did not resume (recovery status %s)", rec.Status)
	default:
		// pending / running / blocked — still in flight; hold.
		return false, nil
	}

	// R7 — fail fast when the dead-execution worker no longer matches the currently-assigned worker.
	if meta.FailedWorkerID != "" && meta.WorkerID != "" && meta.FailedWorkerID != meta.WorkerID {
		return false, fmt.Errorf(
			"step recovery worker changed: dead execution ran on %s but step %s is now assigned to %s — manual re-arbitration required",
			meta.FailedWorkerID, sr.StepID, meta.WorkerID)
	}

	// R7b — fail fast when the dead execution's worker VERSION no longer matches the currently-assigned
	// version.
	if meta.RecoveryWorkerVersion != 0 && meta.WorkerVersion != 0 && meta.RecoveryWorkerVersion != meta.WorkerVersion {
		return false, fmt.Errorf(
			"step recovery worker version changed: dead execution ran on %s v%v (%s) but step %s is now assigned to %s v%v — manual re-arbitration required",
			meta.FailedWorkerID, meta.RecoveryWorkerVersion, meta.RecoveryAdapter, sr.StepID, meta.WorkerID, meta.WorkerVersion)
	}

	// 3. A seed must be resolvable for the exact dispatching worker
	//    (step-run keys, or the recovery-row fallback). nil → hold: the
	//    engine publishes the keys atomically with terminal-resumed, so
	//    this resolves within the same commit window. A hold that never
	//    resolves is bounded by holdRecoveringStep.
	if seed := resolveRecoverySeed(ctx, tx, tenantID, meta.WorkItemID, sr.Result, nil, meta.WorkerID); seed == nil {
		stalled, holdErr := r.holdRecoveringStep(run, sr, "recovery seed never became resolvable after resume")
		if stalled {
			return false, holdErr
		}
		return false, nil
	}
	return true, nil
}

// dispatchLinkGrace is how long a task step running with NO execution link waits before it is
// treated as a lost dispatch (inline DispatchTask failed / adapter down) and routed to recovery.
const dispatchLinkGrace = 15 * time.Second

// recoveringStallTimeout is how long a recovering step may sit in the recovery-dispatch gate —
// holding on a recovery that never materializes.
func recoveringStallTimeout() time.Duration {
	if v := os.Getenv("ORCHICON_RECOVERING_STALL_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return 15 * time.Minute
}

// holdRecoveringStep applies the stalled-recovering bound to one recovery-dispatch hold.
func (r *WorkflowReconciler) holdRecoveringStep(run db.WorkflowRunRow, sr db.WorkflowStepRunRow, waitReason string) (bool, error) {
	wait := recoveringStallTimeout()
	if wait <= 0 {
		return false, nil
	}
	// RC2 — bound the stall by WHEN THIS STEP entered recovering, never the run's age.
	since := recoveringSinceTime(sr)
	if since.IsZero() || time.Since(since) < wait {
		return false, nil
	}
	r.log.Warn("recovering step stalled past limit — failing step terminal",
		"run", run.ID, "step", sr.StepID, "reason", waitReason, "wait", wait.String(),
		"recovering_since", since.Format(time.RFC3339))
	return true, fmt.Errorf("step stuck recovering for %s (%s) — failing step", wait, waitReason)
}

// recoveringSinceTime extracts the `_recovering_since` timestamp stamped at
// the step→recovering transition. Zero when absent (legacy/pre-deploy row).
func recoveringSinceTime(sr db.WorkflowStepRunRow) time.Time {
	var m map[string]any
	if json.Unmarshal(sr.Result, &m) != nil {
		return time.Time{}
	}
	if v, ok := m["_recovering_since"].(string); ok && v != "" {
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			return t
		}
	}
	return time.Time{}
}

// maxDAGPasses bounds the per-run DAG progression loop.
func maxDAGPasses() int {
	if v := os.Getenv("ORCHICON_MAX_DAG_PASSES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 100
}

// pollTaskStep checks the WorkItem linked to a running task step. Returns
// (terminal, failed, error). The WorkItem id is stored in the step run's
// result JSON under _work_item_id.
//
// When the work item has failed and the step has remaining retry attempts,
// behaviour depends on the step's recovery strategy (from
// stepConfig.recovery):
//
//   - "retry" (default): creates a fresh work item and re-dispatches
//     immediately. The step transitions to "recovering".
//   - "summarize_restart": triggers the recovery engine to capture,
//     summarize, and resume the failed work item. The step waits in
//     "recovering" until the recovery completes.
//   - "human_escalation": sets the step to "approval_pending"; a human
//     must mark the task succeeded to continue.
//   - "stop": permanent failure — no retry.
//
// Terminal failure only occurs after all attempts are exhausted,
// regardless of strategy.
// stepRunResultWithVerdict returns a step run's Result with its execution's
// verdict folded in, so the step run is never marked TERMINAL without the
// decision its execution produced.
//
// WHY THIS EXISTS. A step run's `_decision` is written by the TaskReconciler
// (propagateStepRunResults, on the execution's completion) while the WORKFLOW
// reconciler marks the step run terminal from the execution's STATUS in its own
// pass. Those are two reconcilers with two independent scan loops, so there is
// a window in which the step run is `succeeded` and its Result still carries
// only the dispatch-time keys. A loop_decision that reads it in that window
// finds no verdict, falls through to the missing-decision branch, and re-asks
// the reviewer for a decision the reviewer had already given.
//
// MEASURED, not theorised: on run 01M522422MB39XNM0R6819JGXM (SDLC non-human)
// every loop_decision iteration was created 47-108ms BEFORE the reviewer's step
// run received its `_decision` -- loop1 at 10:32:33.873 vs rev1 at
// 10:32:33.981, loop3 at 10:47:41.073 vs rev2 at 10:47:41.120. All three
// re-asks therefore fired on verdicts that already existed in the execution's
// stdout, consumed max_reask (default 3), and failed the run -- while every
// reviewer execution had itself SUCCEEDED and signed off with
// `ORCHICON WORKER SUMMARY: success`.
//
// Reading the execution here is reading GROUND TRUTH rather than waiting on a
// second writer, the same direction effectivePRURL already takes for a PR URL.
// The merge is ADDITIVE -- an existing key always wins -- so a verdict the
// TaskReconciler has already written is never clobbered, and a step run whose
// Result is already complete is returned unchanged in substance.
func (r *WorkflowReconciler) stepRunResultWithVerdict(ctx context.Context, tx pgx.Tx, tenantID string, sr db.WorkflowStepRunRow) ([]byte, error) {
	if sr.WorkerExecutionID == "" {
		return nil, nil
	}
	exec, err := db.GetExecution(ctx, tx, tenantID, sr.WorkerExecutionID)
	if err != nil {
		return nil, err
	}
	merged := map[string]any{}
	if len(sr.Result) > 0 {
		_ = json.Unmarshal(sr.Result, &merged)
	}
	output := string(exec.Output)
	// The decision signal is the first word after the summary marker -- the SAME single source of truth
	// the TaskReconciler extracts from, so the two paths cannot disagree about what the reviewer said.
	if _, ok := merged["_decision"]; !ok {
		if d := extractSummaryDecision(output); d != "" {
			merged["_decision"] = d
		}
	}
	if _, ok := merged["_summary"]; !ok {
		if sum := extractWorkerSummary(output); sum != "" {
			merged["_summary"] = sum
		}
	}
	if _, ok := merged["_worker"]; !ok && exec.WorkerID != "" {
		merged["_worker"] = exec.WorkerID
	}
	if _, ok := merged["_worker_name"]; !ok && exec.WorkerName != "" {
		merged["_worker_name"] = exec.WorkerName
	}
	raw, err := json.Marshal(merged)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func (r *WorkflowReconciler) pollTaskStep(ctx context.Context, tx pgx.Tx, tenantID string, run *db.WorkflowRunRow, sr db.WorkflowStepRunRow, stepConfig string, runByID map[string]db.WorkflowStepRunRow, recoveryTriggers *[]recoveryTriggerReq) (bool, bool, error) {
	var parsed struct {
		WorkItemID string `json:"_work_item_id"`
	}
	if err := json.Unmarshal(sr.Result, &parsed); err != nil || parsed.WorkItemID == "" {
		return false, false, nil
	}
	wi, err := db.GetWorkItem(ctx, tx, tenantID, parsed.WorkItemID)
	if err != nil {
		if err == db.ErrNotFound {
			// The work item this step reads (its shared ticket, a work-item marker's item, or a retry artifact)
			// was hard-deleted mid-run — project cleanup or a manual delete.
			r.log.Warn("task step work item deleted — failing step",
				"run", run.ID, "step", sr.StepID, "work_item", parsed.WorkItemID)
			return true, true, nil
		}
		return false, false, fmt.Errorf("get work item: %w", err)
	}

	// Task AND worker-backed approval steps complete on their OWN execution's terminal state.
	if sr.WorkerExecutionID == "" {
		// A task step with NO execution link has not FAILED — nothing was
		// ever attempted. Three cases reach here, and only one is a problem:
		//   (a) dispatchStep just set this step running (first dispatch
		//       or a recovering-step re-dispatch); the inline
		//       DispatchTask links the replacement execution moments
		//       after the reconcile transaction commits. Wait a short
		//       grace for the link instead of guessing.
		//   (b) the dispatch was DEFERRED — for capacity (the project or
		//       worker max-concurrent-runs gate) or on runtime readiness.
		//       Both are legitimate waits, and both gates' own contract is
		//       that the step "stays ready and the next scan pass
		//       re-evaluates it". The capacity gates stamp `_dispatch_wait`,
		//       which is what makes this case distinguishable at all.
		//   (c) the dispatch was genuinely lost (inline dispatch failed,
		//       adapter down) — no note, no execution, and none coming.
		//
		// NO EXECUTION MEANS NO RECOVERY. Recovery is keyed to the FAILED
		// EXECUTION (TriggerOnFailure → get execution, summarize_restart
		// builds its seed from it), so falling through to the recovery block
		// here asks the engine to recover an execution that does not exist:
		// it fails "recovery trigger: get execution: db: not found", the step
		// is flipped `recovering`, and recoveryDispatchReady then holds it
		// FOREVER waiting for a recovery that can never materialize. The run
		// sits "running" with no current step and the UI reads "waiting for
		// dispatch" — observed on run 01M53DXZSF5G2T2Z592F7G5ETE, whose
		// project sat at its 10/10 cap so EVERY step dispatch was deferred.
		//
		// So hand the step back to `ready` instead: the dispatch section
		// re-attempts it on the next pass, and a deferred step then resumes
		// the moment a slot frees (which is exactly what the gate promises),
		// while a genuinely lost dispatch is retried rather than wedged.
		if !(sr.StartedAt != nil && time.Since(*sr.StartedAt) < dispatchLinkGrace) {
			reason := "no execution was created for the step — re-dispatching"
			if note, ok := dispatchWaitReason(sr.Result); ok {
				reason = note
			}
			ready := domain.StepRunReady
			updated, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
				Status: &ready,
			})
			if err != nil {
				return false, false, fmt.Errorf("return undelivered step to ready: %w", err)
			}
			runByID[sr.StepID] = updated
			r.log.Info("task step has no execution — returning it to ready to re-dispatch",
				"run", run.ID, "step", sr.StepID, "reason", reason)
			return false, false, nil
		}
		return false, false, nil
	} else {
		exec, err := db.GetExecution(ctx, tx, tenantID, sr.WorkerExecutionID)
		if err != nil {
			if err == db.ErrNotFound {
				// The execution this step references was hard-deleted (or never persisted) — e.g. a project cleanup
				// that removed the execution, or a lost inline-dispatch link.
				if sr.StartedAt != nil && time.Since(*sr.StartedAt) < dispatchLinkGrace {
					return false, false, nil
				}
				// fall through to the recovery block below.
			} else {
				// Transient DB error — not an orphan; retry next pass.
				return false, false, nil
			}
		} else {
			switch exec.Status {
			case domain.ExecutionSucceeded:
				// Guard rail: PR-producing steps must have a PR URL on success — but only when the effective git
				// strategy is "pr".
				shouldRequirePR := requiresPRStep(sr.StepID, stepConfig)
				if shouldRequirePR {
					// The project is always consulted for the repo slug (needed by the deterministic PR check)
					// independently of which strategy wins.
					repoSlug := ""
					if run.ProjectID != "" {
						if proj, err := db.GetProject(ctx, tx, tenantID, run.ProjectID); err == nil && proj.RepoSlug != nil {
							repoSlug = *proj.RepoSlug
						}
					}
					// Effective git strategy — workflow-level wins, else project-level, else "local" (single source of
					// truth, db.EffectiveGitStrategy).
					if db.EffectiveGitStrategy(ctx, tx, tenantID, run.WorkflowID, run.ProjectID) == "pr" {
						// Deterministic PR verification (D-PR): the platform itself checks GitHub for a valid PR whose head
						// is the branch created for this run.
						prURL, prState, verified := deterministicPRForBranch(repoSlug, run.WorktreeBranch)
						if !verified {
							// gh unavailable / no repo slug / no branch recorded: degrade to recorded sources (run_context,
							// exec PrURL, worker stdout) rather than fail a possibly-valid run.
							prURL = effectivePRURL(*run, exec)
							// Recover the recorded PR state by the same precedence as the URL fallback chain: run_context, the
							// execution column, then the worker's stdout marker.
							if _, ctxState := db.PrFromRunContext(run.RunContext); ctxState != "" {
								prState = ctxState
							} else if exec.PrState != nil {
								prState = *exec.PrState
							} else {
								prState = markerPRState(string(exec.Output))
							}
						}
						if prURL != "" {
							// Change 1: mirror the authoritative PR onto the step's
							// execution row columns, so the rows agree with run_context
							// at completion time.
							//
							// THIS WRITE USED A STALE VERSION AND WAS BEST-EFFORT, which is
							// how a merged PR became invisible. Field incident (run
							// 01M54EJY52ZW1NSDAQ5X07JN38): exec.Version was captured
							// earlier in this pass, the mirror lost the optimistic-
							// concurrency race, and the only trace was
							//
							//	WARN step-success: update execution PR fields (best-effort)
							//	     execution=01M54FE48… error="db: not found"
							//
							// The run still completed with pr_url in run_context, so the RUN
							// page showed the PR — but the work-item card reads the
							// EXECUTION row, so it showed nothing at all.
							//
							// Each attempt RE-READS the execution for its current version: a
							// re-send of the captured version can never match, so the read is
							// what lets this converge instead of burning attempts. Still
							// best-effort overall — the pass/fail decision is already made, and
							// the work-item card now reads the run as well, so this is no
							// longer the only copy.
							if _, err := retryStaleVersion(ctx, r.log, "execution pr fields", func() (db.ExecutionRow, error) {
								fresh, gerr := db.GetExecution(ctx, tx, tenantID, exec.ID)
								if gerr != nil {
									return db.ExecutionRow{}, gerr
								}
								return db.UpdateExecution(ctx, tx, tenantID, fresh.ID, fresh.Version,
									db.UpdateExecutionFields{PrURL: &prURL, PrState: &prState})
							}); err != nil {
								r.log.Warn("step-success: update execution PR fields (best-effort) — run_context still carries the PR",
									"run", run.ID, "execution", exec.ID, "pr_url", prURL, "error", err)
							}
							if prState == "" {
								prState = "open"
							}
							// Change 1b: when verified, the deterministic result is authoritative and overrides any
							// marker-derived run_context value.
							if curURL, _ := db.PrFromRunContext(run.RunContext); verified || curURL == "" {
								// Merge into existing run_context instead of replacing - preserves worktree_branch and other keys
								merged := run.RunContext
								var m map[string]any
								if len(merged) > 0 {
									_ = json.Unmarshal(merged, &m)
								}
								if m == nil {
									m = map[string]any{}
								}
								m["pr_url"] = prURL
								m["pr_state"] = prState
								newCtx, _ := json.Marshal(m)
								// Route the run_context write through the data-access layer (optimistic CAS on run.Version) instead
								// of a raw `version = version + 1` Exec.
								updatedRun, err := db.UpdateWorkflowRun(ctx, tx, tenantID, run.ID, run.Version, db.UpdateWorkflowRunFields{
									RunContext: &newCtx,
								})
								if err != nil {
									return false, false, fmt.Errorf("merge pr_url into run_context: %w", err)
								}
								*run = updatedRun
							}
						} else {
							r.log.Warn("PR step succeeded but no valid PR exists for the run's branch (git_strategy=pr) — failing step", "run", run.ID, "step", sr.StepID, "branch", run.WorktreeBranch, "repo", repoSlug)
							return true, true, nil
						}
					}
				}
				return true, false, nil

			case domain.ExecutionFailed, domain.ExecutionFailedToStart, domain.ExecutionTerminated:
				// fall through to the recovery block below.
			default:
				return false, false, nil
			}
		}
	}

	{
		// ephemeralDefaultStepRecovery: a machine-managed ephemeral run whose step names no strategy of its
		// own gets the recovery engine rather than a blind retry (see resolveStepRecoveryConfig).
		ephemeralDefaultStepRecovery := wi.Ephemeral && sr.StepKind != domain.StepKindApproval
		rc := resolveStepRecoveryConfig(stepConfig, ephemeralDefaultStepRecovery)

		if sr.Attempt >= rc.MaxAttempts-1 {
			return true, true, nil
		}

		newAttempt := sr.Attempt + 1
		r.log.Info("task step failed, running recovery strategy",
			"run", run.ID, "step", sr.StepID,
			"work_item", wi.ID, "attempt", newAttempt, "max_attempts", rc.MaxAttempts,
			"strategy", rc.Strategy)

		switch rc.Strategy {
		case strategyRetry, "":
			if sr.StepKind == domain.StepKindApproval {
				// Worker-backed approval: the step run IS the approval record — never clone the shared ticket into
				// a fresh work item on retry.
				stepResult := recoveringStepResult(ctx, tx, tenantID, parsed.WorkItemID, sr.WorkerExecutionID, rc.Strategy, sr.Result)
				updated, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
					Status:  strPtr(domain.StepRunRecovering),
					Attempt: &newAttempt,
					Result:  &stepResult,
				})
				if err != nil {
					return false, false, fmt.Errorf("update approval step run for retry: %w", err)
				}
				runByID[sr.StepID] = updated
				return false, false, nil
			}
			freshID := db.NewID()
			fresh := db.WorkItemRow{
				ID:                 freshID,
				TenantID:           wi.TenantID,
				ProjectID:          wi.ProjectID,
				ParentID:           wi.ParentID,
				Kind:               wi.Kind,
				Title:              wi.Title,
				Description:        wi.Description,
				AcceptanceCriteria: wi.AcceptanceCriteria,
				Status:             domain.WorkItemPending,
				Priority:           wi.Priority,
				Budgets:            wi.Budgets,
				ContextWindow:      wi.ContextWindow,
				WorkflowID:         wi.WorkflowID,
				WorkflowRunID:      wi.WorkflowRunID,
				WorkflowStepID:     wi.WorkflowStepID,
				Results:            []byte("{}"),
				PromptContext:      wi.PromptContext,
				// A retry is the SAME job, so the replacement ticket must carry
				// the input material the original was dispatched with. These
				// were silently dropped here:
				//
				//   - Ephemeral: the clone came back as a NORMAL work item, so a
				//     retry of an ephemeral run produced exactly the visible
				//     record the ephemeral flag exists to prevent — a machine
				//     dispatch leaking into every human work-item view.
				//   - RuntimeImage: resolveRuntimeImage reads the bound item, so a
				//     clone without it fell back to the PROJECT DEFAULT image —
				//     the retry could run somewhere other than where the original
				//     ran (and where its toolchain was proven).
				//   - ContextFiles/SecretIDs: the retry lost the context and
				//     credentials the brief was built around, so it re-attempted
				//     the same fault with strictly less information.
				Ephemeral:    wi.Ephemeral,
				RuntimeImage: wi.RuntimeImage,
				ContextFiles: wi.ContextFiles,
				SecretIDs:    wi.SecretIDs,
			}
			if _, err := db.CreateWorkItem(ctx, tx, fresh); err != nil {
				return false, false, fmt.Errorf("create retry work item: %w", err)
			}
			stepResult := recoveringStepResult(ctx, tx, tenantID, freshID, sr.WorkerExecutionID, rc.Strategy, sr.Result)
			updated, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
				Status:  strPtr(domain.StepRunRecovering),
				Attempt: &newAttempt,
				Result:  &stepResult,
			})
			if err != nil {
				return false, false, fmt.Errorf("update step run for retry: %w", err)
			}
			runByID[sr.StepID] = updated
			return false, false, nil

		case strategySummarizeRestart:
			if r.recovery != nil && recoveryTriggers != nil {
				// Defer the trigger to post-commit (see reconcileRun): TriggerOnFailure opens its own transaction,
				// and this pass may hold locks on the work item (dispatchStep re-dispatches a recovering step.
				*recoveryTriggers = append(*recoveryTriggers, recoveryTriggerReq{
					tenantID:     tenantID,
					workItemID:   parsed.WorkItemID,
					failedExecID: sr.WorkerExecutionID,
					stepRunID:    sr.ID,
					reason:       "step_recovery",
				})
			}
			stepResult := recoveringStepResult(ctx, tx, tenantID, parsed.WorkItemID, sr.WorkerExecutionID, rc.Strategy, sr.Result)
			updated, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
				Status:  strPtr(domain.StepRunRecovering),
				Attempt: &newAttempt,
				Result:  &stepResult,
			})
			if err != nil {
				return false, false, fmt.Errorf("update step run for recovery: %w", err)
			}
			runByID[sr.StepID] = updated
			return false, false, nil

		case "human_escalation":
			stepResult, _ := json.Marshal(map[string]string{
				"_work_item_id": parsed.WorkItemID,
				"_decision":     "pending",
			})
			updated, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
				Status:  strPtr(domain.StepRunApprovalPending),
				Attempt: &newAttempt,
				Result:  &stepResult,
			})
			if err != nil {
				return false, false, fmt.Errorf("update step run for human escalation: %w", err)
			}
			runByID[sr.StepID] = updated
			return false, false, nil

		case "stop":
			return true, true, nil

		default:
			return true, true, nil
		}
	}
}

// --- event helpers ---------------------------------------------------------

func (r *WorkflowReconciler) enqueueRunEvent(ctx context.Context, tx pgx.Tx, eventType string, run db.WorkflowRunRow, stepID string) error {
	evt := map[string]any{
		"event_type":      eventType,
		"tenant_id":       run.TenantID,
		"workflow_id":     run.WorkflowID,
		"workflow_run_id": run.ID,
		"run_status":      run.Status,
		"occurred_at":     time.Now().UTC().Format(time.RFC3339Nano),
	}
	if stepID != "" {
		evt["step_id"] = stepID
	}
	// Audit: run_started carries the resolved image + execution mode (acceptance C) so operators can
	// verify what the run dispatched with.
	if eventType == domain.WorkflowEventRunStarted {
		evt["runtime_image"] = run.RuntimeImage
		evt["execution_mode"] = runExecutionMode(ctx, tx, run.TenantID, run)
	}
	payload, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("marshal run event: %w", err)
	}
	return db.EnqueueOutbox(ctx, tx, db.OutboxRow{
		TenantID:      run.TenantID,
		EventType:     eventType,
		AggregateType: "workflow",
		AggregateID:   run.ID,
		AggregateVer:  run.Version,
		Payload:       payload,
		OccurredAt:    time.Now().UTC(),
	})
}

func (r *WorkflowReconciler) enqueueStepEvent(ctx context.Context, tx pgx.Tx, eventType string, run db.WorkflowRunRow, sr db.WorkflowStepRunRow) error {
	evt := map[string]any{
		"event_type":      eventType,
		"tenant_id":       sr.TenantID,
		"workflow_id":     run.WorkflowID,
		"workflow_run_id": sr.WorkflowRunID,
		"step_id":         sr.StepID,
		"step_run_id":     sr.ID,
		"step_status":     sr.Status,
		"occurred_at":     time.Now().UTC().Format(time.RFC3339Nano),
	}
	payload, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("marshal step event: %w", err)
	}
	return db.EnqueueOutbox(ctx, tx, db.OutboxRow{
		TenantID:      sr.TenantID,
		EventType:     eventType,
		AggregateType: "workflow",
		AggregateID:   sr.ID,
		Payload:       payload,
		OccurredAt:    time.Now().UTC(),
	})
}

// nowPtr returns a pointer to t (helper for optional timestamp fields).
// --- loop decision helpers (docs/11 §3) -----------------------------------

// loopDecisionConfig is the step config for a loop_decision node.
type loopDecisionConfig struct {
	BranchFrom    string `json:"branch_from"`
	SuccessBranch string `json:"success_branch"`
	LoopBranch    string `json:"loop_branch"`
	MaxIterations int    `json:"max_iterations"`
	DecisionField string `json:"decision_field"` // field name in work item results to check; default "_decision"
	SuccessValue  string `json:"success_value"`  // value meaning success; default "success"
	FailureValue  string `json:"failure_value"`  // value meaning failure; default "failure"
	MaxReask      int    `json:"max_reask"`      // max re-ask attempts when no decision field found; default 3

	// OnMissingDecision names what to do when NO upstream supplies a value in DecisionField — the gate
	// has no verdict to route on.
	OnMissingDecision string `json:"on_missing_decision"`

	// DecisionField / SuccessValue / FailureValue are PLATFORM CONTRACT, not operator preference, and
	// are therefore exposed in NEITHER client's step editor.
}

// The on_missing_decision vocabulary. The engine routes on these exact strings;
// MissingDecisionReask is the default an absent key resolves to.
const (
	MissingDecisionReask   = "reask"
	MissingDecisionSuccess = "success"
	MissingDecisionFail    = "fail"
)

func parseLoopDecisionConfig(config string) loopDecisionConfig {
	var cfg loopDecisionConfig
	json.Unmarshal([]byte(config), &cfg)
	if cfg.DecisionField == "" {
		cfg.DecisionField = "_decision"
	}
	if cfg.SuccessValue == "" {
		cfg.SuccessValue = "success"
	}
	if cfg.FailureValue == "" {
		cfg.FailureValue = "failure"
	}
	if cfg.MaxReask <= 0 {
		cfg.MaxReask = 3
	}
	// An ABSENT key means the general re-ask path, and so does any value outside the vocabulary.
	if cfg.OnMissingDecision != MissingDecisionReask &&
		cfg.OnMissingDecision != MissingDecisionSuccess &&
		cfg.OnMissingDecision != MissingDecisionFail {
		cfg.OnMissingDecision = MissingDecisionReask
	}
	return cfg
}

// loopDecisionAccept records a loop_decision step run as SUCCEEDED — the "proceed forward" action —
// and emits its step_succeeded event.
func (r *WorkflowReconciler) loopDecisionAccept(ctx context.Context, tx pgx.Tx, tenantID string, run db.WorkflowRunRow, sr db.WorkflowStepRunRow, step workflow.StepWire, runs map[string]db.WorkflowStepRunRow, now time.Time, reason string) error {
	updated, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
		Status:    strPtr(domain.StepRunSucceeded),
		StartedAt: &now,
		EndedAt:   &now,
	})
	if err != nil {
		return fmt.Errorf("mark loop_decision step succeeded: %w", err)
	}
	runs[step.ID] = updated
	if err := r.enqueueStepEvent(ctx, tx, domain.WorkflowEventStepSucceeded, run, updated); err != nil {
		return fmt.Errorf("enqueue loop_decision step_succeeded: %w", err)
	}
	r.log.Info("loop_decision: accepted", "run", run.ID, "step", step.ID, "reason", reason)
	return nil
}

// currentLoopIteration returns the current iteration count for a step within a run.
func currentLoopIteration(runs map[string]db.WorkflowStepRunRow, stepID string) int {
	var maxIter int
	for _, sr := range runs {
		if sr.StepID == stepID && sr.SupersededBy == "" && sr.Iteration > maxIter {
			maxIter = sr.Iteration
		}
	}
	return maxIter
}

// approvalConfig is the step-level configuration for APPROVAL steps.
type approvalConfig struct {
	Reviewer      string `json:"reviewer"`
	WorkerRef     string `json:"worker_ref"`
	WorkerVersion int    `json:"worker_version"`

	LoopBranch    string `json:"loop_branch"`
	MaxIterations int    `json:"max_iterations"`

	DecisionField string `json:"decision_field"`
	SuccessValue  string `json:"success_value"`
	FailureValue  string `json:"failure_value"`
}

func parseApprovalConfig(cfgJSON string) approvalConfig {
	cfg := approvalConfig{
		DecisionField: "_decision",
		SuccessValue:  "approved",
		FailureValue:  "rejected",
	}
	json.Unmarshal([]byte(cfgJSON), &cfg)
	return cfg
}

// approvalReenter creates a new chain of step runs from loop_branch to the approval step,
// supersedes the current approval run, and creates a new PENDING iteration.
func (r *WorkflowReconciler) approvalReenter(ctx context.Context, tx pgx.Tx, tenantID string, run db.WorkflowRunRow, sr db.WorkflowStepRunRow, step workflow.StepWire, runs map[string]db.WorkflowStepRunRow, loopBranch string, currentIter int, now time.Time, allSteps []workflow.StepWire) error {
	nextIter := currentIter + 1

	chainIDs := chainStepIDs(allSteps, loopBranch, step.ID)
	if err := r.createChainRuns(ctx, tx, tenantID, run.ID, chainIDs, nextIter, allSteps); err != nil {
		return fmt.Errorf("approval step %q: create chain runs: %w", step.Name, err)
	}

	// Re-create the approval step itself as a PENDING run so the loop re-approves before the success
	// branch can fire: step-b depends on this approval step.
	newApproval := db.NewID()
	newRun := db.WorkflowStepRunRow{
		ID:            newApproval,
		TenantID:      tenantID,
		WorkflowRunID: run.ID,
		StepID:        step.ID,
		StepName:      step.Name + " (loop)",
		StepKind:      domain.StepKindApproval,
		Status:        domain.StepRunPending,
		Iteration:     nextIter,
	}
	if _, err := db.CreateWorkflowStepRun(ctx, tx, newRun); err != nil {
		return fmt.Errorf("approval step %q: create re-entry run: %w", step.Name, err)
	}

	supersededResult, _ := json.Marshal(map[string]string{"_decision": "rejected", "_loop": "re-entered"})
	superseded, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
		EndedAt:      &now,
		SupersededBy: &newApproval,
		Result:       &supersededResult,
	})
	if err != nil {
		return fmt.Errorf("approval step %q: supersede: %w", step.Name, err)
	}
	runs[step.ID] = superseded
	if err := r.enqueueStepEvent(ctx, tx, domain.WorkflowEventStepSucceeded, run, superseded); err != nil {
		return fmt.Errorf("enqueue approval step_succeeded (supersede): %w", err)
	}

	r.log.Info("approval re-entered",
		"run", run.ID, "step", step.ID, "loop_branch", loopBranch,
		"iteration", nextIter, "chain", chainIDs)
	return nil
}

// listStepRunsByStepID fetches all step runs for a given run+step.
func listStepRunsByStepID(ctx context.Context, tx pgx.Tx, tenantID, runID, stepID string) ([]db.WorkflowStepRunRow, error) {
	all, err := db.ListWorkflowStepRuns(ctx, tx, tenantID, runID)
	if err != nil {
		return nil, err
	}
	var out []db.WorkflowStepRunRow
	for _, sr := range all {
		if sr.StepID == stepID {
			out = append(out, sr)
		}
	}
	return out, nil
}

// reaskRunName marks a step run created by reaskDecisionStep (a loop decision re-asking its
// reviewer for a missing decision signal).
const reaskRunName = "Reviewer (re-ask)"

// countReaskRuns returns how many step runs for a reviewer were created by reaskDecisionStep
// (StepName == reaskRunName).
func countReaskRuns(runs []db.WorkflowStepRunRow) int {
	n := 0
	for _, sr := range runs {
		if sr.StepName == reaskRunName {
			n++
		}
	}
	return n
}

// reaskDecisionStep creates a new step run for the reviewer step (the loop decision's upstream
// dependency) to re-ask for a decision signal.
func (r *WorkflowReconciler) reaskDecisionStep(ctx context.Context, tx pgx.Tx, tenantID string, run db.WorkflowRunRow, step workflow.StepWire, sr db.WorkflowStepRunRow, runs map[string]db.WorkflowStepRunRow, reaskStepID string, now time.Time) (string, error) {
	nextIter := currentLoopIteration(runs, reaskStepID) + 1

	// Supersede the previous run of the re-ask target step.
	srList, err := listStepRunsByStepID(ctx, tx, tenantID, run.ID, reaskStepID)
	if err != nil {
		return "", fmt.Errorf("reask: list prior runs: %w", err)
	}
	for _, prior := range srList {
		if prior.SupersededBy == "" {
			if _, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, prior.ID, prior.Version, db.UpdateWorkflowStepRunFields{
				SupersededBy: &sr.ID,
			}); err != nil {
				return "", fmt.Errorf("reask: supersede prior run: %w", err)
			}
		}
	}

	// Look up the work item from the upstream step's result to amend its
	// prompt_context with the re-ask instruction.
	var upResult struct {
		WorkItemID string `json:"_work_item_id"`
	}
	for _, dep := range step.DependsOn {
		if s, ok := runs[dep]; ok {
			json.Unmarshal(s.Result, &upResult)
			break
		}
	}

	if upResult.WorkItemID != "" {
		wi, err := db.GetWorkItem(ctx, tx, tenantID, upResult.WorkItemID)
		if err == nil {
			reaskMsg := "\n\n# Re-ask\n\n**RE-ASK ATTEMPT " + fmt.Sprint(nextIter) + "**\n" +
				"Your response must end with:\n" +
				"```\nORCHICON WORKER SUMMARY: success — <summary>\n```\n" +
				"or\n" +
				"```\nORCHICON WORKER SUMMARY: failure — <summary>\n```\n" +
				"The first word (`success` or `failure`) routes the workflow. " +
				"If the work is complete and correct, use `success`. " +
				"If there are issues that need fixing, use `failure` and explain what needs fixing in the summary text."

			// Preserve the existing composite (worker identity, project, task, ancestors, workflow context,
			// recovery, instructions) and append the re-ask instruction so the worker doesn't lose all context.
			var pc struct {
				Composite string `json:"composite"`
			}
			existingPC := ""
			_ = json.Unmarshal(wi.PromptContext, &pc)
			if pc.Composite != "" {
				existingPC = pc.Composite
			}
			combined := existingPC + reaskMsg
			pcJSON, _ := json.Marshal(map[string]any{
				"composite": combined,
			})
			if _, err := db.UpdateWorkItem(ctx, tx, tenantID, wi.ID, wi.Version, db.UpdateWorkItemFields{
				PromptContext: &pcJSON,
			}); err != nil {
				r.log.Warn("reask: update work item prompt context", "work_item", wi.ID, "error", err)
			}
		}
	}

	// Create a new step run for the re-ask target.
	newStepRun := db.WorkflowStepRunRow{
		ID:            db.NewID(),
		TenantID:      tenantID,
		WorkflowRunID: run.ID,
		StepID:        reaskStepID,
		StepName:      reaskRunName,
		StepKind:      domain.StepKindTask,
		Status:        domain.StepRunPending,
		Iteration:     nextIter,
	}
	if _, err := db.CreateWorkflowStepRun(ctx, tx, newStepRun); err != nil {
		return "", fmt.Errorf("reask: create re-ask run: %w", err)
	}

	return upResult.WorkItemID, nil
}

// createLoopDecisionIteration creates a new iteration of a loop_decision step run with PENDING
// status (blocking downstream steps) and supersedes the current iteration.
func (r *WorkflowReconciler) createLoopDecisionIteration(ctx context.Context, tx pgx.Tx, tenantID string, run db.WorkflowRunRow, sr db.WorkflowStepRunRow, step workflow.StepWire, runs map[string]db.WorkflowStepRunRow, nextIter int, now time.Time, resultJSON string) error {
	// Create the new iteration first so we can set SupersededBy.
	newID := db.NewID()
	resultRaw := []byte(resultJSON)
	newIter := db.WorkflowStepRunRow{
		ID:            newID,
		TenantID:      tenantID,
		WorkflowRunID: run.ID,
		StepID:        step.ID,
		StepName:      step.Name,
		StepKind:      domain.StepKindLoopDecision,
		Status:        domain.StepRunPending,
		Iteration:     nextIter,
	}
	if _, err := db.CreateWorkflowStepRun(ctx, tx, newIter); err != nil {
		return fmt.Errorf("loop_decision step %q: create next iteration: %w", step.Name, err)
	}

	// Supersede the current loop decision run, pointing at the new one.
	superseded, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
		Status:       strPtr(domain.StepRunSucceeded),
		StartedAt:    &now,
		EndedAt:      &now,
		SupersededBy: &newID,
		Result:       &resultRaw,
	})
	if err != nil {
		return fmt.Errorf("loop_decision step %q: supersede prior iteration: %w", step.Name, err)
	}
	// put the superseded run in the map for the event; the new pending
	// run will be loaded on the next pass and overwrite in runByID.
	runs[step.ID] = superseded
	if err := r.enqueueStepEvent(ctx, tx, domain.WorkflowEventStepSucceeded, run, superseded); err != nil {
		return fmt.Errorf("enqueue loop_decision step_succeeded (supersede): %w", err)
	}
	r.log.Info("loop_decision iteration created",
		"run", run.ID, "step", step.ID, "iteration", nextIter)
	return nil
}

// loopDecisionReenter creates a new step run for the loop branch target, then marks the loop
// decision as RUNNING.
func (r *WorkflowReconciler) loopDecisionReenter(ctx context.Context, tx pgx.Tx, tenantID string, run db.WorkflowRunRow, sr db.WorkflowStepRunRow, step workflow.StepWire, runs map[string]db.WorkflowStepRunRow, loopBranch string, currentIter int, now time.Time, allSteps []workflow.StepWire) error {
	nextIter := currentIter + 1

	// Create new step runs for every step between the loop target and this loop decision (inclusive of
	// target, exclusive of decision) so the whole chain re-executes.
	chainIDs := chainStepIDs(allSteps, loopBranch, step.ID)
	if err := r.createChainRuns(ctx, tx, tenantID, run.ID, chainIDs, nextIter, allSteps); err != nil {
		return fmt.Errorf("loop_decision step %q: create chain runs: %w", step.Name, err)
	}

	// Mark the loop decision as RUNNING so it blocks downstream steps (QA Engineer).
	updated, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
		Status: strPtr(domain.StepRunRunning),
		Result: func() *[]byte { r := []byte(`{"loop":"re-entered"}`); return &r }(),
	})
	if err != nil {
		return fmt.Errorf("loop_decision step %q: mark running: %w", step.Name, err)
	}
	runs[step.ID] = updated
	if err := r.enqueueStepEvent(ctx, tx, domain.WorkflowEventStepStarted, run, updated); err != nil {
		return fmt.Errorf("enqueue loop_decision step_started: %w", err)
	}
	r.log.Info("loop_decision re-entered",
		"run", run.ID, "step", step.ID, "loop_branch", loopBranch,
		"iteration", nextIter, "chain", chainIDs)
	return nil
}

// chainStepIDs returns the IDs of every step strictly between `fromID` and `toID` in DAG order
// (fromID inclusive, toID exclusive).
func chainStepIDs(allSteps []workflow.StepWire, fromID, toID string) []string {
	// Adjacency: step -> its dependents (reverse of depends_on).
	dependents := make(map[string][]string, len(allSteps))
	index := make(map[string]workflow.StepWire, len(allSteps))
	for _, s := range allSteps {
		index[s.ID] = s
		for _, dep := range s.DependsOn {
			dependents[dep] = append(dependents[dep], s.ID)
		}
	}
	if _, ok := index[fromID]; !ok {
		return nil
	}
	if _, ok := index[toID]; !ok {
		return nil
	}

	// Every step reachable downstream from fromID (inclusive).
	downstream := map[string]bool{}
	queue := []string{fromID}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if downstream[id] {
			continue
		}
		downstream[id] = true
		queue = append(queue, dependents[id]...)
	}

	// Every step that is an ancestor of toID (inclusive), walking
	// depends_on upward.
	upstream := map[string]bool{}
	queue = []string{toID}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if upstream[id] {
			continue
		}
		upstream[id] = true
		queue = append(queue, index[id].DependsOn...)
	}

	// The chain is the intersection, in canvas order for determinism,
	// excluding the loop decision itself.
	var ids []string
	for _, s := range allSteps {
		if s.ID == toID {
			continue
		}
		if downstream[s.ID] && upstream[s.ID] {
			ids = append(ids, s.ID)
		}
	}
	return ids
}

// createChainRuns supersedes existing step runs for each step in the given list and creates new
// PENDING iterations.
func (r *WorkflowReconciler) createChainRuns(ctx context.Context, tx pgx.Tx, tenantID, runID string, stepIDs []string, iteration int, allSteps []workflow.StepWire) error {
	// Build a stepKind lookup from the workflow version.
	kindByID := make(map[string]string, len(allSteps))
	for _, s := range allSteps {
		kindByID[s.ID] = s.Kind
	}
	for _, sid := range stepIDs {
		srList, err := listStepRunsByStepID(ctx, tx, tenantID, runID, sid)
		if err != nil {
			return fmt.Errorf("list runs for chain step %s: %w", sid, err)
		}
		var priorID string
		for _, prior := range srList {
			if prior.SupersededBy == "" {
				newID := db.NewID()
				priorID = newID
				if _, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, prior.ID, prior.Version, db.UpdateWorkflowStepRunFields{
					SupersededBy: &newID,
				}); err != nil {
					return fmt.Errorf("supersede chain step %s: %w", sid, err)
				}
				break
			}
		}
		if priorID == "" {
			priorID = db.NewID()
		}
		kind := kindByID[sid]
		if kind == "" {
			kind = domain.StepKindTask
		}
		newRun := db.WorkflowStepRunRow{
			ID:            priorID,
			TenantID:      tenantID,
			WorkflowRunID: runID,
			StepID:        sid,
			StepName:      sid + " (loop)",
			StepKind:      kind,
			Status:        domain.StepRunPending,
			Iteration:     iteration,
		}
		if _, err := db.CreateWorkflowStepRun(ctx, tx, newRun); err != nil {
			return fmt.Errorf("create chain step run %s: %w", sid, err)
		}
	}
	return nil
}

// pollLoopDecisionChain checks whether every step in the chain between the loop decision's
// loop_branch target and its upstream dependency has completed (all terminal).
func (r *WorkflowReconciler) pollLoopDecisionChain(ctx context.Context, tx pgx.Tx, tenantID string, sr db.WorkflowStepRunRow, allSteps []workflow.StepWire) (bool, error) {
	// Find the loop decision's configuration to get loop_branch.
	var stepDef *workflow.StepWire
	for _, s := range allSteps {
		if s.ID == sr.StepID {
			stepDef = &s
			break
		}
	}
	if stepDef == nil {
		return false, fmt.Errorf("step %s not found in version", sr.StepID)
	}
	cfg := parseLoopDecisionConfig(stepDef.Config)

	var chain []string
	if cfg.LoopBranch != "" {
		chain = chainStepIDs(allSteps, cfg.LoopBranch, sr.StepID)
	}
	if len(chain) == 0 {
		return false, nil
	}

	// Load the latest run for each chain step and check if it is terminal.
	for _, cid := range chain {
		srList, err := listStepRunsByStepID(ctx, tx, tenantID, sr.WorkflowRunID, cid)
		if err != nil {
			return false, fmt.Errorf("list chain %s: %w", cid, err)
		}
		// Find the active (non-superseded) run.
		var active db.WorkflowStepRunRow
		for _, s := range srList {
			if s.SupersededBy == "" {
				active = s
				break
			}
		}
		if active.ID == "" {
			return false, nil
		}
		// A chain step is complete once it reaches a terminal state.
		if active.Status != domain.StepRunSucceeded {
			return false, nil
		}
	}
	return true, nil
}

func nowPtr(t time.Time) *time.Time { return &t }

// (nowPtr currently unused after refactor; retained for step-run
// timestamp fields as the reconciler grows.)
var _ = nowPtr

// writeApprovalInitFiles writes initial .orchicon/ files when an APPROVAL step enters
// approval_pending.
func (r *WorkflowReconciler) writeApprovalInitFiles(ctx context.Context, tx pgx.Tx, tenantID string, run db.WorkflowRunRow, sr db.WorkflowStepRunRow, upstreamWorker, upstreamSummary string, upstreamFiles []string) {
	if run.ProjectID == "" {
		return
	}
	proj, err := db.GetProject(ctx, tx, tenantID, run.ProjectID)
	if err != nil {
		r.log.Warn("write approval init files: get project", "project", run.ProjectID, "error", err)
		return
	}
	if proj.ProjectDir == "" {
		return
	}

	orchDir := filepath.Join(proj.ProjectDir, ".orchicon", sr.WorkflowRunID)
	if err := os.MkdirAll(orchDir, 0755); err != nil {
		r.log.Warn("write approval init files: mkdir", "dir", orchDir, "error", err)
		return
	}

	writeFile := func(name, content string) {
		if err2 := os.WriteFile(filepath.Join(orchDir, name), []byte(content), 0644); err2 != nil {
			r.log.Warn("write approval init file", "file", name, "error", err2)
		}
	}

	writeFile("worker", "human_approval")
	writeFile("status", "pending")
	writeFile("summary", upstreamSummary)
}

// extractFactsLearned pulls the text after each `FACTS LEARNED:` line out of a worker's summary.
func extractFactsLearned(summary string) []string {
	const marker = "facts learned:"
	var facts []string
	for _, line := range strings.Split(summary, "\n") {
		s := strings.TrimSpace(line)
		s = strings.TrimLeft(s, "-")
		s = strings.TrimLeft(s, " ")
		if len(s) < len(marker) || !strings.EqualFold(s[:len(marker)], marker) {
			continue
		}
		rest := strings.TrimSpace(s[len(marker):])
		if rest != "" {
			facts = append(facts, rest)
		}
	}
	return facts
}

// aggregateLoopDecisions evaluates the routing decision across multiple upstream steps of a fan-in
// loop decision.
func aggregateLoopDecisions(decisions []string, failureValue, successValue string) string {
	decision := ""
	for _, d := range decisions {
		switch d {
		case failureValue:
			return failureValue
		case successValue:
			decision = successValue
		}
	}
	return decision
}

// imageForRun stamps the runtime image a run carries: ALWAYS the resolved
// image (explicit work-item -> project default -> base). The needsServe
// flag is retained at call sites only as the in-memory serve-gate signal
// (whether the run-start gate must wait for the opencode serve); it no
// longer selects a sentinel image. runtime.NoServeImage survives only as
// the internal serve-skip marker for legacy rows, never as a freshly
// persisted run.RuntimeImage when a container exists.
//
// IT READS db.RuntimeBaseImage(), NOT THE db.BaseRuntimeImage CONSTANT, because the
// fallback has to be an image the runtime daemon's allowlist actually accepts. The
// constant is the historical placeholder ("orchicon-runtime:base"), which matched no
// installed tag on any host — a release tags the base ghcr.io/.../orchicon-runtime:
// latest and a dev host builds it as orchicon-runtime:local. Resolving to it made
// every imageless work item's LAST resort a tag the daemon refuses:
//
//	base "orchicon-runtime:base" -> image not allowed: orchicon-runtime:base
//
// db.RuntimeBaseImage() returns that same placeholder until the daemon publishes its
// configured tag through UseDaemonBaseImage, and the daemon's tag thereafter. THIS IS
// THE ONE PLACE the repair is applied: every leg of the chain funnels through here,
// so a caller cannot hold a path to the raw constant.
// armRuntimeColumns decides what a run start RECORDS in the two runtime columns.
//
// THE DEFECT THIS ENCODES. The arm wrote RuntimeImage = the RESOLVED image and RuntimeReady = true even for a
// LOCAL-mode run, and a local run never starts a container at all (shouldEnsureRunFor is false). The
// operator's own run therefore displayed:
//
//	ExecutionMode (project) : "local"
//	RuntimeImage  (run)     : "ghcr.io/beardedparrott/orchicon-runtime:latest"
//	RuntimeReady  (run)     : true
//
// while all three of the tenant's images read Status: absent. It asserted readiness of an image that was not
// present, and it sent the diagnosis of a failing Windows run toward a container theory for several rounds —
// where the real question was whether the project's directory existed.
//
// So the value a run CARRIES is the value it USES, and the two notions the old code conflated are now separate
// by construction:
//
//	resolved for this run -> what this run will actually run in. Empty for local. Recorded here.
//	inherited for display -> what the project/tenant WOULD use if this were a runtime run. Not this run's,
//	                         and never written onto it.
//
// ready is a POINTER so local mode can leave the column untouched rather than assert either value: the ready
// gate has one owner, the gate-clearing block in the reconcile pass, which records "nothing to wait for" as its
// own decision. Writing true here would be a second owner claiming a readiness this pass never established;
// writing false would arm a gate the same pass must clear, for a container that will never exist.
func armRuntimeColumns(runLocalMode bool, resolved string, needsServe, runtimeEnabled bool) (image string, ready *bool) {
	if runLocalMode {
		// NO CONTAINER: no image to name, and no readiness to assert.
		return "", nil
	}
	// The runtime-serve readiness gate: false when a runtime daemon is wired AND the run actually needs
	// a serve.
	return imageForRun(resolved, needsServe), boolPtr(!needsServe || !runtimeEnabled)
}

func imageForRun(resolved string, needsServe bool) string {
	_ = needsServe
	if strings.TrimSpace(resolved) == "" {
		return db.RuntimeBaseImage()
	}
	return strings.TrimSpace(resolved)
}
