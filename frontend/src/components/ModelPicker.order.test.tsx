import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";

// AC 5: the orchicon adapter must be FIRST in the rendered list of EVERY
// model selector (the shared ModelPicker covers Ask Orchicon's chip,
// Settings → Defaults, worker create, the worker version editor, and the
// bulk-change dialog) as well as being the fresh-selection default.
//
// The TUI implements both via kit2.ModelPicker.PreferredAdapter; the GUI
// implemented only the default while rendering the server's
// sort.Strings-alphabetical order (claude, opencode, orchicon) — which put
// orchicon LAST while being selected. The fix is a single conditional hoist
// in this shared component, so these assertions live here.
describe("ModelPicker adapter order (AC 5 — orchicon first, everywhere)", () => {
  const src = fs.readFileSync(path.join(__dirname, "ModelPicker.tsx"), "utf8");

  it("hoists the orchicon kind to the head of the rendered adapter list", () => {
    const decl = src.slice(src.indexOf("const adapterList = useMemo"), src.indexOf("// Catalog match is by PARSED SEGMENTS"));
    expect(decl).toContain("ORCHICON_ADAPTER_KIND");
    // Hoist ONLY when the kind is actually in the fetched kinds — a plane
    // that does not register it must never be offered a default that cannot
    // dispatch (the same rule the seeding effect follows).
    expect(decl).toMatch(/if \(!adapterKinds\.includes\(ORCHICON_ADAPTER_KIND\)\) return adapterKinds;/);
    expect(decl).toMatch(/return \[ORCHICON_ADAPTER_KIND, \.\.\.adapterKinds\.filter\(\(k\) => k !== ORCHICON_ADAPTER_KIND\)\];/);
  });

  it("preserves the rest of the server order and the fallback", () => {
    const decl = src.slice(src.indexOf("const adapterList = useMemo"), src.indexOf("// Catalog match is by PARSED SEGMENTS"));
    // No re-sorting of the remaining kinds.
    expect(decl).not.toContain("sort()");
    // The kinds-fetch-failed fallback survives intact.
    expect(decl).toMatch(/if \(!adapterKinds \|\| adapterKinds\.length === 0\) return \[DEFAULT_ADAPTER_KIND\];/);
  });

  it("keeps the Tier 1 render un-reordered (one hoist, not a call-site fix)", () => {
    expect(src).toContain("{adapterList.map((kind) => {");
    expect(src).not.toContain("adapterKinds.map((kind)");
  });
});
