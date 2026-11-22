// Placement rules for the portaled parent picker.
//
// These exist because the reported failure was NOT a wrong coordinate — it was a menu painted in
// normal flow at the end of document.body for one frame, after which autofocus scrolled the page
// to the bottom. Every assertion below is about a property that keeps that from recurring:
// out of flow from the first frame, capped to the room it chose, and never taller than the viewport.

import { describe, expect, it, vi } from "vitest";

// The component transitively imports theme-store, which touches `document.documentElement` at
// module load. Vitest runs without a DOM, so install a minimal shim BEFORE the import evaluates
// (same pattern as work-item-parent-select.test.ts).
vi.hoisted(() => {
  const g = globalThis as Record<string, unknown>;
  if (!g.document) {
    g.document = {
      documentElement: {
        classList: { add: () => {}, remove: () => {} },
        setAttribute: () => {},
      },
      addEventListener: () => {},
      removeEventListener: () => {},
    };
  }
  if (!g.localStorage) {
    g.localStorage = {
      getItem: () => null,
      setItem: () => {},
      removeItem: () => {},
      clear: () => {},
      key: () => null,
      length: 0,
    };
  }
});

import { placeMenu } from "@/components/work-items/work-item-parent-select";

const viewport = { width: 1440, height: 900 };

function trigger(top: number, height = 36, left = 400) {
  return { top, bottom: top + height, left, width: 300, height };
}

describe("placeMenu", () => {
  it("is always `fixed`, so it is out of flow on the very first frame", () => {
    // THE ANTI-REGRESSION. A static/relative first frame grows the document and is what made
    // autofocus scroll the page to the bottom.
    const style = placeMenu(trigger(120), viewport);
    expect(style.position).toBe("fixed");
  });

  it("opens DOWNWARD when there is room below", () => {
    const style = placeMenu(trigger(120), viewport, 300);
    expect(style.top).toBe(`${120 + 36 + 4}px`);
    expect(style.bottom).toBeUndefined();
  });

  it("opens UPWARD when there is no room below but room above", () => {
    // Trigger near the bottom of the viewport: 300px of menu will not fit below.
    const t = trigger(820, 36);
    const style = placeMenu(t, viewport, 300);
    // Anchored by `bottom` (distance from the viewport bottom to the trigger's top).
    expect(style.bottom).toBe(`${viewport.height - 820 + 4}px`);
    expect(style.top).toBeUndefined();
  });

  it("prefers DOWNWARD when below is at least as roomy as above", () => {
    // Sitting mid-page with equal room: flipping up would be disorienting.
    const t = trigger(430, 40);
    const style = placeMenu(t, viewport, 200);
    expect(style.top).toBe(`${430 + 40 + 4}px`);
  });

  it("caps to the room on the side it chose, never a fixed max-height", () => {
    // The invariant, stated for every trigger position: whatever side placement picks, the cap
    // is that side's available room (or the 120px floor). A fixed cap is what pushed a long
    // list off-screen in the sibling scope selector — the bug this rule prevents.
    const MARGIN = 8;
    const GAP = 4;
    for (const top of [40, 200, 430, 600, 760, 850]) {
      const t = trigger(top, 36);
      const style = placeMenu(t, viewport, 300);
      const cap = parseFloat(String(style.maxHeight));
      const roomBelow = viewport.height - t.bottom - GAP - MARGIN;
      const roomAbove = t.top - GAP - MARGIN;
      const chosenRoom = style.top !== undefined ? roomBelow : roomAbove;
      expect(cap).toBe(Math.max(120, chosenRoom));
      // And it can never claim more height than the viewport.
      expect(cap).toBeLessThanOrEqual(viewport.height);
    }
  });

  it("a trigger with no room below opens UP and gets the room above", () => {
    const t = trigger(760, 36); // 92 below vs 748 above
    const style = placeMenu(t, viewport, 300);
    expect(style.bottom).toBeDefined();
    expect(parseFloat(String(style.maxHeight))).toBe(748);
  });

  it("keeps a usable minimum height even with almost no room", () => {
    const t = trigger(895, 5); // essentially at the bottom edge
    const style = placeMenu(t, viewport, 400);
    expect(parseFloat(String(style.maxHeight))).toBeGreaterThanOrEqual(120);
  });

  it("never runs off the right edge of the viewport", () => {
    // A trigger near the right edge must still produce a max-width that fits.
    const t = trigger(200, 36, 1200);
    const style = placeMenu(t, viewport);
    const maxW = parseFloat(String(style.maxWidth));
    expect(1200 + maxW).toBeLessThanOrEqual(viewport.width);
    expect(maxW).toBeGreaterThanOrEqual(160);
  });

  it("is visible once placed (the hidden initial style is replaced)", () => {
    expect(placeMenu(trigger(100), viewport).visibility).toBe("visible");
  });

  it("sits above the card's content (z-index set on the portaled node)", () => {
    // The original clipping bug was a z-index scoped inside a backdrop-filter stacking context.
    // The portal escapes it; the stacking order still has to be explicit.
    expect(placeMenu(trigger(100), viewport).zIndex).toBe(50);
  });
});
