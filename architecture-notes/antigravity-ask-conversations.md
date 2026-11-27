# Antigravity Ask conversations — implementation plan (rev 2)

## Finding
This run's branch started at stale `4bacf12a6` with no antigravity code. `develop` (948abf84b) has the worker bridge (#1032),
model listing (#1034) and `live_ask_test.go` (#1038, `TestAskLive` schema guard). A prior attempt at THIS item exists on
`origin/6-antigravity-ask-…-wezsc2ftrwc2k5a7` (HEAD a94256ecb) implementing scope items 1–7.

**Done by the architect step (committed on this branch):** fast-forwarded to `develop`, then merged that prior branch
(`git merge origin/…wezsc2ftrwc2k5a7`) — clean, no conflicts. Verified at the merge: `go build ./...`, `go vet` (antigravity, server,
toolclass, askorchicon, aigateway) and `go test ./internal/antigravity/... ./internal/toolclass ./internal/askorchicon
./internal/aigateway ./internal/protectedpath` all PASS. The SSE therefore starts from a working tree; its job is verification,
the gaps below, and the live evidence — not a rewrite.

## What the code contains (proof)
- `internal/antigravity/ask.go` — registry `askByConv`/`askBySID`, `CloseAsk`, resume (session/resume→session/load), in-place model switch, cwd via `SetProjectDirResolver`.
- `ask_turn.go` — turn = `session/prompt`; `ask_question` refused with reason redirecting to `orchicon_ask_user` (ACP has no answer channel); Orchicon MCP in Ask scope.
- `ask_permission.go` — parks `session/request_permission`, emits `permission` bus event, maps once/always/deny to agy option ids, teardown → reject.
- `ask_mapper.go`, `ask_bus.go` — reasoning/tool/usage streaming, owner kind `ask_conversation`, Ask diff-ledger hook.
- `modegate.go` — Brainstorm/Iteration/Quick Work, pre-card refusal + `--settings` permissions.deny; `toolclass.go` has agy tool names.
- `internal/server/server.go:1024,1044` `antigravityBridge.CloseAsk()` beside claude's; `antigravity_settings.go` project-dir resolver.
- Tests: `ask_permission_test.go` (AC2 `-run AskPermission`), `ask_modegate_test.go` (AC3 `TestAskModeGate`), `ask_session_test.go`, `aigateway/ask_kinds_antigravity_test.go` (AC1), `ask_live_test.go` (`TestAskLive_OneTurnWithACard`, opt-in), `live_ask_test.go` (`TestAskLive`, always-on schema guard).

## Known risks the SSE must check (from prior run + docs/adapters/antigravity-acp.md)
1. agy 1.5.13 default mode may AUTO-APPROVE file edits with no `request_permission` (doc ~line 515) and never emits `agent_thought_chunk` (doc lines 89–101, 331). So AC4's "card for a write" and "streamed reasoning" may be unreachable live. The doc states the Ask launch passes a settings/mode that forces requests; SSE must confirm in `launch.go`/`procspec.go` that Ask sessions are launched so writes DO request permission (without introducing an auto-approve key — `assertSettingsNotBypass` forbids it). Reasoning: map `thoughtTokens` into the usage/context meter and document that no reasoning text exists; do not fabricate.
2. Two live tests both named like `TestAskLive*` — confirm no duplicate symbol (build is green now; keep it so).
3. Three-clients rule: no new field/RPC/setting is added, so no web/TUI/desktop change. Only check that no client hardcodes the Ask kind list: `grep -rn "askCapableKinds\|ask_capable" frontend/src internal/tui` (they consume `AskCapableKinds` from the RPC). Report this in the summary.

## Numbered steps for the SSE
1. `git log --oneline -3` shows the merge commit; `ls internal/antigravity/ask*.go` lists ask.go, ask_turn.go, ask_permission.go, ask_mapper.go, ask_bus.go.
2. Run gate: `cat .orchicon/verify`, then execute it (build, vet, `go test ./internal/... ./cmd/...`). Fix any failure it surfaces; none expected.
3. AC2/AC3 evidence: `go test ./internal/antigravity/... -run 'AskPermission|AskModeGate' -v` (save output).
4. AC1: `go test ./internal/aigateway -run Antigravity -v` plus, on the DEV plane (:8080, never :8091), call `AIGatewayService.ListAdapterKinds` and capture the JSON containing `antigravity` in `ask_capable_kinds` (requires dev binary rebuilt from this branch; restart dev and say so).
5. Resolve risk 1: read `launch.go`/`procspec.go`/`modegate.go` argv for Ask; if agy launched in a mode that auto-approves edits, use agy `--settings` `permissions.ask`/mode (non-bypass) so write tools request permission; add a fixture test in `ask_permission_test.go`. Update docs/adapters/antigravity-acp.md if behaviour changes.
6. Run the three-clients grep (risk 3) and ensure the `ask_question` rejection text is documented (it is, `ask_turn.go` ~471, doc).
7. Commit any change; `git status --porcelain` clean.
8. Hand AC4–AC6 (live GUI cards in Iteration/Brainstorm, `orchicon_ask_user` card, plane restart resume + reused-session log line) to QA: these need real `agy` + auth on DEV with a cheapest `antigravity/google/<model>`, one minimal prompt each, cleanup afterwards. Mark NOT VERIFIED with the precise blocker if agy/credentials are absent; never invent a harness (`ORCHICON_ANTIGRAVITY_LIVE=1 go test ./internal/antigravity -run TestAskLive_OneTurnWithACard` exists).

## Decisions
- Reuse prior implementation (green on current develop) rather than rewrite.
- `ask_question`: documented rejection redirecting to `orchicon_ask_user`.
- No new settings/RPCs → no client changes.

## Rev 3 (this architect step)
Branch was again stale (4bacf12a6). Fast-forwarded to `develop` (948abf84b), then merged the latest prior attempt
`origin/…hbqt40rxe6r6wpdg` (adds live consent-card proof against real agy, MCP-scope test, live resume recall, doc updates;
supersedes the wezsc2ft merge in rev 2). Verified after merge: `go build ./...`, `go vet` (antigravity, server, toolclass,
aigateway) and `go test ./internal/antigravity/... ./internal/toolclass ./internal/askorchicon ./internal/aigateway
./internal/protectedpath ./internal/claude` all PASS. The numbered steps above still apply; the SSE should first read
`docs/adapters/antigravity-acp.md` for what live agy was proven (and proven NOT to card) before re-running risk 1, then run the full gate.
