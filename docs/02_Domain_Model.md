# Orchicon — Domain Model

> **Version:** 0.1
> **Status:** Direction & design intent
> **Parent:** `01_Architecture_Vision.md`

This document fixes the conceptual entities, relationships, and
lifecycle states that the rest of the design set refers to. It is
intentionally agnostic of storage shape (see `09_Database_Schema.md`)
and API shape (see `07_API_Specification.md`).

---

## 1. Design Intent

- The domain is **project-centric**: every entity lives within a
  Project. There is no global scope for work items.
- Workers, Policies, and Workflows are **reusable, versioned resources**
  that can be shared across projects by reference (within a tenant).
- The model separates **what should be done** (Tasks, Workflows) from
  **who does it** (Workers) from **how it is governed** (Policies).
- Workers intentionally do **not** carry runtime capabilities
  (MCP servers, tools, terminal access). Those belong to the runtime
  adapter (see `04_Runtime_Adapter_SDK.md`, `05_Worker_Specification.md`).

---

## 2. Entities

### 2.1 Project

The persistent source of truth. Contains goals, architecture,
documentation, execution history, telemetry, and the work hierarchy.

Key fields: id, tenant, name, slug, goals, status, budget envelope,
default policies, created/updated timestamps.

Lifecycle: `drafting → active → paused → archived → deleted`.

- `drafting`: scaffolding, no execution permitted.
- `active`: scheduler may dispatch work.
- `paused`: reconciler will not start new work; in-flight work completes
  or is checkpointed per policy.
- `archived`: read-only; retention policy applies.
- `deleted`: soft-deleted; recoverable until retention window closes.

### 2.2 Work Hierarchy

`Epic → Feature → Task → Subtask`. All four share a common `WorkItem`
base; depth is constrained (max 4 levels). Dependencies are modeled as
edges in a DAG, not as parent-child links.

**WorkItem (base)**
- id, project_id, parent_id, kind (epic/feature/task/subtask)
- title, description, acceptance_criteria
- status, assigned_worker_id, workflow_id
- priority, budgets (token/cost/time), context_window
- execution_history (ref), results, created/updated

**Dependency edge**
- from_id, to_id, type (`blocks` | `depends_on` | `relates_to`)
- The scheduler treats the union of edges as a DAG; cycles are rejected
  at admission.

Lifecycle (Task/Subtask, the schedulable kinds):
`pending → ready → assigned → running → checkpointing → succeeded | failed | cancelled | recovering`

- `ready`: dependencies satisfied, awaiting scheduler pick.
- `assigned`: worker selected, dispatch in flight (not yet running).
- `running`: adapter confirms execution started.
- `checkpointing`: in-flight checkpoint being written (e.g. on pause).
- `recovering`: handed to the Recovery Workflow Engine.

Epics and Features are not directly schedulable; they aggregate.

### 2.3 Worker

A reusable execution profile. Referenced by Tasks/Subtasks; does not
hold runtime capability configuration.

Key fields: id, tenant, name, description, purpose, runtime_ref,
model_ref, system_prompt, context_sources, permissions, budget_overrides,
concurrency_limit, execution_policy_ref, version.

Workers are **versioned**. A Worker reference is `(worker_id, version)`;
updating a worker creates a new version. Active executions pin a
version; new dispatches use the latest version unless overridden.

Workers do **not** own: MCP servers, tools, terminal access, plugins,
file-edit capability. These are runtime capabilities advertised by the
adapter and gated by Policy at dispatch time.

Lifecycle: `draft → published → deprecated → retired`.

### 2.4 Workflow

