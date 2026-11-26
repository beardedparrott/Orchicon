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
| Recorder (ACP transcripts) | `scripts/record-agy-acp.py` (stdlib only, run with `python3 -I`) |
| Recorder (off-wire probes) | `scripts/probe-agy-acp-offwire.py` → `offwire-probes.jsonl` |

Every fixture's **first line** is a header that records `agy_version`, `os`,
`kernel`, `arch`, `auth_method`, `scenario`, `recorded` and the exact `argv`, so a
later reader can tell a real recording from a synthetic one and know what
produced it. Lines after the header are `{"dir":"c2s"|"s2c"|"meta","line":…}`
where `c2s` is client→server, `s2c` is server→client and `meta` is a recorder
annotation about something that happened off the ACP wire (e.g. what the model
did after a permission answer).

Two facts in section 3 **cannot** appear in an ACP transcript by construction:
the MCP handshake is a *second*, agy-initiated JSON-RPC channel, and the
`agy acp` startup flags change process topology rather than wire traffic. Those
two are recorded separately, as real observations, in
`internal/antigravity/testdata/acp/offwire-probes.jsonl` (header line 1, zero
model turns spent), and cited from there.

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
| `session/cancel` | `{sessionId}` | `cancel_midturn.jsonl:20` |

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
and the recorder's own accounting at `thinking_turn.jsonl:28` —
`session/update variants seen: ["agent_message_chunk","available_commands_update","config_option_update","session_info_update","usage_update"]`
(no `agent_thought_chunk`), with the explicit check at `thinking_turn.jsonl:29` —
`{"note":"agent_thought_chunk present: false"}`.

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
(evidence: `tool_permission_reject.jsonl:21`).

The third ACP outcome, `{"outcome":{"outcome":"cancelled"}}`, is **not
reachable as the first answer to a live request** — `session/cancel` ends the
turn before the client gets to answer. The recorded sequence is: the request
arrives and is deliberately left **outstanding** (`cancel_midturn.jsonl:28`,
options again `allow`/`deny`), `session/cancel` is sent (`:30`), the pending
tool flips to `status:"failed"` (`:32`), the turn ends `stopReason:"cancelled"`
(`:33`), and the still-unanswered request is *then* answered with
`{"outcome":{"outcome":"cancelled"}}` (`:35`) — which the server accepts
silently, with **no** JSON-RPC error and no re-opened turn.

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

The MCP handshake itself happens off the ACP wire (agy ↔ MCP server), so no
`session/update` line can ever show it; the tool call above is the observable
proof of connection *on* the ACP wire. The handshake itself is recorded on its
own channel by `scripts/probe-agy-acp-offwire.py` into
`offwire-probes.jsonl`: agy issues `server/discover` **before** `initialize`
(`offwire-probes.jsonl:5`, `"method":"server/discover"` with
`_meta.io.modelcontextprotocol.clientCapabilities`), then `initialize` with
`protocolVersion:"2025-11-25"` and `clientInfo.name:"antigravity-client"`
(`:7`), then `notifications/initialized` (`:9`) and `tools/list` (`:10`). The
probe spent zero model turns (header, `:1`).

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
`"OK"`); the recorder counted 5 replay notifications (`:18`). `session/resume`
returns the same shape as `session/load` (sessionId + models + modes +
configOptions) and replays nothing (`load_session.jsonl:19-21`). `session/list`
returns `{sessions:[{sessionId,cwd,title,updatedAt}]}` (`load_session.jsonl:7`).

`session/cancel` is a **notification**, and it ends the in-flight turn with
`stopReason:"cancelled"` — including when a permission request is still pending.

Evidence: `cancel_midturn.jsonl:20` (the `session/cancel`) →
`cancel_midturn.jsonl:22` `{"stopReason":"cancelled","usage":{…}}`. The
pending-permission case is recorded for real rather than asserted: the
permission request arrives at `:28` and is left **outstanding**, the
`session/cancel` goes out at `:30`, the tool flips to `status:"failed"` at
`:32`, and the turn ends `{"stopReason":"cancelled"}` at `:33`.

