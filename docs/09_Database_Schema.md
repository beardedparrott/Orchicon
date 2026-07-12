# Orchicon — Database Schema

> **Version:** 0.1
> **Status:** Direction & design intent
> **Parent:** `01_Architecture_Vision.md`

This document fixes the storage direction: Postgres as the source of
truth, table groups, key relationships, the transactional outbox, and
the migrations policy. It is not a final DDL — concrete columns/types
live in the Atlas migrations and evolve independently of this doc.

---

## 1. Design Intent

- **Postgres is the source of truth.** Every durable control-plane
  state lives here. NATS, caches, and telemetry stores are derivable;
  losing any of them must not corrupt control state.
- **One schema, logically partitioned by tenant.** All tables carry
  `tenant_id`; isolation is enforced at **two layers**: the data-access
  layer (primary, where tenant context is known and queries are built)
  and Postgres Row-Level Security (backstop, catches cases the layer
  misses). RLS policies are uniform: `USING (tenant_id = current_setting('app.tenant_id'))`.
  The data-access layer sets `app.tenant_id` per transaction; RLS
  refuses rows outside that tenant regardless of the SQL issued.
- **Append-mostly where it matters.** History tables (executions,
  events, telemetry refs) are append-only; mutable tables (projects,
  workers) use optimistic concurrency via `version` columns.
- **Migrations are reviewable.** Atlas declarative schema, diffed in
  PRs, applied by the control plane on startup with advisory locking.
- **No business logic in the database.** Triggers are limited to
  `updated_at` and outbox relay; no rules in PL/pgSQL. RLS is
  isolation enforcement, not business logic.

---

## 2. Driver

- `pgx` native driver (not `database/sql`) with `pgxpool`.
- Connection pool sized per replica; prepared statement caching on.
- Read-only replicas deferred to v0.2; v0.1 reads from the primary
  with appropriate indexes.

---

## 3. Table Groups

### 3.1 Identity & Tenancy
- `tenants` — tenant root, budget envelope, default policies.
- `identities` — users and service accounts (OIDC subject, API keys).
- `api_keys` — hashed keys, scopes, rotation state.
- `roles`, `role_bindings` — RBAC (tenant-scoped, optionally
  project-scoped).
- `entitlements` — derived view of `(identity, resource, action)`.

### 3.2 Projects & Work Hierarchy
- `projects` — id, tenant, name, slug, status, goals (jsonb),
  budget_envelope (jsonb), default_policy_refs, timestamps.
- `work_items` — id, tenant, project, parent_id, kind
  (epic/feature/task/subtask), title, description, acceptance_criteria,
  status, assigned_worker_ref (jsonb: id+version), workflow_id,
  priority, budgets (jsonb), context_window, results (jsonb), version,
  timestamps.
- `work_item_dependencies` — from_id, to_id, type. Cycles rejected
  at admission (closure-table check or recursive CTE validation).
- `work_item_history` — append-only audit of transitions.

### 3.3 Workers
- `workers` — id, tenant, name, slug, description, purpose,
  current_version, status, created_at, created_by.
- `worker_versions` — worker_id, version, snapshot (jsonb: system_prompt,
  model_ref, model_fallbacks, context_sources, permissions, budget_overrides,
  execution_policy_ref, concurrency_limit, recovery_workflow_ref),
  version_note, published_at, status.
  - Worker mutable state lives here; `workers` is the immutable header.
- `worker_executions` — id, task_id, worker_id, worker_version,
  adapter_id, status, health_state, started_at, ended_at,
  token_usage, cost_usd, checkpoint_ref, recovery_id, version.

### 3.4 Workflows
- `workflows` — id, tenant, project_id (nullable for templates), name,
  current_version, status, timestamps.
- `workflow_versions` — workflow_id, version, steps (jsonb), inputs,
  outputs, recovery_policy_ref, version_note, status.
- `workflow_runs` — id, workflow_id, workflow_version, project_id,
  status, started_at, ended_at, current_step, run_context (jsonb).
- `workflow_step_runs` — id, workflow_run_id, step_id, status,
  started_at, ended_at, attempt, result (jsonb).

### 3.5 Policies
- `policies` — id, tenant, name, current_version, scope, status.
- `policy_versions` — policy_id, version, decision_point, rules (jsonb),
  effect, version_note, status.
- `policy_decisions` — append-only log: id, tenant, policy_version_id,
  decision_point, subject (identity or execution), resource, effect,
  reason, trace_id, decided_at. (Projected to telemetry; also queryable.)

### 3.6 Recovery
- `recovery_workflows` — id, tenant, name, current_version, default_for
  (tenant | project_id), status.
- `recovery_workflow_versions` — id, version, steps, trigger_conditions
  (jsonb), version_note, status.
