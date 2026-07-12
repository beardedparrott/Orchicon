# Orchicon — Recovery Workflow Engine

> **Version:** 0.1
> **Status:** Direction & design intent
> **Parent:** `01_Architecture_Vision.md`

This document specifies the engine that runs when a WorkerExecution
becomes unhealthy, stalls, exhausts its context, or breaches a retry
budget. The objective is **not** to restart failed workers — it is to
**preserve progress, minimize context loss, and resume forward motion**
with minimal human intervention.

---

## 1. Design Intent

- Recovery is **opt-out, not opt-in**. Every workflow recovers by
  default; disabling recovery is an explicit, audited choice.
- Recovery is a **workflow**, not a function. It is composed of the
  same primitives as any Workflow: Steps, Policies, Reviewer Workers.
- Recovery **never transitions a Task to `succeeded`** — only the
  completion Policy may. Recovery resumes execution; it does not assert
  completion.
- Recovery is **deterministic and replayable**: given the captured
  state and the recovery workflow version, replay yields the same
  continuation plan.
- Recovery is **bounded**: a recovery that itself stalls triggers a
  higher-order recovery with a tighter budget, and ultimately escalates
  to a human.

---

## 2. Triggers

Recovery is initiated by the HealthMonitor (per
`03_Scheduler_and_Runtime_Design.md`) when any of the following holds:

| Trigger | Source |
|---|---|
| `unhealthy` health state | heartbeat stale, runtime-reported fault |
| `stalled` health state | no progress within stall window |
| Context-window exhaustion | adapter reports > 90% usage with no checkpoint |
| Budget overrun | AI Gateway / adapter reports ceiling breach |
| Retry budget exhausted | Task retry counter exceeds policy |
| Adapter process death | heartbeat lease lost |
| Policy `require_review` | completion gate refuses; routes to Reviewer Worker |
| Manual trigger | operator-initiated recovery |

On trigger, the engine creates a **RecoveryExecution** (a specialized
WorkerExecution whose Worker is the recovery workflow driver) and hands
the affected WorkerExecution's state to it.

---

## 3. Default Recovery Workflow

The default workflow is composed of these Steps; organizations may
replace any or all of them.

```
1. capture        — snapshot the WorkerExecution's state and recent
                    telemetry; mark the original execution `recovering`.
2. summarize      — a Reviewer-class Worker reads the captured context
                    and produces a concise summary of completed work.
3. preserve       — write artifacts, traces, file-diff refs, and the
                    summary to durable storage (Postgres + object store).
4. review         — a Reviewer Worker validates completed work against
                    acceptance criteria; produces a verdict
                    (accept | reject | needs-human).
5. plan           — produce a continuation_plan describing remaining
                    work and any corrections.
6. resume         — launch a replacement WorkerExecution using the
                    summary + continuation plan; original task moves
                    from `recovering` → `ready` → `assigned` → `running`.
```

The Reviewer Worker in steps 4–5 is a normal Worker (per
`05_Worker_Specification.md`) — specialized only by its system prompt
and permissions, not by any recovery-specific machinery.

---

## 4. Checkpoint vs. Summarize-Resume

Two resumption paths exist; the engine picks based on checkpoint
compatibility:

- **Direct checkpoint replay** — when a fresh adapter of the same
  `kind` + compatible `version` accepts the prior checkpoint, the
  engine skips summarize/plan and replays directly. Fastest; preferred
  when available.
- **Summarize → resume** — when the checkpoint is incompatible or
  absent, the engine runs the full default workflow. Slower but
  universal.

The engine attempts direct replay first and falls back on
incompatibility. The choice and outcome are recorded as telemetry.

---

## 5. Specialized Workers in Recovery

The default workflow references Worker *roles*, not specific Workers:

- **Reviewer** — validates completed work against acceptance criteria.
- **Architect** — produces/refines the continuation plan when work
  deviated from intent.
- **Security** — invoked when artifacts include risky changes
  (network, auth, secrets).
- **Project Manager** — invoked when scope, budget, or timeline
  materially changed.

The binding from role → concrete Worker is resolved by the
`recovery_workflow_ref` on the affected Worker (or the tenant default).
This keeps recovery workflows portable across organizations.

---

## 6. Recovery Budgets

Each RecoveryExecution has its own budget, derived from the affected
Task's recovery budget (default: 25% of the Task's token/cost budget,
capped). A recovery that exhausts its own budget escalates (see §7).

