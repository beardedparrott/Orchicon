import { useEffect, useMemo, useRef, useState } from "react";

import { useListAdapterKinds, useListOpenCodeModels } from "@/api/aigateway";
import { useProviderList, useProviderModelsForPicker } from "@/api/providers";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import type { OpenCodeModel } from "@/api/gen/orchicon/api/v1/ai_gateway_pb";
import type { ProviderEntry } from "@/api/gen/orchicon/api/v1/provider_pb";
import {
  DEFAULT_ADAPTER_KIND,
  ORCHICON_ADAPTER_KIND,
  catalogModelMatches,
  formatModelRef,
  parseModelRef,
} from "@/lib/model-ref";

// Per-adapter provider scope is no longer static: under legacy CLI
// adapters the provider pills derive from the live CLI discovery (the old
// picker's grouping), under orchicon from the merged providers service.

interface ModelPickerProps {
  value: string;
  onChange: (value: string) => void;
}

// Three-tier control (ADR-0004): adapter bubble list (registered kinds) →
// provider list (built-in ∪ tenant custom, adapter-scoped) → searchable model
// list (provider-scoped). The stored model_ref seeds the selection
// (legacy 2-segment refs infer adapter `opencode`); saving writes a
// normalized 3-segment `adapter/provider/model` ref.
export function ModelPicker({ value, onChange }: ModelPickerProps) {
  const parsed = useMemo(() => parseModelRef(value), [value]);

  const { data: adapterKinds, error: kindsError } = useListAdapterKinds();

  // Seeded adapter/provider from the stored ref; DEFAULT_ADAPTER_KIND when the
  // ref is empty or unknown (never blank, never hidden).
  const [adapter, setAdapter] = useState<string>(() => parsed?.adapter ?? DEFAULT_ADAPTER_KIND);
  const [provider, setProvider] = useState<string>(() => parsed?.provider ?? "");
  // The lazy useState initializers seed ONLY at mount — an externally-loaded
  // value (async settings fetch filling the draft AFTER the picker's first
  // render, e.g. client-side navigation into the Defaults tab) would never
  // seed the tiers, leaving blank adapter/provider boxes until a hard
  // refresh. This effect re-seeds the tiers whenever the value changes FROM
  // OUTSIDE: user selections also flow through onChange→value, so re-seeding
  // is idempotent for the same ref and only corrects external loads.
  // While the user is mid-selection (tiers diverged from the stored ref —
  // the stale-selection-guard state), never yank the tiers back.
  const lastSeededRef = useRef<string>("");
  useEffect(() => {
    if (value === lastSeededRef.current) return; // no external change
    lastSeededRef.current = value;
    if (value.trim() === "") return; // empty = fresh selection flow
    const next = parseModelRef(value);
    if (!next) return; // malformed — leave tiers as-is (flagged instead)
    setAdapter(next.adapter);
    setProvider(next.provider);
  }, [value]);
  const [search, setSearch] = useState("");
  const [showDropdown, setShowDropdown] = useState(false);
  const [focusedIdx, setFocusedIdx] = useState(0);
  const [infoModel, setInfoModel] = useState<OpenCodeModel | null>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  const dropdownRef = useRef<HTMLDivElement>(null);

  // Provider tier (ADR-0006): tenant-aware merged view from the providers
  // service — built-ins plus the tenant's ENABLED custom providers,
  // projected into the picker's {id, name, custom} shape. Enabled-only:
  // disabled providers are not selectable.
  const {
    data: providerEntries,
    isLoading: providersLoading,
    error: providersError,
  } = useProviderList();
  // ProviderRegistry semantics (ADR-0003 D3): under the NATIVE kind the
  // tier is the enabled merged view — providers RESOLVE the model list, so
  // the tier gates tier 3. Under legacy CLI adapters the tier is a FILTER:
  // pills derived from the live CLI discovery's distinct providerID values
  // (the old picker's grouping — opencode, opencode-go, deepseek, …), with
  // an "All" reset; the model input never requires a selection.
  const useNativeSourcing = adapter === ORCHICON_ADAPTER_KIND;
  // ONE unfiltered CLI query serves BOTH the provider pills (distinct
  // providerID values) and the model list under legacy adapters — two
  // queries with different keys each shell out `opencode models --verbose`
  // (~1–2s), doubling the picker's open latency.
  const legacyModelsQ = useListOpenCodeModels(undefined, undefined, !useNativeSourcing);
  const providers = useMemo(() => {
    if (!useNativeSourcing) {
      const ids = new Map<string, { id: string; name: string; custom: boolean }>();
      for (const m of legacyModelsQ.data ?? []) {
        if (m.providerId && !ids.has(m.providerId)) {
          ids.set(m.providerId, { id: m.providerId, name: m.providerId, custom: false });
        }
      }
      return [{ id: "", name: "All", custom: false }, ...[...ids.values()]];
    }
    return (providerEntries ?? [])
      .filter((p) => p.enabled)
      .map((p: ProviderEntry) => ({ id: p.id, name: p.displayName || p.id, custom: p.isCustom }));
  }, [useNativeSourcing, providerEntries, legacyModelsQ.data]);
  // Model tier — per-adapter data source (ADR-0004): the NATIVE adapter
  // resolves models from the providers service (vendored catalog ⊕ probe ⊕
  // manual — the Settings → Adapters sourcing view); the legacy CLI
  // adapters keep opencode-CLI discovery (whose provider namespace the
  // native bridge does not share — filtering CLI output by orchicon
  // providers would be structurally empty). Both hooks run unconditionally
  // (rules of hooks); each query fetches only for its own source.
  const nativeModelsQ = useProviderModelsForPicker(
    useNativeSourcing ? provider : "",
    useNativeSourcing && provider !== "",
  );
  // (legacyModelsQ is declared once, above, feeding both the provider
  // pills and the model tier — one RPC, two projections.)
  const models = useNativeSourcing ? nativeModelsQ.models : legacyModelsQ.data;
  const modelsLoading = useNativeSourcing ? nativeModelsQ.isLoading : legacyModelsQ.isLoading;
  const modelsError = useNativeSourcing ? nativeModelsQ.error : legacyModelsQ.error;

  const adapterList = adapterKinds && adapterKinds.length > 0 ? adapterKinds : [DEFAULT_ADAPTER_KIND];
  // Catalog match is by PARSED SEGMENTS (catalogModelMatches), never by raw
  // value: OpenCodeModel.modelRef is the legacy 2-segment "providerId/id"
  // (internal/aigateway), so a raw comparison against a 3-segment ref would
  // false-flag every freshly-written ref (QA BUG-1). parsed === null (empty or
  // malformed ref) never matches — unknown shapes stay flagged (D5).
  const modelKnown = parsed !== null && models?.some((m) => catalogModelMatches(parsed, m)) === true;
  // Stored-ref flags are evaluated against the PARSED ref and the loaded
  // lists — independent of the current tier selection, so navigating the
  // tiers mid-re-selection never flags a previously-valid stored ref.
  const storedAdapterKnown = parsed !== null && adapterList.includes(parsed.adapter);
  const storedProviderKnown =
    parsed === null ||
    parsed.provider === "" ||
    (providers?.some((p) => p.id === parsed.provider) ?? false);
  // Provider/model verification is only meaningful within the stored ref's
  // own adapter scope: while the user browses a different adapter the
  // provider/model lists are scoped elsewhere, so suppress those flags
  // instead of false-flagging a previously-valid ref.
  const adapterDiverged = parsed !== null && parsed.adapter !== adapter;

  // Stale-selection guard: when the seeded adapter/provider are not in the
  // freshly-loaded lists (unknown/stored refs), keep the flagged state rather
  // than resetting the stored value.

  // ADR-0005 D5 (default adapter = "orchicon" for FRESH selections): when
  // the stored ref is empty (no adapter chosen yet) and "orchicon" is a
  // Dispatcher-registered kind, seed the adapter tier with "orchicon".
  // Otherwise seed "opencode" (today's registry — the picker degrades to
  // the only dispatchable kind, never a default that cannot dispatch).
  // Legacy/stored refs keep their own adapter (never repointed).
  useEffect(() => {
    if (value.trim() !== "") return; // stored ref keeps its own selection
    if (!adapterKinds) return; // kinds not loaded yet — no guessing
    setAdapter(adapterKinds.includes(ORCHICON_ADAPTER_KIND) ? ORCHICON_ADAPTER_KIND : DEFAULT_ADAPTER_KIND);
  }, [value, adapterKinds]);
  const selectedProviderObj = providers?.find((p) => p.id === provider);
  const customProvider = selectedProviderObj?.custom ?? false;

  const selectedModel = useMemo(() => {
    if (!models || !parsed) return null;
    return models.find((m) => catalogModelMatches(parsed, m)) ?? null;
  }, [models, parsed]);

  const filtered = useMemo(() => {
    if (!models) return [] as OpenCodeModel[];
    let result = models;
    // Legacy adapters: provider pills are OPTIONAL filters ("All" =
    // unfiltered flat list — the red-cursor gate stays dead).
    if (!useNativeSourcing && provider) {
      result = result.filter((m) => m.providerId === provider);
    }
    if (search) {
      const q = search.toLowerCase();
      result = result.filter(
        (m) =>
          m.id.toLowerCase().includes(q) ||
          m.name.toLowerCase().includes(q) ||
          m.providerId.toLowerCase().includes(q) ||
          m.modelRef.toLowerCase().includes(q) ||
          m.family.toLowerCase().includes(q),
      );
    }
    return result.sort((a, b) => {
      if (a.providerId !== b.providerId) return a.providerId.localeCompare(b.providerId);
      return (a.cost?.input ?? 0) - (b.cost?.input ?? 0);
    });
  }, [models, search, useNativeSourcing, provider]);

  useEffect(() => setFocusedIdx(0), [filtered.length]);

  // Close dropdown on outside click. dropdownRef now wraps the INPUT (the
  // in-box panel design), so the outside check must exclude the whole
  // wrapper — checking only the input would close the panel instantly.
  useEffect(() => {
    function handleClick(e: MouseEvent) {
      if (
        dropdownRef.current &&
        !dropdownRef.current.contains(e.target as Node) &&
        inputRef.current &&
        !inputRef.current.contains(e.target as Node)
      ) {
        setShowDropdown(false);
      }
    }
    document.addEventListener("mousedown", handleClick);
    return () => document.removeEventListener("mousedown", handleClick);
  }, []);

  // Adapter selection: rescope provider and reset the model tier (no stale
  // selection leaks across adapters — ADR-0004 stale-selection guard;
  // ADR-0005 D4 reset contract: switching adapters NEVER carries the
  // previous selection into the new scope and no ref is written until a
  // model under the new adapter is chosen). The panel STAYS OPEN — the
  // tiers live inside the box now, and closing it here would strand the
  // user: orchicon with no provider yet renders a DISABLED input that can
  // never fire onFocus to reopen (QA round 3 bug #2).
  function selectAdapter(kind: string) {
    setAdapter(kind);
    setProvider("");
    setSearch("");
  }

  function selectProvider(id: string) {
    setProvider(id);
    setSearch("");
  }

  function selectModel(model: OpenCodeModel) {
    // model.id is the bare model id — the model segment of the 3-segment
    // grammar. model.modelRef is the legacy 2-segment provider/model and
    // must NOT be used here (it would produce a bogus 4-segment ref).
    onChange(formatModelRef(adapter, provider, model.id));
    setShowDropdown(false);
    setSearch("");
  }

  // Stored-ref review banner: the picker renders the raw ref flagged for
  // review whenever the stored value is unknown in any tier (D5) — never
  // blank, hidden, or erroring. Each tier flags only what its LOADED data
  // can verify: a failed/absent catalog or a mid-navigation tier scope
  // never produces a false flag (and no flash-of-banner before queries
  // resolve). Catalog-known-but-unregistered adapters and 3-seg deleted
  // providers re-save unchanged; unknown-adapter 3-seg and unknown-provider
  // 2-seg refs route to re-selection via the tiers.
  const storedRefFlagged =
    value.trim() !== "" &&
    (!parsed ||
      (adapterKinds !== undefined && !storedAdapterKnown) ||
      (!adapterDiverged && providers !== undefined && !storedProviderKnown) ||
      (!adapterDiverged &&
        parsed !== null &&
        parsed.provider === provider &&
        models !== undefined &&
        !modelKnown));

  const reviewReasons: string[] = [];
  if (value.trim() !== "") {
    if (!parsed) {
      reviewReasons.push("unrecognized ref shape");
    } else {
      if (adapterKinds !== undefined && !storedAdapterKnown) {
        reviewReasons.push("adapter not registered");
      }
      if (!adapterDiverged && providers !== undefined && !storedProviderKnown) {
        reviewReasons.push("provider not found (deleted or unknown)");
      }
      if (
        !adapterDiverged &&
        parsed.provider === provider &&
        models !== undefined &&
        !modelKnown
      ) {
        reviewReasons.push("model not found in the selected provider's catalog");
      }
    }
  }

  // Missing context hint → selectable but annotated (D8). Context is the
  // compaction-critical value; output-max is nice-to-have (most live
  // /models listings don't carry it — warning on it would amber every row).
  function missingHints(model: OpenCodeModel): boolean {
    return !model.limits || !model.limits.context;
  }

  function formatCost(cost?: { input: number; output: number }) {
    if (!cost) return "";
    if (cost.input === 0 && cost.output === 0) return "Free";
    return `$${cost.input}/${cost.output} per 1M tokens`;
  }

  function formatLimit(val?: bigint | number | string) {
    if (!val) return "";
    const n = Number(val);
    if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
    if (n >= 1_000) return `${(n / 1_000).toFixed(0)}K`;
    return String(n);
  }

  function handleKeyDown(e: React.KeyboardEvent) {
    if (!showDropdown) {
      if (e.key === "ArrowDown" || e.key === "Enter") {
        setShowDropdown(true);
        e.preventDefault();
      }
      return;
    }
    switch (e.key) {
      case "ArrowDown":
        e.preventDefault();
        setFocusedIdx((i) => Math.min(i + 1, filtered.length - 1));
        break;
      case "ArrowUp":
        e.preventDefault();
        setFocusedIdx((i) => Math.max(i - 1, 0));
        break;
      case "Enter":
        e.preventDefault();
        if (filtered[focusedIdx]) selectModel(filtered[focusedIdx]);
        break;
      case "Escape":
        setShowDropdown(false);
        break;
    }
  }

  if (infoModel) {
    return (
      <div className="space-y-2">
        <div className="flex items-center gap-2">
          <span className="text-sm font-medium">Selected model:</span>
          <span className="text-sm text-muted-foreground">{value}</span>
          <Button variant="outline" size="sm" onClick={() => setInfoModel(null)}>
            Change
          </Button>
        </div>
        <ModelInfoCard model={infoModel} onClose={() => setInfoModel(null)} />
      </div>
    );
  }

  return (
    <div className="relative">
      {/* Stored-ref review banner (D5): flagged for review, never blank/hidden. */}
      {storedRefFlagged && (
        <div
          role="alert"
          className="mb-2 rounded-lg border border-amber-300 bg-amber-50 p-3 text-xs text-amber-800"
        >
          <div className="flex items-center justify-between gap-2">
            <div className="min-w-0">
              <span className="font-medium">Stored model ref flagged for review:</span>{" "}
              <span className="font-mono">{value}</span>
              <ul className="mt-1 list-inside list-disc">
                {reviewReasons.map((r) => (
                  <li key={r}>{r}</li>
                ))}
              </ul>
              <p className="mt-1 text-amber-700">
                Re-select below to fix; catalog-known refs re-save unchanged. Unknown-adapter or
                unknown-provider refs must be re-selected before saving.
              </p>
            </div>
          </div>
        </div>
      )}

      {/* ALL THREE TIERS LIVE INSIDE ONE BOX (operator UX directive): the
          closed state shows the selected ref; clicking opens the panel with
          search → adapter pills → provider pills → model list. The outside-
          click effect closes it (inputRef + dropdownRef cover both). */}
      {/* Closed state: the selected ref as a clickable trigger (no auto-open
          anywhere — QA round 3: an auto-open effect fought the user on the
          settings Defaults tab, flash-opening the panel over the review
          banner on every mount). When nothing is selected the INPUT is the
          closed state: always enabled, clicking/focusing opens the panel —
          no dead end (the QA round 3 orchicon trap is covered by the input
          being clickable, not by forcing the panel open). */}
      {selectedModel && !showDropdown ? (
        <div
          className="flex w-full cursor-pointer flex-wrap items-center gap-2 rounded-md border px-2.5 py-1.5 hover:bg-muted/50"
          onClick={() => setShowDropdown(true)}
        >
          <span className="min-w-0 flex-1 truncate text-sm font-mono">{selectedModel.modelRef}</span>
          <Button
            variant="ghost"
            size="sm"
            className="text-xs"
            onClick={(e) => {
              e.stopPropagation();
              setInfoModel(selectedModel);
            }}
          >
            Info
          </Button>
        </div>
      ) : (
        <div ref={dropdownRef} className="relative">
          <Input
            ref={inputRef}
            placeholder={
              useNativeSourcing && !provider
                ? "Pick a provider below, then search models"
                : "Search models..."
            }
            value={search}
            onChange={(e) => {
              setSearch(e.target.value);
              setShowDropdown(true);
            }}
            onFocus={() => setShowDropdown(true)}
            onKeyDown={handleKeyDown}
          />
          {showDropdown && (
            <div
              className="absolute z-[100] mt-1 w-full rounded-xl glass-menu shadow-xl"
              style={{
                maxHeight: "420px",
                overflow: "hidden",
                display: "flex",
                flexDirection: "column",
              }}
            >
              {/* Tier 1 — adapter pills (registered kinds, auto from Dispatcher). */}
              <div className="flex flex-wrap items-center gap-1.5 border-b px-3 py-2">
                <span className="text-[10px] font-medium uppercase tracking-wide text-muted-foreground">
                  Adapter
                </span>
                {adapterList.map((kind) => {
                  const active = kind === adapter;
                  return (
                    <button
                      key={kind}
                      type="button"
                      className={`rounded-full border px-3 py-1 text-xs font-medium transition-colors ${
                        active
                          ? "border-primary bg-primary text-primary-foreground"
                          : "border-border bg-muted text-foreground hover:bg-muted/80"
                      }`}
                      onClick={() => selectAdapter(kind)}
                    >
                      {kind}
                    </button>
                  );
                })}
                {kindsError && (
                  <span className="text-xs text-muted-foreground" title={`${String(kindsError)}`}>
                    (kinds unavailable — showing default)
                  </span>
                )}
              </div>

              {/* Tier 2 — provider pills. Orchicon: the provider RESOLVES the
                  model list (gate applies). Legacy CLI adapters: OPTIONAL
                  FILTERS from the live CLI discovery with an All reset. */}
              <div className="flex flex-wrap items-center gap-1.5 border-b px-3 py-2">
                <span className="text-[10px] font-medium uppercase tracking-wide text-muted-foreground">
                  Provider
                </span>
                {providersLoading && (
                  <span className="text-xs text-muted-foreground">Loading providers...</span>
                )}
                {providersError && (
                  <span className="text-xs text-destructive">
                    Failed to load providers: {String(providersError)}
                  </span>
                )}
                {!providersLoading && !providersError && providers && (
                  <>
                    {providers.length === 0 && (
                      <span className="text-xs text-muted-foreground">
                        No providers for adapter “{adapter}”
                      </span>
                    )}
                    {providers.map((p: { id: string; name: string; custom: boolean }) => {
                      const active = p.id === provider;
                      return (
                        <div key={p.id} className="flex items-center gap-1">
                          <button
                            type="button"
                            className={`rounded-md border px-2.5 py-1 text-xs font-medium transition-colors ${
                              active
                                ? "border-primary bg-primary text-primary-foreground"
                                : "border-border bg-muted text-foreground hover:bg-muted/80"
                            }`}
                            onClick={() => selectProvider(p.id)}
                          >
                            {p.name || p.id}
                          </button>
                          {p.custom && (
                            <span
                              className="inline-flex items-center rounded bg-purple-100 px-1 py-0.5 text-[10px] font-medium text-purple-700"
                              title="Manage in Settings → Adapters"
                            >
                              custom
                            </span>
                          )}
                        </div>
                      );
                    })}
                    {customProvider && (
                      <span className="self-center text-[10px] text-muted-foreground">
                        Manage custom providers in Settings → Adapters
                      </span>
                    )}
                  </>
                )}
              </div>

              {/* Tier 3 — searchable model list (provider-scoped under
                  orchicon; flat + optional pill filter under legacy). */}
              <div className="overflow-y-auto" style={{ maxHeight: "320px" }}>
                {modelsLoading && (
                  <p className="p-4 text-xs text-muted-foreground text-center">Loading models...</p>
                )}
                {modelsError && (
                  <p className="p-4 text-xs text-destructive text-center">
                    Failed to load models: {String(modelsError)}
                  </p>
                )}
                {!modelsLoading && !modelsError && filtered.length === 0 && (
                  <p className="p-4 text-xs text-muted-foreground text-center">
                    No models match your search
                  </p>
                )}
                {!modelsLoading &&
                  !modelsError &&
                  filtered.map((model, idx) => (
                    <button
                      key={model.modelRef || model.id}
                      type="button"
                      className={`w-full px-3 py-2 text-left text-sm hover:bg-accent flex items-center justify-between gap-2 ${
                        idx === focusedIdx ? "bg-accent" : ""
                      } ${parsed && catalogModelMatches(parsed, model) ? "bg-primary/10" : ""}`}
                      onMouseEnter={() => setFocusedIdx(idx)}
                      onClick={() => selectModel(model)}
                      onDoubleClick={() => {
                        selectModel(model);
                        setInfoModel(model);
                      }}
                    >
                      <div className="min-w-0 flex-1">
                        <div className="font-medium truncate">{model.name}</div>
                        <div className="text-xs text-muted-foreground truncate">
                          <span className="font-mono">{model.providerId}</span> /{" "}
                          <span className="font-mono">{model.id}</span>
                        </div>
                        {missingHints(model) && (
                          <div className="mt-0.5 text-[10px] text-amber-600">
                            no context hint — compaction math may misbehave
                          </div>
                        )}
                      </div>
                      <div className="text-right shrink-0">
                        <div className="text-xs font-mono">{formatCost(model.cost)}</div>
                        <div className="text-xs text-muted-foreground">
                          {model.limits ? `${formatLimit(model.limits.context)} ctx` : ""}
                        </div>
                      </div>
                    </button>
                  ))}
              </div>
            </div>
          )}
        </div>
      )}
    </div>
  );
}

