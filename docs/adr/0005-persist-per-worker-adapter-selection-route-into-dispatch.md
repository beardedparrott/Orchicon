# ADR-0005: Persist per-worker adapter selection & route into dispatch

**Status:** Proposed (step 1 — Principal Software Architect)
**Work item:** Persist per-worker adapter selection & route into dispatch
**Depends on:** ADR-0003 (model_ref adapter/provider/model namespace, PR #456),
Dispatcher bridge registry (PR #455), ADR-0004 (three-tier picker, PR #457)

## Context

The three-tier picker (ADR-0004) already **selects** an adapter→provider→model
and writes canonical 3-segment refs, and dispatch already resolves the bridge
from the ref (`internal/scheduler/reconciler.go:1084`,
`kind := adapter.AdapterKind(modelRef)` → `dispatcher.Resolve(kind)`; legacy
2-segment refs infer `opencode`). What this work item adds is the
**persist + enforce + route** loop around that selection:

1. **No explicit per-worker adapter surface.** The adapter segment lives
   inside `model_ref`, but the worker API never exposes it as a first-class
   selection and never validates it against what can actually dispatch.
2. **Default selection.** The picker seeds its adapter tier from the stored
   ref (`opencode` for unset refs, `DEFAULT_ADAPTER_KIND` in
   `frontend/src/lib/model-ref.ts`). The decided product default for fresh
   selections is `orchicon` (the native adapter kind; providers
   `command-code`, `local-models` already exist in the catalog).
3. **Adapter-change handling.** Nothing validates that a CHANGED adapter's
   provider/model are valid for the new adapter (e.g. switching
   `opencode`→`claude` while keeping an opencode-only model must be
   rejected/reset). The picker resets tiers client-side, but the API
   boundary does not enforce it.
4. **Empty `runtime_ref` dispatch black hole.** `selectAdapter`
   (`reconciler.go:599`) queries `runtime_adapters` by `version.RuntimeRef`;
   a worker created with an empty `runtime_ref` matches no rows → the task
   requeues forever (warn log, never terminal). The model_ref-derived kind
   was never consulted.

### Established facts (verified in this worktree)

- Bridge routing is ALREADY ref-driven and legacy-safe:
  `startExecution` falls back `manifest.ModelRef` → `manifest.DefaultModelRef`
  → `adapter.DefaultAdapterKind` (`opencode`) for empty/malformed refs, and
  `Resolve` fails the execution **terminal + actionable** (`failed_to_start`)
  for unregistered kinds (reconciler.go:1069–1104). No reconciler routing
  change is needed for the common path.
- `selectAdapter` is keyed on `version.RuntimeRef` (NOT the ref segment):
  `reconciler.go:599`. Empty `runtime_ref` → `kind = ""` → zero rows →
  **infinite requeue** (non-terminal). Only `opencode` has a seeded
  `runtime_adapters` row (`server.go:974`, in-process dev adapter, kind
  `"opencode"`).
- Only `"opencode"` is registered with the Dispatcher (`server.go:338`). The
  catalog (`internal/adapter/providers.go`) knows `opencode`, `claude`,
  `orchicon` — **catalog-known ≠ dispatchable**. There is no native
  `orchicon` bridge yet (a later task delivers it).
- `validateModelRef` (`internal/worker/validate.go:69`) validates against the
  static `BuiltinProviderCatalog` only; 3+ segment refs validate the ADAPTER
  segment only, legacy 2-seg refs validate the PROVIDER (ADR-0004 D5 pinned
  the save-path contract: catalog-known-but-unregistered adapters and 3-seg
  deleted providers **re-save unchanged**).
- The picker's adapter-change path already resets provider + search
  (`selectAdapter`, ModelPicker.tsx:120) and only writes a ref on model
  selection, so a stale model cannot leak — but nothing pins this contract.
- `workers_.new.tsx` hardcodes `runtimeRef: "opencode"` (line 149); the
  picker's chosen adapter never reaches `runtime_ref`.
- The worker service already has the seams this task needs:
  `modelRefRegistry` (`adapter.ProviderRegistry`, swap via
  `SetModelRefRegistry`) and the fn-injection pattern used to avoid the
  `aigateway → scheduler` import cycle (`Dependencies.AdapterKinds
  func() []string`, ADR-0004 D1).
- **No DB migration is required or permitted**: ADR-0003 pinned "no
  adapter-kind column exists or may be added" — the ref IS the persisted
  selection.

## Decision

**D1 — The model_ref's adapter segment IS the persisted per-worker adapter
selection (no new column, no second source of truth).** Persistence is the
existing `model_ref` write path (create / update-version / bulk-model).
Nothing new is stored; the selection is persisted exactly when the ref is.

**D2 — Expose the adapter selection on the worker API (computed, read-only).
**
- `WorkerVersion.adapter` and `WorkerListItem.active_adapter` (proto fields,
  `make gen`), computed server-side via `adapter.AdapterKind(v.ModelRef)`
  (legacy refs report `opencode` — the parser's inference, not a stored
  value).
- Optional **input** `adapter` on CreateWorker / UpdateWorkerVersion /
  CreateWorkerVersion as a validation + consistency affordance for API
  clients: (i) it must be a **Dispatcher-registered kind** (injected
  `AdapterKinds func() []string`; falls back to the catalog kinds when not
  wired — headless/tests), else InvalidArgument with an actionable error;
  (ii) when the request also carries a `model_ref`, the two must AGREE
  (parsed adapter segment == input) else InvalidArgument; (iii) a lone
  `adapter` with no `model_ref` is rejected — there is nowhere to persist it
  (the ref is the only store). The picker keeps writing the 3-seg ref; it
  never needs the input field.

**D3 — Validation of the ref's adapter segment stays catalog-scoped; the
registered-kinds enforcement lands on the explicit input + at dispatch
(already terminal).** Rationale: tightening `validateModelRef` to
registered-kinds-only would (a) break the ADR-0004 D5 re-save contract
(`claude/...` refs re-save flagged) and (b) make the decided `orchicon`
default un-persistable until the native bridge task lands, contradicting
this work item. Unregistered kinds fail LOUDLY at dispatch today
(`failed_to_start`, error names the kind + registered kinds) — the honest
boundary. The explicit adapter input (D2) is where registered-kinds
validation is strict, because an explicit input is a deliberate routing
request.

**D4 — Adapter-change contract (the core new enforcement).**
- **API (fail loud and early, ADR-0003 D4):** on UpdateWorkerVersion /
  BulkUpdateWorkerModel / CreateWorkerVersion, when the incoming ref's
  parsed adapter differs from the version's current ref's parsed adapter,
  the full new pair must be valid for the NEW adapter: the provider segment
  must be known for the new kind (`modelRefRegistry.Providers(newKind)` ∪
  tenant custom via the same registry seam) and the model segment non-empty
  (the parser enforces segment non-emptiness). Rejected: InvalidArgument,
  error names the adapter + valid providers. Unchanged-adapter re-saves keep
  the ADR-0004 D5 semantics verbatim (deleted provider re-saves flagged).
- **Picker (reset, not reject):** selecting a different adapter resets the
  provider AND model tiers (existing `selectAdapter` reset — pinned by a new
  test) and no ref is written until a model under the new adapter is chosen;
  the stale-selection flags stay suppressed while browsing
  (`adapterDiverged`). The user-visible behavior is a clean re-selection,
  never a hidden mutation of the stored ref.

**D5 — Default adapter = `orchicon` at the selection surface; the backend
legacy inference is untouchable.**
- Picker: when the stored ref is empty (no adapter chosen yet) and
  `orchicon` ∈ registered kinds, seed the adapter tier with `orchicon`;
  otherwise seed `opencode` (today's registry: the picker degrades to the
  only dispatchable kind — never a default that cannot dispatch).
- Backend: `ParseModelRef` legacy 2-seg/1-seg inference stays
  `DefaultAdapterKind = opencode` **forever** (backward-compat path pinned by
  the acceptance criteria — existing workers are never repointed).
  `orchicon` becomes the effective new-worker default organically: the
  picker writes 3-seg `orchicon/...` refs once its tier defaults there.
- Until the native `orchicon` bridge registers, an orchicon-ref worker
  dispatches to `failed_to_start` with the actionable Resolve error —
  visible and honest, not silent. When the native bridge task lands it
  registers the bridge AND seeds/heartbeats its `runtime_adapters` row (the
  opencode contract), after which orchicon-defaulted workers dispatch
  end-to-end with zero changes here.

**D6 — Route the selection into adapter-row selection; fix the empty
`runtime_ref` black hole.** `selectAdapter` kind resolution becomes:
`version.RuntimeRef` when non-empty (all existing behavior preserved —
divergent runtime_ref vs ref-kind keeps today's terminal `failed_to_start`
rather than a silent requeue), else `adapter.AdapterKind(effective ref)`
else `adapter.DefaultAdapterKind`. Additionally the worker form
(`workers_.new.tsx`) derives `runtime_ref` from the picker's chosen adapter
when that kind is Dispatcher-registered (kinds list it already loads), else
keeps `"opencode"` — so an adapter explicitly chosen in the picker governs
the whole dispatch path (row selection + bridge) as soon as its bridge
exists, with zero further frontend work.

**D7 — Platform-change sync.** AskOrchicon `create_worker` model_ref
description gains the adapter-selection note (segment 1 selects and routes
the adapter; fresh selections default to `orchicon`; legacy 2-segment refs
infer `opencode`). No new tool param (the ref is the write surface).

## Consequences

- **Good:** the picker's selection is persisted (as the ref) and routed
  (bridge + row selection) with one source of truth; the API exposes and
  validates adapter selection; adapter switches are validated end-to-end at
  the boundary; the empty-runtime_ref dispatch black hole dies; the orchicon
  default is inert-but-correct today and live the moment the native bridge
  registers.
- **Watch:** `orchicon`-defaulted workers fail `failed_to_start` until the
  native bridge task lands — deliberate (loud > silent); the picker's
  orchicon default only appears once `orchicon` is registered, so today's
  UX is unchanged (opencode-only).
- **Watch:** the adapter-change provider check uses the same registry seam
  as `validateModelRef`; tenant custom providers merge there (provider-layer
  task) and are honored automatically.
- **Scale:** all checks are O(1) map hits / O(kinds) list scans at the API
  boundary; dispatch is unchanged. No 10x concern.

## Files touched (delta for the implementer)

| File | Change |
|---|---|
| `proto/orchicon/api/v1/worker.proto` | `WorkerVersion.adapter`, `WorkerListItem.active_adapter` (computed); optional `adapter` on Create/UpdateVersion/CreateVersion requests (D2) |
| `internal/worker/validate.go` | `validateAdapterInput(kind string, registered func() []string)` (registered-kinds + fallback) |
| `internal/worker/service.go` | `SetAdapterKinds` injection; adapter input validation + ref-consistency; adapter-change provider/model validation (D4); fill computed `adapter` on responses |
| `internal/api/api.go` + `internal/server/server.go` | wire `workerSvc.SetAdapterKinds(dispatcher.Kinds)` |
| `internal/scheduler/reconciler.go` | `selectAdapter` empty-runtime_ref → ref-derived kind fallback (D6) + test |
| `frontend/src/components/ModelPicker.tsx` | orchicon-default seeding when unset + registered; adapter-change reset pinned by test (D4/D5) |
| `frontend/src/routes/workers_.new.tsx` | derive `runtime_ref` from the chosen adapter when registered (D6) |
| `internal/askorchicon/tools.go` | `create_worker` model_ref description sync (D7) |
| Tests | Go: worker service (exposure, input validation, consistency, adapter-change rejection, legacy re-save preserved), reconciler fallback. TS: picker default + reset. |

## Test plan (acceptance mapping)

1. Worker persists a 3-seg ref; `GetWorkerVersion.adapter` /
   `WorkerListItem.active_adapter` report the segment; dispatch resolves
   that kind (existing reconciler test extended).
2. Legacy 2-seg ref: adapter reports `opencode`; dispatch kind `opencode`
   (backward compat pinned).
3. Adapter input: unregistered kind → InvalidArgument (actionable);
   mismatch with model_ref → InvalidArgument; lone adapter, no ref →
   InvalidArgument.
4. Adapter change with a model invalid for the new adapter → InvalidArgument
   naming valid providers; unchanged-adapter re-save of a deleted-provider
   ref still succeeds (D5 preserved).
5. Picker: empty stored ref + orchicon registered → orchicon tier seeded;
   adapter switch resets provider+model; no ref written until re-selection.
6. Empty runtime_ref worker dispatches (row selection falls back to the
   ref-derived kind).

## Reviewer checklist

- No new adapter column/field is persisted (ref is the only store).
- Legacy 2-seg inference untouched (`opencode`); existing workers never
  repointed.
- Adapter input validates against REGISTERED kinds, not the catalog.
- Adapter-change validation fires ONLY when the parsed adapter actually
  changes; unchanged refs re-save per ADR-0004 D5.
- The picker never defaults to a kind that is not registered.
- Empty-runtime_ref dispatch black hole is fixed without changing the
  divergent case's terminal-failure semantics.
