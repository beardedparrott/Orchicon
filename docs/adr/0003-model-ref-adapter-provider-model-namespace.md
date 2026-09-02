# ADR-0003: Extend model_ref to adapter/provider/model namespace

**Status:** Proposed (step 1 — Principal Software Architect)
**Work item:** Extend model_ref to adapter/provider/model namespace

## Context

Today the model reference is a two-segment `provider/model` string. The
OpenCode adapter is the only adapter kind, so the provider segment
implicitly names a provider *within* the OpenCode adapter — and every
consumer that needs to reach a specific model re-derives that split
itself:

- `internal/opencode/session.go` `splitModelRef` (first `/` split, `ok=false`
  when absent) — used to map a ref onto the opencode serve's
  `{"providerID": ..., "modelID": ...}` message shape (`SendMessage…`).
- `internal/opencode/adapter.go` `parseModelRef` (first `/` split, `unknown`
  fallbacks) — used for usage attribution (`UsageRecord.Provider`/`Model`).
- `internal/opencode/session_run.go` `doCompact` calls `splitModelRef` for
  the `/summarize` body (`providerID`/`modelID`).

Three problems block the multi-adapter roadmap:

1. **No adapter dimension.** `runtime_ref` selects the adapter kind (e.g.
   `opencode`), but the model ref cannot name a model *of a different
   adapter*. The moment a second adapter kind lands, `provider/model` is
   ambiguous — there is nowhere to put the adapter.
2. **Naive 3-way splits are wrong for slashed model ids.** Model IDs legally
   contain `/` (e.g. Command Code's `deepseek/deepseek-v4-flash`, OpenRouter
   `vendor/model` ids). A naive `strings.Split(ref, "/")` into exactly three
   fields corrupts the model id. The parser must be **left-greedy**: segment 1
   = adapter, segment 2 = provider, remainder = model **verbatim**.
3. **Provider lists are adapter-agnostic today.** The gateway's
   `ListProviders`/`ListOpenCodeModels` return a single global list
   (`internal/aigateway/convert.go` `defaultProviders`, `models.go`
   `ModelDiscoverer`) regardless of which adapter the operator intends to
   route through. Tenant-created custom providers (Settings → Adapters, per
   the provider-layer task) do not exist yet; the design must make the
   provider tier adapter-scoped and include them from day one.

### Established facts (verified in this worktree)

- `runtime_ref` on `worker_versions` is the adapter-kind selector; the
  TaskReconciler passes it as the adapter `kind` to `selectAdapter`
  (`internal/scheduler/reconciler.go:593`). The opencode adapter is the
  only kind wired in `internal/server/server.go` (`opencode.New`).
- The worker/AskOrchicon model picker (`frontend/src/components/ModelPicker.tsx`)
  reads `useListOpenCodeModels()` → `AIGatewayService.ListOpenCodeModels` →
  `aigateway.ModelDiscoverer` (`opencode models --verbose`, mock fallback).
  The provider tier is derived client-side from `model.providerId`.
- `splitModelRef`/`parseModelRef` are the ONLY model-ref splitters in the
  codebase (both first-`/` splits; neither understands adapters or slashed
  model ids end-to-end). `Compact` uses `providerID`/`modelID` on the serve
  API; `SendMessage` uses `{"providerID","modelID"}`.
- The opencode serve's own model references are `provider/model` (verified
  by the real `opencode` runtime the adapter drives and by
  `aigateway/models.go` mock refs `opencode-go/deepseek-v4-flash`,
  `opencode/deepseek-v4-flash-free`, `anthropic/claude-sonnet-4`).
- Model-ref validation today is length-only (`internal/worker/validate.go`
  `validateTextField`, `maxNameLen=500`); no grammar check exists. No DB
  migration is required — `model_ref` is a text column on
  `worker_versions` / `ask_orchicon_conversations`, and `tenant_settings`
  holds `default_worker_model` / `default_ask_orchicon_model`.
- The tenant-provider table for custom providers does not exist yet (the
  provider-layer task creates it); this ADR defines the *contract* (name =
  provider segment, adapter-scoped) the provider-layer task must satisfy.

## Decision