Recovery budgets are enforced the same way as execution budgets — hard
stops at the AI Gateway and Adapter, with Policy evaluating on breach.

---

## 7. Escalation

```
L0  normal execution
L1  recovery (default workflow, original budget fraction)
L2  recovery-of-recovery (tighter budget, summary-only resume)
L3  human escalation (pause task, notify, await approval)
```

- L1 → L2: recovery stalled or its own Reviewer produced `needs-human`
  for the recovery itself.
- L2 → L3: recovery-of-recovery stalls or budget exhausted.
- L3 always pauses the Task and emits a high-severity event; the Task
  cannot resume without a human `approval` on the continuation plan.

A recovery never loops silently. Every escalation level is an event;
every stall is bounded.

---

## 8. Continuation Plan

The `continuation_plan` produced in step 5 is a structured artifact:

- `completed` — list of work items/subtasks verified done
- `in_progress` — what was mid-flight at capture
- `remaining` — outstanding work against acceptance criteria
- `corrections` — changes the Reviewer/Architect require before resume
- `context_summary` — the compacted context to seed the replacement
- `checkpoint_ref` — opaque blob ref (when direct replay is intended)
- `assumptions` — explicit assumptions the replacement should verify

The replacement WorkerExecution receives this plan in its
`StartExecution.continuation_plan` (see `04_Runtime_Adapter_SDK.md` §3.1).
The plan is itself versioned; the replacement records which plan
version it operated from.

---

## 9. Idempotency & Replay Safety

- Each Step in a recovery workflow is idempotent: re-running it for the
  same RecoveryExecution yields the same artifacts.
- Steps that mutate durable state do so via the transactional outbox,
  identical to all other Orchicon mutations.
- A recovery may be re-driven after a control-plane crash; the engine
  reads the RecoveryExecution's status and resumes from the last
  completed Step, never re-running completed Steps with side effects.

---

## 10. Cross-Cutting Invariants

1. Recovery never bypasses the Scheduler. Resumed execution routes
   through `ready → assigned → running` like any fresh dispatch.
2. Recovery never bypasses Policy. The resumed WorkerExecution is
   subject to the same dispatch, budget, and completion Policies.
3. A Task may be marked `succeeded` by the completion Policy, by the
   Reviewer Worker during recovery, or by a human. All three paths
   produce an audit event with the actor recorded. (Updated from the
   prior "recovery never asserts `succeeded`" invariant — both system
   and human can deem a task complete.)
4. The original WorkerExecution's history is **immutable** once
   `recovering` — recovery writes a new WorkerExecution, it does not
   rewrite the old one.
5. Every recovery action emits an OTel span and a NATS event with the
   trigger reason, level, and outcome. No silent recovery.

---

## 11. Resolved Decisions (v0.1)

- **Policy relaxation during recovery**: bounded auto-relax with audit.
  Recovery may automatically increase a Task's budget by up to 25% of
  the original budget, with an audit event. Beyond 150% of the original
  budget, human approval is required. This lets recovery make forward
  progress on minor overruns without waking a human, while preventing
  runaway spend.
- **Task completion by Reviewer or human**: both the system (Reviewer
  Worker) and a human can deem a Task complete. If the Reviewer Worker
  determines the work is already complete during recovery, it may mark
  the Task `succeeded` directly — a no-op replacement execution is not
  required. Similarly, a human can mark a Task `succeeded` at any time.
  This relaxes the prior invariant ("recovery never transitions a Task
  to `succeeded`"); the updated invariant is: "a Task may be marked
  `succeeded` by the completion Policy, by the Reviewer Worker during
  recovery, or by a human. All three paths produce an audit event with
  the actor recorded."
- **Recovery granularity**: Task-level for v0.1. Recovery operates on
  the failed Task as a whole; the Workflow's step DAG re-evaluates
  after the Task recovers. Mid-Workflow step resume is a candidate for
  v0.2 if v0.1 usage demands it.
- **Recovery visualization**: collapsed single timeline with expandable
  levels, but **rich in detail**. Each recovery event in the timeline
  carries full context: the trigger reason (why), the affected
  WorkerExecution and Task (what), the recovery step that executed
  (how), the adapter and runtime involved (where), and the timestamp
  sequence (when). The UI must answer "what went wrong, what did
  recovery do about it, and what was the outcome?" at a glance, with
  drill-down to span-level traces and event payloads. Not just
  "recovery happened" — a full narrative of the recovery arc.
