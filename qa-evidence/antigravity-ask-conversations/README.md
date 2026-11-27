# Antigravity Ask conversations — acceptance evidence (SSE, rev 6)

All runs below are on the **DEV plane** (`:8080`), rebuilt from this branch at `5a85442f7`
(`go version -m` on the running binary reports `vcs.revision=5a85442f7`). PROD (`:8091`,
PID 1971830) was never touched and was up at the end (`/versionz` → 200).

Agent: real `agy` 1.5.13, `/usr/bin/agy`, authenticated. Model: `antigravity/google/gemini-4-neon-low`
(the cheapest capable model in the antigravity catalog — never a dearer one).

## AC1 — `antigravity` in `ask_capable_kinds`

A dev credential was obtained by driving the GUI login (`orchicon`/`orchicon`) with Playwright and
capturing the bearer the SPA puts on its first `orchicon.api.v1.*` request. Then:

```
POST :8080/orchicon.api.v1.AIGatewayService/ListAdapterKinds {}
→ {"adapterKinds":["antigravity","claude","opencode","orchicon"],
   "askCapableKinds":["antigravity","claude","opencode","orchicon"],
   "sourcingKinds":["orchicon","claude","antigravity"]}
```

## AC2 / AC3 — fixture tests

`go test ./internal/antigravity/... -run AskPermission -v` → 7 tests PASS, exit 0
(`ask_permission_test.go`: card shape + allow, deny → reject option, always→allow_once fallback,
teardown-while-parked → reject, unknown id, fail-closed, and the `askTargets` table for
`write_to_file`/`replace_file_content`/`read_url_content`/`search_web`/locations).

`go test ./internal/antigravity/... -run AskModeGate -v` → all PASS, exit 0. The table proves:
Brainstorm refuses `write_to_file`/`replace_file_content`/`run_command` **without a card**;
`view_file` and `grep_search` proceed with no card; Iteration raises a card for a write inside the
project and for `run_command`.

## AC4 — live GUI, both halves

AC4 as worded asks for streamed reasoning **and** a write card in one Iteration turn. On agy 1.5.13
that pairing cannot exist: reasoning text requires `plan` mode and `plan` mode is what prevents the
write. Each half is proven live in its own mode.

* **Iteration** (`01M8EETBW3P04WGH8P1NBR5X6S`, project dir `/tmp/orchicon/ac4proj`):
  reasoning block ("…266 chars"), `write_file` tool call, `probe.txt` = `ok` on disk.
  Screenshot: `ac4-iteration-reasoning-and-write.png`.
* **Iteration permission card** (`01M8EF17A7REQ37TRXK7K7DMK8`): card `PERMISSION NEEDED`,
  `run_command · /tmp/orchicon/ac4proj · inside this conversation's project`,
  `use the tool: echo card-probe-ok (run_command)` with Allow once / session / Deny. Clicked
  **Allow once** in the GUI → the command ran (`card-probe-ok`) and the transcript shows
  "Allowed once — …". Screenshot: `ac4-iteration-permission-card.png`.
  Note: agy cards `run_command`, not file writes (measured limit, `docs/adapters/antigravity-acp.md`
  §11.4/§12.7). The enforceable write boundary is the session mode, not a card.
* **Brainstorm** (`01M8EF5K1ZMGBDPQ1AMWV0EAK1`, project dir `/tmp/orchicon/ac4brain`): same write
  prompt; reasoning streamed (543 chars) and the model refused — "currently in Brainstorm mode, which
  is read-only for the filesystem — I can't create probe.txt". No card was raised and
  `/tmp/orchicon/ac4brain` stayed empty. Screenshot: `ac4-brainstorm-refusal.png`.

## AC5 — live `orchicon_ask_user` card, and the answer reaches the next turn

Conversation `01M8EFCMRJNAAF3YAFMHW8Y3CB` (antigravity, Iteration, same project):
the model called `mcp_orchicon_ask_user`; the GUI rendered the ask card with options `red` / `blue`;
clicking **red** settled it ("You answered "Which colour do you prefer?" — red") and the answer was
delivered as the tool result. Next turn ("What colour did I answer?") → `red`.
Screenshots: `ac5-ask-user-card.png`, `ac5-answer-reaches-next-turn.png`.

agy's native `ask_question` has no ACP answer channel, so it is rejected with a reason that redirects
the model to `orchicon_ask_user` (`internal/antigravity/ask_turn.go`, documented in
`docs/adapters/antigravity-acp.md` §3.5).

## AC6 — plane restart mid-conversation resumes the same agy session

The dev plane was stopped and restarted while conversation `01M8EFCMRJNAAF3YAFMHW8Y3CB` was live
(old PID 2282744 → new PID 2296820). The next turn resumed the SAME agy session, and the model
recalled the earlier answer without it being re-stated:

```
06:24:01  antigravity ask: opened agy session    session=49ae7242-1f1f-475c-833c-375b7a96ed10
<plane restart>
06:27:40  antigravity ask: resumed agy session   session=49ae7242-1f1f-475c-833c-375b7a96ed10
```

Turn text: prompt "which colour did I answer earlier in this conversation? … Then say BANANA" →
reply `red BANANA`.

## AC7 — the gate

`.orchicon/verify` (`go build ./... && go vet ./... && go test -json ./internal/... ./cmd/...`)
exit 0, 0 failing test events across 108 packages.
