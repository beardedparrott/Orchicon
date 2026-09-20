# Ask Orchicon Runtime Routing: Merge vs Dedicated (Design)

**Status:** Decided — implementation-ready plan.
**Work item:** "Design Ask Orchicon runtime routing (merge vs dedicated)"
**Branch:** `design-ask-orchicon-runtime-routing-merge-vs-dedicated-pag1amznys1pt135`
**Scope:** Design only. This document is the decision record + the numbered
implementation plan the later steps execute. It gates routing, telemetry, and
attachment tasks in this feature.

---

## 1. Current truth (verified in code this session)

- **Ask runtime is NOT dispatcher-routed today.** `internal/askorchicon/chat.go`
  drives every turn as a persistent session on a host opencode serve obtained
  via `hostServeClient()` (`chat.go:1032`), which returns a `sessionTurnClient`
  (`chat.go:1020`: `Subscribe / CreateSession / SendMessage / Abort /
  ReplyPermission`). There is **no `Dispatcher` or `AdapterBridge` reference** in
  the chat dispatch path.
- **The real turn machinery (`internal/askorchicon/chat.go`, all verified):**
  - `turnRegistry` one-turn-per-conversation gate — `chat.go:201`, `register`
    `chat.go:234` (rejects a second send with FailedPrecondition at `:629-633`),
    `cancel` `:297` (Stop/supersede/expiry).
  - Broadcast hubs `turnHubRegistry` / `turnHub` (`turn_hub.go`) with
    `WatchTurnStream` re-attach — `chat.go:405` (validates registry entry + hub,
    subscribes, drains), `drainTurnStream` `chat.go:451` publishes every
    drained event to the hub.
  - Sweeper TTL — `startSweeper` (`service.go`) with `askTurnMaxAge()`
    (31 min) + `askSweepInterval()`; evicts via `turnRegistry.sweep`
    (`chat.go:330`).
  - `startConversationTurnOpts` (`chat.go:541`) — the shared dispatch core;
    `turnDispatchOpts{supersede}` (`chat.go:517`): ChatStream passes
    `supersede=false` (`chat.go:530`), InterjectConversationTurn passes
    `supersede=true` (`chat.go:392`).
  - `collectConversationReply` (`chat.go:1168`) — detached collector, loops over
    `runOneTurnAttempt` (`chat.go:1301`, `chat.go:1212`) for
    subscribe+send+drain with re-attach.
  - `modelRefOrFallback` (`chat.go:2088`) — conversation ModelRef →
    `settings.DefaultAskOrchiconModel` → hardcoded `opencode/deepseek-v4-flash-free`
    fallback with a loud warning (`chat.go:704-714`).
- **Dispatcher** (`internal/scheduler/dispatcher.go`): `Register / Resolve / Kinds`.
  `Kinds()` feeds the model-picker adapter bubble tier and the
  `list_adapter_kinds` tool. Registered at server construction
  (`internal/server/server.go:362` `dispatcher.Register("opencode", adapterBridge)`)
  and `:544` `("orchicon", nativeBridge)`.
- **Bridge** (`internal/scheduler/bridge.go`) is execution-shaped:
  `AdapterBridge.Start(ctx, exec db.ExecutionRow, manifest ExecutionManifest,
  callbacks ExecutionCallbacks)` with OPTIONAL capability interfaces
  `MessageInjector` (`SendExecutionMessage`), `SessinContinuer`
  (`ContinueSession`), `Aborter` (`AbortExecution`), `LivenessReporter`,
  `ConfigurableBridge`. The `ExecutionManifest` fields are all worker-execution
  semantics (ExecutionID/TaskID/Goal/AcceptanceCriteria/ProjectDir/Worktree…).
- **Model grammar** is pinned left-greedy in `internal/adapter/modelref.go`:
  3+ seg = `adapter/provider/model` (verbatim remainder); 2-seg infers opencode
  only if seg1 is a known provider; a seg1 that is a known adapter kind is
  rejected as malformed; unknown seg1 → Settings → Adapters. `AdapterKind(ref)`
  (`modelref.go:88`) extracts the kind without validation for the routing view.
