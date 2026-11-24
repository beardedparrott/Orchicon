# `agy acp` — protocol contract as recorded from agy 1.5.13

This is the **recorded** contract, not the ACP specification from memory. Every
claim below cites a line of a real transcript under
`internal/antigravity/testdata/acp/`. Where a scenario could not be reproduced
on this host, the doc says so instead of guessing.

## 1. Provenance

| Fact | Value |
| --- | --- |
| `agy --version` | **1.5.13** (`/usr/bin/agy` — distro package, not the installer path) |
| OS / kernel / arch | linux / 7.3.5-2-cachyos / x86_64 |
| Auth method | `oauth-personal`, existing token in the OS keyring (Linux Secret Service) |
| Tier | `Google AI Pro` (`g1-pro-tier`) |
| Recorded | 2026-11-24 |
| Recorder | `scripts/record-agy-acp.py` (stdlib only, run with `python3 -I`) |

Every fixture's **first line** is a header that records `agy_version`, `os`,
`kernel`, `arch`, `auth_method`, `scenario`, `recorded` and the exact `argv`, so a
later reader can tell a real recording from a synthetic one and know what
produced it. Lines after the header are `{"dir":"c2s"|"s2c"|"meta","line":…}`
where `c2s` is client→server, `s2c` is server→client and `meta` is a recorder
annotation about something that happened off the ACP wire (e.g. what the model
did after a permission answer).

`auth_required` was recorded against an **isolated profile**
(`--app-data-dir empty-appdata --gemini-dir empty-gemini`, relative paths under
the scenario cwd) so the real keyring credentials were never touched.

> `--app-data-dir` and `--gemini-dir` **reject absolute paths**; a relative value
> is resolved against the process cwd.
> Evidence: `auth_required.jsonl:15` — the sub-process attempt with an absolute
> `--app-data-dir /tmp/…` returned
> `{"code":-32603,"message":"Failed to start Antigravity: language server failed to start: must not be absolute: /tmp/orchicon/acp-empty-appdata"}`.

## 2. Method list

### 2.1 Client → server requests

| Method | Params (as recorded) | Evidence |
| --- | --- | --- |
| `initialize` | `protocolVersion`, `clientCapabilities`, `clientInfo` | `handshake_auth.jsonl:2` |
| `authenticate` | `{methodId}` | `handshake_auth.jsonl:4` |
| `session/new` | `{cwd, mcpServers[], [additionalDirectories], [_meta.antigravity.systemPrompt.append]}` | `handshake_auth.jsonl:7` |
| `session/prompt` | `{sessionId, prompt:[{type:"text",text}]}` | `simple_turn.jsonl:10` |
| `session/set_model` | `{sessionId, modelId}` | `unknown_model.jsonl:8` |
| `session/set_mode` | `{sessionId, modeId}` | `tool_read_file.jsonl:8` |
| `session/load` | `{sessionId, cwd, mcpServers[]}` | `load_session.jsonl:9` |
| `session/resume` | `{sessionId, cwd, mcpServers[]}` | `load_session.jsonl:19` |
| `session/list` | `{}` | `load_session.jsonl:6` |

### 2.2 Client → server notifications

| Method | Params | Evidence |
| --- | --- | --- |
| `session/cancel` | `{sessionId}` | `cancel_midturn.jsonl:22` |

### 2.3 Server → client requests

| Method | Params | Evidence |
| --- | --- | --- |
| `session/request_permission` | `{sessionId, toolCall, options[]}` | `tool_run_command_permission_allow.jsonl:20` |

`fs/read_text_file`, `fs/write_text_file` and `terminal/*` were **never** issued
by agy 1.5.13 in any scenario, despite the client advertising
`clientCapabilities.fs` and `terminal`. agy reads and writes files itself.
Evidence: `tool_read_file.jsonl` — the recorder advertised both capabilities and
recorded `s2c client requests seen: []`.

### 2.4 Server → client notifications

| Method | Meaning | Evidence |
| --- | --- | --- |
| `session/update` | the only notification method agy 1.5.13 ever sends | `simple_turn.jsonl:14` |

