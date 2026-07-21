# Orchicon — Workflow Templates & Work-Item Binding

> **Version:** 0.1
> **Status:** Direction & design intent (agreed 2026-07-21)
> **Parent:** `01_Architecture_Vision.md`

This document specifies repeatable, tenant-level workflow templates that
can be bound to a WorkItem and run on a schedule or immediately, plus a
bounded **loop decision** step for iterative review cycles
(e.g. `coder → reviewer → decision → coder`). It builds on the existing
WorkflowReconciler and the existing `work_items.workflow_id` field; one-off
per-project Workflows are unchanged (docs/02 §2.4, docs/03 §2).

Shipped in three PRs:

1. **PR-A — Bound-run dispatch path** (proto + migration + reconciler +
   end-to-end curl verification). Backend-verifiable in isolation.
2. **PR-B — Loop decision step** (`StepKindLoopDecision` + reconciler
   re-entry + canvas palette node + back-edge validation).
3. **PR-C — Scheduled start + frontend** (ScheduledRunReconciler, WorkItem
   workflow selector, schedule date/time picker, bound-run live card).

Each PR is independently shippable.

---

## 1. Motivation

Today a Workflow is the top-level reconcilable object: a user builds one,
starts it, it runs once, it is done (docs/02 §2.4). To run the *same*
multi-step process against many WorkItems (e.g. "Coder → Recovery →
Reviewer → Decision: done or loop back to Coder"), the user rebuilds the
graph per item or falls back to single-Worker dispatch on the WorkItem
itself. There is no notion of "apply this template to that WorkItem and
iterate until it passes review."

This document adds:

- **Tenant-level Workflow templates bound to a WorkItem** (the template
  fully defines the Workers, as in one-shot Workflows).
- **Scheduled start** (a WorkItem can pin a future wall-clock time at
  which the bound template fires).
- **Loop decision step**: a canvas node whose loop outlet may drag back
  to a topologically-prior step, with a hard `max_iterations` cap. On
  cap exhaustion the run fails and opt-out recovery (invariant #8)
  engages as usual.

---

## 2. Conceptual model

### 2.1 Binding

A **WorkItem** carries `workflow_id` (already in the schema today) and,
newly, `scheduled_start_at` and `auto_start_workflow`. When a Workflow
template is bound to a WorkItem:

- If `scheduled_start_at IS NULL` and `auto_start_workflow = true` → a
  WorkflowRun is created and started immediately on save.
- If `scheduled_start_at` is set → the run is created by the
  ScheduledRunReconciler at that wall-clock time (see §5).
- If `auto_start_workflow = false` (the explicit opt-out) → the binding
  is recorded but no run is created until the user invokes an explicit
  "Apply Workflow" action (PR-C frontend button that clears the opt-out
  flag, or directly calls `StartWorkflow`).

Cancelling a pending binding = clearing `workflow_id` /
`scheduled_start_at` before a run exists.

The WorkItem's existing `assigned_worker_ref` is **not used** for bound
runs — the template's task steps pin their own Workers exactly as in
one-shot Workflows. PR-A keeps `bound_worker_ref` on `workflow_runs`
nullable for future use; PR-A bound runs leave it null.

### 2.2 Unit of work

Every `task` step in a bound run operates **directly on the bound
WorkItem row** (the user's confirmed choice): successive task steps
re-assign different Workers onto the same WorkItem across the run, in
place. No Subtasks are spawned. The composite prompt (`prompt_context`
on the WorkItem) is rebuilt by `buildCompositePrompt` before each
dispatch, so each Worker sees the up-to-date description/AC/ancestor +
upstream step summaries.

### 2.3 One-shot Workflows unchanged

Per-project Workflows with explicit canvas work-item marker steps
(`StepKindWorkItem` upstream of each `StepKindTask`) keep working
exactly as today (docs/02 §2.4, `internal/scheduler/workflow_reconciler.go`
`dispatchStep` `StepKindTask`). The bound-run dispatch path is a new
branch in `dispatchStep`, gated on `run.work_item_id <> ''`.

### 2.4 Recovery on a bound run

Recovery continues to operate on the Worker's `recovery_workflow_ref`
the same way as today (docs/06 §1, the user's confirmed choice). The
loop decision does not intercept recovery: if a `task` step inside a
bound run fails, the existing opt-out recovery pipeline fires; if
recovery succeeds, the loop decision re-evaluates normally.

---

## 3. Loop decision step

### 3.1 Step kind

New `StepKindLoopDecision` added to `internal/domain` (and to the proto
`StepKind` enum). The canvas displays it with two outlets:

- **success** → continues forward (the next downstream step).
- **loop** → may be dragged to the input of a **topologically-prior**
  step. The canvas enforces: (a) the target topologically precedes the
  loop node, (b) the target is reachable forward from the run's entry,
  (c) `max_iterations` is set on the loop node.

Cycle-check (currently a hard reject at admission — docs/02 §2.2) is
relaxed **exactly** for back-edges originating from a `loop_decision`
step that satisfies those three conditions. No other back-edges are
admitted.

### 3.2 Step config

```
StepKindLoopDecision:
  branch_from:     string  // upstream step whose result to inspect
  success_branch:  string  // forward step id
  loop_branch:     string  // back-edge target step id (topologically prior)
  max_iterations:  int     // hard cap (≥1)
  // Evaluation:
  //   v0.1: succeeded upstream step → success_branch
  //         failed  upstream step → loop_branch (if iterations < max)
  //                                   else run fails
```

### 3.3 Reconciler behavior

When the loop node runs:

1. Look up the upstream step's `workflow_step_runs.result` (the
   `_work_item_id` + summary from the step it branches from).
2. If the upstream step succeeded → mark the loop node succeeded,
   follow `success_branch`.
3. If the upstream step failed:
   - If the current iteration count for `loop_branch` is <
     `max_iterations` → re-enter `loop_branch`: create a fresh
     `workflow_step_runs` row with `iteration = N+1`, mark the previous
     run of that step `superseded_by = <new id>` (audit preserved),
     reassign the bound WorkItem to that step's Worker, and proceed.
   - If iterations == `max_iterations` → mark the loop node **failed**,
     which fails the WorkflowRun. Opt-out recovery (invariant #8) then
     engages on the Worker of the last failed `task` step.

### 3.4 Iteration tracking

```
ALTER TABLE workflow_step_runs
  ADD COLUMN iteration INT NOT NULL DEFAULT 0,
  ADD COLUMN superseded_by TEXT;
```

A step's "current iteration" = MAX(`iteration`) over its
`workflow_step_runs` rows for the run, where `superseded_by IS NULL`.
Re-entering creates a new row, prior row is terminal-superseded. The
reconciler picks the active (non-superseded) row when polling.

---

## 4. Bound-run dispatch path

The only reconciler change for PR-A is a new branch in
`WorkflowReconciler.dispatchStep` for `StepKindTask`:

```go
case domain.StepKindTask:
    upstream := upstreamWorkItemIDs(step, allSteps)
    if len(upstream) == 0 && run.WorkItemID != "" {
        // Bound run: operate directly on the bound work item.
        upstream = []string{run.WorkItemID}
    }
    // ... existing failure-handling for missing upstream ...
    // step.Ref remains the source of the Worker (template pins it).
```

`buildCompositePrompt` already assembles description + AC + ancestor
chain + upstream step summaries (workflow_reconciler.go:928). For bound
runs the ancestor/AC chain simply comes from the bound WorkItem's own
parent chain inside the project hierarchy rather than from canvas
work-item markers — the existing code path already does this when a
`work_item` marker's id coincides with the run's bound work item.

The TaskReconciler inline dispatch (`DispatchTask` after the workflow
transaction commits — workflow_reconciler.go:624) needs no change: the
WorkItem is already in `ready` status with `assigned_worker_ref` set by
the workflow's task step.

---

## 5. Scheduled start

### 5.1 Schema

```
ALTER TABLE work_items
  ADD COLUMN scheduled_start_at TIMESTAMPTZ,
  ADD COLUMN auto_start_workflow BOOLEAN NOT NULL DEFAULT TRUE;
ALTER TABLE workflow_runs
  ADD COLUMN work_item_id TEXT REFERENCES work_items(id),
  ADD COLUMN bound_worker_ref JSONB;
```

All new tenant_scoped columns get RLS policies (invariant #5).

### 5.2 ScheduledRunReconciler

New reconciler, kind `scheduled_run`, registered in
`internal/server/server.go`:

```
SELECT id FROM work_items
 WHERE workflow_id IS NOT NULL
   AND workflow_run_id IS NULL
   AND scheduled_start_at IS NOT NULL
   AND scheduled_start_at <= now()
   AND status = 'pending'         -- only fire once
   AND auto_start_workflow        -- explicit opt-out
```

For each matching row: call `StartWorkflow(workflow_id, project_id,
work_item_id)` inside a tenant transaction. Idempotent: `workflow_run_id`
on the work item is set in the same transaction; the scan filter
`workflow_run_id IS NULL` prevents re-fire.

When `scheduled_start_at IS NULL` and `auto_start_workflow = true`, the
WorkItem create/update handler calls `StartWorkflow` inline (synchronous
on save), exactly the same code path.

### 5.3 API surface

`StartWorkflowRequest` is extended (no separate RPC, to keep the surface
small):

```proto
message StartWorkflowRequest {
  string workflow_id = 1;
  string project_id  = 2;
  string run_context = 3;
  string work_item_id = 4; // NEW: bind this run to a work item (template)
}
```

The WorkItem create/update RPC accepts `workflow_id` (already present)
plus `scheduled_start_at` and `auto_start_workflow` (new).

---

## 6. Frontend (PR-C)

- **WorkItem form**: a **Workflow selector** appears in place of the
  Worker selector when a workflow is bound (the template fully defines
  the Workers). A **date/time picker** ("Schedule start (optional)")
  is revealed when a Workflow is selected.
- **WorkItem detail**: a "Bound Workflow Run" card showing the run's
  current step, iteration count for loop steps, and a live React Flow
  overlay with step transitions fanned out via `StreamWorkflowEvents`
  (docs/10 §5.1).
- **WorkItem list**: a "Workflow" column + a "Scheduled" badge for
  upcoming runs.
- **Canvas editor**: new **Loop Decision** palette node. The canvas
  validates back-edges only to topologically-prior steps and only when
  `max_iterations` is set; otherwise the drop is rejected with a toast.

---

## 7. Database changes summary

Forward-only migration `db/migrations/<date>_workflow_binding.sql`:

```sql
ALTER TABLE work_items
  ADD COLUMN scheduled_start_at TIMESTAMPTZ,
  ADD COLUMN auto_start_workflow BOOLEAN NOT NULL DEFAULT TRUE;

ALTER TABLE workflow_runs
  ADD COLUMN work_item_id   TEXT REFERENCES work_items(id),
  ADD COLUMN bound_worker_ref JSONB;

ALTER TABLE workflow_step_runs
  ADD COLUMN iteration      INT  NOT NULL DEFAULT 0,
  ADD COLUMN superseded_by  TEXT;

-- RLS policies for the new tenant-scoped columns (invariant #5).
```

Hand-append the RLS policies, then run `make migrate-hash` (Atlas free
tier does not diff `policy` blocks — AGENTS.md).

---

## 8. API surface summary

| RPC | Change |
|---|---|
| `StartWorkflow` | accepts `work_item_id` (the binding). |
| `CreateWorkItem` / `UpdateWorkItem` | accept `scheduled_start_at`, `auto_start_workflow` (the existing `workflow_id` is unchanged). |
| `WorkflowVersion` step kind enum | new `STEP_KIND_LOOP_DECISION` value + its config fields. |

No new top-level RPC in any PR; this keeps the Connect surface small and
makes the slice a pure extension.

---

## 9. Documentation updates

PR-A lands the first version of this doc (`docs/11_Workflow_Templates_and_Binding.md`)
adding it to the Design Doc Index in `AGENTS.md` and `docs/01_Architecture_Vision.md`.
Per-PR doc touch-ups:

- **PR-A**: docs/02 §2.4 (WorkItem workflow binding + bound runs),
  docs/03 §2 (bound-run dispatch branch in `WorkflowReconciler`),
  docs/09 §3.4 (new columns), docs/07 §3.4 (`StartWorkflow` field).
- **PR-B**: docs/02 §2.4 (Step kind `loop_decision` + bounded back-edges),
  docs/03 §2 (loop re-entry iteration semantics), docs/10 (canvas
  palette + drop validation).
- **PR-C**: docs/03 §2 (ScheduledRunReconciler + schedule scan), docs/10
  (WorkItem form Workflow/Worker selector swap, date/time picker, bound
  run card).

`README.md` Last Release Changes section and `UPDATES.md` rows are
appended per PR (AGENTS.md Phases step 5).

---

## 10. Verification

End-to-end with the free model `opencode/deepseek-v4-flash-free`
(AGENTS.md Verification §6). Per-PR minimum:

### PR-A

1. Create a tenant-level Workflow template with two sequential
   `task` steps pinning different Workers.
2. Create a WorkItem with `workflow_id = <template>` and no
   `scheduled_start_at` → the run starts immediately → the bound
   WorkItem is assigned to step 1's Worker, composite prompt carries
   project + ancestor context, after step 1 the WorkItem is reassigned
   to step 2's Worker, on both steps succeeding the run completes.
3. Verify via `curl` that `StreamWorkflowEvents` fans out transitions
   and `GetWorkItem` shows the active `workflow_run_id`.
4. One-off per-project Workflow with canvas work-item markers still
   runs identical to today (no regression).
5. `make ci`, RLS gate, migration applies cleanly, control-plane boot
   `<2s`.

### PR-B

1. Workflow template: `coder → reviewer → loop_decision(loop_branch:
   coder, max_iterations: 2) → done`.
2. Reviewer Worker returns fail → loop re-enters coder step
   (iteration 1 → 2). On the third fail the loop node fails → run fails
   → opt-out recovery fires (existing 6-step flow).
3. Reviewer Worker returns success on iteration 1 → run completes via
   `success_branch`.
4. Audit: prior `workflow_step_runs` rows show `superseded_by` set;
   newest row is the active one.
5. Canvas rejects a loop edge to a topologically-forward step and
   rejects a loop edge when `max_iterations` is unset.

### PR-C

1. WorkItem with `workflow_id` + `scheduled_start_at = now()+2min` → no
   run exists for 2 minutes → ScheduledRunReconciler fires exactly one
   run on the boundary → `workflow_run_id` is set on the work item → a
   second scan pass does not re-fire.
2. WorkItem form: selecting a Workflow hides the Worker selector and
   reveals the date/time picker. Selecting no Workflow restores the
   Worker selector.
3. WorkItem detail page "Bound Workflow Run" card shows current step +
   iteration + live overlay via `StreamWorkflowEvents`.
4. Clearing `workflow_id` on a pending WorkItem before the schedule
   fires → no run is ever created.

All three PRs run `make ci`, `make rls-check`, dev-stack boot, and the
free-model runtime path.