- **Import graph is safe for injection:** `internal/askorchicon/tool_workitems.go:17`
  already imports `internal/scheduler`; `internal/scheduler` does NOT import
  `internal/askorchicon`. So a `*scheduler.Dispatcher` (or `*scheduler.Dispatcher`
  plus a chat-capability interface) can be injected into the askorchicon `Service`
  with no import cycle.
- **Adapter registration today:** only `opencode` is a bridge-registered adapter
  (plus `orchicon` native). There is **no Claude Code adapter** — so this design
  makes **no claim that claude works**; claude becomes an Ask adapter only when a
  Claude Code Adapter lands and registers.

---

## 2. Decision: DEDICATED Ask session runtime, resolved via Dispatcher

**Choose: keep the persistent Ask session runtime (turn machinery), make its
`sessionTurnClient` transport adapter-neutral, resolve the adapter kind from the
conversation's `model_ref` through the shared `Dispatcher`, and dispatch onto a
per-adapter chat-session capability — NOT through `AdapterBridge.Start`.**

Do **not** route Ask turns through the shared worker execution path
(`AdapterBridge.Start(ctx, execRow, manifest, callbacks)`).

### 2.1 Why NOT merge (reject the execution path)

The merge option is: build a DB `execution`/`work_item` row per chat turn and
call `AdapterBridge.Start` with a synthetic `ExecutionManifest`.

1. **Shape mismatch is the core blocker** (`internal/scheduler/bridge.go`):
   `ExecutionManifest` requires `ExecutionID, TaskID, WorkerID, Goal,
   AcceptanceCriteria, ProjectDir, WorktreePath, Budgets, Permissions` — all
   worker-execution semantics with DB rows behind them. A chat turn has **no work
   item, no task, no acceptance criteria, no budget/gate model**. Fabricating
   these rows would co-opt the work-item lifecycle, triggering the
   `TaskReconciler`, recovery, audit `_summary` propagation, and `OnResult`
   path — none of which apply to interactive chat.
2. **`ExecutionCallbacks` drive lifecycle transitions** (`OnStarted/OnResult/
   OnWrittenFiles/OnHealth/OnStall/OnRecovered/OnToolCall/OnText/OnArtifact`)
   that the reconciler reacts to (health, recovery, touched-files). Chat has its
   own lifecycle (turn registry, sweeper TTL, one-turn gate, supersede) already —
   merging would double-book two inconsistent lifecycles on the same row.
3. **Streaming is a different contract.** `AdapterBridge.Start` returns only
   `error`; streaming goes through `ExecutionCallbacks.OnText/OnArtifact`,
   persisted to session parts. Ask already streams typed
   `TextChunk/ReasoningChunk/TurnStarted/Heartbeat` events over a connect stream
   (`ChatStreamResponse`) and broadcast hubs. Forcing those through execution
   telemetry loses the turn/hub semantics.
4. **No work item = no execution** is the cleanest cut: the entire dispatcher +
   reconciler machinery assumes an execution row. Reusing it for chat is
   fighting the intended shape.

### 2.2 Why DEDICATED (the accepted direction)

1. **The seam already exists**: `sessionTurnClient` (`chat.go:1020`) is a
   compact, adapter-neutral *session* contract: `Subscribe / CreateSession /
   SendMessage / Abort / ReplyPermission`. It is exactly the persistent-session
   transport a chat model needs. Generalizing this to a
   dispatcher-resolved, capability-scoped interface is a small, non-breaking
   change — the concrete `*opencode.SessionClient` already satisfies it.
2. **The Dispatcher is the right routing substrate** (not just for executions):
   `Resolve(kind)` is generic; `Kinds()` already powers `list_adapter_kinds` and
   the picker's adapter bubble tier. Reusing it for Ask means one source of
   truth for "which adapters exist" and automatic picker/registration parity.
   There is no need to build a second registry.
3. **Mid-run injection already has an adapter-neutral precedent** in the bridge
   package: `MessageInjector` / `SessionContinuer` / `Aborter` are optional
   capability interfaces (`bridge.go`). The dedicated design mirrors this
   pattern: a chat-session capability interface an adapter implements (or not)
   with an actionable "does not support chat" error when absent. This keeps the
   merge-ability later: a future adapter can implement BOTH
   `AdapterBridge.Start` (executions) and the chat-session interface (Ask) on
   the same `Adapter` type — the shared substrate is the Dispatcher, not the
   execution path.