## 3. The UNVERIFIED list, answered

Each bullet from the work item, answered from a recorded line.

### 3.1 Does `session/update` carry `agent_thought_chunk` (reasoning TEXT)?

**No — not supported by agy 1.5.13.** Reasoning is reported as a *token count*
(`thoughtTokens`), never as text. A model turn explicitly asked to think step by
step (`gemini-3.8-flash-high`, "What is 17*23? Think step by step") produced
`thoughtTokens: 237` and **zero** `agent_thought_chunk` notifications.

Evidence: `thinking_turn.jsonl:26` —
`{"stopReason": "end_turn", "usage": {"inputTokens": 9080, "outputTokens": 433, …, "thoughtTokens": 237, "totalTokens": 9513}}`
and the recorder's own accounting at `thinking_turn.jsonl:29` —
`session/update variants seen: ["agent_message_chunk","available_commands_update","config_option_update","session_info_update","usage_update"]`
(no `agent_thought_chunk`).

### 3.2 Shape of `tool_call` / `tool_call_update`

Ids are `call_<decimal>`; `kind` ∈ `read|edit|execute|other`; `status` ∈
`pending|in_progress|completed|failed`; `rawInput` carries the tool's own
arguments next to a normalized `args`, `title` and `locations[]`.

Evidence: `tool_read_file.jsonl:18` —
`{"sessionUpdate":"tool_call","toolCallId":"call_7623836592473310236","name":"read_file","status":"in_progress","kind":"read","title":"File hello.txt","locations":[{"path":"…/hello.txt"}],"rawInput":{"Path":"…/hello.txt"}}`

The same tool appears once as `tool_call` (`status:"in_progress"`) and then as
`tool_call_update` with `status:"completed"` and the same `toolCallId`
(`tool_read_file.jsonl:19`). Commands use `kind:"execute"`
(`tool_run_command_permission_allow.jsonl:18`).

### 3.3 The exact `session/request_permission` request and answer semantics

Request: `{sessionId, toolCall:<a full tool_call object>, options:[{optionId,name,kind}]}`.

Evidence: `tool_run_command_permission_allow.jsonl:20` —

```json
{"sessionId":"60647e4c-…","toolCall":{"sessionUpdate":"tool_call","toolCallId":"call_13391380448657600714","name":"run_command","status":"pending","kind":"execute","title":"echo hi","rawInput":{"CommandLine":"echo hi","WaitMsBeforeAsync":5000,"toolSummary":"Run echo hi"}},
 "options":[{"optionId":"allow","name":"Allow","kind":"allow_once"},{"optionId":"deny","name":"Deny","kind":"reject_once"}]}
```

**Option kinds actually observed: only `allow_once` and `reject_once`.** The
option *ids* are the short words `allow`/`deny`, not the kind names — a mapper
must echo back `optionId`, never a literal `"allow_once"`. `allow_always` and
`reject_always` were never offered by agy 1.5.13 in any scenario.

Answer envelope: `{"outcome":{"outcome":"selected","optionId":"allow"}}`
(evidence: `tool_run_command_permission_allow.jsonl:21`). `optionId:"deny"`
(evidence: `tool_permission_reject.jsonl:21`). `{"outcome":{"outcome":"cancelled"}}`
was sent in `cancel_midturn.jsonl` (second turn) and ended the turn with
`stopReason:"cancelled"` — `cancel_midturn.jsonl:28`.

**On `deny` the turn continues**: the tool goes `status:"failed"` with an empty
`content` text, and the agent receives the turn again. Evidence:
`tool_permission_reject.jsonl:23` —
`{"sessionUpdate":"tool_call_update","toolCallId":"call_4419582709184514259","name":"run_command","status":"failed",…,"rawOutput":{"output":""},"content":[{"type":"content","content":{"type":"text","text":""}}]}`.

### 3.4 Per-turn token usage

The **`session/prompt` result** carries full per-turn accounting, not just
`used/size`:
`{stopReason, usage:{inputTokens, outputTokens, cachedReadTokens, cachedWriteTokens, thoughtTokens, totalTokens}}`.

