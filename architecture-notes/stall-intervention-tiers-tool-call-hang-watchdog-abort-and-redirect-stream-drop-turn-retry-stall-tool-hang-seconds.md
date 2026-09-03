# Stall intervention tiers: tool-hang watchdog (abort-and-redirect) + stream-drop turn retry

## 1. Context

Workers die two ways that today kill the whole execution: (a) an in-flight tool call
(`tail -f`, `sleep N`, dead server) with zero events — nudges queue behind the hung turn
and never land; (b) a long single-turn model generation truncated mid-stream
(`model response stream truncated or event dropped`) — the turn dies and `sessionRun.run()`
finalize fails the execution outright instead of retrying the turn.

This plan adds the graduated tiers between "nudge that can't land" and "kill + cold recovery".

## 2. Evidence (read this session — file-level proof)

Setting plumbing — ALREADY LANDED, do not rebuild:

- `internal/db/settings.go:TenantSettingsRow.StallToolHangSeconds` + SELECT/INSERT/UPDATE/SCAN
  round-trip (`:83`, `:122`, `:199`, `:261`, `:291`). Column exists.
- `proto/orchicon/api/v1/settings.proto:78` `stall_tool_hang_seconds = 34` with doc comment.
  Generated `frontend/src/api/gen/orchicon/api/v1/settings_pb.ts:113` exists.
- `internal/scheduler/bridge.go:70` `ExecutionManifest.StallToolHangSeconds` with D6 doc comment.
- `internal/scheduler/reconciler.go:1029,1046,1086` reads `s.StallToolHangSeconds`
  into `stallToolHang` and copies onto the manifest. Dispatch path done.
- `frontend/src/routes/settings.tsx:484,518,553,690-696` `draftToolHang` StallField
  (label "Tool hang (seconds)", placeholder "180") — Settings UI done.
- `internal/askorchicon/tool_diagnostics.go:147` + `internal/askorchicon/tools.go:751`
  expose `stall_tool_hang_seconds` to Ask Orchicon — registry in sync.

Session transport — PARTIALLY LANDED (D6 first cut, gaps below):

- `internal/opencode/session_run.go:70-81` `sessionRun.toolHangWindowVal/hangLatched/hangMu/`
  `toolTrackName/toolTrackAt/toolInFlightNow` fields.
- `internal/opencode/session_run.go:154-182` `toolHangWindow()` + `initNudgeTuning()`
  tool-hang resolution (manifest -> window).
- `internal/opencode/session_run.go:190-265` `startToolHangWatchdog()` (1s ticker) +
  `checkToolHang()` (latch + `OnStall(..., "stalled:tool_hang:"+tool, false)` + `SendMessage`
  redirect + `recordPart(UserMessage, source=tool_hang_redirect)`) + `observeToolStart/End` +
  `toolHangRedirectMessage()`.
- `internal/opencode/session_run.go:499-508` `run()` wires monitor + hang watchdog + SSE loop.
- `internal/opencode/session.go:313` `SessionClient.Abort()` (POST `/session/:id/abort`,
  "session kept; next SendMessage starts a new turn") — the abort surface to reuse.
- `internal/opencode/session.go:ToolStartFromBus` (raw-bus tool-start incl. running/no-status)
  + `LegacyEventFromBus` (completed/error only) + `TokenDeltaFromBus` — event taxonomy.
- `internal/opencode/session_run.go:613-680` `handleEvent()` delta/tool-start/tool-end wiring
  (`observeToolEnd()` on deltas and on any legacy telemetry).
- `internal/opencode/session_run.go:833-913` `recordStreamError()` + `recycleOnInfraModelTurn()`
  (abort-echo guard precedent) + `1219-1243` `onStall()` fatal branch
  (`finish(false)` FIRST, then `Abort` — the ordering that preserves the true reason).
- `internal/opencode/session_run.go:567-573` finalize step-balance guard
  (`stats.unfinished()` + missing marker -> fail + "truncated or event dropped" suffix) +
  `1324-1510` `completionProbeDecision/maybeProbeCompletion/sendCompletionProbe`
  (markerless-idle probe sharing the nudge budget).
- `internal/opencode/progress.go:57-120` `stallWindows` + `defaultStallWindows()` +
  `stallWindowsFromManifest()` — **does NOT handle `StallToolHangSeconds` today**.
- `internal/opencode/progress.go:126-193,365-510` `progressMonitor` (`observe/check/run/
  checkRevival/revive`, `isFatalStall`) — **has NO tool-hang signal today**.
