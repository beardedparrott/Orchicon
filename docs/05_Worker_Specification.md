# Orchicon — Worker Specification

> **Version:** 0.1
> **Status:** Direction & design intent
> **Parent:** `01_Architecture_Vision.md`

This document specifies the **Worker** entity: its fields, lifecycle,
versioning, reuse model, and the boundary between Worker concerns and
runtime-adapter concerns. Workers are referenced from
`02_Domain_Model.md` and dispatched by `03_Scheduler_and_Runtime_Design.md`.

---

## 1. Design Intent

- A Worker is a **reusable execution profile**, not a runtime instance.
  It captures intent and governance, not execution machinery.
- Workers are **tenant-owned** and **project-referenced**, enabling the
  drag-and-drop composition described in the parent document.
- Workers are **versioned**; running executions pin a version.
- A Worker's permissions and budgets are **declarative and policy-
  enforceable**, never advisory.

---

## 2. Worker vs. Runtime — the hard boundary

| Owned by Worker | Owned by Runtime Adapter |
|---|---|
| Identity, purpose, model preference | Tool/MCP server manifests |
| System prompt | Terminal access, file-edit capability |
| Context sources (which to use) | Context retrieval mechanism |
| Permissions (what's *allowed*) | Capability advertisement (what's *possible*) |
| Budget overrides | Token/cost accounting feeds |
| Concurrency limit | Actual concurrency capacity |
| Execution policy reference | Health & checkpoint mechanics |

The Worker declares **what is permitted**; the Adapter advertises **what
is possible**; the Policy Engine resolves the intersection at dispatch.
A Worker cannot grant a permission the Adapter does not advertise, and
the Adapter cannot grant a capability the Worker forbids.

---

## 3. Worker Fields

### 3.1 Identity
- `id` — stable ULID
- `tenant_id`
- `name` — human label
- `slug` — URL-safe, unique within tenant
- `description`
- `purpose` — short statement of the worker's role
- `version` — monotonic integer
- `version_note` — optional changelog

### 3.2 Execution profile
- `runtime_ref` — adapter kind, e.g. `opencode`
- `model_ref` — exact model (vendor + model id); the AI Gateway routes
  to this model only; no automatic failover (see §11)
- `system_prompt` — the Worker's standing instructions; supports
  template variables (see §11)
- `context_sources` — ordered list of context source refs
  (project docs, prior summaries, retrieved docs, file trees)

### 3.3 Governance
- `permissions` — capability allowlist
  (subset of what adapters may advertise; intersection enforced)
- `gated_tools` — subset of `permissions` requiring in-flight per-call
  Policy evaluation (Tier 2); v0.1 limited to `terminal`, `web_fetch`,
  `git`; defaults to empty (Tier 1 only)
- `budget_overrides` — per-Worker ceilings for
  tokens / cost / wall-clock / tool-call count
- `execution_policy_ref` — Policy reference applied at dispatch
- `concurrency_limit` — max in-flight WorkerExecutions for this Worker
  (across all projects in the tenant)
- `recovery_workflow_ref` — which Recovery Workflow to use (default =
  tenant default)

### 3.4 Metadata
- `labels` — free-form key/value for filtering & UI grouping
- `created_at`, `updated_at`, `created_by`
- `status` — lifecycle state

---

## 4. Lifecycle

```
draft → published → deprecated → retired
```

- **draft** — editable, not dispatchable. New Workers start here.
- **published** — immutable except for creating a new version. Dispatch
  allowed. This is the only state from which `WorkerExecutions` may be
  created.
- **deprecated** — still dispatchable for in-flight Workflows that
  reference it, but no new Workflows may bind to it. New dispatches
  allowed only for pinned references in existing Workflows.
- **retired** — no new dispatches; existing in-flight executions run to
  completion. A Worker may be retired only when zero active executions
  pin its latest published version; otherwise the retire is queued.

A `published` Worker is **immutable**. To change anything, you create a
new version of the same Worker (new `version`, same `id`). Active
executions keep the version they started with; new dispatches use the
latest version unless the Workflow explicitly pins a version.

---

## 5. Versioning Model

- `(worker_id, version)` is the canonical reference.
- The "latest published version" is a derived view, not a stored pointer,
  so it cannot drift from the versions table.
