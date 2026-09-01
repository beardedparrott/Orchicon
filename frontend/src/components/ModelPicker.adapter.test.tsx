import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";

describe("ModelPicker (per-worker adapter selection, ADR-0005)", () => {
  const src = fs.readFileSync(path.join(__dirname, "ModelPicker.tsx"), "utf8");

  it("seeds the orchicon default for fresh selections when registered (D5)", () => {
    expect(src).toContain("ORCHICON_ADAPTER_KIND");
    expect(src).toContain("adapterKinds.includes(ORCHICON_ADAPTER_KIND)");
    // Only when the stored ref is EMPTY — a stored selection is never
    // repointed by the seeding effect.
    expect(src).toMatch(/value\.trim\(\) !== ""\) return;/);
    // And only once the registered kinds have actually loaded.
    expect(src).toMatch(/if \(!adapterKinds\) return;/);
  });

  it("keeps the legacy default as the degradation path (D5)", () => {
    expect(src).toMatch(
      /setAdapter\(adapterKinds\.includes\(ORCHICON_ADAPTER_KIND\) \? ORCHICON_ADAPTER_KIND : DEFAULT_ADAPTER_KIND\)/,
    );
  });

  it("pins the adapter-change reset contract (D4): switch resets provider+model, no ref written until re-selection", () => {
    expect(src).toContain("ADR-0005 D4 reset contract");
    expect(src).toContain("function selectAdapter");
    expect(src).toMatch(/setAdapter\(kind\);\s*setProvider\(""\);\s*setSearch\(""\);/);
    // selectAdapter never writes a ref — no onChange call inside it.
    const fn = src.slice(src.indexOf("function selectAdapter"), src.indexOf("function selectProvider"));
    expect(fn).not.toContain("onChange(");
  });

  it("suppresses stale-selection flags while browsing another adapter (D4 reset, not reject)", () => {
    expect(src).toContain("const adapterDiverged");
    expect(src).toMatch(/!adapterDiverged &&/);
  });
});