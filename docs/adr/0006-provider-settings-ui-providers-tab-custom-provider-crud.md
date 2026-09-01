# ADR-0006: Provider settings UI — Providers tab, custom provider CRUD & secret auto-write

Status: **Proposed** (architect step; pending Design Approver ratification)
Work item: "Provider settings UI: Providers tab, custom provider CRUD & secret auto-write"
Full design + test plan: `architecture-notes/provider-settings-ui-providers-tab-custom-provider-crud-secret-auto-write.md` (worker artifact of the architect step).

## Context

The provider-layer substrate (ADR-0003, PR #459) landed on `develop`: `internal/orchicon/` profiles (`BuiltinProfile`, `ValidateProfile`, `SetCustomProfileLoader` hook), `Registry.Get/Invalidate`, `SourcingService.ListModels` (catalog → probe → manual, manual-wins dedupe, visibility filter, `Degraded` flag), `CredentialResolver.Resolve` (tenant secret by NAME → host env → actionable `ErrAuthMissing`), and the model-ref grammar (`internal/adapter/modelref.go`, registry-driven legacy 2-segment parsing). The tenant-facing management surface for all of this does not exist: no UI, no storage for overrides/customs, no token auto-write, no custom-provider CRUD, no deletion guard.

## Decision

1. **Storage**: new `provider_settings` table (migration `20260914000000`), tenant-scoped, `UNIQUE (tenant_id, provider_id)`, RLS `tenant_isolation` (tenant_secrets pattern). Built-ins get a row only when overridden (enabled/baseURL/hidden models/num_ctx); no row = pure built-in default. `manual_models` JSONB mirrors `orchicon.ModelInfo` hints.
2. **Ref-id grammar**: custom refs match `^[a-z0-9][a-z0-9_-]{1,63}$`; derived secret name `CUSTOM_<REF uppercased, -→_>_API_KEY` (e.g. `local-models` → `CUSTOM_LOCAL_MODELS_API_KEY`) is enforced against the secrets name regex `^[A-Z][A-Z0-9_]+$`. Collisions with built-in ids and other tenant customs rejected. NO model-ref parser change — `ParseModelRef(ref, reg)` resolves legacy 2-segment refs once the registry knows the provider; the delta is wiring, not grammar.
3. **Service**: new `internal/providers/service.go` (mirrors `internal/secrets`): merged `ListForTenant` (built-ins read-only ⊕ overrides + customs), `EffectiveProfile(tenantID, ref) orchicon.Profile` (the ONE function behind the `SetCustomProfileLoader` registration — loader returns only ENABLED providers), `SetProviderToken` (standard-name upsert into the existing AES-256-GCM `tenant_secrets` store via `internal/secrets` primitives + `secretcrypto`; idempotent decrypt-compare skip; audit-logged; `Registry.Invalidate` after), `ClearProviderToken`, and the **deletion guard**: tenant-scoped `worker_versions ⋈ workers` scan of `model_ref` (+ `tenant_settings.default_ask_orchicon_model` / default worker model) parsed with `adapter.ParseModelRef(ref, nil)` matching `.Provider == ref`; on hit → `FailedPrecondition` listing the referencing worker names (+ structured `referencing_workers`).
4. **RPC**: new `ProviderService` (`provider.proto`/`provider_service.proto`, buf-generated): `ListProviders` (merged view, enabled + disabled), `UpdateProviderSettings` (partial merge, built-ins and customs), `CreateCustomProvider` / `UpdateCustomProvider` / `DeleteCustomProvider`, `SetProviderToken` / `ClearProviderToken`, `ListProviderModels` (sourcing view: `ProviderModel{id, context, max_output, reasoning, source, warn_no_context}` + `degraded`). Plaintext never returned — only `has_token_stored` + expected secret name. No synchronous probe (TTL cache; UI never hangs on a dead local server).
5. **Registry wiring**: the global `deps.ModelRefRegistry` stays built-in-only (tenant-agnostic singleton; avoids a per-tenant cache-invalidation correctness trap); tenant custom ids merge into the registry instance used by the worker validation path (`internal/worker/validate.go`), where tenant context exists. `SetCustomProfileLoader` gets its real implementation at daemon wiring as a closure over `EffectiveProfile`.
6. **Secrets**: auto-write reuses the existing store end-to-end (same table, KEK, crypto, audit). Standard-name map (ANTHROPIC/OPENAI/OPENROUTER/OPENCODE/COMMANDCODE `_API_KEY`; customs `CUSTOM_<REF>_API_KEY`; ollama rejects tokens) matches the resolver's `expectedSecretName` — resolution "just works" after auto-write, env fallback and actionable failure text come from the substrate. Nothing baked into images/seed data.
7. **UI**: Settings gains a "Providers" tab (`SettingsTab` union + `ProvidersTab.tsx` + `frontend/src/api/providers.ts`): built-ins read-only with badge; per-provider enabled toggle, baseURL override, masked token field (Save/Remove + `has_token_stored`), ollama num_ctx input, model-visibility checkbox list with WARN "no context hint" badges and degraded banner; custom create/edit dialog (display name, ref id with slug validation + derived-secret preview, baseURL, auth mode none|token; ref immutable after create); delete confirm surfaces referencing workers. All mutations invalidate `['providers']`; the ModelPicker provider hook migrates to `ProviderService.ListProviders` (enabled-only) — pickers auto-refresh on save.
8. **Scope fence**: no wire-client/sourcing-logic changes (consume the substrate), no synchronous probe RPC, no secrets-UI changes, no Ask-Orchicon default changes beyond the guard's read.

## Consequences

- One migration (table + RLS + unique), one new RPC surface + generated code registered in `internal/api/api.go`, one new service package, one new frontend tab + hooks + dialogs.
- `SetCustomProfileLoader` gets its real implementation — custom providers resolve end-to-end through `Registry.Get` (profile → `CUSTOM_<REF>_API_KEY` secret → client).
- Deletion guard scans tenant `worker_versions.model_ref` values in Go (delete-only path; negligible).
- Tests: Go CRUD + auto-write idempotency + deletion-guard + effective-profile/loader wiring; vitest for tab render, token-save invalidation, guard error display, WARN/degraded rendering, picker auto-refresh.

## Alternatives considered

- **JSONB blob on tenant_settings** — rejected: RLS/uniqueness/audit granularity; a first-class table gives the guard clean SQL.
- **Per-tenant ProviderRegistry singletons** — rejected: global-singleton tenant-cache invalidation trap; validate custom refs where tenant context lives.
- **Tokens in an encrypted column on provider_settings** — rejected: bypasses the audited secrets store and the resolver's by-name contract.
- **Extend SettingsService** — rejected: provider management is a distinct bounded surface with its own proto types; a separate service keeps SettingsService's merge-update semantics untouched.
