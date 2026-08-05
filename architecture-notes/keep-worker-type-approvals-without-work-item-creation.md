# Keep worker type approvals without work item creation

Work item: "Keep worker type approvals but try to find a way to do it without creating extra work items. The approval records are enough."

Acceptance criteria:
- No work item creation from approvals.
- Worker approval types still work as is.

## Context

Workflow `approval` steps come in two flavors (see `dispatchStep` in
`internal/scheduler/workflow_reconciler.go`, `case domain.StepKindApproval`):

1. **Human approval** (`config.reviewer == "human"`, default): the step run goes to
   `approval_pending` and a human resolves it via the `ApproveStep` RPC
   (`internal/approval/service.go`). No work item is ever created. The Approvals
   page (`/approvals`, `ListPendingStepApprovals`) lists these **step runs** —
   they are the approval records.
2. **Worker-backed approval** (`config.reviewer == "worker"`, e.g. the AI Approver
   `w_se_ai_approver`): the reconciler **creates a per-step work item**
   (`Kind:"task"`, `Title:"Approval: <step> (<workflow>)"`, `Status:"ready"`,
   bound to `workflow_run_id`/`workflow_step_id`, with `assigned_worker_ref`),
   builds the composite prompt onto it, marks the step run `running`, and inline
   dispatches `{taskID: <approval wid>, stepRunID}`.

Those per-step "Approval:" work items are the clutter on the Work Items page.
The task asks to keep the worker-backed approval behavior (dispatch an approver
worker, record its decision, loop back on rejection) without creating work items.

### How the worker-backed approval flow works today (end to end)

1. `dispatchStep` (approval, reviewer=worker) — `workflow_reconciler.go:1315-1388`:
   - creates `wi` (the artifact work item) via `db.CreateWorkItem`
   - `buildCompositePrompt(wi, workerVer, ...)` → writes composite into the
     **work item's** `prompt_context` (NOT the step run's `_prompt`)
   - step run → `running`, result `{"_work_item_id": wid, "_upstream_worker": …,
     "_decision": "pending"}`
   - queues `dispatchReq{taskID: wid, stepRunID: sr.ID}`
2. Post-commit inline dispatch → `TaskReconciler.DispatchTask(taskID, stepRunID)`
   (`reconciler.go:92`) → `reconcileOne`:
   - loads the work item by ID (must exist)
   - worker resolution: `workerVersionForStepRun` reads `_worker_id`/`_worker_version`
     off the step run result first; for worker-backed approval those fields are
     **absent**, so it falls back to `selectWorker` on the ticket's
     `assigned_worker_ref` (`reconciler.go:382-392`)
   - creates `worker_executions` with `task_id = wid`, `project_id = wi.ProjectID`,
     `workflow_run_id`/`workflow_step_id` from the ticket, links the step run's
     `worker_execution_id`
   - `startExecution` reads the composite from the step run `_prompt`; **for
     worker-backed approval `_prompt` is empty**, so it falls back to the work
     item's `prompt_context`
3. Adapter finishes → `TaskReconciler.OnResult` → `transitionWorkItemOnResult`:
   - the artifact is bound to an active run → `boundToActiveRun` is true →
     `propagateStepRunResults` writes `_summary`/`_decision`/`_issues`/
     `_touched_files`/`_worker` onto the **step run's result** (NOT the work
     item; the artifact's own results stay untouched — see comment at
     `workflow_reconciler.go:2423`)
   - `writeOrchiconFiles` writes `.orchicon/<run>/` status/worker/summary
   - notifies the WorkflowReconciler
4. `pollTaskStep` (`workflow_reconciler.go:2402`) polls running task + approval
   steps:
   - reads `_work_item_id` from the step run result
   - terminal detection uses **`sr.WorkerExecutionID`** (the execution status),
     NOT the work item — the work item is only used for recovery bookkeeping
   - on execution failure, `retry` strategy clones `wi` into a **fresh work item**
     (a second artifact per retry!) and sets the step run `recovering`
5. Next pass, Phase 0 (`workflow_reconciler.go:301-369`): reads `_decision`
   from the step run result first (already populated by `propagateStepRunResults`);
   falls back to `wi.Results["_decision"]` (effectively dead for worker-backed
   approvals). `failure`/`rejected` → `approvalReenter` (loop-back, step-run-only,
   never touches the work item) or fail when `max_iterations` exhausted.

### Key facts that make the change small

- The step run is already the **approval record**: `workflow_step_runs` rows with
  `step_kind='approval'` carry `_decision`, `_summary`, `_upstream_worker`,
  `_upstream_summary`, `_upstream_files`, `_ac`, `_work_item_id`. The Approvals
  page queries step runs, never work items.
- The TASK step path already dispatches WITHOUT artifact creation: it resolves the
  run's shared ticket (`run.WorkItemID` or WORK_ITEM markers), writes
  `_work_item_id`/`_prompt`/`_worker_id`/`_worker_version` onto the step run, and
  dispatches `{taskID: <ticket>, stepRunID}` (`workflow_reconciler.go:947-1063`).
  Worker-backed approval should be made structurally identical.
- `worker_executions` already carries `workflow_run_id` + `workflow_step_id`
  (migration 20260716000000) and the step run already links `worker_execution_id`,
  so the execution does not depend on the artifact for run-view navigation.
- `approvalReenter` (loop-back) only creates step runs — no work item dependency.
- `readStepRecoveryConfig` defaults: strategy `retry`, `max_attempts=3`.

## Decision

