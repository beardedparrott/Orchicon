# Orchicon — Scheduler & Runtime Design

> **Version:** 0.1
> **Status:** Direction & design intent
> **Parent:** `01_Architecture_Vision.md`

This document covers the continuously-running control loop that
reconciles desired project state with observed runtime state, dispatches
work to runtime adapters, and drives health monitoring and lifecycle
management.

---

## 1. Design Intent

- **Reconcile, don't execute.** The scheduler never performs AI work.
  It moves objects between states and delegates execution to adapters.
- **Idempotent applies.** Re-running a reconcile pass for an object must
  converge to the same state; side effects are guarded by status
  transitions.
- **De-duplicated work queues.** Each object is reconciled by at most
  one goroutine at a time, per replica, with leadership per kind.
- **No silent failures.** A reconcile error advances a retry budget and
  surfaces as an event + telemetry span.

---

## 2. Reconciler Architecture

### 2.1 Reconciler kinds

Each top-level entity has a dedicated reconciler:

- `ProjectReconciler` — lifecycle, budget enforcement, child health.
- `WorkflowReconciler` — step DAG progression, gate evaluation.
- `TaskReconciler` — dependency resolution, dispatch decision,
  completion policy.
- `WorkerReconciler` — Worker version lifecycle, retirement queue.
- `PolicyReconciler` — publication and supersession.
- `RuntimeAdapterReconciler` — registration health, deregistration.

A reconciler is a pure function:

```
Reconcile(ctx, key) -> Result{RequeueAfter, Error}
```

### 2.2 Work queue

- Each reconciler owns a de-duplicating work queue keyed by object ID.
- Enqueue sources: API mutations, outbox events, scheduled requeues,
  telemetry-triggered health transitions.
- A single enqueue per object ID per tick collapses coalesced events.
- Backoff on error: exponential with jitter, capped; after N failures
  the object is marked `degraded` and a human-notified event is fired.

### 2.3 Leadership & scaling

- Multiple control-plane replicas run concurrently.
- **Leader election is per reconciler kind**, not global, via Postgres
  advisory locks (`pg_try_advisory_lock(hash(kind))`). A replica that
  holds the lock for a kind runs that kind's work queue; others stand
  by.
- Standby replicas still serve the API and consume telemetry; they
  simply do not reconcile.
- Lock lease is renewed on a heartbeat; on loss, the reconciler
  drains in-flight and stops enqueuing.

This intentionally avoids etcd. Postgres is already the source of
truth, so it is also the lock authority.

---

## 3. Scheduling Inputs

The TaskReconciler evaluates the following when deciding dispatch:

| Input | Source |
|---|---|
| Task status & dependencies | Postgres (WorkItem graph) |
| Worker availability | Concurrency counters (in-memory + DB-backed) |
| Worker health | RuntimeAdapter heartbeats + WorkerExecution health_state |
| Policy decisions | Policy Engine (admission, dispatch, budget) |
| Budgets (token/cost/time) | AI Gateway accounting + Task budgets |
| Resource utilization | Adapter-reported + cluster-wide metrics |
| Project state | Project status & pause/active flag |
| Priority | WorkItem priority field + Workflow step gates |

---

## 4. Dispatch Flow

```
TaskReconciler.Reconcile(taskID):
  if status != ready: return
  if deps not satisfied: requeue
  worker = selectWorker(task)            # respects policy + availability
  adapter = selectAdapter(worker)        # respects runtime_ref + health
  if policy.dispatch(worker,task) != allow:
     requeue with delay or require_approval
  exec = createWorkerExecution(task, worker, adapter)
  call adapter.Start(exec, manifest)      # gRPC, see 04_Runtime_Adapter_SDK
  task.status = assigned
  emit event task.assigned
  requeue after heartbeat interval
```

### 4.1 Worker selection

- Filter candidates by `runtime_ref` and `model_ref` compatibility
  with the task's requirements.
- Filter by `concurrency_limit` (current in-flight < limit).
- Filter by health state `healthy`.
- Rank by: project-affinity, lowest utilization, least-recently-used.
- Deterministic tiebreak: lexicographic worker id (so re-reconciliation
  is stable).

### 4.2 Adapter selection

- Among registered adapters of the matching `kind`, prefer those
  advertising the required capabilities.
