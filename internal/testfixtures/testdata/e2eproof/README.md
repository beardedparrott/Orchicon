# End-to-end proof evidence (criterion 7)

`evidence.jsonl` is the RECORDED OBSERVATION SET for the end-to-end proof child:
a real worker per adapter and a live Ask turn using the project's MCP server and
its skill file.

## Where it comes from

Every leg appends one JSON object per observation to the path in
`ORCHICON_E2E_EVIDENCE` (default `<repo>/.gotmp/e2eproof/evidence.jsonl`, which is
git-ignored so a live run never dirties the tree). This file is the committed
SNAPSHOT of one full run:

```
ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable' \
ORCHICON_E2E_EVIDENCE=$PWD/internal/testfixtures/testdata/e2eproof/evidence.jsonl \
go test ./internal/mcpclient/ ./internal/orchicon/ ./internal/scheduler/ \
        ./internal/claude/ ./internal/opencode/ ./internal/askorchicon/ \
        -run 'E2E' -count=1
```

Re-running writes that same shape. The server ids are per-run (a fresh project +
worker + conversation per test), so a regenerated file will name different
`mcp__<serverID>__orchicon_e2e_probe` values — that IS the anti-staleness property
the distinctive tool name and the per-call nonce exist to give.

## What each record carries

`leg`, `adapter`, `surface`, `execution_id` / `conversation_id`, `project_id`,
`server_id`, `tool`, `args`, `result`, `offered_tools`, `skill_path`,
`skill_in_prompt`, `system_prompt_sha256`, and — for the one thing this container
cannot manufacture — `model` + `model_access` (`available` | `unavailable`).

## The legs

| leg | adapter | what was observed |
|---|---|---|
| `native-worker` | native | real DB rows → real resolver → real bridge `Start`; the project's HTTP **and** stdio servers were OFFERED, the call SUCCEEDED, its output reached the model's next round, and the durable transcript carries it |
| `native-worker-negative` | native | AC 5: after deleting the project-owned row, the NEXT session resolves 0 servers and is offered no `mcp__` tool |
| `worker-skill-render` | shared | the project's selected skill FILE reaches the worker's composite prompt by path AND inline body |
| `claude-worker-argv` | claude | real DB rows → real resolver → real `session.argv()`; `--mcp-config` names the project server with its url, with `provenance=project:<id>` logged |
| `claude-worker-live` | claude | **model_access: unavailable** — no `claude` binary in this container (recorded distinction, never a red) |
| `claude-ask-negative` | claude | AC 5.1: the fixed `--mcp-config` argv drops the deleted project server on the next session and the fingerprint changed |
| `opencode-serve-config` | opencode | real DB rows → real resolver → the config the opencode serve boots from names the project server + the materialised skill instruction |
| `opencode-pool-keying` | opencode | AC 3 granularity: the pool keys one serve + data dir per resolved set |
| `opencode-worker-live` | opencode | **model_access: unavailable** — no `opencode` binary in this container (the actionable failure criterion 3 permits) |
| `ask-native-turn` | native | a LIVE Ask turn in the project: the model's system prompt carries the project's + conversation's skill files AND it was offered + successfully called the project's tool |
| `ask-native-mode-policy` | native | AC 4/mode policy: brainstorm + quick_work neither OFFERED nor EXECUTED the project's opaque MCP tool; the refusal names the mode |
| `ask-claude-scope` | claude | the claude Ask conversation scope resolves the project's server from the real row |
| `ask-native-negative` | native | AC 5.2: `refreshAskMCP`/`askMCPFor` reconcile per turn — the deleted server leaves the fingerprint and the live client is retired |
| `ask-tool-list` | ask | AC 6: the Ask tool registry carries no tenant-tier MCP tool |

## The two recorded unavailabilities

`claude-worker-live` and `opencode-worker-live` record `model_access: unavailable`
WITH the reason. No permitted model (nothing outside `-free` / `ollama/*` /
`local-models/*`) and no `claude`/`opencode` binary exists in this runtime
container, so the live half of those two legs cannot run here. That is a recorded
distinction, not a failed criterion: the task says a model-access skip proves
nothing about MCP, and turning it into a red would be the wrong signal. The
offline halves (argv / serve config) carry those two legs' configuration proof,
and the live legs run unchanged where a binary and a permitted model exist.
