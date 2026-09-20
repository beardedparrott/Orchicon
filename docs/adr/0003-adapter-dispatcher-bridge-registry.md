# ADR 0003 — Adapter Dispatcher / bridge registry

Status: Accepted (architectural design for work item "Build adapter Dispatcher / bridge registry")

## Context

`internal/scheduler/bridge.go` declares `AdapterBridge` with a single method (`Start`), but `internal/server/server.go` wires the opencode adapter through a broader CONCRETE surface (SendExecutionMessage, ContinueSession, AbortExecution, IsExecutionActive, capability setters). The TaskReconciler is constructed with a single hardcoded opencode bridge and calls `r.bridge.Start` (reconciler.go:1063). A second adapter cannot plug in until this surface is adapter-neutral. The adapter kind must come from the model_ref grammar (`ParseModelRef(ref).Adapter`) — the single source of truth; no adapter-kind column exists or may be added.

## Decision

1. **Dispatcher** (`internal/scheduler/dispatcher.go`): thread-safe `Dispatcher` with `Register(kind string, bridge AdapterBridge)` and `Resolve(kind string) (AdapterBridge, error)`. Unknown kind → actionable error (contains the kind), never a panic.
2. **Capability interfaces (optional, adapter-implemented)**: keep `AdapterBridge` = required core (Start); add scheduler-level `MessageInjector` (SendExecutionMessage), `SessionContinuer` (ContinueSession), `Aborter` (AbortExecution), `LivenessReporter` (IsExecutionActive). Server/RPC paths type-assert per capability and return an actionable error when unsupported — never a panic.
3. **Contract-level shared types in scheduler**: `ContinueSessionOpts`, usage-recorder func + `UsageRecord`, session-part store func, and a `RuntimeClient` interface satisfied by `*runtime.Client` (acyclic: internal/runtime imports neither scheduler nor opencode). Capability setters ride the optional-interface surface so construction stays adapter-neutral.
4. **TaskReconciler resolves at dispatch**: `NewTaskReconciler(pool, log, dispatcher)`; `startExecution` parses `adapter.ParseModelRef(manifest.ModelRef).Adapter` (2-segment refs default to "opencode"), resolves via Dispatcher, fails the execution with the actionable error on unknown kind.
5. **Server** registers opencode under kind "opencode" at construction; concrete adapter types appear only at construction/registration, never in dispatch or callback paths. Behavior for existing executions is unchanged.

## Consequences

- Adapters become pluggable: Register + implement the capabilities they support; no server.go changes, no claude special-casing.
- Unknown kind fails the execution with an actionable message instead of panicking.
- One source of truth (model_ref); no schema change.
- Cost: capability type-assertions; reaper liveness for non-LivenessReporter kinds is fail-closed (logged, treated as lost).
- `ParseModelRef` must land in `internal/adapter` (does not exist yet; sibling model-ref namespace task defines the 3-segment grammar).
