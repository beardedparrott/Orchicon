# Orchicon — Runtime Adapter SDK

> **Version:** 0.1
> **Status:** Direction & design intent
> **Parent:** `01_Architecture_Vision.md`
> **First runtime:** OpenCode

This document defines the contract between the Orchicon control plane
and pluggable execution runtimes. Runtimes integrate by implementing a
gRPC sidecar ("adapter") that the control plane drives. The SDK is the
reference implementation of that sidecar plus the language bindings.

---

## 1. Design Intent

- **Adapters are sidecars, not libraries.** A runtime ships as a
  process the control plane drives over gRPC. This keeps runtimes
  language-agnostic and crash-isolated.
- **Adapters hold no durable state.** Anything that must survive a
  crash is checkpointed through the control plane.
- **Capabilities are negotiated, not assumed.** Each execution begins
  with a capability handshake; the control plane refuses to dispatch
  work the adapter cannot fulfill.
- **Telemetry is streamed back, not polled.** An execution is a
  bidirectional stream: control messages down, telemetry up.
- **The control plane never trusts adapter self-reported success.**
  Completion is gated by the completion Policy.

---

## 2. Adapter Lifecycle

```
register ──► ready ──► (per-execution) ──► deregister
                         handshake
                         start
                         streaming (events up / control down)
                         checkpoint
                         stop | terminate
```

- **Register**: adapter announces itself with kind/version/capabilities
  and an endpoint. Control plane records `RuntimeAdapter` row,
  validates version compatibility, begins heartbeat lease.
- **Ready**: adapter eligible for dispatch.
- **Per-execution**: each WorkerExecution is an isolated session on the
  adapter. Adapters may run multiple concurrent executions up to a
  declared `max_concurrent_executions`.
- **Deregister**: graceful drain — new executions refused, in-flight
  allowed to complete or checkpoint per Policy; forced after lease
  expiry.

---

## 3. gRPC Contract (sketch)

A single service, `RuntimeAdapterService`, with both unary RPCs for
lifecycle and a bidirectional stream for execution.

```proto
service RuntimeAdapterService {
  rpc Register(RegisterRequest) returns (RegisterResponse);
  rpc Deregister(DeregisterRequest) returns (DeregisterResponse);
  rpc Heartbeat(HeartbeatRequest) returns (HeartbeatResponse);

  // One stream per WorkerExecution.
  rpc Execute(stream ClientMessage) returns (stream AdapterMessage);
}

message ClientMessage {
  oneof payload {
    StartExecution   start     = 1;
    ControlCommand   control   = 2;   // pause | resume | checkpoint | cancel
    ApprovalResponse approval  = 3;   // human-approved transition
  }
}

message AdapterMessage {
  oneof payload {
    ExecutionStarted   started      = 1;
    Telemetry          telemetry   = 2;   // progress, tokens, cost, traces
    ToolCall           tool_call   = 3;   // runtime-initiated tool use (where exposed)
    Checkpoint         checkpoint  = 4;   // serialized progress snapshot
    ApprovalRequest    approval    = 5;   // request human approval
    HealthState        health      = 6;
    ExecutionResult    result      = 7;   // terminal
    Error              error       = 8;   // terminal or transient
  }
}
```

### 3.1 StartExecution manifest

The control plane sends everything the adapter needs to begin a
WorkerExecution:

- `execution_id`, `task_id`, `project_id` (for tenancy tagging)
- `worker_ref` (id, version) — identity of the Worker profile
- `system_prompt`, `goal`, `acceptance_criteria`
- `model_ref` (preferred model; adapter may negotiate)
- `context_sources` — references the adapter resolves (files, prior
  summaries, retrieved docs)
- `continuation_plan` (present only on recovery resume) — produced by
  the Recovery Workflow Engine
- `budgets` — token / cost / wall-clock ceilings; adapter must enforce
  hard stops
