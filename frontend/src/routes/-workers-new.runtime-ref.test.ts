import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";

describe("workers/new (runtime_ref derivation, ADR-0005 D6)", () => {
  const src = fs.readFileSync(path.join(__dirname, "workers_.new.tsx"), "utf8");

  it("derives runtime_ref from the picker's chosen adapter when registered", () => {
    expect(src).toContain("useListAdapterKinds");
    expect(src).toContain("parseModelRef(modelRef)");
    expect(src).toContain("kinds.includes(parsed.adapter)");
    expect(src).toMatch(/setValue\("runtimeRef", next/);
  });

  it("keeps opencode when the chosen kind is not registered", () => {
    expect(src).toMatch(/\? parsed\.adapter : "opencode"/);
  });

  it("no longer hardcodes a runtime_ref that ignores the picker's selection", () => {
    // The form still defaults the FIELD to opencode (the only dispatchable
    // kind today), but the picker's selection now governs it via the effect.
    expect(src).toContain('runtimeRef: "opencode"');
    expect(src).toContain("ADR-0005 D6");
  });
});