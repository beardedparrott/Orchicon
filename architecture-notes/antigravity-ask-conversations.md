# Antigravity Ask conversations — implementation plan

## Finding (the load-bearing fact)
This run's branch is based on stale `main` (4bacf12a6) and contains NO antigravity code. `origin/develop` (8bf67570e) has
the worker bridge (task 5, #1032) and model listing (5a, #1034): `internal/antigravity/**` (bridge.go, session.go,
sessionrun.go, mapper.go, permissions.go, toolnames.go, acp/…), `internal/adapter/antigravity_*`, settings, etc.

A **prior attempt at THIS task already exists** on local branch
`6-antigravity-ask-…-s53btc7jnczv9zzm` (commits `ba4c01fab` Ask feature, `04ab9665e` bounded close). It was based on an
older worker-bridge variant (`8bc1d93e2`). I trial-cherry-picked both onto `develop` in a scratch worktree (since removed):
only trivial conflicts, then `go build ./...`, `go vet` (antigravity/server/toolclass/askorchicon/aigateway) clean and
`go test ./internal/antigravity/... ./internal/toolclass ./internal/askorchicon ./internal/aigateway ./internal/claude ./internal/protectedpath` all PASS.

So the plan = **land that work on develop**, then verify the criteria, not re-design.

## What the prior commits contain (proof, all read in the trial tree)
- `internal/antigravity/ask.go` (session registry: `askByConv`/`askBySID`, `CloseAsk`, resume via session/resume→session/load fallback, in-place model switch, cwd = project dir via `SetProjectDirResolver`)
- `ask_turn.go` (turn = session/prompt; `ask_question` refused with reason pointing to `orchicon_ask_user`, which round-trips via the Orchicon MCP sidecar in Ask scope; MCP withheld in non-acting modes), `ask_permission.go` (parks `session/request_permission`, emits `permission` bus event, maps once/always/deny to agy option ids, teardown → reject), `ask_mapper.go` + `ask_bus.go` (reasoning/tool/usage streaming, owner kind `ask_conversation`, Ask diff ledger hook), `modegate.go` (Brainstorm/Iteration/Quick Work, pre-card refusal + `--settings` permissions.deny), `bridge.go` (+askMu, askByConv, askBySID, askRootOverride, projectDir fields), `launch.go`/`procspec.go` (settings argv).
- `internal/toolclass/toolclass.go` (+agy tool names), `internal/claude/modegate.go` (shared classification, +9 lines), `internal/askorchicon/tool_ledger.go` (+14).
- `internal/server/server.go` (`SetProjectDirResolver`; `CloseAsk()` at the two shutdown sites ≈ lines 1024/1044), `internal/server/antigravity_settings.go` (`resolveAntigravityAskProjectDir`).
- Tests: `ask_permission_test.go` (`TestAskPermission_*` = AC2), `ask_modegate_test.go` (`TestAskModeGate` = AC3), `ask_session_test.go` (resume/model-switch/cwd/ledger), `aigateway/ask_kinds_antigravity_test.go` (AC1 at service level), `ask_live_test.go`, docs `docs/adapters/antigravity-acp.md` (+113).

## Steps for the SSE (mechanical)
1. In this worktree: `git merge origin/develop` (fast-forwardable; branch has no own commits). Confirm `ls internal/antigravity/bridge.go`.
2. `git cherry-pick ba4c01fab`. Expected conflicts, resolve as follows:
   - `internal/antigravity/bridge.go` struct: keep BOTH develop's `models modelCache` field and the Ask fields block (askMu, askByConv, askBySID, askRootOverride, projectDir).
   - `internal/protectedpath/protectedpath_test.go`: take develop's (HEAD) side (the hermetic XDG comment); drop the other.
3. `git cherry-pick 04ab9665e`. `acp/proc.go` and `acp/ac_test.go` conflict because develop already bounds the child drain (2731ee83d): `git checkout HEAD -- internal/antigravity/acp/proc.go internal/antigravity/acp/ac_test.go`, `git add -A`, continue (keeps `ask_turn.go` + `ask_live_test.go`). Then check `CloseAsk` cannot hang: if `TestCloseAsk_RetiresEveryChild` is green with a hung-child fixture, done; if the bounded-close behaviour is missing from develop's proc.go, port only the Close timeout from 04ab9665e.
4. Verify wiring: `grep -n "SetChatKinds\|ChatKinds" internal/scheduler/dispatcher.go internal/server/server.go` — confirm `antigravity` appears in `Dispatcher.ChatKinds()` output (it derives from the registered adapter implementing the chat interface; `TestListAdapterKindsIncludesAntigravityAsAskCapable` is the proof). Add a one-line fix only if the test shows otherwise.
5. Three-clients rule: no new field/RPC is introduced, so no client change needed; state that in the report. Check `internal/tui` and `frontend` need no kind allow-list (`grep -rn '"claude"' frontend/src internal/tui | grep -i ask`); add `antigravity` only where an Ask kind list is hardcoded.
6. Run: `go build ./... && go vet ./... && go test -json ./internal/... ./cmd/...` (the `.orchicon/verify` gate); also `go test ./internal/antigravity/... -run 'AskPermission|AskModeGate' -v` for AC2/AC3 evidence. Commit (`feat(antigravity): Ask conversations …`), tree clean.
7. Live criteria AC4–AC6 (GUI screenshots, `orchicon_ask_user` card, plane-restart resume + log line) need a real `agy` + auth and a sandbox plane; the QA step owns them. Mark NOT VERIFIED if no agy/credentials; never spend model tokens beyond one bounded call. AC1 RPC JSON: run sandbox plane and call `AIGatewayService.ListAdapterKinds`, or cite the service-level test.

## Decisions
- Reuse the prior implementation (tested green on develop) rather than rewrite: it covers all 7 scope items.
- `ask_question`: documented rejection with redirect to `orchicon_ask_user` (ACP cannot return an answer) — already in `ask_turn.go:471-481` and docs.