Evidence: `simple_turn.jsonl:18` —
`{"stopReason":"end_turn","usage":{"inputTokens":9064,"outputTokens":1,"cachedReadTokens":0,"cachedWriteTokens":0,"thoughtTokens":0,"totalTokens":9065}}`.

`usage_update` is a *separate, coarser* signal — a context-window gauge, not a
cost meter: `{"sessionUpdate":"usage_update","used":9065,"size":1048576}`
(`simple_turn.jsonl:17`). `size` is the window; `used` is tokens currently in it.
An adapter that needs cost must read `usage` off the prompt result.

### 3.5 How `ask_question` surfaces over ACP

**As a `tool_call`, not as a permission request and not as an elicitation.**
`name:"ask_question"`, `kind:"other"`. The options arrive in `rawInput.questions[]`
with `options:[]` as *display strings*; agy then renders them itself as an
`agent_message_chunk` and the turn ends — the client is never asked for an answer.

Evidence: `ask_question.jsonl:18` —
`{"sessionUpdate":"tool_call","toolCallId":"call_18247274058962690290","name":"ask_question","status":"in_progress","kind":"other","title":"Run ask_question","rawInput":{"questions":[{"is_multi_select":false,"options":["(Recommended) Red","Blue"],"question":"Please choose between red and blue:"}]}}`
and `ask_question.jsonl:20` — the `agent_message_chunk`
`"\nPlease choose between red and blue:\n- [1] (Recommended) Red\n- [2] Blue\n"`.

**Consequence for Orchicon ask cards:** there is no ACP-level answer channel for
`ask_question`. It cannot be answered over ACP; a card can only be *displayed*
unless the prompt is answered by sending the choice as the next prompt.

### 3.6 `mcpServers` in `session/new`

**stdio works.** A server entry is `{name, command, args[]}` and its tools appear
as `mcp_<serverName>_<toolName>` with `kind:"other"`.

Evidence: `mcp_tool_call.jsonl:6` (the `session/new` request) and
`mcp_tool_call.jsonl:19` —
`{"sessionUpdate":"tool_call","toolCallId":"call_5019695473337414840","name":"mcp_echo_echo","args":{"text":"hi","toolSummary":"Echo hi"},"status":"in_progress","kind":"other","title":"Run mcp_echo_echo"}`.

The MCP handshake itself happens off the ACP wire (agy ↔ MCP server), so it is
not in the transcript; the tool call above is the observable proof of connection.
agy also issues `server/discover` before `initialize` and negotiates
`protocolVersion:"2025-11-25"` — recorded in the recorder's meta notes.

An **http** (streamable/SSE) `mcpServers` entry was **not exercised** — no
cheap recording target was available on this host, so its shape is unverified.

### 3.7 `_meta.antigravity.systemPrompt.append`

The shape is `session/new` params `_meta.antigravity.systemPrompt.append` = a
**string**. `initialize` advertises support for it.

Evidence: `handshake_auth.jsonl:3` —
`"_meta":{"antigravity":{"tier":true,"systemPrompt":{"append":true}}}` (the
capability), and `handshake_auth.jsonl:7` — the accepted request
`"_meta":{"antigravity":{"systemPrompt":{"append":"You are recorded by the Orchicon spike recorder. Ignore this line."}}}`.

A **non-string** `append` is accepted silently rather than rejected
(`handshake_auth.jsonl:13`, and the accepted reply at `:19`, `append:42` still returned a `sessionId`), so the
field is not validated client-side — Orchicon must send a string. The appended
text is **not** echoed back as a `user_message_chunk` on a fresh session
(`handshake_auth.jsonl`, `user_message_chunk count on a fresh session: 0`).

### 3.8 Model selection and the `--effort` mapping

There is **no `--effort` flag on `agy acp`**. Effort is part of the model id and
is also exposed as a separate `thought_level` config option.

