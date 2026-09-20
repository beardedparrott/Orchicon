import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";

// mode-toggle-source.test.ts — THE WIRING, WHICH THE UNIT TESTS CANNOT SEE.
//
// conversationModes.test.ts pins the VOCABULARY (which modes exist, what they are called). It cannot pin whether
// a component RENDERS it — the route and the composer cannot be rendered in this setup, so a component that
// hardcoded a single-element list again would leave the vocabulary tests green while the dropdown regressed to
// exactly the bug the operator reported.
//
// So these read the components as text, the idiom BuildLogViewer.test.tsx already uses here (and ask-metrics
// makes the same split: pure logic in lib/, thin components on top). The assertions are deliberately about the
// SHAPE that broke — a local option list — rather than about exact formatting, so a refactor that keeps the
// derivation will not trip them.

describe("ModeToggle", () => {
  const src = fs.readFileSync(path.join(__dirname, "mode-toggle.tsx"), "utf8");

  it("takes its options from the shared, enum-derived list", () => {
    expect(src).toContain("CONVERSATION_MODES");
    expect(src).toContain("@/lib/conversationModes");
  });

  it("does not carry a local option list any more", () => {
    // THE REGRESSION. The bug was `const options = [{ value: ConversationMode.BRAINSTORM, ... }] as const;` —
    // one mode, hardcoded, in this file. A local `options` array is what to catch.
    expect(src).not.toContain("const options = [");
    expect(src).not.toContain("as const;");
    // And no icon map of its own: icons come from the shared ModeIcon component, so a second map (the other
    // half of the original hardcoded option) cannot drift from this one.
    expect(src).not.toContain("icon: Brain");
    expect(src).toContain('from "@/components/ui/ModeIcon"');
  });

  it("announces the chosen mode instead of a literal", () => {
    // The screen-reader announcement hardcoded "Brainstorm", so a switch to Iteration announced the wrong mode.
    expect(src).toContain("conversationModeLabel(next)");
    expect(src).not.toContain('const label = "Brainstorm"');
  });
});

describe("the conversation header's mode pill", () => {
  const src = fs.readFileSync(
    path.join(__dirname, "..", "..", "routes", "ask-orchicon.tsx"),
    "utf8",
  );

  it("names the conversation's real mode", () => {
    // The pill rendered the literal word "Brainstorm" on every conversation, whatever its mode.
    expect(src).toContain("conversationModeLabel(localMode)");
    expect(src).toContain("<ModeIcon mode={localMode}");
  });

  it("no longer draws a bare Brainstorm label with a hand-picked icon", () => {
    expect(src).not.toContain('<Brain aria-hidden="true"');
    expect(src).not.toContain("  Brain,\n");
  });
});
