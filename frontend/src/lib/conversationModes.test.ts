import { describe, expect, it } from "vitest";

import { ConversationMode } from "@/api/gen/orchicon/api/v1/ask_orchicon_pb";
import {
  CONVERSATION_MODES,
  conversationModeLabel,
  conversationModeMeta,
} from "@/lib/conversationModes";

// The operator: "The mode drop down for ask orchicon in the gui does not include iteration or quick work mode."
//
// WHAT THESE CAN AND CANNOT SEE. They pin the VOCABULARY — which modes are offered and what they are called —
// and that is the half that was wrong. They cannot see whether a component actually renders this list, because
// the route cannot be rendered in this setup; that half is guarded by a source assertion in
// mode-toggle-source.test.ts, the same idiom BuildLogViewer.test.tsx uses.

describe("CONVERSATION_MODES", () => {
  it("offers every mode the operator can choose", () => {
    // THE REPORT ITSELF, as an assertion: all three, in the enum's order.
    expect(CONVERSATION_MODES.map((m) => m.label)).toEqual(["Brainstorm", "Iteration", "Quick Work"]);
  });

  it("does not offer UNSPECIFIED", () => {
    // It is the wire's "absent" value, not a mode; the server reads it as BRAINSTORM.
    expect(CONVERSATION_MODES.some((m) => m.value === ConversationMode.UNSPECIFIED)).toBe(false);
  });

  it("covers every non-UNSPECIFIED value the proto defines", () => {
    // THE DRIFT GUARD. The list is derived, so this holds by construction today — and that is the point: it
    // states the RULE so that replacing the derivation with a literal (which is how this broke) fails here.
    const selectable = Object.values(ConversationMode)
      .filter((v): v is ConversationMode => typeof v === "number" && v !== ConversationMode.UNSPECIFIED)
      .sort((a, b) => a - b);
    expect(CONVERSATION_MODES.map((m) => m.value)).toEqual(selectable);
  });

  it("describes each mode, so the dropdown has something to explain", () => {
    for (const m of CONVERSATION_MODES) {
      expect(m.blurb.length).toBeGreaterThan(0);
    }
  });
});

describe("conversationModeLabel", () => {
  it("names the three modes the way the operator sees them", () => {
    expect(conversationModeLabel(ConversationMode.BRAINSTORM)).toBe("Brainstorm");
    expect(conversationModeLabel(ConversationMode.ITERATION)).toBe("Iteration");
    expect(conversationModeLabel(ConversationMode.QUICK_WORK)).toBe("Quick Work");
  });

  it("falls back rather than blanking", () => {
    // UNSPECIFIED is not selectable, but it must still render as something — a blank mode pill would claim the
    // conversation has no mode, which is never true.
    expect(conversationModeLabel(ConversationMode.UNSPECIFIED)).toBe("UNSPECIFIED");
    // An unknown number (a mode from a newer server, or a hand-edited DB value) names itself as a number rather
    // than vanishing.
    expect(conversationModeLabel(99 as ConversationMode)).toBe("mode 99");
  });
});

describe("conversationModeMeta", () => {
  it("resolves a selectable mode and reports an unselectable one as absent", () => {
    expect(conversationModeMeta(ConversationMode.ITERATION)?.label).toBe("Iteration");
    expect(conversationModeMeta(ConversationMode.UNSPECIFIED)).toBeUndefined();
  });
});