Evidence: `handshake_auth.jsonl:8` — `session/new` returns
`models.availableModels[{modelId,name}]`, e.g. `gemini-4-neon-high`,
`gemini-4-neon-xhigh`, `gemini-3.8-flash-low`, `claude-sonnet-5-5-low`,
`gpt-oss-120b-medium`, plus `models.currentModelId` (default
`gemini-4-neon-high`); and `configOptions`
`{"id":"model","category":"model","type":"select","currentValue":"gemini-4-neon",…}`
with a **separate** `{"id":"thought_level","category":"thought_level","options":["low","medium","high","xhigh"]}`.

The **model-id grammar** is therefore `<family>[-<variant>]-<effort>` where
effort ∈ `low|medium|high|xhigh`, and the bare `model` config value is the id
*without* the effort suffix. Some ids carry a variant segment instead:
`gemini-pro-orchestrator-neon`, `gemini-pro-orchestrator` (no effort suffix at
all). Selection is `session/set_model{sessionId, modelId}` — it returns
`result:{}` on success (`simple_turn.jsonl:9`). `session/set_mode` likewise
returns `null`.

### 3.9 `session/load` / `resume` replay, and `session/cancel`

`session/load` **replays the transcript** as ordinary `session/update`
notifications — `user_message_chunk` then `agent_message_chunk` — so a client
rebuilds history from the same handler it already has.

Evidence: `load_session.jsonl:10` (`user_message_chunk`, replaying the original
prompt text) and `load_session.jsonl:11` (`agent_message_chunk` replaying
`"OK"`); the recorder counted 5 replay notifications (`:17`). `session/resume`
returns the same shape as `session/load` (sessionId + models + modes +
configOptions) and replays nothing (`load_session.jsonl:19`). `session/list`
returns `{sessions:[{sessionId,cwd,title,updatedAt}]}` (`load_session.jsonl:7`).

`session/cancel` is a **notification**, and it ends the in-flight turn with
`stopReason:"cancelled"` — including when a permission request is still pending.

Evidence: `cancel_midturn.jsonl:22` (the `session/cancel`) →
`cancel_midturn.jsonl:24` `{"stopReason":"cancelled","usage":{…}}`; and
`cancel_midturn.jsonl:27` → `:28` `{"stopReason":"cancelled"}` for the
pending-permission case.

### 3.10 `/compact` via `available_commands`

**Yes — a slash command sent as an ordinary prompt.** `session/new` emits
`available_commands_update` listing the commands (`goal`, `compact`, `schedule`,
`browser`, `plan`, …), and prompting the literal text `/compact` compacts the
conversation and returns `stopReason:"end_turn"` with a fresh, smaller usage.

Evidence: `compact.jsonl:13` (the `available_commands_update`) and
`compact.jsonl:22` (the `session/prompt` whose text is `/compact`), with the
recorder noting a drop from 9064 to 2942 input tokens in the turn itself.

### 3.11 Flags: `--process-per-session`, `--disable-telemetry`, `--app-data-dir`, `--sandbox`

All four are **accepted** on `agy acp` and none changes the ACP handshake.
`--process-per-session`, `--disable-telemetry` and `--sandbox` each produced an
identical `initialize` + `authenticate` + `session/new` triplet with 27 available
models. `--app-data-dir` / `--gemini-dir` isolate the profile but **must be
relative** (`auth_required.jsonl:15`).

Sandbox **is available on Linux** (kernel namespaces) per the docs, and
`agy acp --sandbox` starts normally here; containment itself was not exercised.

### 3.12 Auth-failure shape

`initialize` and `session/new` **succeed without authentication** — auth is
deferred to the first model call, so an adapter must not treat a successful
`session/new` as proof of being signed in.

Evidence: `auth_required.jsonl:6` — `session/new` on a profile with no token
returned a `sessionId` and the model list.

Two distinct error shapes matter for the auth-failure mapper:

| Case | Code | Message | Evidence |
| --- | --- | --- | --- |
| unknown auth method | `-32602` | `Unsupported auth method: "no-such-auth-method"` | `auth_required.jsonl:9` |
| `gemini-api-key` with no key | `-32602` | `GEMINI_API_KEY is not set; pass an API key in _meta.apiKey or set the GEMINI_API_KEY environment variable.` | `auth_required.jsonl:12` |
| absolute `--app-data-dir` | `-32603` | `Failed to start Antigravity: language server failed to start: must not be absolute: …` | `auth_required.jsonl:15` |
| unknown model | `-32602` | `Unknown model ID: nonexistent-x` | `unknown_model.jsonl:9` |

