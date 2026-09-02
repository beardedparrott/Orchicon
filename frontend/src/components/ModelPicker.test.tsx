import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";

describe("ModelPicker (three-tier, ADR-0004)", () => {
  const src = fs.readFileSync(path.join(__dirname, "ModelPicker.tsx"), "utf8");

  it("renders three tiers: adapter bubbles, provider list, searchable model list", () => {
    // ALL THREE TIERS LIVE INSIDE ONE BOX (operator UX directive): the
    // closed state shows the selected ref; the open panel contains the
    // search input, adapter pills, provider pills, and the model list.
    expect(src).toContain("ALL THREE TIERS LIVE INSIDE ONE BOX");
    expect(src).toMatch(/rounded-full border px-3 py-1/); // adapter pills (in-box)
    expect(src).toContain("Search models...");
    // Tier 2 — provider pills from the tenant-aware providers service
    // (ADR-0006: built-ins ⊕ enabled tenant customs, auto-refresh on save).
    expect(src).toContain("useProviderList()");
    expect(src).toMatch(/No providers for adapter/);
    // Tier 3 — searchable model list scoped to the selected provider.
    // Per-adapter data source (ADR-0004): native → providers service,
    // legacy CLI adapters → opencode-CLI discovery.
    expect(src).toContain("useProviderModelsForPicker");
    expect(src).toContain("useListOpenCodeModels(");
  });

  it("scopes provider list by adapter and model list by provider (stale-selection guard)", () => {
    expect(src).toContain("selectAdapter");
    expect(src).toMatch(/setProvider\(""\)/);
    expect(src).toContain("selectProvider");
    expect(src).toContain("useListOpenCodeModels(");
    expect(src).toMatch(/Search models\.\.\./);
    expect(src).toMatch(/Select a provider first/);
  });

  it("scopes the merged provider tier to the selected adapter (ProviderRegistry semantics)", () => {
    // The merged providers list is tenant-wide; the tier filters it to the
    // selected adapter's provider set (legacy kinds: pills derived from the
    // live CLI discovery's distinct providerID values with an All reset —
    // optional filters, never a gate; orchicon = the merged providers
    // service view, which gates its model tier). Regression guard: the
    // legacy tier must never render a static hardcoded set — it must
    // auto-pull from the same discovery the model tier uses.
    expect(src).toContain("cliModelsQ");
    expect(src).toContain("All");
    expect(src).toContain("adapter === ORCHICON_ADAPTER_KIND");
  });

  it("marks custom providers with a badge and manage affordance", () => {
    expect(src).toContain("p.custom");
    expect(src).toMatch(/custom/);
    expect(src).toContain("Manage custom providers in Settings → Adapters");
    expect(src).toContain("Manage in Settings → Adapters");
  });

  it("annotates models missing the context hint but keeps them selectable", () => {
    expect(src).toContain("missingHints");
    // Context is the compaction-critical hint; output-max is not
    // warning-worthy (live /models listings rarely carry it).
    expect(src).toMatch(/!model\.limits \|\| !model\.limits\.context/);
    expect(src).not.toMatch(/!model\.limits\.output/);
  });

  it("matches catalog models by parsed segments, not raw value (QA BUG-1)", () => {
    expect(src).toContain("catalogModelMatches(parsed, m)");
    expect(src).toContain("catalogModelMatches(parsed, model)");
    // The raw-value comparisons that false-flagged every 3-segment ref are gone.
    expect(src).not.toContain("m.modelRef === value");
    expect(src).not.toContain("m.id === value");
  });

  it("flags stored refs only from loaded tier data — no flash-of-banner, no RPC-failure false flags", () => {
    expect(src).toContain("const storedAdapterKnown");
    expect(src).toContain("const storedProviderKnown");
    expect(src).toMatch(/adapterKinds !== undefined && !storedAdapterKnown/);
    expect(src).toMatch(/providers !== undefined && !storedProviderKnown/);
    expect(src).toMatch(/models !== undefined &&/);
  });

  it("suppresses provider/model flags while browsing a different adapter tier", () => {
    expect(src).toContain("const adapterDiverged");
    expect(src).toMatch(/!adapterDiverged &&/);
  });

  it("writes normalized 3-segment refs via formatModelRef", () => {
    expect(src).toContain("formatModelRef(adapter, provider, model.id)");
  });

  it("flags unknown stored refs for review — never blank or hidden (D5)", () => {
    expect(src).toContain('role="alert"');
    expect(src).toContain("Stored model ref flagged for review:");
    expect(src).toContain("adapter not registered");
    expect(src).toContain("provider not found (deleted or unknown)");
    expect(src).toMatch(/re-select/i);
  });

  it("degrades to the default adapter kind when kinds are unavailable", () => {
    expect(src).toContain("DEFAULT_ADAPTER_KIND");
    expect(src).toMatch(
      /adapterKinds && adapterKinds\.length > 0 \? adapterKinds : \[DEFAULT_ADAPTER_KIND\]/,
    );
  });

  it("preserves the {value, onChange} props contract", () => {
    expect(src).toMatch(
      /interface ModelPickerProps \{\s*value: string;\s*onChange: \(value: string\) => void;\s*\}/,
    );
    expect(src).not.toContain("adapter?: string");
  });

  it("handles legacy 2-segment refs via parseModelRef (inferred opencode)", () => {
    expect(src).toContain("parseModelRef");
    expect(src).toContain("parsed?.adapter ?? DEFAULT_ADAPTER_KIND");
  });
});