- `permissions` — capabilities the runtime is permitted to use
- `policy_overrides` — per-execution policy deltas
- `capabilities_required` — what the adapter must support

### 3.2 Capability negotiation

Capabilities advertised at registration, re-asserted in
`ExecutionStarted`. Categories:

| Category | Example capabilities |
|---|---|
| `model_providers` | anthropic, openai, local |
| `tools` | file_edit, terminal, web_fetch, mcp_servers |
| `context` | vector_retrieval, file_index, prior_summary |
| `telemetry` | tool_calls_streamed, file_diffs, prompt_response |
| `execution` | checkpoint, pause_resume, cancellation |
| `mcp` | named MCP server manifests |

If `capabilities_required ⊄ advertised`, dispatch is refused and the
Task requeues with an alternate adapter (per Scheduler design).

---

## 4. Telemetry & Control Channel

- **Telemetry up**: every meaningful action emits an `AdapterMessage`.
  Tool calls, file changes, prompts/responses (where the runtime
  exposes them), token consumption, cost accrual, progress markers,
  health signals.
- **Control down**: pause, resume, checkpoint-now, cancel. The adapter
  must acknowledge each with a control ACK message; failure to ACK
  within a deadline counts as a health violation.
- **Approvals**: a runtime may request human approval for an action the
  Policy has gated. The control plane routes the request, returns the
  decision, and the adapter proceeds or aborts.

All telemetry is dual-written: onto the gRPC stream (low-latency, for
live UI) and onto the NATS telemetry subjects (durable, for storage and
replay) by the control plane, never by the adapter. Adapters never
touch NATS or Postgres directly.

---

## 5. Checkpointing

A checkpoint is an opaque, versioned blob produced by the adapter and
stored by the control plane (Postgres metadata + object store blob).
On resume, the control plane sends the checkpoint back as part of
`continuation_plan`.

Contract:

- `CheckpointRequest` may originate from the control plane (on pause or
  pre-recovery) or the adapter (on natural progress milestones).
- The checkpoint must be sufficient for a **fresh** adapter instance of
  the same `kind` + compatible `version` to resume execution without
  re-reading the full prior transcript.
- Checkpoint format is adapter-defined; the control plane treats it as
  an opaque blob with a `format_version`.

If a checkpoint is incompatible with the resuming adapter version, the
Recovery Workflow Engine falls back to the **summarize → resume** path
rather than direct checkpoint replay.

---

## 6. OpenCode Adapter (first implementation)

OpenCode is a TypeScript runtime. The OpenCode adapter is a small Go
or Node/TS process that wraps OpenCode's execution surface and
translates it to the gRPC contract above.

### 6.0 Transport strategy: CLI now, IPC later

The adapter's internal transport (how it actually drives OpenCode) is
an implementation detail hidden behind the gRPC contract — the control
plane never knows or cares which surface the adapter uses.

- **v0.1: CLI subprocess.** The adapter spawns OpenCode as a
  subprocess, drives it via CLI flags/commands, and parses JSON from
  stdout. This is the only stable surface available today and is
  sufficient to validate the orchestration model end-to-end.
- **v0.2+: IPC/plugin API.** When OpenCode ships a stable IPC, plugin,
  or session API (socket, JSON-RPC, or similar), the adapter swaps its
  internals from subprocess management to IPC client. This unlocks
  structured streaming events, in-flight control (pause/resume/cancel
  at the application level), tool-call interception, and session-state
  snapshots for real checkpointing.

The migration is adapter-internal: bump the adapter version, re-register
with updated capabilities, and let in-flight executions finish on the
prior version while new dispatches use the new one. The control plane,
the gRPC contract, and all other docs are unaffected.

**v0.1 CLI limitations (contained to the adapter):**

| Capability | v0.1 (CLI) | v0.2+ (IPC) |
|---|---|---|
| Streaming telemetry | line-buffered stdout parsing | structured streaming events |
| Pause/resume | best-effort (process signal or session hand-off) | application-level |
| Checkpoint | coarse (transcript summary + working tree ref) | fine-grained session snapshot |
| Tool-call interception | limited (Tier 2 gating best-effort) | first-class |
| Health detection | process liveness + stdout heuristics | runtime-reported health |

