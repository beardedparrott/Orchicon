# ADR — Actionable runtime image build/deploy failures

## Title
Structured failure reasons + readable error UI + stream-drop / ghost-build recovery

## Status
Proposed

## Context
Runtime image builds surface as `ConnectError: [internal] image build failed: runtime image build: exit status 1` and UI renders raw red error text. Two real incidents:

1. `:web-research` build failed `exit status 1`; true cause was `EBADENGINE / engine mismatch: Playwright 1.62 requires node >=20, base ships 18.20.4` buried in `BuildLog` only.
2. Rebuild dropped build-log stream `ConnectError: [unknown] Error in input stream` — daemon build kept running, row stuck `building` 25+ min, spec update rejected (`draft/failed` only), no cancel/reset API — recovery required host daemon restart.

Current architecture:
- `internal/runtime/images.go` `handleBuild` streams `docker build` stdout/stderr as NDJSON `AgentEvent{stream,data}` and final `{event:"exit", exit_code, error}`. Client `internal/runtime/client.go:BuildImage` scans stream, returns exit code. Service `internal/runtimeimage/service.go:buildCore` locks row `FOR UPDATE`, marks `building` (StatusOnly, no version bump), streams via `rt.BuildImage`, truncates log to 1MiB, on failure sets `status=failed, error=err.Error()` (currently `runtime image build: exit status 1`), success stamps `built_version=version`.
- No failure classification, no `failed_step`/`failure_reason`/`log_tail` fields. Proto `api/proto/orchicon/api/v1/runtime_image.proto` `RuntimeImage` has `status, build_log, error` only; `BuildRuntimeImageResponse` streams `{log, status, tag, skipped, error}`.
- DB `internal/db/runtime_images.go` row: `status, build_log, error, version, built_version`.
- Frontend runtime-images page raw red text; build log single-line horizontal scroll.
- Stream lifecycle: `handleBuild` kills `docker build` on `r.Context().Done()` (client disconnect). `buildCore` runs outside row lock — daemon build orphan persists; no reconciliation, no heartbeat/TTL, no cancel RPC.
- Seeder/canned images share same table with `status=building` guard.

Constraints:
- Preserve full `BuildLog` retention + success path unchanged. `make ci` must pass. Public repo, never touch PROD.
- `/tmp` exec-capable tmpfs; `GOTMPDIR` inside worktree.

## Decision

### 1) Failure classification — backend parser

Introduce `internal/runtimeimage/failure.go` pure parser:

```go
type BuildFailure struct {
  FailedStep string
  Reason     string
  Hint       string
  Category   string // engine_mismatch | apt_dpkg | network | invalid_tag | oom | dockerfile | unknown
  LogTail    string // last N lines (40-80, truncated per line)
}
func ClassifyBuildLog(log string, exitCode int, exitErr string) BuildFailure
```

- Parser extracts failing step via regex: `^Step \d+/\d+ :`, `executor failed running \[.*\]: exit code`, `ERROR \[stage\]` (BuildKit). Last match wins. Tests per class.
- Categories + hints:
  - `EBADENGINE` / `engine.*mismatch` / `requires Node >= 20` -> "Playwright requires Node >= 20 but the base image ships Node 18 — install Node 20+ in Dockerfile before installing Playwright (e.g. via NodeSource or `mise install node@20` / `toolchains: [\"node 20\"]`)."
  - apt/dpkg: `E: Unable to locate package`, `dpkg returned an error` -> hint install source/apt update.
  - network/timeout: `dial tcp`, `TLS handshake timeout`, `Could not resolve host`, `429` -> retry hint.
  - invalid tag: `invalid reference format` -> tag hint.
  - OOM: `killed`, `signal 9`, `out of memory`, `ex137` -> increase memory.
  - Dockerfile syntax -> surface syntax error tail.
- `LogTail` = last 60 lines capped 8KiB; used when `Reason` empty fallback to `exit status 1: see log tail`.
- Done entirely from stored `BuildLog`; no daemon log fetch. Daemon still streams raw lines; classification happens service-side after `buildCore` collects `logBuf`, so no docker output parsing in hot path changes daemon contract.

### 2) Persist + expose structured fields

- DB migration: add columns to `runtime_images`: `failure_reason TEXT`, `failed_step TEXT`, `log_tail TEXT`, `failure_category TEXT` (or single JSONB `failure JSONB`). Prefer discrete columns for query simplicity; nullable.
- `db.RuntimeImageRow` and `db.UpdateRuntimeImageFields` add fields; `toProto()` maps to proto.
- Proto additive only (backward compatible):