### 3.10 `/compact` via `available_commands`

**Yes — a slash command sent as an ordinary prompt.** `session/new` emits
`available_commands_update` listing the commands (`goal`, `compact`, `schedule`,
`browser`, `plan`, …), and prompting the literal text `/compact` compacts the
conversation and returns `stopReason:"end_turn"` with a fresh, smaller usage.

Evidence: `compact.jsonl:13` (the `available_commands_update`) and
`compact.jsonl:22` (the `session/prompt` whose text is `/compact`), with the
recorder noting a drop from 9063 to 2942 input tokens in the turn itself
(`compact.jsonl:20` before, `:24` after).

### 3.11 Flags: `--process-per-session`, `--disable-telemetry`, `--app-data-dir`, `--sandbox`

All four are **accepted** on `agy acp` and none changes the ACP handshake.
`--process-per-session`, `--disable-telemetry` and `--sandbox` each produced an
identical `initialize` + `session/new` pair with **27** available models and
`currentModelId:"gemini-4-neon-high"` — recorded in `offwire-probes.jsonl:12`
(no flag), `:13` (`--process-per-session`), `:14` (`--disable-telemetry`) and
`:15` (`--sandbox`). These flags change process topology and telemetry, not wire
traffic, so they cannot appear in an ACP transcript; the probe records the
handshake under each flag instead (zero model turns). `--app-data-dir` /
`--gemini-dir` isolate the profile but **must be relative**
(`auth_required.jsonl:15`).

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

Source: <https://antigravity.google/docs/getting-started?tab=cli> (the CLI tab is
the one that documents the binary path),
<https://antigravity.google/docs/cli/reference>,
<https://antigravity.google/docs/install?tab=cli>, <https://antigravity.google/docs/sandbox>.
All four were re-fetched while writing this section; the quoted strings below are
taken from those pages.

| Fact | Value | Source |
| --- | --- | --- |
| Install (macOS/Linux) | `curl -fsSL https://antigravity.google/cli/install.sh \| bash` | getting-started?tab=cli |
| Default binary path (macOS/Linux) | **`~/.local/bin/agy`** (literal: "Default binary location: `~/.local/bin/agy`") | getting-started?tab=cli |
| Binary path on THIS host | **`/usr/bin/agy`** — installed via the distro package, not the installer | observed, all fixture headers |
| Install (Windows) | the CLI install page offers **Windows (PowerShell)** and **Windows (CMD)** tabs alongside macOS/Linux; the `.exe` wizard on the same page is the **App**, not the CLI | getting-started?tab=cli |
| Config dir | **`~/.gemini/antigravity-cli/`** (`settings.json` lives there) | cli/reference, "Configuration keys" |
| App data dir | `--app-data-dir` default `antigravity-cli`; on this host resolved to `~/.gemini/antigravity-cli` | cli/reference + agy log |
| Credentials | OS native keyring: Apple Keychain, Linux Secret Service/dbus, Windows Credential Manager. Silent sign-in if a token profile is present | install, "Local silent keyring sign-in" |
| Plugin secrets | a **file** at `~/.config/antigravity/plugin-secrets.json`, not the keyring | agy log (`auth_store.go:199`) |
| Headless auth | `GEMINI_API_KEY` **plus** `{"modelProvider":"gemini"}` in `~/.gemini/antigravity-cli/settings.json` (literal: "Only setting a `GEMINI_API_KEY` environment variable on its own has no effect"); or ADC via `AGY_ADC_AUTH=true` | install?tab=cli |
| Sandbox | Linux = kernel namespaces, macOS = `sandbox-exec`/Seatbelt. **Windows requires Windows 11 24H2 or later with the August 2026 update** | sandbox doc |
| Remote/SSH auth | manual URL + code loop when no browser is available | install, "Remote SSH OAuth flow" |