- Prefer adapters with recent healthy heartbeats.
- Allow adapter affinity: a WorkerExecution may pin to its adapter
  across reconnects within an execution.

---

## 5. Health Monitoring

A dedicated `HealthMonitor` consumes adapter heartbeats and telemetry
events and recomputes `WorkerExecution.health_state` on each signal:

- `healthy` — heartbeats fresh, progress non-zero, errors below threshold.
- `stalled` — heartbeats fresh but no progress for `stall_window`.
- `unhealthy` — heartbeat stale OR error rate above threshold OR
  runtime-reported unhealthy OR context-window exhausted.
- `terminating` — graceful shutdown in progress.

### Trigger thresholds (defaults, per-policy overridable)

| Signal | Default | Triggers |
|---|---|---|
| Heartbeat age | > 60s | `unhealthy` |
| No progress | > 5 min | `stalled` |
| Context usage | > 90% | `unhealthy` (recoverable) |
| Error rate | > 30% / 5 min | `unhealthy` |
| Cost overrun | > 100% of budget | `unhealthy` + policy halt |

On transition into `stalled` or `unhealthy`, the TaskReconciler hands
the WorkerExecution to the **Recovery Workflow Engine**
(see `06_Recovery_Workflow_Engine.md`).

---

## 6. Lifecycle Management

The scheduler owns the following transitions and nothing else:

- `ready → assigned` (dispatch)
- `assigned → running` (adapter confirms start)
- `running → checkpointing` (on pause/evict)
- `checkpointing → running` (resume)
- `running → recovering` (hand to Recovery Engine)
- `recovering → ready` (recovery completes; re-dispatched)
- `running → succeeded|failed|cancelled` (terminal, policy-gated)

Recovery outcomes that resume execution route back through `ready` and a
fresh dispatch — they do not bypass the scheduler.

---

## 7. Concurrency & Fairness

- Per-project fairness: a weighted round-robin across ready tasks so a
  single busy project cannot starve others.
- Per-tenant concurrency caps enforced globally (sum of in-flight
  WorkerExecutions per tenant).
- Adapter capacity is a hard ceiling; over-subscription queues at the
  control plane, never at the adapter.
- Priority is advisory: a high-priority task preempts only if its
  Policy grants preemption; default is no preemption.

---

## 8. Failure Modes & Invariants

- **Reconciler crash mid-pass**: object remains in its pre-transition
  state; the next pass re-derives. No partial side effects because
  adapter calls are gated by a status CAS.
- **Adapter unreachable mid-dispatch**: dispatch times out, the
  WorkerExecution is marked `failed_to_start`, the task requeues with
  backoff and an alternate adapter may be selected.
- **Postgres unavailable**: control plane stops accepting mutations;
  read paths degrade; in-flight executions continue under their
  adapter's local policy but cannot complete through the scheduler.
- **NATS unavailable**: events accumulate in the outbox; reconcilers
  continue operating off direct DB reads; downstream consumers see a
  backlog until NATS recovers.

Invariants:

1. The scheduler is the only component permitted to call
   `adapter.Start`. API and recovery both go through it.
2. No object is reconciled by two replicas simultaneously for the same
   kind.
3. Every dispatch decision produces a Policy evaluation span and an
   event — no "implicit" dispatches.
4. Health state is recomputed from signals, not asserted; only the
   HealthMonitor writes `health_state`.

---

## 9. Resolved Decisions (v0.1)

- **Preemption**: deferred to v0.2. v0.1 is non-preemptive; tasks run
  to completion or terminal state. Priority affects dispatch order
  only, not in-flight work.
- **Worker selection**: rule-based for v0.1. Filter by
  runtime/model compatibility and health, rank by project-affinity +
  lowest utilization + LRU. Deterministic, debuggable, explainable
  in the UI. Learned affinity models deferred.
- **Reconciler leadership granularity**: per reconciler kind for v0.1.
  One leader per kind (ProjectReconciler, TaskReconciler, etc.).
  Sharding (per kind × shard) deferred until v0.1's workload demands it.
- **Capacity planning**: reactive scaling for v0.1. The scheduler
  dispatches what adapters can handle; if capacity is exhausted, tasks
  queue at the control plane. Operators scale adapters manually based
  on queue depth metrics. Proactive capacity forecasting deferred.
