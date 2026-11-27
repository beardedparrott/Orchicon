import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";

import { ANTIGRAVITY_AUTH_METHODS, ANTIGRAVITY_API_KEY_SECRET, methodCaveat } from "@/components/AntigravityAdapterCard";

// AntigravityAdapterCard.test.tsx — Settings → Adapters shows the antigravity auth-method selector and the
// ToS notice (child 8, AC4's GUI half).
//
// WHY THIS SUITE PINS THE CARD'S SOURCE AND ITS EXPORTED TABLES RATHER THAN RENDERING IT. This repo ships
// no jsdom (see ModelPicker.test.tsx, which pins that component the same way), so a render assertion is not
// available. What IS assertable — and what actually regresses — is the four things the card must show:
//
//   1. the AUTH METHOD, with the caveat that method carries (the same facts the TUI's
//      antigravityMethodSummary renders, so the two panes cannot disagree);
//   2. the ToS NOTICE, on exactly `oauth-personal`, and NEVER re-typed here — it comes from the server as
//      TenantSettings.antigravityTosNotice, which is authmethod.ToSNotice;
//   3. the TELEMETRY toggle's current state, and
//   4. the API-key secret as PRESENCE ONLY — there is no branch that could render a value.
describe("AntigravityAdapterCard — the operator's four questions", () => {
  const src = fs.readFileSync(path.join(__dirname, "AntigravityAdapterCard.tsx"), "utf8");

  it("offers all four auth methods agy accepts, in the server's own order", () => {
    expect(ANTIGRAVITY_AUTH_METHODS.map((m) => m.value)).toEqual([
      "oauth-personal",
      "oauth-business",
      "gemini-api-key",
      "agent-platform",
    ]);
    // The WIRE values, not friendly aliases: agy's own `authenticate {methodId}` strings.
    for (const m of ANTIGRAVITY_AUTH_METHODS) {
      expect(m.label).toContain(m.value);
    }
  });

  it("renders a SELECT with those four methods and the server's current value", () => {
    expect(src).toContain("<select");
    expect(src).toContain("ANTIGRAVITY_AUTH_METHODS.map");
    expect(src).toContain("settings?.antigravityAuthMethod");
    // An unset method reads as the server's default rather than as "no method".
    expect(src).toContain('settings?.antigravityAuthMethod || "oauth-personal"');
  });

  it("names the caveat each method carries, so a container operator can predict the refusal", () => {
    expect(methodCaveat("oauth-personal")).toContain("host keyring");
    expect(methodCaveat("oauth-personal")).toContain("container");
    expect(methodCaveat("gemini-api-key")).toContain(ANTIGRAVITY_API_KEY_SECRET);
    // The enterprise paths carry no caveat, which is the honest answer (not "unknown").
    expect(methodCaveat("oauth-business")).toBe("");
    expect(methodCaveat("agent-platform")).toBe("");
  });

  it("shows the ToS notice ONLY for oauth-personal, and takes the text from the SERVER", () => {
    // The gate: exactly the method whose terms it is about.
    expect(src).toContain('method === "oauth-personal" && tosNotice');
    // The text is the server's field — never a literal re-typed in the component.
    expect(src).toContain("settings?.antigravityTosNotice");
    expect(src).not.toMatch(/Google's Antigravity terms restrict/);
    // The card must not carry its own copy of the sentence anywhere.
    expect(src).not.toContain("antigravity.google/terms");
  });

  it("reports telemetry as its current state, not as an editable second switch", () => {
    expect(src).toContain("antigravityShareTelemetry");
    expect(src).toContain("--disable-telemetry");
  });

  it("reports the API-key secret as PRESENCE ONLY", () => {
    // Presence is computed from the secret NAMES, and the rendered row says set / not set.
    expect(src).toContain("useSecretList");
    expect(src).toContain(".some((s) => s.name === ANTIGRAVITY_API_KEY_SECRET)");
    expect(src).toContain("set");
    expect(src).toContain("not set");
    // There is no branch that could print a value: the secret metadata carries only id/name/
    // description, and the ONE `.value` in this file is the auth-method table's own field.
    expect(src).not.toMatch(/\bs\.(value|secret|data)\b/);
    expect(src).not.toMatch(/secret\.(value|secret|data)/);
    // `s.name` is the only field read off a secret — that is presence, by construction.
    const secretReads = [...src.matchAll(/\bs\.(\w+)/g)].map((m) => m[1]);
    expect(new Set(secretReads)).toEqual(new Set(["name"]));
  });

  it("says agy is not installed rather than showing a fabricated path", () => {
    // The install sentence is the shared constant, and `installed === false` is what selects it. An
    // ABSENT probe result (undefined) must not read as "not installed".
    expect(src).toContain("ANTIGRAVITY_NOT_INSTALLED_MESSAGE");
    expect(src).toContain("installed === false");
  });
});

// The install sentence the card renders is the SERVER's, byte for byte. This compares the TS constant
// against the Go source, so a reworded Go message fails here instead of shipping two different sentences.
describe("the install sentence matches the Go constant", () => {
  it("is byte-identical to adapter.AntigravityNotInstalledMessage", async () => {
    const { ANTIGRAVITY_NOT_INSTALLED_MESSAGE } = await import("@/lib/adapter-errors");
    const goSrc = fs.readFileSync(
      path.join(process.cwd(), "../internal/adapter/antigravity_auth.go"),
      "utf8",
    );
    // The Go constant is a concatenation of string literals; join them the way Go does.
    const arm = goSrc.slice(
      goSrc.indexOf("const AntigravityNotInstalledMessage"),
      goSrc.indexOf("// antigravityAuthSignals"),
    );
    const parts = [...arm.matchAll(/"((?:[^"\\]|\\.)*)"/g)].map((m) => m[1].replace(/\\"/g, '"'));
    expect(parts.length).toBeGreaterThan(0);
    expect(ANTIGRAVITY_NOT_INSTALLED_MESSAGE).toBe(parts.join(""));
  });
});