**Unverified on this host: only Linux was run.** Windows and macOS binary paths,
keyring behaviour and sandbox availability are doc-sourced, not observed. Note
what *was* observed on Linux contradicts the installer default: the binary here
is `/usr/bin/agy` from the distro package, **not** `~/.local/bin/agy`
(`ls ~/.local/bin/agy` → *No such file or directory*; `which agy` →
`/usr/bin/agy`). An adapter must therefore resolve `agy` from `PATH` and not
hard-code the installer path. Likewise `~/.gemini/antigravity-cli/settings.json`
exists here but carries only `colorScheme` and `trustedWorkspaces` — no
`modelProvider` — which is why this host is OAuth-signed-in rather than
API-key-authenticated.

## 7. GAP TABLE vs the Claude bridge

Baseline: `internal/claude/capabilities.go:41-44` — the claude bridge advertises
`execution: [cancellation, resume, mid_run_injection, context_compaction]` (`:42`)
and `telemetry: [tool_calls_streamed, file_diffs, transcript_jsonl]` (`:44`).

| Capability | Claude bridge | agy ACP 1.5.13 | Workaround / gap |
| --- | --- | --- | --- |
| **cancellation** | `Aborter` — SIGINT then SIGTERM | **supported** — `session/cancel` notification → `stopReason:"cancelled"` (`cancel_midturn.jsonl:20-22`, and with a permission left outstanding at `:28-33`) | none needed |
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
| 13 | **resume / cancel** (parity with the claude bridge) | **supported** — `session/cancel` ends a turn with `stopReason:"cancelled"` (`cancel_midturn.jsonl:20-22`, including with a permission request left outstanding at `:28-33`); `session/load` replays history and `session/resume` re-attaches (`load_session.jsonl:9,19`) | none; the one gap against Claude is **mid-run injection**, which agy has no channel for (see the GAP TABLE) |

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
python3 -I scripts/record-agy-acp.py --scenario cancel_midturn --model gemini-3.8-flash-low

