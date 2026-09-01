import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";

describe("ModelPicker (three-tier, ADR-0004)", () => {
  const src = fs.readFileSync(path.join(__dirname, "ModelPicker.tsx"), "utf8");

  it("renders three tiers: adapter bubbles, provider list, searchable model list", () => {
    // Tier 1 — adapter bubble list from registered kinds.
    expect(src).toContain("useListAdapterKinds");
    expect(src).toMatch(/rounded-full border px-3 py-1/);
    // Tier 2 — provider list scoped to the selected adapter.
    expect(src).toContain("useListProviders(adapter)");
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

  it("marks custom providers with a badge and manage affordance", () => {
    expect(src).toContain("p.custom");
    expect(src).toMatch(/custom/);
    expect(src).toContain("Manage custom providers in Settings → Adapters");
    expect(src).toContain("Manage in Settings → Adapters");
  });

  it("annotates models missing context/output hints but keeps them selectable", () => {
    expect(src).toContain("missingHints");
    expect(src).toContain("no context/output hint — may misbehave in compaction");
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
