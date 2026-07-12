# Orchicon — Event Bus & Telemetry Model

> **Version:** 0.1
> **Status:** Direction & design intent
> **Parent:** `01_Architecture_Vision.md`

This document fixes the event taxonomy, the durable backbone, the
telemetry signal model, and the mapping between them. It is the
contract every other component relies on for "no silent paths."

---

## 1. Design Intent

- **Two channels, one model.** Control events (state transitions,
  mutations) and telemetry signals (traces/metrics/logs) share an
  OTel-native shape but flow through different sinks for different
  durability/throughput characteristics.
- **Postgres is the source of truth; NATS is the bus.** Mutations
  write to Postgres + the transactional outbox; the outbox drains to
  NATS JetStream. Telemetry bypasses Postgres entirely — it flows from
  the producer to the OTel collector to **SigNoz** (ClickHouse-backed).
- **Telemetry is append-only.** Corrections are new events, never
  mutations of historical signals.
- **Events are the integration boundary.** External systems subscribe
  to NATS subjects; webhooks are projections of the same events
  (per `07_API_Specification.md`).

---

## 2. Event Bus — NATS JetStream

### 2.1 Why NATS JetStream
- Durable, at-least-once delivery with consumer-side idempotency.
- Lightweight: no Kafka ops burden, single binary.
- Native stream semantics (subjects, consumers, retention) without a
  separate broker to operate alongside Postgres.
- Adequate throughput for control-plane event volume; v0.1 does not
  require partitioned streaming.

### 2.2 Stream layout

| Stream | Subject prefix | Retention | Purpose |
|---|---|---|---|
| `ORCHICON.CONTROL` | `orchicon.control.>` | interest + 7d max-age | state transitions, mutations, lifecycle |
| `ORCHICON.EXEC` | `orchicon.exec.>` | interest + 30d | WorkerExecution lifecycle + health |
| `ORCHICON.RECOVERY` | `orchicon.recovery.>` | interest + 90d | recovery workflow events |
| `ORCHICON.POLICY` | `orchicon.policy.>` | interest + 90d | policy decisions (allow/deny/explain) |
| `ORCHICON.AUDIT` | `orchicon.audit.>` | work-queue + 1y | auth/RBAC decisions, sensitive ops |
| `ORCHICON.TASKS` | `orchicon.tasks.>` | work-queue + 30d | scheduler work items (reconciler feeds) |

Telemetry does **not** flow through NATS. Tool calls, traces, metrics,
and logs go directly to the OTel collector. NATS is for control and
domain events only.

### 2.3 Subject taxonomy

```
orchicon.control.{tenant}.{entity}.{action}
orchicon.exec.{tenant}.{project}.{execution_id}.{signal}
orchicon.recovery.{tenant}.{project}.{recovery_id}.{step}
orchicon.policy.{tenant}.{decision_point}.{effect}
orchicon.audit.{tenant}.{identity}.{action}
orchicon.tasks.{kind}.{event}
```

Examples:
- `orchicon.control.acme.project.paused`
- `orchicon.exec.acme.proj_01.exe_42.health.unhealthy`
- `orchicon.recovery.acme.proj_01.rec_7.step.review.completed`
- `orchicon.policy.acme.dispatch.deny`

### 2.4 Outbox & delivery guarantees

- All control mutations write to Postgres and the `outbox` table in the
  same transaction. A background relay publishes to NATS and marks
  rows published.
- **At-least-once** delivery; consumers must be idempotent. Every event
  carries `event_id` (ULID) + `aggregate_id` + `aggregate_version` for
  deduplication and ordering per aggregate.
- Per-aggregate ordering is guaranteed within a stream; cross-aggregate
  ordering is not.

---

## 3. Event Envelope

All events share a canonical envelope:

```json
{
  "event_id": "01HXY...",
  "event_type": "task.assigned",
  "tenant_id": "acme",
  "aggregate": {"type": "Task", "id": "...", "version": 7},
  "occurred_at": "2026-07-09T12:00:00Z",
  "actor": {"type": "system" | "user" | "adapter", "id": "..."},
  "trace_id": "...",
  "span_id": "...",
  "correlation_id": "...",
  "payload": { /* event-specific */ }
}
```