**Important negative result:** on this host a turn on the "isolated profile"
still *succeeded* (`stopReason:"end_turn"`), because `agy` fell back to the
keyring token rather than reporting a signed-out error. A genuine `401`-style
mid-turn auth failure was therefore **not observed**; treat it as unverified.

## 4. `session/update` variant catalogue

Every variant agy 1.5.13 was observed to emit, with one example each.

| Variant | Example | Evidence |
| --- | --- | --- |
| `agent_message_chunk` | `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"OK"}}` | `simple_turn.jsonl:16` |
| `user_message_chunk` | replayed user text on `session/load` | `load_session.jsonl:10` |
| `tool_call` | full tool_call object, `status:"in_progress"` | `tool_read_file.jsonl:18` |
| `tool_call_update` | same id, `status:"completed"`/`"failed"`, carries `content[]` | `tool_read_file.jsonl:19` |
| `usage_update` | `{"sessionUpdate":"usage_update","used":9065,"size":1048576}` | `simple_turn.jsonl:17` |
| `available_commands_update` | `{availableCommands:[{name,description}]}` | `compact.jsonl:13` |
| `config_option_update` | `{configOptions:[model, thought_level, mode]}` | `simple_turn.jsonl:11` |
| `current_mode_update` | `{"sessionUpdate":"current_mode_update","currentModeId":"accept-edits"}` | `tool_read_file.jsonl:12` |
| `session_info_update` | `{"sessionUpdate":"session_info_update","title":"Red Or Blue Choice"}` | `ask_question.jsonl:16` |
| `agent_thought_chunk` | **never emitted** by agy 1.5.13 | `thinking_turn.jsonl:29` |

## 5. Error codes

| Code | Meaning | Example |
| --- | --- | --- |
| `-32602` | invalid params (bad model id, bad auth method, missing API key) | `unknown_model.jsonl:9` |
| `-32603` | internal / failed to start (bad `--app-data-dir`) | `auth_required.jsonl:15` |

Permission *denials* are not JSON-RPC errors — they are a `tool_call_update`
with `status:"failed"`.

## 6. Per-OS facts (from Google's docs)

Source: <https://antigravity.google/docs/getting-started>,
<https://antigravity.google/docs/cli/reference>,
<https://antigravity.google/docs/install>, <https://antigravity.google/docs/sandbox>.

| Fact | Value | Source |
| --- | --- | --- |
| Install (macOS/Linux) | `curl -fsSL https://antigravity.google/cli/install.sh \| bash` | getting-started |
| Default binary path (macOS/Linux) | **`~/.local/bin/agy`** | getting-started, "Default binary location" |
| Binary path on THIS host | **`/usr/bin/agy`** — installed via the distro package, not the installer | observed, all fixture headers |
| Install (Windows) | `PowerShell` / `CMD` installer variant of the install script; the `.exe` wizard ships the App, the CLI uses the install script tab | getting-started |
| Config dir | **`~/.gemini/antigravity-cli/`** (`settings.json` lives there) | cli/reference, "Configuration keys" |
| App data dir | `--app-data-dir` default `antigravity-cli`; on this host resolved to `~/.gemini/antigravity-cli` | cli/reference + agy log |
| Credentials | OS native keyring: Apple Keychain, Linux Secret Service/dbus, Windows Credential Manager. Silent sign-in if a token profile is present | install, "Local silent keyring sign-in" |
| Plugin secrets | a **file** at `~/.config/antigravity/plugin-secrets.json`, not the keyring | agy log (`auth_store.go:199`) |
| Headless auth | `GEMINI_API_KEY` **plus** `{"modelProvider":"gemini"}` in `settings.json` (the env var alone has no effect); or ADC via `AGY_ADC_AUTH=true` | install |
| Sandbox | Linux = kernel namespaces, macOS = `sandbox-exec`/Seatbelt. **Windows requires Windows 11 24H2 or later with the August 2026 update** | sandbox doc |
| Remote/SSH auth | manual URL + code loop when no browser is available | install, "Remote SSH OAuth flow" |