- `internal/opencode/session_test.go:725-841` `TestToolHangWatchdogFiresAndLoopCompletes`
  (latch + advisory OnStall + redirect send + transcript source) — extend, don't replace.
- `internal/orchicon/bridge.go:118-220` `NativeBridge.Start` + `Session.Run` loop — sibling
  owns native-loop parity (this plan only pins the contract it must reproduce).

## 3. Decisions (one option + one-line rationale each)

- D1. Detection lives in `progressMonitor` (new `toolHang` window + in-flight tool
  tracking), `sessionRun` keeps only the actuator (abort + redirect). Rationale: monitor
  is the pure, clock-injectable, unit-testable owner of ALL stall signals; the run
  owns I/O (Abort/SendMessage).
- D2. Env name is `ORCHICON_STALL_TOOL_HANG_WINDOW` (task contract). Rationale: every
  other stall window is `ORCHICON_STALL_*`; the current `ORCHICON_TOOL_HANG_WINDOW` is
  a D6 typo to keep as a deprecated fallback for one release.
- D3. Abort-then-redirect order: `Abort` FIRST, `finish()` NEVER, `SendMessage` redirect
  second. Rationale: `Abort` cancels only the in-flight turn (serve keeps session +
  history); queuing the redirect behind a hung turn without aborting is why the current
  cut cannot land the redirect.
- D4. `stalled:tool_hang:<tool>` is ADVISORY (`fatal=false`, `isFatalStall=false`).
  Rationale: matches all looping-worker signals (nudge-first routing); kill path stays
  `no_progress` + exhausted nudge budget (`onStall` escalate branch).
- D5. Stream-drop retry is a same-session re-prompt (bounded 2), NOT a session recreate.
  Rationale: transcript already holds the partial turn; recreate would duplicate history
  and nuke prefix cache — resend a short continue/redirect turn instead.
- D6. Retry budget is a dedicated `streamRetries` counter (max 2), SEPARATE from the
  nudge budget. Rationale: truncation is transport failure, not worker misbehavior —
  spending nudges on it starves the advisory path.
- DECISION (revisitable): turn-retry trigger classification = `session.error` with
  truncation/drop signature OR `step_finish(reason=unknown/0-tokens)` mid-turn OR SSE
  disconnect with `pendingTurns>0` and no `session.idle`. Default: all three trigger;
  narrow later if false-positives appear.

## 4. Design — Tier A (tool-hang abort-and-redirect)

Move DETECTION into `progressMonitor` (`internal/opencode/progress.go`); keep ACTUATION in
`sessionRun` (`internal/opencode/session_run.go`).

- 4.1 `progress.go:stallWindows` += `toolHang time.Duration`. `defaultStallWindows()`
  default `envDuration("ORCHICON_STALL_TOOL_HANG_WINDOW", 180s)` with deprecated fallback:
  if `ORCHICON_TOOL_HANG_WINDOW` set and canonical unset, use it (log once via adapter).
  `stallWindowsFromManifest()` += manifest branch mirroring textLoop pattern:
  `if m.StallToolHangSeconds != 0 && env canonical=="" && env legacy==""` →
  `w.toolHang = seconds`; negative => `<=0` => disabled (existing `>0` gates honor it).
  Insertion point: `progress.go:92-120` after the repetition-window block.
- 4.2 `progressMonitor` += `toolHangName string; toolHangStart time.Time; toolInFlight bool`
  (guarded by existing `mu`). New methods `observeToolStart(name)` /
  `observeToolEnd()` / `noteToolActivity()` called from `observe()`:
  `tool_use/tool_call` with non-terminal status arms/refreshes; completed/error legacy
  events + `text/delta/step_finish/file_diff` disarm-or-refresh per D-hang rule:
  ANY event while in-flight refreshes `toolHangStart` (zero-events-only trips), model
  deltas prove no hang. Only the longest/single in-flight call tracked (one slot;
  a new start supersedes). Insertion: `progress.go:193-260` `observe()` switch.
- 4.3 `check()` += tool-hang branch BEFORE `no_progress` (a hung tool must redirect
  before silence kills): `if w.toolHang>0 && toolInFlight && now-toolHangStart>w.toolHang`
  → latch (`toolHangFired bool`, once per monitor=per execution) →
  return `"stalled:tool_hang:"+name`. Disabled gate: `w.toolHang<=0` skips. Reset:
  `observeToolEnd()` clears `toolInFlight`. `isFatalStall()` UNCHANGED (hang stays
  advisory). Insertion: `progress.go:406-460` top of `check()`.