- `recovery_executions` — id, triggered_by_execution_id, task_id,
  workflow_version_id, status, level, trigger_reason, budget, budget_used,
  continuation_plan_ref, started_at, ended_at.
- `recovery_steps` — id, recovery_execution_id, step_id, status,
  started_at, ended_at, attempt, result (jsonb), worker_execution_id.

### 3.7 Runtime Adapters
- `runtime_adapters` — id, kind, version, endpoint, capabilities (jsonb),
  status, registered_at, last_heartbeat_at, max_concurrent_executions.
- `adapter_heartbeats` — append-only: adapter_id, ts, health (jsonb).
  Retained short-term (e.g. 7d) for stall/health analysis.

### 3.8 Checkpoints & Artifacts
- `checkpoints` — id, worker_execution_id, format_version, blob_ref,
  size_bytes, sha256, created_at.
- `artifacts` — id, tenant, project, kind (transcript | diff | plan |
  summary), blob_ref, mime_type, size_bytes, sha256, created_at,
  retention_class. Metadata in Postgres; blob in object store.

### 3.9 Eventing & Outbox
- `outbox` — id, aggregate_type, aggregate_id, aggregate_version,
  event_type, payload (jsonb), occurred_at, published_at, trace_id,
  correlation_id. Indexed on `(published_at IS NULL, occurred_at)`.
- `event_subscriptions` — id, tenant, filter (jsonb), delivery
  (webhook | nats_export), target, status, secret_ref. Powers webhooks.

### 3.10 AI Gateway
- `providers` — id, kind, config_ref (secret store), status.
- `usage_records` — id, tenant, project, worker_execution_id, provider,
  model, input_tokens, output_tokens, cost_usd, request_id, ts.
  Append-only; high-write; consider partitioning by time.

### 3.11 Scheduler & Locks
- `reconciler_leases` — kind, holder (replica id), acquired_at,
  renewed_at, expires_at. Backed by advisory locks at runtime; this
  table is for observability of leadership.
- `work_queue` (optional, derived) — materialized view of ready tasks
  per reconciler kind; not authoritative (Postgres is). v0.1 may skip
  materialization and derive on read.

---

## 4. Key Relationships (simplified)

```
tenants ───< projects ───< work_items ───< work_item_dependencies
                │              │
                │              └──< worker_executions >── workers
                │                       │                  │
                ├──< workflows ──< workflow_runs ──< workflow_step_runs
                │
                ├──< policies ──< policy_decisions
                │
                └──< recovery_executions ──< recovery_steps

runtime_adapters ──< adapter_heartbeats
worker_executions ──< checkpoints
worker_executions ──< artifacts (via project + execution)

outbox ──> (relayed to NATS)
usage_records ──> (high-volume, time-partitioned)
```

---

## 5. Concurrency Control

- Every mutable table has a `version` column; updates use
  `UPDATE ... WHERE id = $ AND version = $` and bump version. Lost
  updates are detected, not silently overwritten.
- Long-running operations (workflow step transitions, recovery) use
  status CAS, not row locks held open.
- Advisory locks (`pg_advisory_lock`) for reconciler leadership, keyed
  by `hash(kind)`; released on shutdown or lease expiry.
- No `SELECT FOR UPDATE` held across external calls (adapter RPCs).
  The pattern is: CAS the status to `running`, release the row, do the
  RPC, CAS the result back.

---

## 6. Outbox Pattern

```
BEGIN;
  UPDATE work_items SET status='assigned', version=version+1 ...;
  INSERT INTO outbox(aggregate_type, aggregate_id, aggregate_version,
                     event_type, payload, trace_id, correlation_id, ...);
COMMIT;
```

A single background relay per replica:
- Polls `outbox WHERE published_at IS NULL ORDER BY occurred_at LIMIT N`.
- Publishes to NATS with the envelope from §3 of
  `08_Event_Bus_and_Telemetry_Model.md`.
- Marks rows `published_at = now()` (idempotent — relay uses
  `event_id` for NATS dedup).
- Lag metric exported; alert before correctness suffers.

Multiple replicas may run the relay; NATS deduplication on `event_id`
makes concurrent publication safe.

---

## 7. Indexing Direction

- All tables: primary key on ULID `id`; tenant + project scoping indexes.
- Hot paths indexed:
  - `work_items (project_id, status, priority)` for scheduler reads.
  - `worker_executions (worker_id, status)` for concurrency counts.
  - `worker_executions (status, health_state)` for health monitor.
  - `outbox (published_at IS NULL, occurred_at)` partial index.
  - `usage_records (tenant_id, ts DESC)` partitioned by month.
- Partial indexes on status enums for the common "ready to reconcile"
  queries (e.g. `WHERE status='ready'`).

---

## 8. Migrations & Schema Review