```proto
message RuntimeImage {
  // existing ...
  string failure_reason = 15;
  string failed_step = 16;
  string log_tail = 17;
  string failure_category = 18;
}
message BuildRuntimeImageResponse {
  string log = 1;
  RuntimeImageStatus status = 2;
  string error = 3; // deprecated: use failure_reason
  string tag = 4;
  bool skipped = 5;
  string failure_reason = 6;
  string failed_step = 7;
  string log_tail = 8;
  string failure_category = 9;
}
```

- `buildCore` on failure: `f := ClassifyBuildLog(buildLog, exit, failMsg)`; persist `failure_reason=f.Reason`, `failed_step=f.FailedStep`, `log_tail=f.LogTail`, `failure_category=f.Category`, `error=f.Reason` (keep legacy `error` but now human-readable). Success clears failure fields. Update `truncate` still caps `build_log`.
- `GetRuntimeImage`/`ListRuntimeImages` return populated fields; `ListAvailableRuntimeImages` unchanged. MCP `orchicon_*` surfaces same proto.

### 3) Stream-drop / ghost-build — backend + API reconciliation + cancel/reset

Problem split: (a) client-perceived failure vs daemon still building, (b) row stuck forever.

- **Daemon `handleBuild` robustness**: ensure final `exit` event always flushed even when client disconnects mid-stream. Today `r.Context().Done()` kills `cmd`; that leaves no exit event for already-disconnected scanner. Instead: on context cancel, do NOT kill immediately — wait up to 5s for `cmd.Wait()` and encode final `exit`; only kill if still running. Also set `w.Header().Set("X-Accel-Buffering","no")` to avoid proxy buffering hiding stream. Scanner in `client.BuildImage` currently `return 1, err` on `sc.Err()` — surface as typed `StreamDroppedError` so service can distinguish drop vs build failure.

- **Service `buildCore` drop handling**: if `rt.BuildImage` returns `StreamDroppedError`, do NOT mark `failed` immediately. Instead query daemon liveness: `rt.Images(ctx)` / new `GET /v1/images/build?tag=` probe (or reuse `docker ps` via daemon lightweight `GET /v1/images/status?tag=`). If daemon reports build still running, keep row `building` but set `error="stream disconnected — build still running (reconnect to follow logs)"` (not clearing). Return `CodeUnavailable` with reason to UI so UI can poll. If daemon unreachable -> `failure_reason="daemon unreachable — build may still be running on host"`.

- **Reconciliation**: new `runtimeimage.Reconciler` (or extend existing `daemonPool` idleReap-like ticker) every 60s: `SELECT * FROM runtime_images WHERE status='building' AND updated_at < now()-5m` (TTL). For each, probe daemon: if image now exists with `spec_version == row.version` -> mark `ready, built_version=version`; else if daemon reports no active build and image missing -> mark `failed` with `failure_reason="build abandoned (no image produced)"` + existing log tail. Also on daemon restart reconciliation: `daemon.ListenAndServe` already `resetPool()` for runtimes; add `ReconcileBuildingImages()` at boot that transitions stale `building` rows older than boot time to `failed` with hint "daemon restarted while building — retry Deploy". Ensure `failed` allows spec update (already) and retry.

- **Cancel/reset API**: new RPC `CancelRuntimeImageBuild` (id + version) and daemon `DELETE /v1/images/build?tag=` (or `POST /v1/images/cancel`). Service verifies caller tenant, row `building`, then `rt.CancelBuild(tag)` which sends SIGTERM to `docker build` PID (track builds in `daemon.activeBuilds map[tag]*exec.Cmd` with mutex). On cancel, daemon encodes `exit` with `code=130` + `error="cancelled"`; service persists `failed` with `failure_reason="cancelled by user"`. Also `ResetRuntimeImage` allowed from `building` when stuck > TTL to force `failed` without daemon (escape hatch for orphan rows where daemon already gone).

- **Edit guard**: `UpdateRuntimeImage` today rejects `building` unconditionally. With cancel, UX is Cancel -> then edit. Optionally allow `force=true` to auto-cancel then edit, but keep simple: UI shows Cancel button when `building`.

### 4) Frontend — readable alert + log viewer

