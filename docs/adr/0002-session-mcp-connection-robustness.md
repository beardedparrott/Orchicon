# ADR-0002: Session & MCP connection robustness for Ask Orchicon (watchdog, refresh-survival, mid-run interjection, multi-session stability)

**Status:** Proposed (step 1 — Principal Software Architect)
**Work item:** Session & MCP connection robustness: watchdog, refresh-survival, mid-run interjection, multi-session stability

## Context

Four live production observations about the **Ask Orchicon conversation session** (the human-facing chat agent that drives an opencode serve session and calls the `orchicon_*` MCP tools; not the work-item execution session):

1. **MCP wedges mid-session** — tool dispatch stops reaching the server. A tool call is issued (`tool_use` event on the serve bus) but the tool result / `step_finish` / `session.idle` never arrive. The turn wedges.
2. **Page refresh breaks sessions** — the ChatStream socket drops and the UI reports the misleading `Connection lost — still working…` notice even when nothing is actually progressing server-side.
3. **A session locked on stop can't be interjected** — a mid-run steering message cannot be delivered without killing the run, because the serve session is wedged and the interject's fresh dispatch cannot get a healthy session.
4. **Flakiness with multiple concurrent sessions** — contention, connection pooling, shared transport, backpressure, and per-session isolation degrade under N concurrent conversations.

### Established facts (verified in this worktree)

- The Ask Orchicon turn collector is `internal/askorchicon/chat.go`. `startConversationTurnOpts` persists the user message, registers a one-turn-per-conversation entry in `turnRegistry`, and launches a **detached** reply collector (`collectConversationReply` -> `runOneTurnAttempt`) on `context.WithoutCancel(ctx)` that survives a client disconnect/refresh. `turnRegistry` (`turnEntry`) carries `started`, `token`, `tenant`, `assistantMsgID` — the source of `Conversation.turn_in_flight` + `pending_assistant_message_id` (service.go `turnStatus`/`conversationRowToProto`), which the frontend uses to re-attach after a refresh.
- The collector already re-attaches across serve loss (`turnReattach`), re-creates a seeded session on a 404 (`turnRecreated`), and recreates the session when `sid == ""`. It is bounded by `askReplyWindow` (30m), a serve-down grace (`askServeDownGrace`, 15s), a handshake timeout (`askTimeout`, 60s), and per-cause finalization (`errUserStop` / `errTurnSuperseded` / `errTurnExpired`).
- The existing watchdog is `chatStallMonitor` (`internal/askorchicon/stall.go`): `no_progress` (120s silence) and `repetition` (same tool arg > N in the window). On a trip it calls `SessionClient.Abort` and **fails the turn** — it never heals/reconnects. Critically, a `tool_use` event counts as activity and resets the no-progress clock, so a wedged tool call (tool started, never resolved) is exactly the case `no_progress` does **not** catch promptly — it must time out silently until the 120s window expires, then abort.
- The execution transport has a **recycle precedent** the chat path lacks: `internal/opencode/session_run.go` `recycleOnWedgedServe` / `recycleOnInfraModelTurn` detect a poisoned serve and kill the runtime container so the next dispatch rebuilds a fresh serve. `recycleOnInfraModelTurn` fires on the FIRST infra-class failure (socket/connect errors) and is bounded by `sessionRepairBudget`.
- `HostServe` (`internal/opencode/servehost.go`) supervises the single shared host serve via `Watch` (polls `/global/health` every 15s, restarts with backoff) — but `/global/health` only proves the process is up, **not** that its MCP connections are usable. `ProbeUsable` (`session.go`) adds a session-create round-trip but still does not probe MCP.
- **Shared transport:** every `SessionClient` (`session.go` `NewSessionClient`) is built with `&http.Client{}`, which uses the package-level `http.DefaultTransport` (`MaxIdleConnsPerHost=2`, `MaxIdleConns=100`, `IdleConnTimeout=90s`). All API calls and all `/event` SSE subscriptions to the one host serve share it.
- **O(N) bus fan-out + blocking feed:** each collector opens its own `/event` subscription and reads the entire bus, filtering per-session. `Subscription.read` (`session.go`) has a 256-event buffer and **blocks** on `s.events <- evt` when full (no `default`), so a slow consumer parks its own SSE reader/connection. The `onStreamEvent` callback in `chat.go` drops events when the 64-slot stream channel is full (`default:`).
- The frontend session view is `frontend/src/routes/ask-orchicon.tsx` (the `ConvStream` slot, `runStream`, re-attach effect, `handleStopConversation`, `interjectStreaming`) and `frontend/src/api/askOrchicon.ts`. It already reconciles against `turn_in_flight` / `pending_assistant_message_id` on refresh and shows the `Connection lost — still working… You can interject or stop this reply.` notice (line ~1181).
- The MCP connection is **per-serve**, not per-session: the host serve (and each runtime-container serve) connects to the `orchicon mcp` sidecar at startup (`config.go` `BuildConfigContent`, `RuntimeServeConfig` in `adapter.go`). A single wedged MCP therefore contaminates every session on that serve — the cross-session isolation risk.