- `trace_id`/`span_id` join every event to OTel; consumers may enrich
  with full trace context.
- `correlation_id` propagates from the originating API request through
  reconcilers, adapters, and recovery — the unit of "this came from
  one user action."
- `aggregate.version` is the post-event version of the aggregate;
  consumers detect ordering gaps by monotonic version per aggregate.

---

## 4. Event Taxonomy (selected)

Not exhaustive; the schema is the source of truth.

### 4.1 Control
- `project.created | updated | archived | paused | resumed`
- `workitem.created | updated | dependency.added | dependency.removed`
- `worker.version.published | deprecated | retired`
- `workflow.published | started | step.transitioned | completed | aborted`
- `policy.published | superseded`

### 4.2 Execution
- `execution.dispatching | started | healthy | stalled | unhealthy`
- `execution.checkpoint.written | checkpoint.replayed`
- `execution.paused | resumed | cancelled`
- `execution.completed` (terminal, pre-Policy)
- `execution.failed` (terminal)

### 4.3 Recovery
- `recovery.triggered` (carries trigger reason + level)
- `recovery.step.started | completed | failed`
- `recovery.plan.produced | plan.approved | plan.rejected`
- `recovery.escalated` (L1→L2→L3)
- `recovery.resumed` (handoff back to scheduler)

### 4.4 Policy
- `policy.evaluated` (carries decision_point, effect, trace_ref)
- `policy.budget.breach` (with severity)

### 4.5 Audit
- `auth.login | token.issued | token.revoked`
- `rbac.decision` (identity, resource, action, allowed)
- `adapter.registered | deregistered`

---

## 5. Telemetry Signals (OTel)