**D1 — One shared parser: `adapter.ParseModelRef` (left-greedy).**
Create a new package `internal/adapter` (or extend the existing
`internal/adapter/service.go` package) exporting:

```go
type ModelRef struct {
    Adapter  string // segment 1, lowercased, canonical
    Provider string // segment 2
    Model    string // remainder, verbatim (slashes preserved)
}

// ParseModelRef parses a model ref under the pinned grammar:
//   left-greedy, exactly three semantic fields.
//   "adapter/provider/model"         -> {adapter, provider, model}
//   "provider/model" (legacy)        -> {opencode, provider, model}   (see D2)
//   "adapter/provider/a/b/c"         -> {adapter, provider, "a/b/c"}  (verbatim remainder)
// Errors carry actionable text (see D4).
func ParseModelRef(ref string, known adapter.ProviderRegistry) (ModelRef, error)
```

`ParseModelRef` receives a small `ProviderRegistry` interface (D3) so it can
(a) recognize legacy 2-segment refs whose first segment is a known provider
of the default adapter and (b) reject unknown adapter segments with an
actionable error. Pure and dependency-free otherwise.

Grammar rules (pinned):
- **Left-greedy.** Segment 1 = adapter, segment 2 = provider, remainder =
  model **verbatim** including internal slashes. Never split the model field.
- **2-segment legacy.** When the ref has exactly two segments and the first
  segment is a KNOWN provider (built-in profile **or** tenant-created custom
  provider) of the default adapter (`opencode`), infer
  `{adapter: opencode, provider: seg1, model: seg2}`. This keeps every
  existing `opencode/...` and `local-models/Qwen3.6-35B-A3B-UD-Q4_K_XL`
  ref working unchanged once `local-models` is defined in Settings →
  Adapters.
- **3+ segments.** `{adapter: seg1, provider: seg2, model: seg3..}`. The
  4-segment Command Code shape `orchicon/commandcode/deepseek/deepseek-v4-flash`
  parses to `{adapter: orchicon, provider: commandcode, model: deepseek/deepseek-v4-flash}`.
- **Rejection.** Unknown adapter segment (a 3+ segment ref whose segment 1 is
  not a known adapter kind) is rejected with an error pointing at
  Settings → Adapters. A 2-segment ref whose first segment is neither a known
  provider nor a known adapter is rejected the same way. Malformed refs
  (empty, no slash, whitespace-only segments) are rejected with a clear
  message. The parser NEVER splits the model field at an internal slash, and
  model-id validation must not forbid `/`.

`ParseModelRef` is the single implementation. All three current split sites
delegate to it (adapter.go `recordUsage`, session.go `SendMessage…`, and
session_run.go `doCompact`). No behavior change at the serve boundary: the
opencode serve still receives `{"providerID","modelID"}`; the adapter
segment is consumed by the control plane (routing + usage attribution) and
dropped before the serve call.

**D2 — Legacy inference covers built-ins AND tenant custom providers.**
The 2-segment inference in D1 resolves the first segment against the union
of the default adapter's built-in provider profiles **and** the tenant's
created custom providers (the `ProviderRegistry`). Consequences:
- `opencode/deepseek-v4-flash-free` keeps parsing (provider `opencode` is a
  built-in profile).
- `local-models/Qwen3.6-35B-A3B-UD-Q4_K_XL` keeps parsing (adapter inferred
  `opencode`) as soon as the operator defines `local-models` in Settings →
  Adapters — no worker edits, no migration.
- A 2-segment ref whose first segment is unknown is rejected with
  "unknown provider `X` — create it in Settings → Adapters, or use
  `adapter/provider/model`".

**D3 — Adapter-scoped provider lists.**
The per-adapter provider list = the adapter's built-in provider profiles ∪
the tenant-created custom providers bound to that adapter. Introduce a
`ProviderRegistry` (interface) with two implementations:
- a built-in catalog (adapter kind → built-in provider profiles; today
  `opencode` → the `defaultProviders()`/mock-model providers),
- a DB-backed source for tenant custom providers (adapter-scoped; the
  provider-layer task owns the table; this ADR pins the contract: provider
  name = the ref segment, `adapter_kind` column, tenant-scoped).

