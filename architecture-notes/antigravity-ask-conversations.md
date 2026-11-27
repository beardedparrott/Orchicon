# Antigravity Ask conversations — plan (rev 3)

## Finding
The branch started at stale `4bacf12a6` with no antigravity code. I ran `git merge --ff-only develop` (develop = 948abf84b, has worker bridge + model list + `TestAskLive` guard), then merged `origin/6-…-wezsc2ftrwc2k5a7`, which holds the full Ask implementation. The merge was clean (commit 8fc9af0ba). `go build ./...` and `go test ./internal/antigravity/... ./internal/toolclass ./internal/aigateway` pass here. The SSE verifies, closes gaps and gathers evidence; it does not rewrite.

## Code map (all under internal/antigravity unless noted)
- `ask.go` registry `askByConv`/`askBySID`, `CloseAsk`, resume (session/load), model switch, cwd resolver.
- `ask_turn.go` turn = session/prompt; `ask_question` refused → redirect to `orchicon_ask_user`.
- `ask_permission.go` park request_permission → `permission` bus event; once/always/deny → option ids; teardown → reject.
- `ask_mapper.go`, `ask_bus.go` streaming, owner kind `ask_conversation`, Ask diff-ledger hook.
- `modegate.go` Brainstorm/Iteration/Quick Work + `--settings` permissions.deny; agy tool names in `internal/toolclass`.
- `internal/server/server.go` `antigravityBridge.CloseAsk()` beside claude's.
- Tests: `ask_permission_test.go` (AC2), `ask_modegate_test.go` (AC3), `internal/aigateway/ask_kinds_antigravity_test.go` (AC1), `live_ask_test.go` (`TestAskLive`), `ask_live_test.go` (opt-in `ORCHICON_ANTIGRAVITY_LIVE=1`).

## Risks
1. agy 1.5.13 may auto-approve edits (no request_permission) and emits no `agent_thought_chunk` (docs/adapters/antigravity-acp.md). Live AC4 reasoning/write card may be unreachable. Do NOT add an auto-approve key (`assertSettingsNotBypass`). If unreachable, report NOT VERIFIED with the doc line as the blocker.
2. Three clients: no new field/RPC/setting. Only confirm no client hardcodes the Ask kind list: `grep -rn "askCapableKinds\|ask_capable" frontend/src internal/tui`.

## Steps for the SSE
1. `git log --oneline -3` shows the merge; `ls internal/antigravity/ask*.go`.
2. Run the `.orchicon/verify` gate (build, vet, `go test ./internal/... ./cmd/...`); fix any failure.
3. `go test ./internal/antigravity/... -run 'AskPermission|AskModeGate' -v`; save output (AC2/AC3).
4. AC1: `go test ./internal/aigateway -run Antigravity -v`; on DEV :8080 only, rebuild and restart, then call `ListAdapterKinds` and capture the JSON.
5. Check the Ask launch argv in `launch.go`/`procspec.go`/`modegate.go` for risk 1; add a fixture test if behaviour changes.
6. Run the clients grep. Ensure the `ask_question` rejection is documented.
7. Commit; keep `git status --porcelain` clean.
8. Hand AC4–AC6 to QA (live DEV, cheapest `antigravity/google/<model>`, minimal prompts, clean up). NOT VERIFIED with a precise blocker if agy credentials are absent.

## Decisions
- Reuse the prior implementation. `ask_question` gets a documented rejection. No client changes.