### Root causes

- **Obs 1 (MCP wedge):** no per-tool health probe; a wedged tool call is invisible to `no_progress` and ends in an abort (fail), not a reconnect; the serve health probe ignores MCP usability.
- **Obs 2 (refresh/status):** the collector survives refresh, but the frontend declares `reconnecting`/`working` purely from a socket drop without confirming the server turn is still actively progressing — so a wedged turn still shows `still working…`. The refresh re-attach also ping-pongs with the completion effect (documented in `ask-orchicon.tsx`) over the `turn_in_flight` -> reply persisted lag.
- **Obs 3 (interjection):** `InterjectConversationTurn` supersedes (cancels collector, removes registry entry, aborts the serve session) then dispatches a fresh turn. On a wedged session the abort is best-effort and the fresh `runOneTurnAttempt` cannot get a healthy session, so the fresh dispatch fails instead of landing; `prompt_async` queues behind the wedged turn when the abort does not take effect.
- **Obs 4 (multi-session):** shared `http.DefaultTransport` (per-host idle=2) for all API+SSE traffic to one serve; O(N) full-bus readers each with a blocking 256-event feed (no backpressure, no drop); no per-session MCP isolation (MCP is per-serve); no admission bound.

## Decision

**D1 — Serve-watchdog verifies MCP usability (AC1, part 1).** Extend the serve liveness contract so a serve with a wedged/unusable MCP is detected and restarted — the plane-level watchdog for the single shared host serve, mirroring `recycleOnWedgedServe` for the runtime containers.
- `HostServe.Watch` currently polls `/global/health`; add an MCP usability probe to the readiness gate: `SessionClient.ProbeUsable` performs a `tools/list` round-trip against the Orchicon MCP (and/or a cheap `orchicon_*` read) in addition to session-create. A serve that passes `/global/health` but fails MCP is treated as unhealthy -> restart with the existing backoff, preserving the data dir so sessions re-attach by id.
- Add `SessionClient.MCPHealthy(ctx)` (a `tools/list` round-trip) and wire it into `ProbeUsable` and health polling. Env override for the probe timeout (`ORCHICON_ASK_MCP_PROBE_TIMEOUT`).
- Consequence: a single wedged MCP no longer contaminates all sessions; the watchdog heals it transparently.

**D2 — Per-turn tool-wedge monitor + session recycle (AC1, part 2).** Add a tool-level wedge signal to the chat collector so a tool call that is issued but never resolves is detected and healed, not silently awaited to the no-progress window.
- Extend `chatStallMonitor` (`stall.go`) with an **unresolved-tool_use** signal: on `tool_use`, mark that tool call open (`tool` + `args` + `time`); require a terminating event (`step_finish`, a later `tool_use` for another tool, or completed `text`) to close it. If an open tool call exceeds `ORCHICON_ASK_MCP_TOOL_WEDGE_WINDOW` (default 30s) with no further activity, treat it as an MCP wedge. This is precise: it does not trip on legitimate slow tools that still stream activity.
- On a wedge, the collector performs a **bounded session recycle** instead of a hard fail:
  1. probe MCP health; if unhealthy, rely on D1's serve restart and enter the existing `turnReattach`/serve-down retry within the reply window;
  2. if MCP is healthy but the session is wedged, abort the session, re-create a fresh session seeded from the DB transcript (`turnRecreated` machinery), and re-dispatch the SAME user message once;
  3. bound by `ORCHICON_ASK_MCP_RECONNECT_ATTEMPTS` (default 1) and the reply window; on budget exhaustion, fail the turn with a clear retryable message (the existing abort-and-fail path).
- The recycle is **transparent**: the turn continues on a healthy session, the user's message is never lost, and the reply window resets on the new session.

**D3 — Accurate turn status: server-confirmed progress (AC2).** Make the refresh/"still working" state truthful.
- Surface the collector's live progress on the conversation read path: add `Conversation.turn_last_activity_at` (and/or a derived `turn_progressing` / `turn_wedged` boolean) computed from the `turnRegistry` entry's stall-monitor `lastActivity` (service.go `turnStatus`). When in-flight, report `turn_progressing = now-lastActivity < noProgressWindow`.
- Frontend (`ask-orchicon.tsx`): show the `Connection lost — still working…` notice **only** when the server confirms the turn is progressing. Otherwise show an accurate `Turn stalled — stop or retry` state (with Stop + retry/interject affordances). Keep the existing server-turn re-attach; this changes the status label, not the re-attach mechanics.
- Fix the re-attach/completion ping-pong by keying completion to a single authoritative signal (the persisted `pending_assistant_message_id` in `ListMessages`) and making the re-arm guard server-progress-aware so a wedged turn is not re-armed as `working`.