The picker data flow becomes:
`ListOpenCodeModels(adapter)` → built-in models (discoverer) ∪ custom-provider
models, both tagged with the adapter kind; the frontend ModelPicker gets an
**adapter tier** (All / opencode / … / custom adapters) and the provider tier
is derived from the models of the selected adapter — the provider options a
picker shows depend on the selected adapter, per the work item.

**D4 — Error contract (actionable).**
All parser errors are of the form "`<ref>`: <what is wrong>, <how to fix>".
Specifically:
- unknown adapter segment → "unknown adapter `X` in model ref `<ref>` —
  register an adapter of that kind, or use `adapter/provider/model` with a
  known adapter";
- unknown provider in a 2-segment ref → "unknown provider `X` in model ref
  `<ref>` — create it in Settings → Adapters (custom provider), or use
  `adapter/provider/model`";
- malformed → "model ref `<ref>` must be `adapter/provider/model` (or the
  legacy `provider/model`)". These surface at the validation boundary
  (`internal/worker` on save, settings save, and AskOrchicon dispatch) so a
  bad ref fails **loud and early** with a copy-pasteable fix, not at dispatch
  time as an opaque "no ready adapter".

**D5 — Normalize on read/write (migration-free).**
Stored refs stay as-is on disk (no data migration — `model_ref` is text and
legacy refs are valid). Normalization is **on read**: every consumer that
touches a ref (dispatch, usage attribution, picker selection echo, Ask
Orchicon) goes through `ParseModelRef`, which yields the 3-segment semantic
form. New writes SHOULD be normalized to `adapter/provider/model` at the
validation boundary so the UI's canonical display is the 3-segment form, but
legacy refs remain first-class forever (D2).

**D6 — Wiring.**
- `internal/opencode/session.go` `SendMessage…` and
  `internal/opencode/session_run.go` `doCompact`: replace `splitModelRef`
  with `adapter.ParseModelRef`; pass the parsed `provider`/`model` to the
  serve (the adapter segment is not sent to the serve).
- `internal/opencode/adapter.go` `recordUsage`: use `ParseModelRef`;
  `UsageRecord` keeps `Provider`/`Model` (the adapter segment does not need a
  new column; attribution stays provider/model).
- Validation: `internal/worker/validate.go` gains a model-ref grammar check
  (calls `ParseModelRef` with the registry) in the `model_ref` path;
  `internal/settings` applies the same check to
  `default_worker_model`/`default_ask_orchicon_model`; AskOrchicon dispatch
  already routes through `modelRefOrFallback` → `ParseModelRef` before the
  serve call.
- `runtime_ref` continues to select the adapter kind for dispatch
  (`selectAdapter`); the model ref's adapter segment must MATCH the worker's
  `runtime_ref` adapter kind when both are present (validation-time
  consistency check, mismatch → actionable error). This is the invariant
  that keeps `adapter/provider/model` honest.

**D7 — Platform-change sync (AGENTS.md).**
The AskOrchicon tool registry (`internal/askorchicon/tools.go` + tool files)
gains/updates any user-facing surface this feature adds: the `create_worker`
`model_ref` description shows the 3-segment form with a legacy-compat note,
and the model/`ListOpenCodeModels` path is adapter-aware. The tool registry
must never drift from the platform's actual grammar.

## Consequences

- **Good:** one parser, one grammar, no more divergent `splitModelRef`
  semantics; slashed model ids (Command Code `deepseek/deepseek-v4-flash`,
  OpenRouter ids) survive parsing verbatim; legacy refs keep working with
  zero operator edits; provider pickers become adapter-scoped; errors are
  actionable at the input boundary.
- **Good:** the pinned left-greedy rule fixes the "4-segment ref breaks a
  naive 3-way split" class forever — a ref is NEVER split inside the model
  field, so the Command Code "mode" dimension stays out of the ref (resolved
  at runtime per request by the provider profile, per the pinned grammar).
- **Watch:** legacy inference needs the provider registry; until the
  provider-layer task lands, the registry resolves built-in profiles only
  (custom providers are inert). The parser must degrade gracefully: a
  `local-models/...` ref parses once the provider exists, and is rejected
  with the Settings → Adapters pointer before that.