**Unverified on this host: only Linux was run.** Windows and macOS binary paths,
keyring behaviour and sandbox availability are doc-sourced, not observed.

## 7. GAP TABLE vs the Claude bridge

Baseline: `internal/claude/capabilities.go:51-53` — the claude bridge advertises
`execution: [cancellation, resume, mid_run_injection, context_compaction]` and
`telemetry: [tool_calls_streamed, file_diffs, transcript_jsonl]`.

| Capability | Claude bridge | agy ACP 1.5.13 | Workaround / gap |
| --- | --- | --- | --- |
| **cancellation** | `Aborter` — SIGINT then SIGTERM | **supported** — `session/cancel` notification → `stopReason:"cancelled"` (`cancel_midturn.jsonl:22-24`) | none needed |
| **resume** | `SessionContinuer` — `--resume <id>` | **supported** — `session/load` replays the transcript; `session/resume` re-attaches (`load_session.jsonl:9,19`) | none needed; `session/list` even enumerates sessions |
| **mid_run_injection** | `MessageInjector` — writes a turn to live stdin | **not supported** — no ACP method injects a message into a running turn; a second `session/prompt` while one is in flight is not defined by the recording | must queue the message and send it after `stopReason`; cannot interrupt |
| **context_compaction** | Claude-native compact turn | **supported** — prompt the literal `/compact` from `available_commands_update` (`compact.jsonl:22`) | none needed, but it costs a turn |
| **tool_calls_streamed** | callback mapper over the JSONL transcript | **supported** — `tool_call` / `tool_call_update` with stable `toolCallId`, `kind`, `status` | none needed |
| **file_diffs** | parsed from transcript tool calls | **supported** — `content:[{type:"diff",path,oldText,newText}]` on the edit's `tool_call_update` (`file_edit_diff.jsonl:22`) | `oldText` is omitted for a create (only `newText`), so treat absent `oldText` as "new file" |
| **transcript** | the CLI's own `~/.claude/projects` JSONL | **supported with workaround** — no transcript file is advertised; rebuild it from `session/load` replay (`user_message_chunk`/`agent_message_chunk`) or from the live stream | keep our own JSONL by mirroring `session/update` |

## 8. GO / NO-GO