- 4.4 `sessionRun.checkToolHang()` becomes the actuator for the monitor signal (or
  keep the 1s ticker as the poller calling `monitor.checkToolHang()`): on trip →
  (1) `OnStall(ctx, id, "stalled:tool_hang:<tool>", false)`; (2) `client.Abort(ctx,
  sessionID)` FIRST (esc-esc: kill in-flight turn, keep session) — THIS CALL IS MISSING
  TODAY (`session_run.go:220-265` only SendMessages); (3) `SendMessage(redirect)` +
  `bumpPending()` + `recordPart(UserMessage, source=tool_hang_redirect)`; (4) set
  `hangLatched=true`. Order matters: Abort→redirect, never `finish()`.
- 4.5 Abort-echo guard: extend `recordStreamError()` (`session_run.go:835-860`) to ignore
  `session.error: Aborted` arriving within N sec after a tool-hang abort (new
  `hangAbortAt time.Time`): no `lastStreamErr` overwrite, no `consecutiveSessionErrors`
  bump, no `finish()`. Same pattern as the fatal-stall `finish()-first` ordering
  (`session_run.go:1233-1241`). Aborted-turn events must never settle/fail the run.
- 4.6 Escalation unchanged: monitor keeps ticking; still-stuck worker trips
  `text_loop/repetition/no_progress` → `onStall()` → `maybeNudge()` budget
  (`session_run.go:1219-1283`) → exhausted → `finish(false)+Abort` (existing kill path).
  `no_progress` untouched (silent-death backstop per task §4).

## 5. Design — Tier B (stream-drop turn retry, bounded 2)

- 5.1 New `sessionRun` fields (`session_run.go:26-81` struct block): `streamRetries int`
  (+ `const maxStreamRetries = 2`), `hangAbortAt time.Time` (echo guard), reuse
  existing `probePending/probeGracePending/nudgesSent` untouched. Retry budget is
  DEDICATED — never touches `nudgesSent`.
- 5.2 Classifier `isStreamDrop(evt BusEvent, stats *execStreamState) bool` (new func near
  `sessionErrorMessage`, `session_run.go:~800-835`): true for (a) `session.error` whose
  message matches truncation/drop signature (contains "truncat"/"event dropped"/
  "stream"+"drop"/EOF-reset substrings — enumerate explicitly); (b) legacy
  `step_finish` with reason unknown/empty + 0 tokens mid-turn (thread through
  `handleEvent` legacy branch); (c) `runSSE` reconnect with `pendingTurns>0` and no
  `session.idle` since disconnect (transport blip, not a clean finish). False for clean
  errors (auth/404/permission) and for Abort-echoes (5.4 guard first).
- 5.3 Actuator `retryStreamTurn(reason)` (new, beside `sendCompletionProbe`): if
  `streamRetries >= 2` → fall through to existing kill path (`finish(false, reason)` +
  `Abort`); else `streamRetries++`, `client.Abort` (only if a turn is still in flight),
  `SendMessage(short continue-turn: "your previous turn was cut off mid-stream; continue
  from where you stopped without repeating completed work")`, `bumpPending()`,
  `recordPart(UserMessage, source=stream_retry)`. No transcript duplication: partial
  turn parts already recorded stay; retry appends ONE user turn (same rule as
  completion probe `source=nudge`). Insertion call sites: `recordStreamError()`
  stream-drop branch + `runSSE()` reconnect-drop branch; finalize guard
  (`session_run.go:567-570`) must NOT fire while a retry is in flight
  (`pendingTurns>0 || probePending` → skip; retry resolves via normal idle/settle).