4. **No disruption to the battle-tested turn machinery.** None of the one-turn
   gate, hubs, watcher re-attach, sweeper TTL, supersede, or cause-aware
   finalize need to change semantics. Only the transport below
   `collectConversationReply`/`runOpenCodeTurn` is swapped for the resolved
   adapter's chat client.

### 2.3 Trade-offs

| | Dedicated (chosen) | Merge (rejected) |
|---|---|---|
| New surface | small: 1 generic chat-session interface + client wiring | large: DB rows/types + reconciler coupling per turn |
| Reuses dispatcher | yes | yes |
| Streaming/broadcast | untouched | forces execution-telemetry shape |
| Lifecycle | stays in turn registry / sweeper | second (conflicting) lifecycle via callbacks |
| New-adapter effort | implement chat interface (opencode can share) | implement full execution bridge per chat |
| Claude today | **not wired** (no Claude adapter) | **not wired** (no Claude adapter) |
| Downside | one additional capability interface to keep in sync with bridge capabilities | n/a |

**DECISION (revisitable):** dedicated Ask session runtime. If a future
adapter genuinely runs chat as executions (e.g. a serverless agent), revisit via
a new ADR; the capability interface keeps that option open without committing to
it now.

---

## 3. Routing map (how an Ask conversation resolves its adapter)

1. Dispatch time (`startConversationTurnOpts`, `chat.go:541`) already resolves
   `modelRef := s.modelRefOrFallback(ctx, tenantID, conv.ModelRef)` (`chat.go:704`).
2. New: parse the adapter kind from that modelRef with
   `adapter.AdapterKind(modelRef)` (`modelref.go:88`, structural, no validation).
   For a legacy/1-2-seg ref it yields `DefaultAdapterKind` ("opencode"); for a
   3+ seg ref the explicit segment-1 kind.
3. New: `s.dispatcher.Resolve(kind)` (`dispatcher.go:65`). Unknown kind →
   the actionable "register an adapter of that kind" error → surfaced as a clean
   turn failure (mirrors `list_adapter_kinds`).
4. New: resolve the chat-session capability from the resolved bridge: type-assert
   to the new chat-session interface (defined §5.1). A bridge that registers but
   does not implement chat → actionable "adapter kind X does not support Ask
   chat" error.
5. The chat-session client drives the existing turn loop. The **adapter kind is
   conditioned on REGISTERED kinds today**; until a Claude Code adapter registers,
   `Resolve("claude")` fails exactly as `Resolve("claude")` fails for
   executions — no silent claude support claim. Verify at build time that `Resolve`
   on the default/opencode path is the only one exercised (fake bridge in tests).

---

## 4. Ask-vs-execution differences the shared path MUST accommodate

The dedicated path must absorb these (they are why it cannot ride the execution
path):

1. **No work item / no task**: no `ExecutionRow`, no `Goal/AcceptanceCriteria`,
   no `TaskID`. The turn identity is a *conversation* (`convID`) + assistant
   message id.
2. **Interactive**: a turn returns a stream back to the caller and the
   conversation lives across many turns on one persistent session.
3. **Streaming**: typed `ChatStreamResponse` events (TurnStarted/TextChunk/
   ReasoningChunk/Heartbeat) over connect server-stream, plus the per-conversation
   broadcast hub for dropped-socket re-attach. This is outside the
   `OnText/OnArtifact` execution-telemetry shape.
4. **Mid-run injection**: Ask's `InterjectConversationTurn` (`chat.go:376`)
   supersedes + aborts the in-flight turn and re-dispatches on a fresh or
   wedged-recycled session — conceptually similar to `MessageInjector` but
   conversation-scoped (not execution-scoped) and with the one-turn gate.
5. **Cancellation**: `stopConversationTurn`/Stop aborts the serve session and
   cancels the collector with a *cause* (`errUserStop` vs `errTurnSuperseded`
   vs `errTurnExpired`); the execution path's `Aborter.AbortExecution` is
   exec-scoped and cause-less.