- A Workflow step references either:
  - `latest` (resolved at dispatch time, pinned at execution start), or
  - a specific version (stable across the Workflow's life).
- Deprecating a version is allowed; retirement is per-Worker (all
  versions), not per-version.

---

## 6. Reuse & Composition

- Workers are referenced from Tasks/Subtasks and Workflow steps via
  `(worker_id, version)` or `latest`.
- The same Worker may be used across many Projects in the same tenant.
- Drag-and-drop composition (per the parent doc) is enabled by this
  reference model: a Project binds a Worker by reference, not by copy.
- Workers compose into **teams** via Workflows: a Workflow that wires a
  Reviewer Worker, an Architect Worker, and an Implementer Worker
  together is the unit of "AI team" — Workers themselves remain single-
  role.

---

## 7. Permission Model

Permissions are an **allowlist** of capabilities the Worker is
authorized to use. Resolution at dispatch:

```
allowed = worker.permissions ∩ adapter.advertised_capabilities
required = task.capabilities_required
if required ⊄ allowed: dispatch refused (Policy: deny)
```

Permission categories (mirroring adapter capabilities):

- `tools` — e.g. `file_edit`, `terminal`, `web_fetch`, `git`
- `mcp_servers` — named MCP servers (referenced by manifest id)
- `model_providers` — which providers the Worker may call
- `context` — which context sources the Worker may read
- `network` — outbound network classes permitted
- `filesystem` — path-prefix scopes for file access

Permissions default to **empty** (deny-by-default). Granting a
permission requires the Worker author to have the
`worker:grant:<category>` entitlement.

### 7.1 Per-tool-call gating (Tier 2, opt-in)

Permissions resolve at dispatch (Tier 1) and are sufficient for most
Workers. A Worker may additionally declare a **gated tool set**: tool
categories in this set route through in-flight Policy evaluation on
every call, even after dispatch has approved the Worker's general use
of that capability.

```
worker.permissions        → what the Worker MAY use (Tier 1, dispatch-time)
worker.gated_tools       → what must be re-approved per call (Tier 2, in-flight)
```

v0.1 supports gating on a narrow allowlist of high-risk categories:

- `terminal` — every shell command
- `web_fetch` — every outbound HTTP request
- `git` — every git operation (`push`, `commit`, etc.)

Other tool categories (`file_edit`, `mcp_servers`, etc.) flow ungated
once dispatch policy passes; gating them is a v0.2 concern. The intent
is to keep the default path basic (Tier 1 only) while preserving a
narrow escape valve for operations where coarse dispatch-time approval
is genuinely insufficient for unattended autonomous work.

When a gated tool call is initiated by the adapter, the control plane
evaluates the applicable Policy and either allows the call, denies it,
or routes an `ApprovalRequest` to a human (per
`04_Runtime_Adapter_SDK.md` §3, §4).

---

## 8. Budget Model

Budgets are layered, narrowest-wins:

```
task.budgets          (most specific)
  ← worker.budget_overrides
  ← project.budget_envelope
  ← tenant.budget_limits           (least specific)
```

Effective budget = min across the chain for each dimension
(tokens, cost, wall-clock, tool-call count). The AI Gateway and the
Adapter enforce hard stops at the effective budget; exceeding it is a
Policy-evaluated event (default: halt and trigger recovery).

---

## 9. Concurrency

- `concurrency_limit` is a tenant-wide ceiling on in-flight
  WorkerExecutions for this Worker, enforced by the Scheduler.
- A Worker may also declare a per-project cap via the execution policy.
- The Adapter's own `max_concurrent_executions` is a separate, harder
  ceiling; the Scheduler never dispatches beyond either.

---

## 10. Conformance Invariants

1. A Worker may not advertise a capability it does not permit — Workers
   only narrow, never expand, relative to the Adapter.
2. No Worker field is mutable in `published` state; mutations create a
   new version.
3. A `retired` Worker with zero pinned executions cannot be resurrected;
   a new Worker with a new id must be created. This preserves audit
   history.
4. Every WorkerExecution records the exact `(worker_id, version)` it
   pinned; replay and audit must be deterministic from this reference.
5. Budgets are enforced as hard stops, never soft warnings — exceeding
   is an event, not a log line.

---

## 11. Resolved Decisions (v0.1)

- **Worker templating**: composition via Workflows only. No
  Worker-extends-Worker. Workers are single-role; teams are composed
  by wiring multiple Workers into a Workflow. (Consistent with
  `02_Domain_Model.md`.)
- **`system_prompt` template variables**: supported. The system prompt
  supports a well-defined variable namespace (e.g. `{{project.name}}`,
  `{{task.title}}`, `{{worker.purpose}}`, `{{budget.remaining}}`)
  resolved by the control plane before sending to the adapter. This
  makes Workers reusable across projects with project-specific framing
  automatically. Variables are value-substituted with strict escaping
  (not prompt-concatenated) to prevent template injection.
- **Worker permission inheritance**: Policy's job. Workers don't
  inherit from each other. If a Worker needs additional permissions in
  a specific context, Policy grants them at dispatch based on
  Task/Project scope.
- **Model selection**: **human-defined, no automatic failover.** The
  Worker declares an exact `model_ref` (provider + model id); the AI
  Gateway routes to that exact model and that model only. The system
  does not decide models or runtimes on behalf of the user, and does
  not automatically failover to alternate models if the preferred one
  is unavailable. If the declared model is unavailable, the execution
  fails and the Recovery Workflow Engine handles it (which may prompt
  a human to update the Worker's model_ref). This keeps the system
  deterministic: the human designing the Workflow controls exactly
  which model executes each step. `model_fallbacks` is removed from
  the Worker spec; the AI Gateway handles routing and accounting only,
  not failover decisions.
