import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";

describe("ModelPicker (three-tier, ADR-0004)", () => {
  const src = fs.readFileSync(path.join(__dirname, "ModelPicker.tsx"), "utf8");

  it("renders three tiers: adapter bubbles, provider list, searchable model list", () => {
    // Tier 1 — adapter bubble list from registered kinds.
    expect(src).toContain("useListAdapterKinds");
    expect(src).toMatch(/rounded-full border px-3 py-1/);
    // Tier 2 — provider list from the tenant-aware providers service
    // (ADR-0006: built-ins ⊕ enabled tenant customs, auto-refresh on save).
    expect(src).toContain("useProviderList()");
    expect(src).toMatch(/No providers for adapter/);
    // Tier 3 — searchable model list scoped to the selected provider.
    expect(src).toContain("useListOpenCodeModels(adapter, provider)");
    expect(src).toMatch(/Search models\.\.\./);
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
    // selected adapter's provider set (legacy kinds from the catalog mirror,
    // orchicon = full union). Regression guard: tier 2 must never render the
    // whole tenant union under a legacy adapter kind.
    expect(src).toContain("LEGACY_ADAPTER_PROVIDER_IDS");
    expect(src).toContain("scopedIds");
    expect(src).toContain("adapter === ORCHICON_ADAPTER_KIND");
  });

  it("marks custom providers with a badge and manage affordance", () => {
    expect(src).toContain("p.custom");
    expect(src).toMatch(/custom/);
    expect(src).toContain("Manage custom providers in Settings → Adapters");
    expect(src).toContain("Manage in Settings → Adapters");
  });

  it("annotates models missing context/output hints but keeps them selectable", () => {
    expect(src).toContain("missingHints");
    expect(src).toContain("no context/output hint — may misbehave in compaction");
    // Either hint missing counts (AC says context/output, not all-three).
    expect(src).toMatch(
      /!model\.limits \|\| !model\.limits\.context \|\| !model\.limits\.output/,
    );
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