The adapter MUST NOT advertise capabilities the CLI surface cannot
honestly deliver; v0.1 advertises a reduced capability set and the
control plane adapts dispatch accordingly. When IPC arrives, advertised
capabilities expand.

### 6.1 Mapping table (v0.1 CLI surface)

| gRPC concept | OpenCode v0.1 surface (CLI) | v0.2+ (IPC) |
|---|---|---|
| `Register` | adapter process startup; spawn `opencode --version` to confirm | same, plus capability handshake |
| `StartExecution` | spawn `opencode` with the manifest's system prompt, goal, permissions | open IPC session with manifest |
| `telemetry.tool_call` | parse tool-call lines from stdout JSON | structured tool-call events |
| `telemetry.file_diff` | parse file-change lines from stdout JSON | structured file-diff events |
| `telemetry.prompt_response` | parse transcript lines (gated by Policy) | streaming transcript events |
| `control.pause/resume` | SIGSTOP/SIGCONT or session hand-off (best-effort) | application-level pause/resume |
| `checkpoint` | transcript summary + `git stash`/working-tree ref (coarse) | fine-grained session snapshot |
| `HealthState` | process liveness + stdout heuristics + token parsing | runtime-reported health |

### 6.2 Capability exposure

OpenCode's exposed surface determines which capabilities the adapter can
honestly advertise. The adapter MUST NOT advertise a capability it
cannot actually deliver; doing so is treated as a security violation.

Where OpenCode does not expose a needed surface (e.g. streamed
prompts), the adapter advertises the absence and the Policy/Worker
model adapts — the contract never lies about capabilities.

### 6.3 Packaging

- Adapter ships as a container image alongside OpenCode.
- Control plane discovers adapters via a registry table (endpoint +
  kind + version). For local dev, an in-process adapter is supported
  for tests only, never production.

---

## 7. Versioning & Compatibility

- Adapter `kind` is stable (e.g. `opencode`).
- Adapter `version` follows semver; the control plane records a
  compatibility matrix per kind (min/max version it can drive).
- The gRPC contract is itself versioned; breaking changes ship a new
  service name (e.g. `RuntimeAdapterServiceV2`) so old adapters keep
  working during migration.
- Worker versions may pin an adapter `kind` but not a specific adapter
  `version`; cross-version resumption is governed by checkpoint
  compatibility (see §5).

---

## 8. Failure & Recovery Hooks

The adapter owns **detection**, the control plane owns **response**.

- Adapter detects internal fault → emits `Error` with severity and
  sets `HealthState=unhealthy`. Control plane routes to Recovery Engine.
- Adapter loses its stream without graceful close → control plane marks
  the WorkerExecution `unhealthy` after heartbeat deadline and triggers
  recovery.
- Adapter must be kill-safe: killing the adapter process at any time
  must not corrupt control-plane state, because all durable state is
  checkpointed through it.

---

## 9. Resolved Decisions (v0.1)

- **Checkpoint format**: opaque blob + `format_version`. The control
  plane treats checkpoints as opaque blobs; each adapter defines its
  own format and tags it with a `format_version`. A recommended JSON
  envelope (`{format_version, adapter_kind, adapter_version, summary,
  state_ref}`) is documented but not mandated. Cross-version resumption
  is governed by the adapter's own compatibility checks; if
  incompatible, the Recovery Engine falls back to summarize-resume.
- **Adapter channel to control plane**: the gRPC `Execute` bistream
  is the sole channel. Adapters never call the control-plane REST/gRPC
  API directly, never hold API keys or auth credentials, and never
  need network access beyond the single gRPC connection. All control,
  telemetry, approval routing, and artifact handoff flows through the
  stream. This keeps adapters simple, auditable, and secure.