A composable execution plan referencing Workers and Steps. Workflows
live at the project level (a Project's execution plan) or as reusable
templates at the tenant level.

Key fields: id, project_id (nullable for templates), name, version,
steps, inputs, outputs, recovery_policy_ref.

A **Step** is: id, kind (`task` | `decision` | `approval` | `parallel`
| `recover`), ref (Task template or sub-workflow), depends_on,
gate_policy_ref.

Lifecycle: `draft → published → running → completed | failed | aborted`.

A **bound run** (docs/11 §2.1) is a WorkflowRun whose `work_item_id`
links it directly to a work item. Successive `task` steps reassign
different Workers onto the same bound WorkItem in place, without
spawning Subtasks. Bound runs are the unit of template-based execution.

Workflows are the unit that the Scheduler treats as the top-level
reconcilable object for execution; Tasks are reconciled as children.

### 2.5 Policy

First-class, versioned, reusable rules evaluated by the Policy Engine
at well-defined decision points. The Policy layer is intentionally
**basic by default**: a small set of lifecycle decision points, always
on, sufficient to govern autonomous work without evaluating policy on
every adapter call.

**Two tiers, deliberately separate:**

- **Tier 1 — Decision-point policy (v0.1 baseline, always-on).**
  Evaluated at a handful of lifecycle transitions per task, not per
  tool call. This is the governance floor for a control plane that
  enterprises trust to run autonomous agents with terminal/file/git
  access. Implemented in **Rego** (Open Policy Agent); no imperative
  hooks in v0.1.

  Decision points (Tier 1):
  - **admission** — may this work item be created/started?
  - **dispatch** — may this worker begin this task on this runtime?
  - **budget** — is spend/time within envelope?
  - **approval** — does this transition require a human?
  - **recovery** — which recovery workflow applies?
  - **completion** — may this task be marked succeeded?

- **Tier 2 — Per-tool-call gating (opt-in per Worker, narrow in v0.1).**
  Evaluated in-flight only for high-risk tool categories the Worker
  explicitly declares as gated. Everything else flows ungated once
  dispatch policy passes. v0.1 gates only `terminal`, `web_fetch`,
  and `git` operations; other tool categories are not gated. This is
  the escape valve for cases where coarse dispatch-time approval is
  insufficient (e.g. autonomous `git push`, outbound network to
  non-allowlisted hosts, destructive file ops).

  Go hooks (imperative escape hatch for policies needing live data
  such as real-time AI Gateway spend) are **deferred to v0.2**. Every
  v0.1 decision point is expressible in Rego with the context
  available at evaluation time.

Policy shape: id, tenant, name, version, scope
(`tenant` | `project` | `worker` | `task`), decision_point, rules
(Rego module reference + input bindings), effect
(`allow` | `deny` | `require_approval` | `require_review`).

Policies are evaluated in order of narrowest scope first; first
definitive decision wins. The `Explain` RPC returns the Rego
evaluation trace; no hook trace is needed in v0.1.

Lifecycle: `draft → published → superseded`.

### 2.6 Recovery Workflow

A specialized Workflow that runs when a worker becomes unhealthy or
stalls. See `06_Recovery_Workflow_Engine.md` for the engine; this
section fixes only the entity.

Key fields: id, tenant, name, version, trigger_conditions, steps,
default_for (project_id | tenant | global).

Lifecycle: `draft → published → superseded`.

A single **default** recovery workflow exists per tenant; projects may
override.

### 2.7 WorkerExecution (runtime entity)

A concrete invocation of a Worker against a Task on a specific Runtime
Adapter instance. Created by the scheduler at dispatch; owns the
adapter session.

Key fields: id, task_id, worker_ref (id+version), runtime_adapter_id,
status, started_at, checkpoints (ref), token_usage, cost, health_state.

Lifecycle:
`dispatching → running → healthy | stalled | unhealthy → terminating → terminated`

`health_state` is the union of: heartbeat freshness, progress rate,
error rate, context-window usage, runtime-reported health.

### 2.8 RuntimeAdapter (registration entity)

A registered adapter process offering execution capabilities.

Key fields: id, kind (`opencode` | `claude-code` | `codex` | …),
version, capabilities, endpoint, registered_at, last_heartbeat.

Capabilities are advertised at registration and re-negotiated per
execution (see `04_Runtime_Adapter_SDK.md`).

---

## 3. Relationships

```
Tenant
 └─ Project
     ├─ WorkItem (Epic/Feature/Task/Subtask) ──┐
     │      └─ WorkItemDependency (DAG edges)  │
     ├─ Workflow ──── Step ──── TaskTemplate   │ references
     ├─ Policy (project-scoped)                │
     └─ WorkerExecution ─── Worker (tenant) ───┘
              └── RuntimeAdapter (tenant)

Tenant (also)
 ├─ Worker (reusable)
 ├─ Workflow (template)
 ├─ Policy (tenant-scoped)
 └─ RecoveryWorkflow (template)
```

Notable relationship rules:

- A **Worker** is owned by the tenant and referenced by Projects. This
  enables drag-and-drop reuse across projects (per the parent doc).
- A **Workflow** may be a template (tenant) or an instance (project).
- A **WorkerExecution** always belongs to exactly one Task and pins one
  Worker version.
- A **Task** may have many sequential WorkerExecutions over its life
  (retries, recovery hand-offs).
- **Policies** attach at any scope; narrowest-scope match wins.

---

## 4. Lifecycle Invariants

1. No WorkItem may transition to `running` unless its dependencies are
   in a terminal-success state.
2. No Task may be marked `succeeded` unless its completion policy has
   evaluated `allow`, OR the Reviewer Worker during recovery deems it
   complete, OR a human marks it complete. All three paths produce an
   audit event with the actor recorded.
3. A Worker may be `retired` only when no active WorkerExecution pins
   its version; otherwise the retire is queued.
4. A Workflow step may reference a deprecated Worker only if the
  Workflow itself is deprecated.
5. The model selection (provider + model) is deterministic: the human
   defining the Worker specifies the exact model; the system does not
   automatically select or failover to alternate models.

---

## 5. Resolved Decisions (v0.1)

- **Epics/Features**: organizational labels only. Only Tasks and
  Subtasks are schedulable. The scheduler's state machine covers one
  kind of schedulable unit; Epics and Features group Tasks for human
  readability, project management, and UI hierarchy.
- **Worker inheritance/templates**: composition via Workflows only.
  Workers are single-role; teams are composed by wiring multiple
  Workers into a Workflow. No Worker-extends-Worker, no template
  inheritance. If you need a variant, create a new Worker.
- **Cost attribution**: WorkerExecution is the atomic unit of cost.
  Every token/cost record FKs to a WorkerExecution. Task, Project,
  and Tenant costs are **derived roll-up queries** — not stored
  separately — with full drill-down capability: the API, dashboards,
  and reports support navigating from a Tenant roll-up → Project →
  Task → individual WorkerExecution cost breakdowns, and vice versa.
  This keeps storage simple (one FK) while giving users multi-level
  cost visibility with breakdowns at every level.