# the facts that cannot live on the ACP wire (MCP handshake, acp startup flags).
# Free: both probes stop after initialize + session/new, no model turn.
python3 -I scripts/probe-agy-acp-offwire.py
```

The `cancel_midturn` scenario needs the recorder to hold a
`session/request_permission` **unanswered** so the cancel genuinely lands while
the permission is outstanding; `scripts/record-agy-acp.py` does that via
`Recorder.defer_permission`, then answers the orphaned request with
`{"outcome":{"outcome":"cancelled"}}` after the turn has already ended.

## 11. The Orchicon worker bridge (child 5) — decisions

This section records the choices `internal/antigravity` makes on top of the
protocol above, so a later change reads as a change to a decision rather than a
surprise. It is the README for the bridge, not a second protocol document.

### 11.1 Tool names: canonical for the callbacks, raw in the transcript

`OnToolCall`, the diff ledger, the todo parser and the execution detail pages are
adapter-neutral and key on `bash` / `write` / `edit` / `read` / `todowrite`
(claude's canonicalisation, `internal/claude/parse.go` `canonicalToolName`).
agy's own names are `run_command`, `write_file`, `edit_file`, `read_file`,
`grep_search`, `ask_question` and `mcp_<server>_<tool>`. The mapper therefore
**canonicalises for the callbacks** and keeps the RAW agy spelling beside the
canonical one in the durable part (`"agy_name"`), so the UI can render either.
This deviates from the work item's literal "with agy tool names" wording, and is
deliberate: the alternative is a second parser branch in every reader above, the
exact drift claude's canonicalisation exists to prevent.

### 11.2 The AGENTS.md / `.agents/` customizations decision

**agy's own repo customizations LOAD, and that is parity with claude.** The
claude bridge launches with no `--setting-sources` override, so claude's own
`CLAUDE.md` in the worktree is loaded by the CLI. The agy bridge does the same
thing by passing neither `--include-default-customizations` nor anything that
disables it: `agy acp --help` (verified against 1.5.13) lists
`--include-default-customizations` with a default of **true**, so agy's
`AGENTS.md` and `.agents/` in the worktree load the way the CLI loads them.
Spelling the flag explicitly is deliberately avoided too — it would pin a vendor
default into our argv where a later agy could change it. The worker's composed
prompt (worker prompt + project context files + skills) does NOT rely on that
path: it travels as `_meta.antigravity.systemPrompt.append` on `session/new`
(`composeGoalPrompt` deliberately omits it from the first turn, so it is not
double-counted). The decision and its rationale are stated in a code comment in
`internal/antigravity/launch.go`;
`TestBridge_LaunchArgvKeepsDefaultCustomizations` asserts the argv carries no
customization-disabling flag, and `internal/antigravity/mcp_test.go` asserts the
bridge writes no `.agents/` state of its own.

### 11.3 Mid-run injection is QUEUED, never preemptive

`SendExecutionMessage` queues the text and delivers it as the next
`session/prompt` the instant the in-flight turn reports its `stopReason` —
agy's own `queuedMessages:"queue"` semantics. The capability is advertised as
`mid_run_injection` (delivery is guaranteed) with the queuing stated in the code
and here. Delivery is proven by
`TestBridge_SendExecutionMessageIsQueuedUntilTheTurnEnds`, which asserts the
second prompt does NOT exist while the first turn is running.

### 11.4 Permissions: the shared rule set, answered with no human

`session/request_permission` is answered by `PolicyHandler`, which normalises
agy's tool call into claude's vocabulary and calls **`claude.DecideTool`** — the
same never-allow class, protected roots, operator policy and project boundary
the claude worker profile uses — plus the one agy-specific rule (its subagent
tools `invoke_subagent` / `define_subagent`). Re-implementing the rules here
would create a second copy that can disagree about whether `rm -rf /` is
allowed. Every abnormal path answers REJECT. `--dangerously-skip-permissions`
and its spellings are refused at launch (`assertNoBypass`), because a worker
launched that way would never send a permission request and the policy would be
silently skipped.

**Known limitation, stated plainly:** agy 1.5.13's DEFAULT mode can auto-approve
an edit without ever sending a permission request (§3.3 does not promise it
will ask). The bridge cannot prevent that, so it DETECTS: the mapper checks
every tool call against the same boundary and never-allow rules and, on an
escape, cancels the session and fails the execution (`onMapperViolation`). The
OS-level guard shim on the local transport refuses the destructive commands
regardless. `--sandbox` exists but containment was never exercised, so it is
deliberately NOT enabled.

### 11.5 Compaction, resume and the session store

`/compact` is sent as an ordinary prompt (§3.10) and queued at the FRONT of the
turn queue; `remainingScope` is logged and otherwise ignored because agy
compacts its own context. Resume uses `session/resume` (no replay), falling back
to `session/load` when a build lacks it. The agy session id is persisted through
`SetSessionStore` as a `session_info` part, which is what recovery and
`ContinueSession` read. One live writer per agy session id is enforced
(`Bridge.bindSessionID`).

### 11.6 MCP travels on the wire, never on disk

The resolved owner-scoped union (the same `mcpclient.ScopeResolver` the native
and claude bridges consume) is rendered as `session/new`'s `mcpServers`, with
`${SECRET_NAME}` refs expanded by the shared resolver. The platform's own
`orchicon` sidecar is added unconditionally, so a worker always has
`orchicon_*`. **No `~/.gemini/config/mcp_config.json` and no
`.agents/mcp_config.json` is ever written** — `mcp_test.go` runs a full session
under a temp `HOME` and asserts it is untouched. A remote (`http`) server is
rendered but its agy support is UNVERIFIED (§3.6 recorded only a stdio server),
so it is logged as an advisory.

### 11.7 Capabilities advertised

`execution: [cancellation, resume, mid_run_injection, context_compaction]`,
`telemetry: [tool_calls_streamed, file_diffs, usage]`, plus `context:
[system_prompt_append, mcp_servers, additional_directories]`. Two values are
deliberately ABSENT: `transcript_jsonl` (agy advertises no transcript file, and
the bridge keeps its own durable transcript instead) and `reasoning` (agy never
streams reasoning text — only `usage.thoughtTokens`; the mapper still handles
`agent_thought_chunk` for a future agy).

## 12. Ask (interactive) conversations (child 6) — decisions

The same `agy acp` client drives an Ask Orchicon conversation. The transport is a SECOND use of the contract
above, not a second contract: the bridge implements `scheduler.ChatTurnClient`, which is the only thing
`Dispatcher.ChatKinds()` reads, so `antigravity` appears in `AIGatewayService.ListAdapterKinds.ask_capable_kinds`
and in `askorchicon.SetChatKinds` with no registry entry anywhere. `CloseAsk()` in the server's shutdown path
(beside `claudeBridge.CloseAsk()`) retires the children, which the plane's exit does not reap.

### 12.1 The session id is agy's, so the session is created eagerly

claude's Ask transport CHOOSES its session id (`--session-id`). agy allocates it in `session/new`, so the id
exists only after spawn + authenticate + session/new. `CreateConversationSession` therefore spawns eagerly and
returns **agy's own id** — because askorchicon persists whatever it returns, and a placeholder id would make the
next restart's resume name a conversation agy has never heard of. That is why the "open the session" step is
separate from "send a turn".

### 12.2 Resume, in place, across a plane restart

`SendTurnMessage` with a session id the bridge is not holding does `session/resume` (which replays nothing),
falling back to `session/load` (which replays the history as updates the mapper ignores for user chunks) on a
build without resume. It logs:

	antigravity ask: resumed agy session conversation=<conv> session=<agy session id>

which is the log line a restart's recall is provable from. A model change is `session/set_model` on the LIVE
conversation, so the context survives; a project change or an MCP-set change respawns the child but resumes the
same agy session.

### 12.3 cwd is the conversation's project directory

claude's Ask profile runs in `<askRoot>/<convID>` (`Cwd = askDir`). An agy Ask conversation ASSIGNED to a
project runs **in that project** (`Bridge.SetProjectDirResolver`, wired in the server from `db.GetProject`), with
the worker carve-outs (`/tmp/orchicon`, `.orchicon/`) as additional readable roots; an unassigned one runs in
`$ORCHICON_ANTIGRAVITY_ASK_ROOT/<convID>` (default `$TMPDIR/orchicon-agy-ask`). That is what makes "approving a
write writes the file into the project" true rather than aspirational.

### 12.4 Permission cards, and the order that makes them a boundary

Every `session/request_permission` goes through `askPermissionHandler.RequestPermission`, in this order:

1. **Mode gate** (`modegate.go`). A tool the turn's mode refuses is answered with the reject option and **no
   card is raised** — the platform has already decided, and a card would invite a per-call override of a
   decision that is not the operator's to make. Because ACP carries no reason on a rejection, the refusal is
   ALSO published as an `IsError` `tool_result` (so the operator sees it inline where the call was) and
   prepended to the next prompt as `[platform] …` (so the model learns the rule instead of retrying).
2. **Never-allow + boundary**, still without a card: `claude.DecideToolForAsk` runs the never-allow command
   class, the protected roots, the operator's policy file and the `workerrestrict` boundary — the SAME rules
   claude's Ask hook runs, called rather than copied.
3. **Auto-allow** the tools nobody needs to be asked about: agy's reads, greps and lists, and the platform's own
   MCP tools. The carded set is claude's Ask set translated: `run_command`, the write/edit family, `search_web`,
   `read_url_content`, and third-party MCP tools.
4. **The card**, as the identical `permission` bus event the claude path raises (`Kind:"permission"`,
   `PermissionID`, `Tool` = the raw agy name, `Command` = `CommandLine`, `Targets`, `InputJSON`), blocking with
   **no timeout by default** (`ORCHICON_ANTIGRAVITY_CONSENT_WAIT` shortens it for tests/an operator who wants a
   leash). Answers map to the option ids agy actually OFFERED (`allow`/`deny` today), never to literals:
   `once|allow|approve` → allow, `always|permanent` → allow_always if offered else allow_once,
   `reject|deny` → reject. **The bridge records no grant** — the grant store lives in askorchicon, exactly as on
   the claude path.

**Teardown answers reject.** Parked cards are settled before the child goes, and the handlers are given a
bounded window to write their decision, so a shutdown produces a refusal rather than a call left hanging.

### 12.5 Mode gating, twice

The in-handler gate above is the authority (the mode can change between turns while argv is fixed at spawn).
The mode's denied tools are ALSO passed to agy as `--settings '{"permissions":{"deny":[…]}}'` — session-only,
never saved — as defence in depth. **The flag is a TOP-LEVEL agy flag and must precede the subcommand**:
`agy --settings '<json>' acp` works; `agy acp --settings '<json>'` exits 1 with the acp usage text (verified on
1.5.13; it is absent from `agy acp --help` for that reason). `LaunchOptions.Argv()` emits it in the right place and
`assertSettingsNotBypass` refuses a document naming any auto-approve key, so the feature cannot be turned into a
bypass.

A mode that may not ACT (Brainstorm, Quick Work) is also not OFFERED a third-party MCP server at all
(`withholdOpaqueMCP`), which is the offered half of `askmode.MayAct`.

### 12.6 `ask_question` cannot be answered — document it, deny it, route it

agy's native multiple-choice tool is a `tool_call` that agy renders as its own assistant text and then ends the
turn. There is **no permission request and no answer channel**, so there is no way to send the operator's choice
back. The decision, in three parts:

* **Deny it** via `--settings` in every mode, so a model that tries it gets a tool error it can see rather than a
  question that goes nowhere.
* **Tell the model what to use instead**: the first prompt of every conversation carries `[platform] To ask the
  user a multiple-choice question, call the Orchicon MCP tool orchicon_ask_user … Do NOT use agy's own
  ask_question`.
* **Document it here** (this section). The platform's `orchicon_ask_user` works as it does on any MCP-served
  adapter: it is a tool on the Orchicon MCP sidecar, registered in `mcpServers` with the CONVERSATION scope.
  askorchicon intercepts the call and renders the option card; the operator's answer arrives as the next turn.

Agy's spelling of the platform's tools is `mcp_orchicon_orchicon_ask_user` (single underscores, tool keeps its
own `orchicon_` prefix). `askorchicon.isAskToolName` accepts it and `toolclass.Classify` folds it to the ask card,
so it neither vanishes from the transcript nor inflates the activity line.

### 12.7 Streaming, and the honest gap

`agent_message_chunk` → `delta{text}`, coalesced into ONE `part{text}` at turn end. `tool_call` →
`part{tool_use}`; the terminal `tool_call_update` → `tool_result` with its output. `usage_update` is the CONTEXT
GAUGE; the prompt result's `usage` is the billable accounting, recorded under owner kind `ask_conversation`
(`SessionID` = the conversation id, with `ExecutionID`/`TaskID`/`WorkerID` empty — a conversation has none).

**Reasoning text does not exist on agy 1.5.13.** It never emits `agent_thought_chunk` (only
`usage.thoughtTokens`), so an Ask turn shows no reasoning text. The arm is implemented for a future agy and is
**never fabricated** — the reasoning TOKENS still reach the usage row, and a client that shows an empty
reasoning block is showing the truth.

### 12.8 The Ask diff ledger

A conversation's file edits go to the ASK ledger (`SetDiffLedger`'s second argument, owner kind
`ask_conversation`, owner id = the conversation id), never the execution ledger — a conversation has no
execution to attribute an edit to. The ledger fires on the TERMINAL `tool_call_update`, because the hook diffs
the file's fresh on-disk state and an `in_progress` call has not written anything yet.
