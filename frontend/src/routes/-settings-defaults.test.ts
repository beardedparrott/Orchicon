import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";

// The Settings → Defaults tab binds BOTH tenant defaults (worker + Ask
// Orchicon) to the shared three-tier adapter/provider/searchable-model
// ModelPicker bound to DefaultAskOrchiconModel — reuse, never a fork
// (ADR-0003). These source-level pins guard against a future regression
// swapping the Ask picker for a text input or a local fork.
describe("settings-defaults (Ask default uses the shared ModelPicker)", () => {
  const src = fs.readFileSync(path.join(__dirname, "settings.tsx"), "utf8");

  it("imports the shared ModelPicker exactly once (no local fork)", () => {
    // Exactly one import of the shared picker component.
    const importCount = (src.match(/from\s+"@\/components\/ModelPicker"/g) || []).length;
    expect(importCount).toBe(1);
    // No model-selection component is defined inline in settings.tsx.
    expect(src).not.toMatch(/\bfunction\s+Model\b/);
    expect(src).not.toMatch(/const\s+\w*Model\w*\s*=\s*\([^)]*\)\s*=>/);
  });

  it("binds the Ask default to the shared picker against draftAskOrchiconModel", () => {
    // The Ask block renders <ModelPicker value={draftAskOrchiconModel}
    // onChange={setDraftAskOrchiconModel} askMode /> — the shared three-tier
    // picker in Ask mode (the Ask-capability guard surfaces at selection).
    expect(src).toMatch(
      /<ModelPicker\s+value=\{draftAskOrchiconModel\}\s+onChange=\{setDraftAskOrchiconModel\}\s+askMode\s*\/>/,
    );
  });

  it("labels and persists the Ask default", () => {
    expect(src).toMatch(/Default Ask Orchicon model/);
    // State is initialised from the tenant default and saved back as
    // defaultAskOrchiconModel (persist as a 3-seg ref).
    expect(src).toMatch(/defaultAskOrchiconModel:\s*draftAskOrchiconModel/);
    expect(src).toMatch(/setDraftAskOrchiconModel\(settings\.defaultAskOrchiconModel/);
  });
});