- **Watch:** `runtime_ref`/model-ref adapter mismatch is a new validation
  failure; the error must tell the operator which field to fix. Keep the
  check at save time, not dispatch time.
- **Watch:** the serve boundary is untouched (`providerID`/`modelID`), so
  opencode itself never sees the adapter segment — no vendor coupling.
- **Scale:** parsing is O(n) over the ref string and registry lookups are
  map hits; the picker merge (built-in ∪ custom) is done once per
  request/cache-TTL. No hot-path concern at 10x.

## Files touched (delta for the implementer)

| File | Change |
|---|---|
| `internal/adapter/modelref.go` (new) | `ModelRef` struct, `ParseModelRef`, grammar + errors (D1, D2, D4) |
| `internal/adapter/modelref_test.go` (new) | table tests: legacy 2-seg (built-in + custom first segments), 3-seg, slashed-model refs (incl. `orchicon/commandcode/deepseek/deepseek-v4-flash`), malformed, unknown-adapter-segment (D8) |
| `internal/adapter/providers.go` (new) | `ProviderRegistry` interface + built-in catalog (D3) |
| `internal/opencode/session.go` | `splitModelRef` → `adapter.ParseModelRef` in `SendMessage…` (D6) |
| `internal/opencode/session_run.go` | `doCompact` → `adapter.ParseModelRef` (D6) |
| `internal/opencode/adapter.go` | `recordUsage` → `adapter.ParseModelRef`; delete local `parseModelRef` (D6) |
| `internal/worker/validate.go` | model-ref grammar check on `model_ref` (D6, D4) |
| `internal/settings/service.go` | same check on default models (D6, D4) |
| `internal/aigateway/service.go` + `models.go` | adapter-scoped model listing (adapter param on `ListOpenCodeModels`) (D3) |
| `proto/orchicon/api/v1/ai_gateway.proto` | `ListOpenCodeModelsRequest.adapter` (D3) |
| `frontend/src/api/aigateway.ts` + `frontend/src/components/ModelPicker.tsx` | adapter tier; provider tier scoped to the selected adapter (D3) |
| `internal/askorchicon/tools.go` | `create_worker` `model_ref` description → 3-segment form (D7) |

## Test plan (acceptance mapping)

All pure-parser tests are unit tests (no DB/serve). `ParseModelRef` is the
single entry point exercised.

1. **Legacy 2-seg (built-in first segment):** `opencode/deepseek-v4-flash-free`
   → `{opencode, opencode, deepseek-v4-flash-free}`; `anthropic/claude-sonnet-4`
   → `{opencode, anthropic, claude-sonnet-4}`.
2. **Legacy 2-seg (custom-provider first segment):**
   `local-models/Qwen3.6-35B-A3B-UD-Q4_K_XL` → `{opencode, local-models, Qwen3.6-35B-A3B-UD-Q4_K_XL}`
   when `local-models` is in the registry; unknown-provider rejection
   (with Settings → Adapters pointer) when it is not.
3. **3-seg:** `claude/anthropic/claude-sonnet-5` →
   `{claude, anthropic, claude-sonnet-5}`; `opencode/opencode-go/deepseek-v4-flash` →
   `{opencode, opencode-go, deepseek-v4-flash}`.
4. **Slashed model ids:** `orchicon/commandcode/deepseek/deepseek-v4-flash` →
   `{orchicon, commandcode, deepseek/deepseek-v4-flash}` (model verbatim);
   `adapter/provider/a/b/c` → model `a/b/c`.
5. **Malformed:** empty, single segment, whitespace-only, empty adapter/
   provider/model segments → clear actionable errors.
6. **Unknown adapter segment:** `foo/anthropic/claude-sonnet-5` → rejection
   naming the adapter and pointing at registration/Settings → Adapters.

## Reviewer checklist

- Does the left-greedy grammar hold for every pinned example?
- Is the model field guaranteed verbatim (no split at internal slashes)?
- Do legacy refs (built-in AND custom-provider first segments) resolve to
  `opencode` without manual edits?
- Are provider options adapter-scoped (built-in ∪ tenant custom)?
- Are malformed/unknown refs rejected with actionable errors?
- Does the opencode serve boundary stay `providerID`/`modelID` (no adapter
  segment leak)?