- 5.4 Abort/echo + settle guards: `recordStreamError` order: (1) Abort-echo guard
  (fatal-stall AND tool-hang/staleness windows) → ignore; (2) stream-drop classifier →
  `retryStreamTurn`; (3) else existing infra-recycle/fail path. `allTurnsDone/settleFinish`
  unchanged except they must not settle while `streamRetries` turn is pending
  (pendingTurns covers it — verify, don't add a second gate).
- 5.5 Native parity contract (sibling owns it, this plan pins it): `Session.Run` loop
  (`internal/orchicon/`) must reproduce: abort-in-flight-turn-keeps-session,
  one-shot tool-hang latch + redirect, bounded-2 same-session turn retry without history
  duplication, Abort-echo not treated as death. No code here — contract note only.

## 6. Wiring / registration

- Settings chain (verify, mostly done): `tenant_settings.stall_tool_hang_seconds`
  → `db/TenantSettingsRow` → proto `settings.proto:78` → `ExecutionManifest` →
  `reconciler.go:1029,1046,1086` → `stallWindowsFromManifest` + `initNudgeTuning`.
  ONLY gaps to close: (a) `stallWindowsFromManifest` tool-hang branch (§4.1);
  (b) canonical env `ORCHICON_STALL_TOOL_HANG_WINDOW` in `defaultStallWindows()` +
  `toolHangWindow()` + `initNudgeTuning` manifest-vs-env precedence (env wins;
  manifest `!=0` beats code default; negative disables) — touch
  `progress.go:65-73` and `session_run.go:154-182`.
- `OnStall` reason `stalled:tool_hang:<tool>` flows through existing
  `scheduler.ExecutionCallbacks.OnStall` (`bridge.go:109-118`) →
  `reconciler.go:2002 OnStall` (advisory: non-terminal stalled notice). No reconciler
  change (fatal=false already handled).
- Ask Orchicon registry already exposes the knob (`tools.go:751`); no change unless
  help text needs the canonical env name — one-line description touch if touched.
- Settings UI already has the field (`settings.tsx:690-696`); only touch if the
  description must name the canonical env var / 0-vs-negative semantics.

## 7. Tests (SSE owns; architect pins the list)

- Unit (`internal/opencode/progress_test.go` style, clock-injected `newTestMonitor`):
  trip (in-flight + silence > window → `stalled:tool_hang:<tool>`); reset (completion
  event before window → no trip); single-slot supersede (second start replaces first);
  latch (second window after fire → no second reason); disabled (`toolHang<=0` → never);
  escalation-neutral (`isFatalStall(tool_hang)==false`, `no_progress` still fires).
- Unit (`internal/opencode/session_test.go` style, httptest serve): abort-then-redirect
  order (Abort endpoint hit BEFORE SendMessage); echo guard (post-abort `Aborted`
  error → no `lastStreamErr`, no `finish`, run alive); retry bound (2 retries then fail);
  no-duplication (retry appends exactly one `source=stream_retry` user part).
- Integration (mock or live serve per `session_live_test.go` pattern): hang a tool past
  a short window → assert session answers the redirect and completes the task
  afterwards (the AC: "session survives abort-and-redirect and completes").

## 8. Numbered implementation list (mechanically executable, zero blocking questions)

1. `internal/opencode/progress.go`: add `toolHang` to `stallWindows`; default from
   `ORCHICON_STALL_TOOL_HANG_WINDOW` (fallback legacy `ORCHICON_TOOL_HANG_WINDOW`);
   add manifest branch in `stallWindowsFromManifest`; add in-flight fields +
   `observeToolStart/End` + activity refresh in `observe()`; add trip+latch branch in
   `check()`; keep `isFatalStall` unchanged. Extend `progress_test.go` (trip/reset/
   latch/disabled/escalation-neutral).
2. `internal/opencode/session_run.go`: rewire `checkToolHang/startToolHangWatchdog` to
   the monitor signal; insert `client.Abort` BEFORE `SendMessage(redirect)`; add
   `hangAbortAt` + extend `recordStreamError` abort-echo guard; keep `finish()` out of
   the hang path; keep `source=tool_hang_redirect` record. Extend `session_test.go`
   (abort order, echo guard, latch-once).
3. `internal/opencode/session_run.go`: add `streamRetries/maxStreamRetries=2` +
   `isStreamDrop` classifier + `retryStreamTurn` actuator; hook into `recordStreamError`
   and `runSSE` reconnect; guard finalize (`:567-570`) against in-flight retry; record
   `source=stream_retry`. Unit tests for bound + no-duplication + fall-through kill.
4. Env rename: canonical `ORCHICON_STALL_TOOL_HANG_WINDOW` in `progress.go` +
   `session_run.go:toolHangWindow/initNudgeTuning`; keep legacy as fallback; update
   proto comment (`settings.proto:70-78`), Ask tool description (`tools.go:751`),
   settings UI description (`settings.tsx:692`) ONLY if strings name the env.
5. Integration test proving session-survives-abort-and-completes + `go test ./internal/opencode/` +
   `semgrep scan --config .orchicon/semgrep_orchicon.yml --error` scoped to touched files.
6. Native-parity note to sibling session-engine feature (no code here): reproduce
   abort-keeps-session + latch-once + bounded-2 retry + echo guard natively.