6. **Longer-lived conversations**: sweeper TTL (31 min) + reply window
   (30 min) + re-attach backoff govern turn lifetime — not execution
   health/stall-recovery. The turn-level stall monitor is Ask's own (not the
   reconciler's `OnStall`).

What the shared (Dispatcher-routed, chat-session) path must simply **accommodate**:
the conversation object (not execution), the typed stream + hubs, cause-aware
cancel, and the sweeper TTL. None require changing the bridge's execution
contract — they live entirely in the chat-session interface.

---

## 5. Surface area of change (files/packages)

New generic chat-session capability in `internal/scheduler/bridge.go`:

- Add a **new interface** (mirroring `MessageInjector`/`SessionContinuer`/`Aborter`):
  ```go
  // ChatTurnClient is the adapter-neutral persistent-session surface an Ask
  // Orchicon turn drives. Adapters that support Ask chat implement it as an
  // OPTIONAL capability; the askorchicon service type-asserts it off the
  // Dispatcher-resolved bridge and surfaces an actionable error when absent.
  // Semantically the generalization of askorchicon.sessionTurnClient.
  type ChatTurnClient interface {
      Subscribe(ctx context.Context, conversationID string) (SessionBus, error)
      CreateConversationSession(ctx context.Context, conversationID, title string) (string, error)
      SendTurnMessage(ctx context.Context, conversationID, sessionID, system, modelRef, text string) error
      AbortConversationSession(ctx context.Context, sessionID string) error
      ReplyPermission(ctx context.Context, sessionID, permissionID string) error
  }
  ```
  (`SessionBus` is the typed event channel the adapter emits; it is NOT a thin
  `opencode.BusSub` wrapper — scope it per §5.2.) This lives in
  `internal/scheduler` so `internal/opencode` and `internal/askorchicon` can
  both consume it without a new package or a scheduler→askorchicon dependency.

### 5.2 The real cost of adapter-neutral `SessionBus` (the demux is coupled)

The largest refactor in this whole design is NOT the dispatcher wiring — it is
decoupling the drain loop from the concrete `opencode.BusEvent` type and its
event vocabulary. Today `runOneTurnAttempt` (chat.go:1540-1730) switches on
opencode-specific semantics directly:

- **`evt.Type` literals** — `"session.idle"` (turn complete), `"permission.asked"`
  (chat.go:1555), `"session.error"` (model/API failure, chat.go:1565),
  `"message.part.updated"` (tool part via `activeToolName` chat.go:1123).
- **`evt.Properties` keys** — `["sessionID"]` (bus multiplexes ALL sessions; the
  drain filters by `sid`), `["id"]` (permission id), `["error"].message`.
- **`opencode.TokenDeltaInfoFromBus(evt)`** (chat.go:1592) — mid-generation
  token deltas + kind for the stall monitor / live mirror; `kind ==
  "reasoning"` and the **folded-think segmenter** (`thinkSegmenter.feed`, :1628)
  demux GLM/DeepSeek think bodies out of the TEXT delta stream on the
  `field:"text"` convention.
- **`opencode.LegacyEventFromBus(evt)`** (chat.go:1680) — completed text /
  reasoning part telemetry.
- **`opencode.ErrSessionNotFound`** (chat.go:1523) — recreate + re-seed trigger.
- **`opencode.AttachmentPart`** + the `*opencode.SessionClient` type-assert for
  the extended attachment sender (chat.go:1325).

**Design intent for §5.2:** `SessionBus` should abstract ONLY the turn-visible
semantics the drain loop already needs, not the raw bus:

```go
// SessionBus is the adapter-neutral per-turn event surface an Ask drain loop
// consumes. Adapters map their own event/protocol vocabulary onto these
// turn-visible signals; the folded-think demux, kind inference
// (reasoning vs text), and the per-session filter that today live in chat.go
// fall to the ADAPTER's implementation. The scheduler and askorchicon
// packages see only this interface, never the adapter's raw event type.
type SessionBus interface {
    Events() <-chan SessionEvent
    Done() <-chan struct{}
    Close()
}

// SessionEvent is one turn-visible signal, adapter-neutral.
type SessionEvent struct {
    // Kind: "idle" (turn complete), "error" (turn failed, Text = message),
    // "permission" (PermissionID auto-approve),
    // "tool_part" (Text = active tool name; stall/wedge detection),
    // "delta" (Text/Reasoning token delta; stall monitor + live mirror),
    // "part" (completed Text/Reasoning part; durable telemetry).
    Kind         string
    Text         string
    IsReasoning  bool
    PermissionID string
}
```

The opencode `ChatTurnClient.Subscribe` maps `SessionClient.Subscribe` events
onto `SessionEvent` (it already owns `TokenDeltaInfoFromBus`/`LegacyEventFromBus`;
move the folded-think demux and per-session filter there). Tests feed a fake
`SessionBus` directly, removing the concrete `opencode.BusEvent` dependence
from chat tests. This is items §7.2/§7.5's real substance — the "map subscribe"
step is a small event-adapter, not a pass-through.

### 5.3 Files touched

| File | Change |
|---|---|
| `internal/scheduler/bridge.go` | add `ChatTurnClient` + `SessionBus` interfaces (compile-time `var _ ChatTurnClient = (*opencode.Adapter)(nil)` asserted in `internal/opencode/adapter.go`) |
| `internal/askorchicon/chat.go` | replace the `sessionTurnClient` type (line 1020) usage with the resolved `scheduler.ChatTurnClient`; add a `Service.dispatcher` field; new `resolveChatClient` helper |
| `internal/askorchicon/service.go` | add `dispatcher *scheduler.Dispatcher` field + `SetDispatcher` setter; update `New` (or wiring) |
| `internal/opencode/adapter.go` | implement `ChatTurnClient` (delegating to `SessionClient` — `CreateSession`/`SendMessage`/`Abort`/`ReplyPermission` already exist on the client); assert the interface |
| `internal/server/server.go` | wire `askSvc.SetDispatcher(dispatcher)` in the Mount sequence near the existing setters |
| `internal/api/api.go` | add `Dispatcher *scheduler.Dispatcher` to `Deps` and pass it into `askorchicon.New` / setter (only if not already reachable from the server Mount — server.go wires directly) |
| tests | `internal/askorchicon/*_test.go` fake `ChatTurnClient` (replaces fake `sessionTurnClient`); `internal/scheduler/dispatcher_test.go` + a fake bridge asserting `Resolve` + chat-capability routing |

No new package. No new DB table. No change to the `AdapterBridge.Start`
execution contract.

---

## 6. Wiring / insertion points

1. **`internal/scheduler/bridge.go`** — insert the two interface declarations
   (near the other capability interfaces, e.g. after `Aborter`, around
   `bridge.go:130`).
2. **`internal/opencode/adapter.go`** — add `func (a *Adapter) ChatTurnClient`
   methods, delegating to the host serve's `SessionClient`
   (`servehost.go:81 Client()`); add `var _ scheduler.ChatTurnClient = (*Adapter)(nil)`
   beside the existing `var _ scheduler.AdapterBridge` at `adapter.go:1493`.
   (Note: an Ask turn has no directory/ProjectDir — `NewSessionClient` with a
   directory; chat already runs directory-less on the host serve.)
3. **`internal/askorchicon/service.go`** — add field `dispatcher *scheduler.Dispatcher`
   and setter `func (s *Service) SetDispatcher(d *scheduler.Dispatcher)`; no import
   cycle (scheduler already imported).