**D4 — Interjection lands on a healthy session (AC3).** Guarantee a mid-run interjection is delivered without killing the run.
- `InterjectConversationTurn` already persists the user message and releases the one-turn gate. Change the dispatch so, before the fresh `runOneTurnAttempt`, the interject recycles a wedged session: when `turnRegistry.get(convID)` reports the superseded turn was wedged (D2 signal) or the session abort does not confirm, re-create a seeded session (`turnRecreated`) for the interjection.
- Make `SessionClient.Abort` distinguishable: if the session is unreachable/wedged, abort returns a signal so the dispatcher knows a recycle is warranted rather than assuming the queued `prompt_async` will answer at the next boundary.
- Net behavior: the interjection is persisted as a user message, a turn is registered, and it is answered by a healthy session (re-created if needed); a message is never silently dropped. If the serve is genuinely down, the collector's serve-down grace frees the turn with a clear retryable error.

**D5 — Per-serve dedicated transport (AC4, part 1).** Stop sharing `http.DefaultTransport` across all `SessionClient`s.
- Give `NewSessionClient` a dedicated `http.Transport` tuned for a single long-lived serve host: `MaxIdleConnsPerHost`/`MaxIdleConns` sized to the concurrent-session ceiling (or a dedicated client pool per host serve), `IdleConnTimeout` tuned, and keep the long-lived `/event` streams out of the idle pool.
- Ensure the `/event` SSE subscriptions are created on a separate transport from the short API calls, or at least that they never land in the shared idle pool.

**D6 — Non-blocking, bounded subscription feed (AC4, part 2).** `Subscription.read` must never park the SSE reader on a full channel.
- Change `read`'s event send to a **non-blocking select with `default` (drop newest when full)** — bus events are telemetry/liveness only; the durable record is the persisted transcript/reply, so a dropped event never loses data. Increase the buffer modestly (256->1024) for burst tolerance. The collector's `onStreamEvent` drop-on-full path is already safe.
- Consequence: a slow consumer no longer holds its connection/reader; backpressure becomes per-session-drop rather than a connection stall.

**D7 — Session admission bound + isolation (AC4, part 3).** Bound concurrent turns and document/contain the per-serve MCP blast radius.
- Add `ORCHICON_ASK_MAX_CONCURRENT_TURNS` (default e.g. 16): a turn that would exceed the cap is rejected with a clear `CodeResourceExhausted`/`FailedPrecondition` "too many conversations processing — retry" message instead of degrading. This bounds serve connections, bus read goroutines, and the 256/1024-event buffers.
- Keep MCP as per-serve but document the single-wedge cross-session radius and mitigate via D1 (serve restart). Per-session MCP isolation is out of scope (vendor); the watchdog is the containment.

**D8 — Regression coverage (all ACs).** Add tests using the existing `sessionTurnClient` fake (`chat_session_test.go`) and the opt-in DB/host-serve pattern (`ORCHICON_TEST_DSN`).
- **refresh-while-running:** start a turn via the fake client, drop the subscription/simulate the client disconnect, assert the collector persists the reply detached and the read path reports `turn_in_flight` + `turn_progressing` until the reply lands, then clears.
- **interject-while-running:** with a wedged (never-resolving-tool) turn, `InterjectConversationTurn` recycles the session and the interjection's reply is persisted on a fresh session id; the superseded turn's partial content is preserved; the one-turn gate is never left locked.
- **N-concurrent-sessions stress:** N simultaneous turns feed the fake bus; assert no reader goroutine blocks (drop-on-full), total open SSE connections stay within the cap, and per-turn replies are isolated (no cross-session event leakage).
- **watchdog:** a wedged MCP (`tools/list` failure) flips `MCPHealthy`/`ProbeUsable` to unhealthy and the `HostServe.Watch` restarts the serve; a wedged open tool call trips the tool-wedge monitor and recycles the session once, then succeeds on the re-dispatch.

## Consequences

