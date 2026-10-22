import { describe, expect, it } from "vitest";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";

import { AskModelChip } from "./AskModelChip";

/**
 * AskModelChip.empty.test.tsx — THE CONTROL MUST EXIST WHEN NO MODEL IS CONFIGURED.
 *
 * The operator: "If no default ask model is set, it doesn't show a picker in the conversation. That
 * is the wrong approach. We should definitely let people still choose from the conversation if no
 * default is set."
 *
 * They were right, and it was a regression I introduced. The composer rendered
 *
 *     {modelRef && <AskModelChip model={modelRef} onModelChange={onModelChange} />}
 *
 * so with no default Ask model configured `modelRef` was empty and the chip — WHICH IS THE PICKER —
 * was not rendered at all. The comment immediately above that gate already said the chip "is the
 * CONTROL and must survive"; the gate contradicted it, and an empty ref is the state that needs the
 * control MOST.
 *
 * These assertions are about the EMPTY value specifically, because that is the case that broke. A
 * non-empty ref was always fine.
 */

/** The chip with no model and a working handler: the reported state. */
const html = renderToStaticMarkup(
  createElement(AskModelChip, { model: "", onModelChange: () => {} }),
);

describe("AskModelChip with no model configured", () => {
  it("renders a clickable control, not nothing", () => {
    // The interactive chip (not the display-only fallback) is what opens the picker.
    expect(html).toContain('data-testid="ask-model-chip"');
    expect(html).not.toContain('data-testid="ask-model-chip-static"');
    expect(html).toContain("<button");
  });

  it("invites a choice rather than showing an empty box", () => {
    // An empty label reads as a broken control; the placeholder is the affordance.
    expect(html).toContain("choose model");
  });

  it("explains in its tooltip that no model is set and it can be changed", () => {
    // "No model set — change the model for the new conversation": the operator learns WHY it is
    // empty and that clicking is the fix.
    expect(html).toContain("No model set");
    expect(html).toContain("change the model");
  });

  it("is announced as a dialog trigger, so it is reachable by assistive tech", () => {
    expect(html).toContain('aria-haspopup="dialog"');
    expect(html).toContain('aria-expanded="false"');
  });
});

describe("AskModelChip display-only fallback", () => {
  // Without a handler the chip must NOT look clickable — that behaviour is deliberate and must not
  // be lost while fixing the empty case above.
  it("stays static text when no handler is supplied", () => {
    const staticHtml = renderToStaticMarkup(createElement(AskModelChip, { model: "" }));
    expect(staticHtml).toContain('data-testid="ask-model-chip-static"');
    expect(staticHtml).not.toContain("<button");
  });
});
