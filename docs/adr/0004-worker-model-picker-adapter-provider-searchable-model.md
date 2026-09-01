# ADR-0004: Worker model picker — adapter → provider → searchable model

**Status:** Proposed (step 1 — Principal Software Architect)
**Work item:** Worker model picker: adapter -> provider -> searchable model
**Depends on:** ADR-0003 (model_ref namespace), Dispatcher bridge registry
(PR #455), provider-layer task (tenant custom providers — NOT landed; contract only)

## Context

Workers pick a model from a flat, provider-agnostic list
(`frontend/src/components/ModelPicker.tsx` → `useListOpenCodeModels()`). The
model_ref namespace (ADR-0003, PR #456) introduced the 3-segment
`adapter/provider/model` grammar (`internal/adapter/modelref.go`,
left-greedy, legacy 2-segment infers adapter `opencode`), and the Dispatcher
bridge registry (PR #455) introduced `internal/scheduler/dispatcher.go` with
`Register(kind, bridge)`/`Resolve(kind)` — but **registered adapter kinds are
not exposed anywhere** (no public `Kinds()` method, no RPC), the provider
list is a single global list with no adapter scoping and no custom flag, and
tenant-created custom providers do not exist yet (provider-layer task).

This work item is the UI half of the adapter ecosystem: the picker must
become a three-tier control — adapter bubble list (from registered kinds) →
provider list (scoped to the selected adapter; built-in profiles ∪ tenant
custom, custom flagged with a badge + "manage in Settings → Adapters"
affordance) → searchable model list (scoped to the selected provider; manual
entries on custom providers included; models missing context/output hints
selectable but annotated). It must render existing 2-segment refs (inferring
adapter `opencode`) and write new 3-segment refs, and degrade gracefully for
unknown adapter/provider/model selections (flagged for review, still
re-savable — never hidden, blank, or erroring).

### Established facts (verified in this worktree)

- `internal/adapter/modelref.go`: `ParseModelRef` (left-greedy 3-segment,
  legacy 2-segment infers `DefaultAdapterKind` = `opencode`), `AdapterKind(ref)`.
- `internal/adapter/providers.go`: `ProviderRegistry` interface
  (`IsKnownAdapter`, `IsKnownProvider`, `Providers(adapterKind) []string`) +
  `BuiltinProviderCatalog`. **Tenant custom providers are contract-only** — no
  custom_provider table in `db/migrations` yet.
- `internal/scheduler/dispatcher.go`: `Dispatcher.Register`/`Resolve`; only
  `opencode` registered today (`internal/server/server.go` ~line 338). No
  `Kinds()` method; no RPC exposes registered kinds.
- `internal/aigateway/service.go`: `ListOpenCodeModels(adapter?)` (adapter
  filter added in PR #456), scoped to `registry.Providers(adapterKind)`;
  unknown adapter → InvalidArgument. `ListProviders` is global and
  adapter-agnostic; `AIProvider` has no `custom` flag.
- **Import-cycle constraint:** `internal/scheduler` imports
  `internal/adapter`, so `internal/adapter`/`aigateway` cannot import the
  Dispatcher type — inject registered kinds as `func() []string`.
- `internal/api/api.go`: `Dependencies.ModelRefRegistry adapter.ProviderRegistry`;
  `adapterSvc` and `aiGatewaySvc` construction points.
- `internal/worker/validate.go` + `internal/settings/service.go` validate
  model_ref via the built-in catalog.
- Frontend `ModelPicker.tsx` (develop): has an unused `adapter?: string`
  prop, calls `useListOpenCodeModels(adapter)`; flat list + provider filter
  chips derived from models' providerId. Call sites:
  `workers_.$id.tsx` (~590), `workers_.new.tsx` (~304), `settings.tsx`
  (×2), `BulkChangeWorkerModelDialog.tsx` (~122) — none pass `adapter`.
- `internal/askorchicon/tools.go` documents the 3-segment model_ref (PR #456).

## Decision

**D1 — Adapter bubble list from the Dispatcher's registered kinds.**
- Add a public `Kinds() []string` method to `internal/scheduler/dispatcher.go`.
- New RPC `ListAdapterKinds` on the adapter/gateway service returning the
  registered kinds (ordered, deduped).
- Thread the kinds via `api.Dependencies` as an injected
  `AdapterKinds func() []string` (fn injection avoids the
  `scheduler → adapter` import cycle; the server wires `dispatcher.Kinds`).
- Frontend: `useListAdapterKinds()` hook rendering the bubble list; **new
  adapters appear automatically once registered** (no frontend code change).
- **Degradation:** if the RPC errors/unavailable, fall back to
  `[DefaultAdapterKind]` (`opencode`) so the picker never blanks.

**D2 — Provider tier: built-in profiles ∪ tenant custom providers, adapter-scoped.**
- Extend `ListProviders` with an optional `adapter` filter (empty = all, or
  keep current behavior) so the provider tier is scoped to the selected
  adapter: `registry.Providers(adapterKind)` (built-in) ∪ tenant custom
  providers bound to that adapter (provider-layer task).
- Add `custom: bool` to `AIProvider` (built-in profile → false; tenant-created
  → true).
- Frontend: custom entries render a **"custom" badge** and a **"manage in
  Settings → Adapters"** affordance (link to the adapters settings section).
  Until the provider-layer task lands, the union is built-in only and the UI
  renders whatever the backend returns; the merge contract is the
  `ProviderRegistry`.

**D3 — Model tier: searchable, scoped, annotated.**
- Extend `ListOpenCodeModels` (already takes `adapter`) with a `provider`
  filter, so the model list is scoped to the selected provider.
- Manual model entries on custom providers appear in the tier (they surface
  through the provider-layer task's model sourcing; the picker renders them
  like catalog/probe-sourced models).
- Models missing context/output hints (per the provider-layer sourcing
  rules) are **selectable but annotated** (e.g. a warning note "no context
  hint — may misbehave in compaction").

**D4 — TS-side model-ref grammar mirror.**
- New `frontend/src/lib/model-ref.ts`: `parseModelRef(ref)` mirrors the Go
  left-greedy grammar — `adapter/provider/model` (model verbatim incl.
  internal slashes); legacy 2-segment `provider/model` → `{opencode,
  provider, model}`; unknown-shape → flagged. `formatModelRef(adapter,
  provider, model)` builds the 3-segment ref.
- Vitest table tests mirror the Go table tests in
  `internal/adapter/modelref_test.go` (legacy, 3-seg, slashed model ids,
  malformed).

**D5 — Graceful unknown states (acceptance-critical).**
- Stored ref whose adapter is no longer registered, whose provider was
  deleted, or whose model is unknown: the control renders the ref **flagged
  for review** (banner with the raw ref shown), the tiers render best-effort
  (matching selection highlighted where possible), and **save still writes
  the stored ref unchanged** — never hidden, blank, or erroring.
- RPC failures degrade: adapter list → `[opencode]` fallback; provider/model
  lists → empty with a retry affordance. The `{value, onChange}` contract is
  preserved so no call site breaks.

**D6 — Wiring + persist.**
- `ModelPicker` keeps `{value, onChange}`; the unused `adapter?: string` prop
  is superseded (the adapter is now a first-class tier; the value ref's
  adapter segment seeds the selection).
- On change, the picker writes the normalized 3-segment ref via
  `formatModelRef` (legacy 2-segment stored refs render via
  `parseModelRef`; saving upgrades them to 3-segment).
- Go: new RPC + proto changes; `make gen` regenerates Go + TS.
- AskOrchicon tool registry: add `list_adapter_kinds`; make
  provider/model tool descriptions adapter-aware (AGENTS.md platform-change
  rule).

**D7 — Out of scope (provider-layer task owns).**
Tenant custom-provider CRUD + table, manual model entry editing, dispatcher
registration of new adapters (the picker consumes kinds automatically), card
display polish.

## Consequences

- **Good:** new adapters appear in the picker automatically once registered
  (single source of truth = Dispatcher); provider options are adapter-scoped
  and merge built-in ∪ custom with clear custom signaling; legacy refs keep
  working and upgrade on save; unknown states degrade to flagged-for-review
  instead of blank/erroring; no call-site breakage (props contract
  preserved); import cycle avoided by fn injection.
- **Watch:** until the provider-layer task lands, the custom badge/manage
  affordance and manual model entries render from whatever the backend
  returns (built-in only) — the provider tier is real but the custom half is
  contract-only. Tests for the custom path use fixture data.
- **Watch:** provider/model lists must reset when the adapter above them
  changes (stale-selection guard).
- **Watch:** the flagged-for-review state must still allow re-save with the
  original ref — the "re-savable" requirement is what keeps it from being a
  dead end.
- **Scale:** three-tier scoping is O(lists) per selection change; the RPCs
  are cheap registry reads. No 10x concern.

## Files touched (delta for the implementer)

| File | Change |
|---|---|
| `internal/scheduler/dispatcher.go` | `Kinds() []string` (D1) |
| `internal/api/api.go` | `Dependencies.AdapterKinds func() []string`; thread to gateway service (D1, D6) |
| `internal/server/server.go` | wire `AdapterKinds: dispatcher.Kinds` (D1, D6) |
| `internal/aigateway/service.go` + `models.go` | `ListAdapterKinds`; `ListProviders(adapter)` filter; `custom` flag; `ListOpenCodeModels(adapter, provider)` (D1–D3) |
| `proto/orchicon/api/v1/*.proto` | `ListAdapterKinds` RPC; `ListProvidersRequest.adapter`; `AIProvider.custom` (D1–D3) |
| `internal/askorchicon/tools.go` + tool files | `list_adapter_kinds` tool; adapter-aware descriptions (D6) |
| `frontend/src/lib/model-ref.ts` (new) | `parseModelRef` / `formatModelRef` (D4) |
| `frontend/src/lib/model-ref.test.ts` (new) | vitest table tests (D4) |
| `frontend/src/api/aigateway.ts` | `useListAdapterKinds`, `useListProviders(adapter)`, provider param on `useListOpenCodeModels` (D1–D3) |
| `frontend/src/components/ModelPicker.tsx` | three-tier control, custom badge + manage affordance, missing-hints annotation, flagged-review state (D1–D5) |
| `frontend/src/components/ModelPicker.test.tsx` (new) | vitest component tests (D5) |

## Test plan (acceptance mapping)

1. Three tiers render: adapter bubbles top, provider list scoped, model list scoped.
2. Selecting an adapter rescopes the provider list; selecting a provider rescopes the model list.
3. Provider tier merges built-in ∪ custom; custom entries show "custom" badge + manage affordance.
4. Manual models on custom providers appear; missing context/output hints → selectable + annotated.
5. Save persists a valid 3-segment `adapter/provider/model` ref on the worker.
6. Legacy 2-segment ref renders (inferred opencode); saving upgrades to 3-segment.
7. Unknown adapter / deleted provider / unknown model → flagged-for-review banner, raw ref visible, re-savable.

## Reviewer checklist

- Three-tier scoping holds for every selection path; no stale provider/model lists on adapter switch.
- Provider merge (built-in ∪ custom) correct; custom badge + manage affordance present.
- Missing-hints models selectable but annotated.
- Legacy 2-segment refs render + upgrade to 3-segment on save.
- Unknown adapter/provider/model → flagged, re-savable, never blank/erroring.
- RPC failure degradation keeps the picker usable.
- AskOrchicon tool registry in sync with the new RPC surface (AGENTS.md rule).
- Import cycle avoided (fn injection, not the Dispatcher type).