- **Alert/banner component** `web/src/components/RuntimeImageFailureAlert.tsx` (or reuse `Alert`): props `{reason, category, failedStep, logTail, buildLog}`. Visual: amber/warning `bg-amber-50/10 border-amber-500/30` or `neutral-900` high-contrast; icon (AlertTriangle); title `FailedStep` line; body `Reason` (not `error`); never `text-red-500` on dark. WCAG AA contrast verified (amber-600 on zinc-900 = 4.8:1). Copy button copies `Reason + failedStep + logTail`.
- Runtime Images page `web/src/pages/RuntimeImages.tsx` (or `RuntimeImagesPage.tsx`): when image `status==failed`, render alert inline under row + Deploy button disabled until Cancel/Retry. Building with stream drop: show `Building... (stream disconnected — still running)` spinner + `Cancel` + `Re-connect logs` button that re-streams `BuildRuntimeImage` (daemon will re-attach if buildId still active, otherwise reconciliation says failed). Success path untouched.
- **Build-log viewer**: replace single-line `overflow-x-auto whitespace-nowrap` with `web/src/components/BuildLogViewer.tsx`: `pre.font-mono.text-xs.bg-zinc-950 border rounded max-h-[32rem] overflow-y-auto whitespace-pre-wrap break-words` with `ref` + `useEffect(() => el.scrollTop = el.scrollHeight, [log])` for auto-scroll following stream; copy button; collapsible (shadcn Collapsible). Streams append lines via state, viewer appends without re-rendering full log (virtualized if >10k lines, else plain). Keep `maxBuildLogLen` server cap.

### 5) Testing strategy

- Parser unit tests: table per category + `failedStep` extraction, `logTail` slicing, OOM, invalid tag, network, apt, Dockerfile, unknown falls back to tail. Golden logs from real web-research failure.
- Stream-drop: mock `runtime.Client` returning `StreamDroppedError`; assert service keeps `building` + surfaces dropped reason, not `exit status 1`.
- Reconciliation: insert `building` row with `updated_at = now()-10m`, daemon probe stub returns not-building -> assert row transitions to `failed` with abandoned reason; and success path where image appears.
- Cancel: `DELETE /v1/images/build` kills cmd, service transitions.
- Frontend: Vitest + Testing Library for alert contrast snapshot, log viewer auto-scroll behavior (mock scrollHeight), no red text assertion (`queryByText` not containing `text-red`).
- `make ci` (go vet + semgrep scoped to `git diff origin/develop`) must pass.

## Alternatives considered

1. **Parse in daemon instead of service** — rejected: daemon is host privileged, minimal logic; service owns DB/proto, easier to test without docker.
2. **Single JSONB `failure` column vs discrete columns** — discrete chosen for simpler filtering + proto mapping; JSONB would need migration anyway and obscure indexing. Could add JSONB later if schema grows.
3. **WebSocket/SSE for log stream vs NDJSON** — keep NDJSON (current `application/x-ndjson`) to avoid new transport; just make it resilient with reconnection.
4. **TTL reconcile only, no cancel** — rejected: leaves operator stuck until TTL fires (5m) for ghost builds; cancel is required acceptance criterion ("never requires daemon restart").

## Consequences

- Positive: builds always produce actionable reason + hint without opening full log; stream drops are visible and recoverable; no daemon restart needed; UI accessible and readable.
- Negative: migration adds columns (needs `make gen` + atlas). Reconciler adds periodic probe load (60s, negligible). Daemon `activeBuilds` map needs concurrency safety.
- Operational: `ORCHICON_RUNTIME_BUILD_TTL` env (default 5m) tunable; metrics `runtime_image_build_failed_total{category}` for observability.

## Security / Operability

- Build log sanitization: hints never echo secrets (dockerfile override/ENV not logged). Log tail truncated; env map not in snapshot.
- Daemon `activeBuilds` only keyed by tag allowlisted via `validateBuild` (slug pattern + base check), so cancel surface closed.
- Audit: `runtime_image.build_failed` with category, not full log.

## Rollout

- Phase 1: parser + DB/proto + service persist + daemon activeBuilds + cancel RPC + reconciliation (backend).
- Phase 2: frontend alert + log viewer + stream-drop polling + cancel button.
- Feature flag: parser categories behind func; deploy behind `RUNTIME_IMAGE_FAILURE_V2` if needed (default on).

## Review checklist

- Scales at 10x: parser O(n) on log tail (1MiB) per build; reconciliation scans only `building` rows (tiny). No 10x break.
- Problem fit: directly addresses incident anchors (engine mismatch surfaced, stream-drop ghost eliminated).
- Security/observability/operability: covered above.
- Trade-offs documented, consistent with existing `StatusOnly` / `built_version` rebuild gate architecture.