4. **`internal/askorchicon/chat.go`** — in `startConversationTurnOpts` at the
   model-resolution step (`chat.go:704` region), after `modelRef` is resolved,
   call a new `resolveChatClient(tenantID, convID, modelRef)` helper that:
   `kind := adapter.AdapterKind(modelRef)`; `bridge, err := s.dispatcher.Resolve(kind)`;
   `client, ok := bridge.(scheduler.ChatTurnClient)`; surface actionable errors.
   Pass `client` into `collectConversationReply` (replacing `hostServeClient()`).
   Keep `hostServeClient()` for the supersede/Stop/sweeper abort paths ONLY as the
   opencode fallback for the default kind, or route those through the resolved
   client too (preferred: route through the turn's `client`).
5. **`internal/server/server.go`** — after `dispatcher := scheduler.NewDispatcher()`
   (`:261`), call `askSvc.SetDispatcher(dispatcher)` in the Mount wiring alongside
   `askSvc.SetAdapterKinds(deps.AdapterKinds)` (`api.go:358`).

---

## 7. Numbered implementation list

1. In `internal/scheduler/bridge.go`, declare `SessionBus` + `SessionEvent`
   (the adapter-neutral turn signals of §5.2) and `ChatTurnClient` (the 5-method
   interface in §5.1), placed with the other optional capability interfaces.
2. In `internal/opencode/adapter.go`, implement `ChatTurnClient` on
   `*Adapter`, delegating `CreateConversationSession`→`SessionClient.CreateSession`,
   `SendTurnMessage`→`SessionClient.SendMessage`, `AbortConversationSession`→
   `SessionClient.Abort`, `ReplyPermission`→`SessionClient.ReplyPermission`.
   Implement `Subscribe` as the `SessionClient.Subscribe` **event adapter**: map
   each `BusEvent` (using the existing `TokenDeltaInfoFromBus`/`LegacyEventFromBus`,
   the per-session `sessionID` filter, and the folded-think demux — all moved here)
   onto the `SessionEvent` kinds of §5.2. Add
   `var _ scheduler.ChatTurnClient = (*Adapter)(nil)`.
3. In `internal/askorchicon/service.go`, add `dispatcher *scheduler.Dispatcher`
   field + `SetDispatcher`; wire no import cycle (already imports scheduler).
4. In `internal/askorchicon/chat.go`, add `resolveChatClient(tenantID, convID,
   modelRef string) (scheduler.ChatTurnClient, error)`:
   `kind := adapter.AdapterKind(modelRef)` → `s.dispatcher.Resolve(kind)` →
   type-assert `scheduler.ChatTurnClient`, returning the actionable
   "kind X does not support Ask chat" error when absent.
5. In `startConversationTurnOpts` (after `modelRef` at `chat.go:704`), resolve
   the chat client and thread it through `collectConversationReply`/`runOneTurnAttempt`
   (`chat.go:1168`/`:1301`) replacing `hostServeClient()`. In `runOneTurnAttempt`,
   swap the `opencode.BusEvent` demux (`evt.Type` switch, `TokenDeltaInfoFromBus`,
   `LegacyEventFromBus`, folded-think demux, `ErrSessionNotFound` check,
   attachment type-assert at `chat.go:1325`) for a `SessionEvent`-typed drain per
   §5.2. Keep the default-kind opencode behavior identical when the resolved kind
   is `opencode`.
6. Route the supersede-abort (`chat.go:572`), Stop-abort (`chat.go:911`),
   DeleteConversation-abort (`service.go`), and sweeper-abort (service.go
   `startSweeper`) through the turn's resolved chat client (or fall back to the
   opencode host serve for the default kind).
7. Add a `SetDispatcher` call in the server Mount wiring
   (`internal/server/server.go` after `dispatcher` creation at `:261`, and/or
   `internal/api/api.go` Deps → `askorcicon.New`) — follow the exact spot the
   other `askSvc.Set*` setters are called (`api.go:357-364`).
8. Update the askorchicon fake in `chat_*_test.go`/`robustness_*_test.go` from
   `sessionTurnClient` to the new interface (or a thin adapter).
9. Add a scheduler unit test with a fake `AdapterBridge` that implements
   `ChatTurnClient`, registering two kinds and asserting `Resolve` routes Ask
   to the chat-capable bridge and yields the actionable error for a
   bridge that implements only `AdapterBridge.Start`.
10. `go build ./... && go test ./internal/askorchicon/... ./internal/scheduler/... ./internal/opencode/...`.

**Explicitly out of scope:** Claude Code adapter (does not exist — no claim of
claude support until a Claude adapter registers its kind); any change to
`AdapterBridge.Start` / `ExecutionManifest`; telemetry/attachment work (their own
tasks, but this routing doc is their gate).