| # | Requirement | Verdict | Workaround |
| --- | --- | --- | --- |
| 1 | **Ask** (one-shot question to an agent) | **supported** — a normal `session/prompt` turn with a `stopReason` (`simple_turn.jsonl:18`) | none |
| 2 | **worker** (long-running autonomous run) | **supported** — `session/prompt` blocks until `stopReason`; `available_commands_update` offers `goal` for run-to-completion | no heartbeats mid-turn; rely on `session/update` traffic for liveness |
| 3 | **permission cards** | **supported** — `session/request_permission` with `options[{optionId,name,kind}]` (`tool_run_command_permission_allow.jsonl:20`) | only `allow_once`/`reject_once` are ever offered; a real "always allow" card needs client-side rule memory, not an agy option |
| 4 | **ask cards** (multiple-choice from the agent) | **not supported** as an ACP interaction — `ask_question` is a `tool_call` with `kind:"other"` whose options are display strings, and agy renders them itself (`ask_question.jsonl:18,20`) | render the card from `rawInput.questions`; deliver the answer as the *next prompt* — there is no answer channel in the protocol |
| 5 | **skills / context** | **supported with workaround** — no skills API over ACP; system context is injected via `_meta.antigravity.systemPrompt.append` (`handshake_auth.jsonl:7`) and skills come from agy's own on-disk config | ship skills as files in agy's config dir, or as appended system prompt |
| 6 | **model listing** | **supported** — `session/new` returns `models.availableModels` + `currentModelId`, plus `configOptions` (`handshake_auth.jsonl:8`) | 27 models on this account; filter per tier if needed |
| 7 | **context** (window gauge) | **supported** — `usage_update{used,size}` (`simple_turn.jsonl:17`) | it is a context gauge, not a cost meter; no "compacting" state is announced |
| 8 | **cost** (token accounting) | **supported** — per-turn `usage{inputTokens,outputTokens,cachedReadTokens,cachedWriteTokens,thoughtTokens,totalTokens}` on the `session/prompt` result (`simple_turn.jsonl:18`) | no money figure; Orchicon must price the tokens itself. Cache reads are separately reported |
| 9 | **reasoning** | **supported with workaround** — reasoning is **not** streamed; only `thoughtTokens` (`thinking_turn.jsonl:26`) | show a "thinking…" indicator from the token count, never reasoning text. `thought_level` config option also lets you choose low→xhigh |
| 10 | **tool calls** | **supported** — `tool_call`/`tool_call_update`, kinds `read|edit|execute|other`, MCP tools named `mcp_<server>_<tool>` (`mcp_tool_call.jsonl:19`) | none |
| 11 | **GUI/TUI** | **not supported** — `agy acp` is a stdio JSON-RPC server only; there is no GUI or TUI channel in the protocol | Orchicon renders its own UI from `session/update`; the agy TUI is a separate, unrelated surface |
| 12 | **3 OSes** | **supported with workaround** — the CLI ships for macOS, Linux and Windows. macOS/Linux binary `~/.local/bin/agy`; Windows via the PowerShell/CMD install script. Auth is the OS keyring on all three (Apple Keychain / Secret Service / Credential Manager) | **only Linux was run here**; macOS and Windows are doc-sourced. Windows `auto` mode and the terminal sandbox need **Windows 11 24H2 + the Aug 2026 update** — ship `default` mode where the sandbox is unavailable |
| 13 | **resume / cancel** (parity with the claude bridge) | **supported** — `session/cancel` ends a turn with `stopReason:"cancelled"` (`cancel_midturn.jsonl:22-24`, including with a pending permission at `:27-28`); `session/load` replays history and `session/resume` re-attaches (`load_session.jsonl:9,19`) | none; the one gap against Claude is **mid-run injection**, which agy has no channel for (see the GAP TABLE) |

The 13 rows above are the 12 requirements named in the work item — Ask, worker,
permission cards, ask cards, skills/context, model listing, context, cost,
reasoning, tool calls, GUI/TUI, 3 OSes — plus **resume/cancel**. The 3 OSes
requirement is kept as ONE row; macOS/Windows observations live in section 6.

**Overall: GO**, with two hard limits — no reasoning text ever, and no ACP
answer channel for `ask_question`.

## 9. License

| Item | Status |
| --- | --- |
| `google-antigravity` Python SDK | **Apache License 2.0** (PyPI, `google-antigravity` 0.1.24) — recorded for completeness only |
| Used by the ACP design? | **No.** The ACP design spawns the user's own installed `agy` binary over stdio; the SDK is an unrelated Python client library |
| Anything from `agy` redistributed? | **No.** No `agy` binary, font, asset or source is copied into this repository. The fixtures are *transcripts of wire traffic*, and OAuth tokens are redacted (`grep -rniE 'ya29\.|refresh_token|access_token|GEMINI_API_KEY='` is clean) |

## 10. Reproducing

```bash
# free scenarios (no model turn)
python3 -I scripts/record-agy-acp.py --scenario handshake_auth
python3 -I scripts/record-agy-acp.py --scenario unknown_model
python3 -I scripts/record-agy-acp.py --scenario auth_required \
  --agy-arg=--app-data-dir --agy-arg=empty-appdata \
  --agy-arg=--gemini-dir --agy-arg=empty-gemini --auth-method none

# scenarios that cost one cheap turn each (--model pins the cheapest available)
python3 -I scripts/record-agy-acp.py --scenario simple_turn --model gemini-3.8-flash-low
python3 -I scripts/record-agy-acp.py --scenario tool_read_file --model gemini-3.8-flash-low --mode accept-edits
```