Telemetry is OTel-native, exported via OTLP to a collector fronting
**SigNoz** — an open-source, Apache 2.0 licensed, OTel-native
observability platform backed by ClickHouse. SigNoz provides a single
unified UI and query model (builder + SQL escape hatch) across traces,
metrics, and logs, replacing the fragmented Tempo/Mimir/Loki stack.
All three signal types land in one ClickHouse store, enabling
cross-signal joins (e.g. "show me the log lines from the span whose
trace_id matches this slow API call") without a separate datasource.

### 5.1 Trace topology

Every meaningful action is a span. Canonical spans:

| Span name | Producer | Notes |
|---|---|---|
| `api.<service>.<rpc>` | API layer | root for user-initiated actions |
| `reconcile.<kind>.<id>` | Scheduler | root for control-loop work |
| `policy.evaluate.<decision_point>` | Policy Engine | child of caller |
| `dispatch.<task_id>` | Scheduler | wraps adapter.Start |
| `adapter.execute.<execution_id>` | Adapter | long-lived, per WorkerExecution |
| `adapter.tool_call.<tool>` | Adapter | child of execute |
| `recovery.<recovery_id>.<step>` | Recovery Engine | |
| `gateway.<provider>.<model>.request` | AI Gateway | per LLM call |

A single `correlation_id` propagates across all spans in a user action;
the OTel `Link` mechanism ties a recovery span to its originating failed
execution span without forcing them into one trace.

### 5.2 Metrics (selected)

- `orchicon_tasks_total{tenant,project,status}`
- `orchicon_executions_active{tenant,worker,adapter}`
- `orchicon_dispatch_latency_seconds` (histogram)
- `orchicon_recovery_total{tenant,trigger,outcome,level}`
- `orchicon_policy_decisions_total{decision_point,effect}`
- `orchicon_tokens_consumed{tenant,project,worker,provider,model}`
- `orchicon_cost_usd{tenant,project,provider,model}`
- `orchicon_context_usage_ratio` (gauge per execution)
- `orchicon_reconcile_requeues_total{kind,reason}`
- `orchicon_outbox_lag` (gauge — relay health)

### 5.3 Logs

- Structured (OTel log records), carrying `trace_id` + `correlation_id`.
- High-volume adapter transcripts are **not** logged as OTel logs —
  they are stored as artifacts (object store) with metadata in Postgres,
  referenced from spans. This keeps ClickHouse log volume bounded.

### 5.4 Telemetry retention

Retention is configured in SigNoz/ClickHouse per signal type. Cold
tiering moves aged data to object storage.

| Signal | Hot retention (ClickHouse) | Cold retention (object store) |
|---|---|---|
| Traces | 7d | 90d |
| Metrics | 30d (raw) | 13 months (1h rollup) |
| Logs | 14d | 90d |
| Transcripts/artifacts | 30d hot | 1y cold (per Policy) |

---

## 6. Mapping: Events ↔ Telemetry

| NATS event field | OTel equivalent |
|---|---|
| `event_id` | OTel log record `event_id` |
| `trace_id` / `span_id` | span context (joins event to trace) |
| `correlation_id` | OTel baggage / span attribute |
| `aggregate.version` | OTel span attribute (consumer-side) |
| `actor` | span attribute `orchicon.actor.*` |

Every event MUST carry a valid span context; an event without a
`trace_id` is a bug, not a feature. The outbox relay refuses to publish
events lacking trace context.

---

## 7. Consumers & Projections

- **Frontend** subscribes via streaming RPCs (server-stream) that
  project NATS subjects to the client (see `07_API_Specification.md`).
- **Webhooks** are a projection: a subscription entity maps event
  filters to outbound HTTP deliveries with retries + dead-letter.
- **Data warehouse / BI** consumes `ORCHICON.CONTROL` and a curated
  metrics feed via a dedicated NATS export, not direct DB reads.
- **Reconcilers** consume `ORCHICON.TASKS` as their work queue in
  addition to Postgres-driven requeues.

---

## 8. Failure Modes & Invariants

- **NATS down**: outbox accumulates in Postgres; reconcilers continue
  off DB reads; frontend realtime degrades (reconnect on recovery).
- **OTel collector down**: telemetry is dropped with bounded in-process
  buffering; control events still publish when NATS recovers. Losing
  telemetry does not block control flow.
- **Outbox relay lag**: `orchicon_outbox_lag` metric alerts before
  it harms correctness. Lag > threshold triggers reconciliation against
  raw DB state.

Invariants:

1. No component publishes to NATS except the outbox relay and the
   (separate) telemetry producers. Adapters never touch NATS.
2. Every control mutation produces exactly one outbox row in the same
   transaction; partial publication is impossible.
3. Every event carries `trace_id`; events without it are rejected.
4. Telemetry is never mutated; corrections are new signals.
5. Telemetry loss never corrupts control state, because control state
   is reconciled from Postgres, not from telemetry.

---

## 9. Resolved Decisions (v0.1)

- **Control event wire format**: Protobuf-encoded on the wire. Control
  events on NATS use the same Protobuf schema as the API — no separate
  JSON schema registry, no drift. Consumers use generated types.
  Binary encoding is smaller and faster than JSON.
- **Kafka-compatible NATS facade**: deferred. v0.1 uses NATS JetStream
  only. If an enterprise customer needs Kafka integration, a separate
  bridge process (NATS → Kafka) can be added without changing the
  control plane. The project is initially for single-user / small-team
  use; enterprise Kafka integration is not a day-1 need.
- **Encryption at rest for transcripts/artifacts**: SSE for v0.1, with
  a local option. The object store (S3/MinIO) handles encryption with
  its own keys for cloud deployments. For fully-local deployments
  (LocalFilesystemBlobStore), encryption at rest uses file-system-level
  encryption (e.g. LUKS) or OS-level security; the BlobStore interface
  does not mandate app-level encryption in v0.1. Per-tenant data keys
  (envelope-encrypted via KMS/Vault) are a v0.2 compliance feature for
  orgs that require it (FedRAMP, healthcare).
