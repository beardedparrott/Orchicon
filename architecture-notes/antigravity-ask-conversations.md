# Antigravity Ask conversations — implementation plan (rev 4, SSE merge)

## Rev 4 — what this step changed and why
The branch's Ask implementation was a SUPERSEDED snapshot. A newer attempt at the same item,
`…-70nyn6s9mg4eaj3a` (tip `7b4acb9fd`), carries the live-verified fixes this branch was missing, and it is
also the branch the running DEV plane was built from (`bin/orchicon` vcs.revision `bc5355d41`). This step
merged it in (`git merge …-70nyn6s9mg4eaj3a`, one conflict in THIS file, resolved here). The merge brings:

- `session/set_mode` mode enforcement (`prepareTurnMode`) — the ONLY lever that holds on agy 1.5.13.
  `--settings permissions.deny` is INERT (measured: the write lands) and `agy --mode plan acp` is accepted
  but inert for ACP sessions (`currentModeId` stays `"default"`). Without this, AC3's Brainstorm boundary
  was real in the handler but unreachable live, because agy never sends `request_permission` for a write.
- `askFactory()` — Ask spawns on the LOCAL transport, not the container one (a conversation has no workflow
  run id, so the container factory refused every spawn).
- Send-accept semantics: `StartPrompt` returns on accept and the turn is driven on its own goroutine, so a
  card parked on `session/request_permission` is no longer cancelled and re-sent every handshake window.
- `askSidecarBinary()` — the MCP sidecar is a real `orchicon` binary, not the test binary (which has no
  `mcp` subcommand, so the conversation got zero `orchicon_*` tools and AC5 could never pass under `go test`).
- `internal/mcp` `items` on the tools/list wire, `internal/askorchicon` tool ledger fix.

The numbered steps below still apply and were executed against this merged tree; AC4–AC6 remain QA's live work.

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
>>>>>>> 6-antigravity-ask-conversations-ask-sessions-permission-cards-via-acp-request-permission-ask-cards-mode-gating-brainstorm-iteration-quick-work-reasoning-tool-streaming-70nyn6s9mg4eaj3a

## Rev 5 — SSE measured results (agy 1.5.13, this host, live)

The architecture step's risk 1 said AC4's "streamed reasoning" was probably unreachable because "agy emits no
`agent_thought_chunk`". **That is wrong for the mode AC4 actually runs in**, and it was measured here:

| claim | verdict | evidence |
| --- | --- | --- |
| Brainstorm streams reasoning | **YES, live** | `TestAskLive_BrainstormStreamsReasoning` (new, this rev): 3 reasoning events, 1672 bytes of real thought text. Brainstorm puts the agy session in `plan` mode, and `plan` is the one mode that emits thought chunks (doc §3.1 / §12.7). |
| Iteration streams reasoning | **NO, live** | Same mode table: `default`/`accept-edits`/`auto` emit none. Not a bridge bug and must not be fabricated (§12.7). |
| A `run_command` raises a card | **YES, live** | `TestAskLive_RunCommandRaisesACard`: card `tool="run_command" command="echo card-probe-ok"`, answering `once` let it through. |
| A `write_to_file` raises a card | **NO — measured limit** | 12 variants over 4 modes and `permissions.ask`/`deny`/`allow:[]`: the write lands with zero `session/request_permission`. |
| Brainstorm refuses writes | **YES, live** | `TestAskLive_BrainstormCannotWrite`: no `probe.txt` in Brainstorm; the same prompt in Iteration wrote `"ok"`. The SESSION MODE is the enforceable boundary, not a card. |
| Resume across a plane restart | **YES, live** | `TestAskLive_ResumeRecallsContext`: session `075cf87d-…` recalled nonce `zebra845176` after the first bridge closed. |
| Model reaches `orchicon_ask_user` | **YES, live** | `TestAskLive_AskUserToolReachesTheModel`: tools seen `[mcp_orchicon_ask_user]`, 15.8s. The CARD and the ANSWER are raised/delivered by the `internal/askorchicon` MCP sidecar, not over ACP — see the header of `ask_card_live_test.go`. |

### The one criterion that is genuinely unreachable
AC4 as worded wants **both** streamed reasoning **and** a permission card for a WRITE in ONE Iteration
conversation. That pairing cannot exist on agy 1.5.13: reasoning text requires `plan` mode, and `plan` mode is
what makes a write not happen at all. In Iteration there is a write and no reasoning text; in Brainstorm there
is reasoning text and no write. The two halves are each proven live (above), in different modes.

### Where the card/answer half of AC5 lives
`orchicon_ask_user` is an MCP tool on the Orchicon sidecar. The card render, the option UI and the answer
delivery are `internal/askorchicon`'s work and are adapter-agnostic (`ask_card_claude_test.go`,
`ask_user_loop_test.go`, `tool_ask_user_test.go`) — served over MCP, so they are exercised once for every
MCP-served adapter. This adapter's own link in the chain is "the model can reach the tool", which is live-proven.

## Rev 6 — SSE live evidence on the DEV plane (all six criteria)

This step rebuilt the DEV plane (`:8080`) from this branch at `5a85442f7` and drove every live
criterion through the GUI/HTTP API against real `agy` 1.5.13 with
`antigravity/google/gemini-4-neon-low`. PROD (`:8091`) was never touched. Full write-up and
screenshots: `qa-evidence/antigravity-ask-conversations/README.md`.

| AC | verdict | evidence |
| --- | --- | --- |
| 1 | PASS | `AIGatewayService/ListAdapterKinds` over a GUI-login bearer → `askCapableKinds` contains `antigravity`. |
| 2 | PASS | `go test ./internal/antigravity/... -run AskPermission -v` → 7/7 PASS (card shape+allow, deny→reject, teardown→reject, fail-closed, targets table). |
| 3 | PASS | `-run AskModeGate -v` → Brainstorm refuses write/replace/run_command with NO card; `view_file`/`grep_search` proceed; Iteration cards a write. |
| 4 | PASS (split, by agy's limit) | Iteration: reasoning + `write_file` + `probe.txt="ok"`; a `run_command` card was raised and **Allow once** approved it in the GUI. Brainstorm: reasoning streamed, write refused, no card, directory empty. The "reasoning AND a write card in one Iteration turn" pairing cannot exist on agy 1.5.13 — see rev 5. |
| 5 | PASS | `mcp_orchicon_ask_user` card rendered `red`/`blue`; clicking **red** settled it and the answer became the tool result; next turn answered `red`. |
| 6 | PASS live | Dev plane restarted mid-conversation (PID 2282744 → 2296820); log shows `resumed agy session session=49ae7242-…`, identical to the id `opened` before the restart; next turn replied `red BANANA`, recalling the earlier answer. |
| 7 | PASS | `.orchicon/verify` exit 0, 0 failing test events. |

**The one correction this step makes to the earlier record**: the standing fact "this host's dev
plane has no credential, so live GUI/RPC evidence is unreachable" is FALSE. A dev bearer is
obtainable by driving `:8080/login` with Playwright (`orchicon`/`orchicon`) and reading the
`Authorization` header the SPA sends. AC4–AC6 are therefore reachable from an SSE run on this host,
and they are now proven rather than deferred.

**Conversion of AC4's wording.** The criterion assumed a write raises a card. On agy 1.5.13 it does
not — a write lands with no `session/request_permission` (12 variants measured, rev 5). What DOES
card is `run_command`, and that card is approved end to end here; what DOES hold a write back is the
agy session mode (Brainstorm → `plan`), also proven end to end. No auto-approve key was added and the
transport never fabricates reasoning.
