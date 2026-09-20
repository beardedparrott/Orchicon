import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";

// Worker-level runtime_ref is RETIRED (ADR-0003 single source of truth):
// the model_ref's adapter segment alone governs dispatch. These tests pin
// the retirement on the create-worker form — no runtimeRef field, no
// derivation effect, and no runtimeRef in the submitted payload. This
// supersedes the former ADR-0005 D6 derivation tests (which pinned the
// now-removed useEffect that copied the picker's adapter into runtimeRef).

describe("workers/new (runtime_ref retired, ADR-0003 single source of truth)", () => {
  const src = fs.readFileSync(path.join(__dirname, "workers_.new.tsx"), "utf8");

  it("no longer sends runtime_ref in the create payload", () => {
    expect(src).not.toMatch(/runtimeRef:\s*values\.runtimeRef/);
    expect(src).not.toContain("runtimeRef");
  });

  it("no longer derives runtime_ref from the picker's selection", () => {
    // The ADR-0005 D6 useEffect (useListAdapterKinds + parseModelRef +
    // setValue("runtimeRef", …)) is gone — the model_ref's adapter segment
    // governs dispatch server-side.
    expect(src).not.toContain("useListAdapterKinds");
    expect(src).not.toContain("parseModelRef(modelRef)");
    expect(src).not.toContain('setValue("runtimeRef"');
  });

  it("documents the retirement where the field used to live", () => {
    expect(src).toContain("runtime_ref is retired");
    expect(src).toContain("ADR-0003");
  });

  it("still requires a model_ref (the single source of dispatch truth)", () => {
    expect(src).toContain('modelRef: z\n    .string()\n    .min(1, "Model ref is required")');
  });
});