function ModelInfoCard({ model, onClose }: { model: OpenCodeModel; onClose: () => void }) {
  return (
    <Card className="border-primary/20">
      <CardHeader className="pb-2 flex flex-row items-center justify-between">
        <div>
          <CardTitle className="text-base">{model.name}</CardTitle>
          <p className="text-xs text-muted-foreground font-mono">{model.modelRef}</p>
        </div>
        <Button variant="ghost" size="sm" onClick={onClose}>
          Close
        </Button>
      </CardHeader>
      <CardContent className="space-y-3 text-sm">
        <div className="grid grid-cols-2 gap-3">
          <div className="space-y-1">
            <span className="text-xs font-medium text-muted-foreground">Cost per 1M tokens</span>
            {model.cost ? (
              <div className="text-xs space-y-0.5">
                <div className="flex justify-between">
                  <span>Input</span>
                  <span className="font-mono">${model.cost.input}</span>
                </div>
                <div className="flex justify-between">
                  <span>Output</span>
                  <span className="font-mono">${model.cost.output}</span>
                </div>
                {(model.cost.cacheRead > 0 || model.cost.cacheWrite > 0) && (
                  <>
                    <div className="flex justify-between text-muted-foreground">
                      <span>Cache read</span>
                      <span className="font-mono">${model.cost.cacheRead}</span>
                    </div>
                    <div className="flex justify-between text-muted-foreground">
                      <span>Cache write</span>
                      <span className="font-mono">${model.cost.cacheWrite}</span>
                    </div>
                  </>
                )}
              </div>
            ) : (
              <span className="text-xs text-muted-foreground">N/A</span>
            )}
          </div>

          <div className="space-y-1">
            <span className="text-xs font-medium text-muted-foreground">Token limits</span>
            {model.limits ? (
              <div className="text-xs space-y-0.5">
                <div className="flex justify-between">
                  <span>Context</span>
                  <span className="font-mono">{Number(model.limits.context).toLocaleString()}</span>
                </div>
                <div className="flex justify-between">
                  <span>Max input</span>
                  <span className="font-mono">
                    {Number(model.limits.input || 0).toLocaleString() || "N/A"}
                  </span>
                </div>
                <div className="flex justify-between">
                  <span>Max output</span>
                  <span className="font-mono">{Number(model.limits.output).toLocaleString()}</span>
                </div>
              </div>
            ) : (
              <span className="text-xs text-muted-foreground">N/A</span>
            )}
          </div>
        </div>

        {model.capabilities && (
          <div>
            <span className="text-xs font-medium text-muted-foreground">Capabilities</span>
            <div className="flex flex-wrap gap-1 mt-1">
              {model.capabilities.reasoning && <CapBadge label="Reasoning" />}
              {model.capabilities.temperature && <CapBadge label="Temperature" />}
              {model.capabilities.toolcall && <CapBadge label="Tool calls" />}
              {model.capabilities.attachment && <CapBadge label="Attachments" />}
              {model.capabilities.inputImage && <CapBadge label="Image input" />}
              {model.capabilities.inputPdf && <CapBadge label="PDF input" />}
              {model.capabilities.inputAudio && <CapBadge label="Audio input" />}
              {model.capabilities.interleaved && <CapBadge label="Stream reasoning" />}
            </div>
          </div>
        )}

        {model.variants.length > 0 && (
          <div>
            <span className="text-xs font-medium text-muted-foreground">Reasoning effort variants</span>
            <div className="flex flex-wrap gap-1 mt-1">
              {model.variants.map((v) => (
                <span key={v} className="inline-block rounded bg-muted px-1.5 py-0.5 text-xs font-mono">
                  {v}
                </span>
              ))}
            </div>
          </div>
        )}

        {model.releaseDate && (
          <p className="text-xs text-muted-foreground">Released: {model.releaseDate}</p>
        )}
      </CardContent>
    </Card>
  );
}

function CapBadge({ label }: { label: string }) {
  return (
    <span className="inline-block rounded bg-primary/10 px-1.5 py-0.5 text-xs text-primary">
      {label}
    </span>
  );
}