- **Atlas** declarative schema as source of truth; HCL or SQL fragments
  in the repo.
- PRs include the generated diff; schema review is part of code review.
- Control plane applies migrations on startup with a cluster-wide
  advisory lock (`pg_advisory_lock(hash('orchicon-migrate'))`); only
  one replica migrates at a time.
- Migrations are **forward-only**; rollbacks are new forward migrations.
- Backward-incompatible migrations ship in three phases: add →
  dual-write → drop, with a release between each.
- **RLS enforcement at migration time**: a migration-time check refuses
  to apply a schema in which any `tenant_id`-bearing table lacks an
  RLS policy. This prevents silent holes where a new table is created
  without the backstop. The check runs as a CI gate before the
  migration applies.

---

## 8.5. Row-Level Security (Backstop)

Tenant isolation is enforced at **two layers**:

1. **Data-access layer (primary).** Every query goes through a
   tenant-scoped data-access layer in Go that injects `tenant_id` into
   every `WHERE` clause and `INSERT` value. No raw SQL exists outside
   this layer. Developers reason about this layer day-to-day.

2. **Postgres RLS (backstop).** Every `tenant_id`-bearing table has an
   RLS policy:
   ```sql
   ALTER TABLE <table> ENABLE ROW LEVEL SECURITY;
   CREATE POLICY tenant_isolation ON <table>
     FOR ALL
     USING (tenant_id = current_setting('app.tenant_id', true)::text);
   ```
   The data-access layer sets the session variable per transaction:
   `SET LOCAL app.tenant_id = $1`. RLS refuses rows outside that tenant
   regardless of the SQL issued.

### Properties

- A bug in the data-access layer (missing `tenant_id` filter) **cannot**
  leak cross-tenant data, because Postgres blocks the row at the RLS
  layer.
- RLS policies are uniform and mechanical — generated from a convention,
  not hand-written per table.
- Administrative/DBA queries use a `BYPASSRLS` role to see all rows;
  this role is never used by the control plane.
- The `outbox` table is tenant-scoped via RLS so a buggy relay cannot
  publish another tenant's events.

### Migration-time enforcement

A CI gate verifies that every table with a `tenant_id` column has:
1. An enabled RLS policy matching the convention above.
2. No `BYPASSRLS` granted to any role used by the control plane.

A schema change introducing a `tenant_id` table without RLS fails the
CI gate and refuses to merge. This prevents RLS drift from schema
evolution — the most common way RLS backstops silently rot.

---

## 9. Retention & Growth Management

| Table | Growth | Retention |
|---|---|---|
| `usage_records` | high | partition monthly, drop after 13 months |
| `adapter_heartbeats` | high | 7 days |
| `policy_decisions` | high | 90 days hot, then aggregate to metrics |
| `work_item_history` | medium | project lifetime |
| `artifacts` (metadata) | high | 30d hot, 1y cold per Policy |
| `outbox` | high | purge published rows > 7 days |
| `checkpoints` | medium | execution lifetime + 30d |

Partitioning is applied early on the highest-growth tables
(`usage_records`, `outbox`, `adapter_heartbeats`) to avoid painful
migrations later. Partitioned tables inherit RLS policies automatically.

---

## 10. Cross-Cutting Invariants

1. No table may be written by a component other than the control plane
   (adapters and frontend never touch Postgres directly).
2. Every mutation produces an outbox row in the same transaction;
   partial publication is impossible by construction.
3. Every mutable row has a `version` column; lost-update detection is
   universal, not per-table.
4. No business logic in triggers or views — the schema is data, not
   behavior. RLS is isolation enforcement, not business logic.
5. Migrations are forward-only, reviewable, and three-phase for
   incompatible changes.
6. Every `tenant_id`-bearing table has an RLS policy; the CI migration
   gate refuses schemas where this invariant is violated.
7. The control plane's DB role never has `BYPASSRLS`; only the
   administrative/DBA role does, and it is never used by the app.

---

## 11. Resolved Decisions (v0.1)

- **Audit database**: co-locate in primary Postgres for v0.1.
  `policy_decisions` and RBAC audit events live alongside other
  control state. When rows age past hot retention (90 days), they're
  archived to object storage (Parquet/JSON) and queryable via
  SigNoz/ClickHouse. A separate audit database (WORM-style append-only
  system) is a **future feature** for compliance regimes that require
  audit data isolated from operational data — deferred to v0.2+.
- **Work-item dependency traversal**: recursive CTE for v0.1. Dependency
  traversal uses `WITH RECURSIVE` on the `work_item_dependencies` table.
  Simple, correct, performs well for typical graphs (a few hundred
  nodes per project). A closure table (pre-computed transitive closure)
  can be added later if dependency depth or query volume demands it
  without changing the API.
