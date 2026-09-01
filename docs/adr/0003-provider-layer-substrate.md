# ADR-0003: Provider layer substrate — native wire clients, registry & model sourcing

Status: **Accepted** (ratified by Design Approver — run 01M1DXRYQNYMWA824NJP80JXGD, 2026-09-01)
Work item: "Provider layer: native wire clients, provider registry & sourcing service"
Full design + test plan: `architecture-notes/provider-layer-native-wire-clients-provider-registry-sourcing-service.md` (worker artifact of the architect step).

## Context

Executions resolve models through the opencode runtime today; the native adapter needs hand-written Go wire clients (no third-party SDKs, no licensing entanglement) to stream turns directly. Verified in-repo: `model_ref` format is `provider/id` verbatim; tenant secrets (`internal/secrets`, KEK-gated) decrypt by secret-ID only — no by-name lookup exists; ADRs live here in `docs/adr/`. Transport facts verified against the MIT-licensed reference plugin `rashidrazak/opencode-cmd-provider@1.6.1` (behavior reference, never imported) and Ollama's official API docs.

## Decision

1. **Package**: new `internal/orchicon/` substrate with per-format subpackages (`anthropic/`, `openaicompat/`, `ollama/`, `commandcode/`, `legacycc/`, `catalog/`, `sourcing/`) plus `provider.go` (interfaces), `registry.go`, `profile.go`, `credential.go`, `retry.go`, `sse.go`.
2. **Provider interface** streams normalized events (`TextDelta`, `ReasoningDelta`, `ToolCall*`, `Error`, `Finish{StopReason,Usage}`); usage carries cache-read + cache-write tokens; reasoning tokens are a sub-bucket of output, never summed into totals (matches the usage-records cache migration). History marshaling NEVER replays assistant reasoning blocks.
3. **Anthropic client**: Messages API SSE, `tool_use` block accumulation, `cache_control` breakpoints (system + tools policy), usage incl. `cache_read_input_tokens` / `cache_creation_input_tokens`.
4. **OpenAI-compat client + profile table**: one client driven by `Profile{BaseURL, AuthEnv, AuthSecretRef, Quirks}` covering `openai`, `openrouter`, `opencode` (Zen /zen/v1), `opencode-go` (/zen/go/v1, distinct from Zen), `commandcode` (wrapped, not driven), `ollama` (via compat), and tenant-created `custom` entries. Quirks are first-class fields (`stream_options.include_usage`, tool support, reasoning field, system-role merging, …). OpenAI-style **trailing usage-only chunk** (`choices:[]` after `finish_reason`) is handled by holding `Finish` until the stream is fully drained — costs are never zeroed.
5. **Command Code dual transport**: one Bearer key (`COMMANDCODE_API_KEY`, base override `COMMANDCODE_API_BASE`), transport selected **per request by model id** — `claude-*` → `POST {base}/provider/v1/messages` (Anthropic wire), everything else (incl. slashed ids) → `POST {base}/provider/v1/chat/completions` (OpenAI wire). Plan resolution: explicit override → `COMMANDCODE_PLAN` env → cached `GET {base}/alpha/whoami` (once per instance) → default `provider`. Documented `403 upgrade_required` on the Provider route pins the instance to the legacy `/alpha/generate` transport and retries ONCE (bounded, sticky for instance lifetime); legacy envelope `{config, params{model,messages,tools,system,max_tokens,stream:true,…}, threadId}` with `x-command-code-version` / `x-cli-environment` / `x-project-slug` / `x-taste-learning` / `x-co-flag` headers, custom SSE events, cache-inclusive `totalUsage` (`noCache = total − cacheRead − cacheWrite`). ZDR: `x-cmd-zdr: 1` only when `CMD_ZDR=1`, never otherwise. `reasoning_effort` only for models with known effort levels.
6. **Ollama**: turns via OpenAI-compat `/v1`; metadata via native API — `/api/tags` (discovery), `/api/show` (TRUE context length from `model_info["<arch>.context_length"]` + `capabilities[]`), `/api/ps` (effective context of a loaded model). `num_ctx` is sent per-request through native `POST /api/chat` `options.num_ctx` (OpenAI-compat does NOT accept it — docs mandate Modelfile/env/native API). WARN when configured/effective context < model max (silent truncation to ~4096 breaks compaction math) and when a selectable model has no context hint.
7. **Credential resolution**: tenant secret (by NAME) → host env (`ANTHROPIC_API_KEY`, `OPENCODE_API_KEY`, …) → actionable failure naming the expected secret/env. Requires one new `internal/db` query (`GetSecretByName`); nothing baked into images or seed data.
8. **Vendored catalog** (`catalog/catalog.json`, go:embed): `provider/id`-keyed context window, max output, tool support, reasoning levels, pricing per token class (input/output/cache read/cache write). Missing pricing displays zero with an explicit "billing applies" disclaimer. `GetModel(ref)` is the compaction-trigger lookup for the context-management task.
9. **Model sourcing** fallback: vendored catalog (built-ins) → `GET {baseURL}/models` probe (custom compat entries; TTL-cached 5 min; non-fatal, visibly degraded on failure) → manual model entries (operator-set, optional context/output/reasoning hints). Probed + manual merge deduped (manual wins), filtered by per-provider visibility toggles.
10. **Registry**: `Get(providerID, tenantID)` resolves profile → credentials → concrete client; per-tenant-instance cache invalidated on settings change. No new RPC/proto in this task — Go APIs consumed by the sibling settings-UI task, the native-adapter task, and the pickers.
11. **Resilience**: exponential backoff + jitter on transient pre-stream failures (408/409/429/5xx, honor `Retry-After`, max 3 attempts), never retry after content is emitted, mid-stream disconnect fails cleanly (reconnect-from-transcript is out of scope), per-provider connect/idle-read timeouts, shared minimal SSE reader.

## Consequences

- Zero new third-party dependencies (stdlib only; backoff implemented in-repo).
- New OpenAI-compatible providers become configuration, not code (custom profiles + probe + manual entries).
- Cost correctness (trailing usage drain) and transport-correctness (403 flip) are fixture-pinned invariants.
- One reviewed DB addition (`GetSecretByName`); second minimal turn decoder for Ollama native /api/chat.
- Tests: canned SSE fixtures only in CI; live smokes env-gated + cost-capped (`ORCHICON_TEST_LIVE_<PROVIDER>=1`, incl. `COMMANDCODE_PLAN=go` legacy smoke).

## Alternatives considered

- **Official/SDK clients** — rejected (licensing, dep weight, test opacity).
- **Monolithic provider package** — rejected (wire formats genuinely differ; subpackages localize fixtures/quirks).
- **Runtime-fetched catalog (models.dev)** — rejected (offline/air-gapped operation; vendored snapshot keeps provenance).
- **whoami before every turn** — rejected (latency/rate-limits; cached once per instance).
