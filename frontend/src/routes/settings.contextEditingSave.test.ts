import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";

// THE BUG THIS PINS. The two context-editing knobs are typed tenant settings. The Defaults tab SEEDED its
// drafts from `settings.maxToolOutputBytes` / `settings.contextEditingKeepToolCalls`, but handleSave never put
// them in the UpdateSettings payload. The server reads an omitted optional field as "leave unchanged", so every
// Save succeeded with no error and stored nothing.
//
// The check is scoped to the PAYLOAD of handleSave, not the whole file: a field that is seeded and rendered but
// absent from the save is exactly the failure, and a whole-file grep would pass on the seeding alone.
describe("Defaults tab saves the typed context-editing settings", () => {
  const src = fs.readFileSync(path.join(__dirname, "settings.tsx"), "utf8");
  const start = src.indexOf("async function handleSave()");
  const payloadStart = src.indexOf("updateSettings.mutateAsync({", start);
  const payloadEnd = src.indexOf("});", payloadStart);
  const payload = src.slice(payloadStart, payloadEnd);

  it("finds the Defaults tab save payload", () => {
    expect(start).toBeGreaterThan(0);
    expect(payloadStart).toBeGreaterThan(start);
    expect(payload).toContain("defaultWorkerModel");
  });

  it("sends maxToolOutputBytes from its draft, blank = undefined (leave as stored)", () => {
    expect(payload).toMatch(/maxToolOutputBytes:\s*\n?\s*draftMaxToolOutputBytes\.trim\(\) === ""\s*\n?\s*\? undefined/);
  });

  it("sends contextEditingKeepToolCalls from its draft, blank = undefined (leave as stored)", () => {
    expect(payload).toMatch(/contextEditingKeepToolCalls:\s*\n?\s*draftKeepToolCalls\.trim\(\) === "" \? undefined/);
  });

  it("does not write the byte cap into the budget document any more", () => {
    expect(payload).toContain('"", // maxToolOutputBytes is a typed tenant setting now');
  });
});