**Mirror the TASK-step pattern for worker-backed approval steps: dispatch the
approver execution against the run's shared work item (the ticket) and put all
per-step context on the step run. Never create an approval work item.**

Specifically, in `dispatchStep` `case StepKindApproval` `reviewer == "worker"`:

1. Resolve the execution's work item the same way TASK steps do:
   `upstreamWorkItemIDs(step, allSteps)`; fall back to `run.WorkItemID`.
   (Reuse the existing `_work_item_id` when re-dispatching a recovering step.)
   Fail the step with a clear message if neither exists (same as TASK steps).
2. Build the composite prompt against that ticket and write it, plus the worker
   pin, onto the **step run result**:
   `{"_work_item_id": <ticket>, "_prompt": composite, "_worker_id": step.Ref,
   "_worker_version": step.WorkerVersion}` (exactly the TASK step shape at
   `workflow_reconciler.go:1022-1027`).
3. Keep the step run `running` with the approval context (`_decision:"pending"`,
   `_upstream_worker`, `_upstream_summary`, `_upstream_files`, `_ac`).
4. Dispatch `dispatchReq{taskID: <ticket>, stepRunID: sr.ID}`.

In `pollTaskStep`, make the failure/recovery path approval-aware so it never
clones the ticket into a new work item:

- For `StepKindApproval`, the `retry` branch must NOT `db.CreateWorkItem`. Instead
  keep `_work_item_id` (the ticket) on the step run, set the step run
  `recovering` with `Attempt+1`; the dispatch section re-dispatches the same
  step run on the next pass (already supported: dispatch loop accepts
  `StepRunRecovering`, `dispatchStep` re-resolves the ticket from the step run
  result or `run.WorkItemID`). Bounded by `max_attempts` as today.
- The `summarize_restart` recovery trigger keeps using `_work_item_id` (the
  ticket) — recovery is scoped to the failing step run/execution and does not
  create work items.

### Why not the alternatives

- **Nullable `task_id` / execution-only dispatch (Option B):** conceptually
  cleanest ("the step run is the record"), but `worker_executions.task_id` is
  `NOT NULL`, and the executions list/detail UI, API, and several DAO queries
  join on `task_id`. Making it nullable ripples across schema, data-access, and
  frontend, and buys little: pointing `task_id` at the shared ticket (Option A)
  already removes the artifact while keeping every existing display path intact.
- **Hide the artifact from the Work Items page (Option C):** cheapest (filter the
  list query/UI), but violates the explicit acceptance criterion "No work item
  creation from approvals" and leaves ghost rows in the DB.

## Consequences

- Work Items page: no more "Approval: …" rows — clutter gone.
- Approvals page: unchanged (already step-run based).
- Executions page: approval executions now appear under the shared ticket's title
  instead of "Approval: <step>". Acceptable (arguably better — the ticket is the
  run's real record). The run view's step → execution navigation still works via
  `worker_execution_id`.
- Run narrative / composite prompt / `.orchicon/` writes / loop-back: unchanged.
- Worker resolution: `_worker_id`/`_worker_version` on the step run become the
  primary source (the `workerVersionForStepRun` ticket fallback stays for
  legacy/human paths).
- Constraint introduced: worker-backed approval steps on **one-shot runs** (no
  bound ticket AND no WORK_ITEM marker upstream) can no longer dispatch — they
  fail with a clear message, same as TASK steps today. If that case matters,
  revisit Option B later.
- No DB migration needed.

## Implementation guidance

- `internal/scheduler/workflow_reconciler.go`
  - `dispatchStep` approval/worker branch (lines ~1315-1388): replace the
    `CreateWorkItem` + `UpdateWorkItem` block with ticket resolution +
    step-run result fields (TASK-step shape). Preserve `_upstream_*` context and
    the `_decision:"pending"` marker.
  - `pollTaskStep` retry branch (lines ~2471-2506): branch on
    `sr.StepKind == domain.StepKindApproval` → no clone, keep `_work_item_id`,
    set `recovering` + `Attempt+1`.
  - Phase 0 (lines ~301-369): unchanged; the step-run `_decision` is already the
    source of truth for worker-backed approvals.
- `internal/scheduler/reconciler.go`
  - `workerVersionForStepRun`: unchanged (step-run fields now always present for
    worker-backed approvals).
- Tests: unit test `dispatchStep` for the approval/worker branch (no work item
  created, step-run result carries `_worker_id`/`_prompt`); test `pollTaskStep`
  approval retry (no clone, attempt increment); existing approval tests must pass.
- Docs: update DOCUMENTATION.md worker-backed approval section to say executions
  bind to the shared ticket / step run, not a per-step artifact.
- UPDATES.md: add a row when merged.

## Verification plan

1. `make ci`.
2. Run the AI Approver workflow (worker-backed approval, e.g. seeded
   `wf_feature_approval_demo` with `reviewer=worker` + `w_se_ai_approver`) with a
   free model (e.g. `opencode/deepseek-v4-flash-free`):
   - count `work_items` rows before/after → unchanged (no new rows)
   - approver execution appears under the ticket, step run reaches `succeeded`
     with `_decision: success`
   - success branch proceeds downstream
3. Rejection path: force the approver to reject (change AC so the worker rejects)
   → `approvalReenter` loops back; verify no work items created across iterations.
4. Failure/retry path: make the approver execution fail (bad model ref) →
   verify re-dispatch with `Attempt+1`, still no work items created.
5. Frontend: `/work-items` shows no "Approval:" rows; `/approvals` still lists
   step-run approval records; run view step → execution link works.