- **Good:** MCP wedges are detected and healed (serve restart / session recycle) rather than silently failing turns; refresh reports accurate status; interjections are delivered on a healthy session; concurrent conversations no longer contend on `http.DefaultTransport` and a slow session no longer parks a connection; flakiness is bounded.
- **Watch:** D1 changes the serve liveness contract — a serve is restarted when MCP is unusable, which momentarily drops all sessions on it (they re-attach by id per `HostServe`/`Watch`). Keep the MCP probe cheap and failure-tolerant so a single slow `tools/list` does not churn restarts. The tool-wedge window must be generous enough (default 30s) to avoid recycling on legitimately slow tool calls.
- **Watch:** D2/D4 re-create a session on a wedge; the seeded transcript must be complete enough to continue (the `seedSystem` path already injects DB history), and the reconnect budget must not loop unboundedly (default 1).
- **Watch:** D6 dropping bus events on a full buffer is safe only because the durable record is the persisted reply; do not drop on the path that writes the sole copy of the assistant reply (`session.idle`).
- **Watch:** D7 admission adds a new failure mode; the message must be clear and the cap configurable (env + default), and the execution path is untouched.
- **Behavior parity:** the execution (work-item) session transport is unchanged except for the shared D1/D5/D6 primitives (`SessionClient`/`HostServe`/`Subscription`), which both paths use; those changes must be verified against the execution path's tests (`session_test.go`, `stream_state_test.go`, `session_repair_test.go`).

## Files touched (delta for the implementer)

| File | Change |
|---|---|
| `internal/opencode/session.go` | `SessionClient` dedicated `http.Transport` (D5); `Subscription.read` non-blocking drop-on-full (D6); `MCPHealthy` probe (D1) |
| `internal/opencode/servehost.go` | `HostServe.Watch`/`ProbeUsable` gate on MCP usability (D1) |
| `internal/askorchicon/chat.go` | tool-wedge detection + bounded session recycle in `collectConversationReply`/`runOneTurnAttempt` (D2); interject recycles a wedged session (D4) |
| `internal/askorchicon/stall.go` | `chatStallMonitor` unresolved-tool_use signal + `turn_progressing` derivation (D2, D3) |
| `internal/askorchicon/service.go` | `turnStatus`/`conversationRowToProto` expose `turn_progressing`/`turn_last_activity_at` (D3); admission cap on dispatch (D7) |
| `proto/orchicon/api/v1/ask_orchicon.proto` | `Conversation.turn_progressing` / `turn_last_activity_at` (D3) |
| `frontend/src/api/askOrchicon.ts` + `frontend/src/routes/ask-orchicon.tsx` | accurate status label; gate the `still working…` notice on server progress (D3); completion/re-attach de-ping-pong (D3) |
| `internal/askorchicon/chat_session_test.go` | refresh-while-running, interject-while-running, N-concurrent stress (D8) |
| `internal/opencode/session_repair_test.go` / `stream_state_test.go` | MCP-probe + tool-wedge watchdog tests (D1, D2) |

## Test plan (acceptance mapping)

DB/host-serve tests skip unless `ORCHICON_TEST_DSN` is set (repo pattern); `make ci` gates what runs in CI. The unit tests below use the `sessionTurnClient` fake (no live serve/model).

**AC1 (watchdog):**
1. `MCPHealthy` returns false when `tools/list` fails; `ProbeUsable`/`Watch` treats it as unhealthy and restarts the serve.
2. A wedged open tool call (tool_use with no terminating event) trips `chatStallMonitor` tool-wedge within the window; the collector aborts, re-creates a seeded session, and re-dispatches the same user message once (assert the re-dispatch reaches a fresh session id); success on the re-dispatch.
3. Reconnect budget exhausted -> turn fails with a clear retryable error, no infinite loop.

**AC2 (refresh survival):**
4. A turn started, then the client disconnect simulates a refresh; the collector persists the reply on `pending_assistant_message_id`; `GetConversation` reports `turn_in_flight=true` and `turn_progressing=true` while the reply is absent, and `turn_progressing=false` (or a wedge flag) once the monitor reports no progress; completing the reply clears both.
5. `Conversation` proto carries the new fields; frontend shows `still working…` only when `turn_progressing`, else `Turn stalled — stop or retry`.

**AC3 (mid-run interjection):**
6. With a wedged in-flight turn, `InterjectConversationTurn` persists the user message, releases the gate, re-creates a seeded session, and persists the interjection's reply on that session; the superseded turn's partial content is preserved as a plain assistant message.
7. Interjecting with no running turn behaves exactly like `ChatStream` (idempotent).

**AC4 (multi-session stability):**
8. N concurrent turns over the fake bus: no `Subscription.read` blocks (drop-on-full), open connection count <= `ORCHICON_ASK_MAX_CONCURRENT_TURNS`, and per-turn replies are isolated (no cross-session event leak).
9. Exceeding the admission cap returns a clear busy error without degrading active turns.

**Non-goals (recorded, not built):** per-session MCP isolation (vendor — opencode owns MCP connections), a UI conversation-storage migration, and changes to the work-item execution transport semantics beyond the shared `SessionClient`/`HostServe`/`Subscription` primitives.
