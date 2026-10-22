import { describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

/**
 * -ask-composer-model-chip.test.ts — THE MODEL CONTROL MUST NOT BE GATED ON A MODEL EXISTING.
 *
 * The operator: "If no default ask model is set, it doesn't show a picker in the conversation. That
 * is the wrong approach. We should definitely let people still choose from the conversation if no
 * default is set."
 *
 * THE DEFECT WAS ONE CONDITION:
 *
 *     {modelRef && <AskModelChip model={modelRef} onModelChange={onModelChange} />}
 *
 * `modelRef` is the conversation's ref, then the tenant default, then "" — so with no default
 * configured the chip was not rendered, and since the chip IS the picker, the conversation had no
 * way to choose a model at all. The comment directly above it already said the chip "is the CONTROL
 * and must survive"; the gate contradicted the comment, which is why this is asserted as a property
 * of the source rather than trusted to review.
 *
 * A RENDERING test would be better but cannot reach it: the composer needs a full tRPC/query/stream
 * context, and the component is not exported for isolated mount. So this reads the source and
 * asserts the gate is gone — the same technique ModelPicker.adapter.test.tsx uses for its
 * ordering rule.
 */
const SRC = readFileSync(resolve(__dirname, "ask-orchicon.tsx"), "utf8");

describe("Ask composer's model control", () => {
  it("renders the chip unconditionally (no `modelRef &&` guard)", () => {
    expect(
      SRC,
      "the composer gates <AskModelChip> on a non-empty modelRef again. That removes the ONLY way " +
        "to choose a model from a conversation when no default Ask model is configured — the " +
        "operator's reported issue.",
    ).not.toMatch(/\{\s*modelRef\s*&&\s*\(?\s*<AskModelChip/);
  });

  it("still renders it somewhere, so 'unconditional' is not achieved by deleting it", () => {
    expect(SRC).toMatch(/<AskModelChip\b/);
  });

  it("passes the handler on both composer instances (hero and open conversation)", () => {
    // Without onModelChange the chip degrades to display-only text, which is a different way to
    // lose the picker: it would render and look inert.
    const chipUses = SRC.match(/<AskModelChip\b[^>]*\/>/g) ?? [];
    expect(chipUses.length).toBeGreaterThan(0);
    for (const use of chipUses) {
      expect(use, `a <AskModelChip> is rendered without onModelChange: ${use}`).toContain(
        "onModelChange=",
      );
    }
  });
});
