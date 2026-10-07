// Pure clamp/step rules for the drag-resizable diff rail.
//
// These are the acceptance-critical numbers: they are what keeps the CHAT
// column beside the rail usable and the page free of horizontal scroll when a
// drag (or a stale stored width on a shrunken window) asks for too much.

import { describe, expect, it } from "vitest";

import {
  clampRailWidth,
  maxRailWidth,
  MIN_CHAT_WIDTH,
  RAIL_DEFAULT_WIDTH,
  RAIL_MAX_WIDTH,
  RAIL_MIN_WIDTH,
  RAIL_STEP,
  stepRailWidth,
} from "./railResize";

describe("railResize constants", () => {
  it("has a default inside [min, max]", () => {
    expect(RAIL_DEFAULT_WIDTH).toBeGreaterThanOrEqual(RAIL_MIN_WIDTH);
    expect(RAIL_DEFAULT_WIDTH).toBeLessThanOrEqual(RAIL_MAX_WIDTH);
    expect(RAIL_STEP).toBeGreaterThan(0);
  });
});

describe("clampRailWidth", () => {
  it("enforces the minimum on a wide container", () => {
    expect(clampRailWidth(100, 1400)).toBe(RAIL_MIN_WIDTH);
  });

  it("enforces the constant maximum on a wide container", () => {
    expect(clampRailWidth(5000, 1400)).toBe(RAIL_MAX_WIDTH);
  });

  it("passes a width through untouched when it is already legal", () => {
    expect(clampRailWidth(600, 1600)).toBe(600);
  });

  // The container clamp — the actual fix for min-w-[480px] squeezing the chat.
  it("clamps against the CONTAINER, not only constants", () => {
    // 800px row - 360px reserved for the chat = 440px rail ceiling.
    expect(clampRailWidth(960, 800)).toBe(440);
  });

  it("re-clamps a stored wide width after the window shrinks", () => {
    const wide = clampRailWidth(RAIL_MAX_WIDTH, 1600);
    expect(wide).toBe(RAIL_MAX_WIDTH);
    const shrunk = clampRailWidth(wide, 900);
    expect(shrunk).toBe(540); // 900 - 360
    expect(shrunk).toBeLessThan(wide);
  });

  it("never collapses the rail below the minimum even on a tiny container", () => {
    expect(clampRailWidth(RAIL_DEFAULT_WIDTH, 400)).toBe(RAIL_MIN_WIDTH);
  });

  it("falls back to the constant ceiling when the container is unmeasured", () => {
    expect(clampRailWidth(600, 0)).toBe(600);
    expect(clampRailWidth(5000, 0)).toBe(RAIL_MAX_WIDTH);
  });

  it("never yields NaN for a non-finite width or container", () => {
    expect(clampRailWidth(Number.NaN, 1400)).toBe(RAIL_DEFAULT_WIDTH);
    expect(clampRailWidth(500, Number.NaN)).toBe(500);
    expect(Number.isFinite(clampRailWidth(500, Number.POSITIVE_INFINITY))).toBe(true);
  });

  it("the default reservation is the bare [rail | chat] MIN_CHAT_WIDTH", () => {
    expect(clampRailWidth(960, 800)).toBe(clampRailWidth(960, 800, MIN_CHAT_WIDTH));
  });
});

// The Ask page row is [rail | chat(flex-1) | 288px conversations panel]: the
// reservation must include that fixed panel (and any flex gap), or a wide rail
// collapses the chat to 1248 - rail - 288 = 72px at the 888 ceiling.
describe("clampRailWidth reserves the row's fixed siblings", () => {
  const ASK_ROW = 1248; // 1280 viewport - 2 x 16px page padding
  const PANEL = 288;

  it("reserves the fixed conversations panel so the chat keeps its minimum", () => {
    const reserved = MIN_CHAT_WIDTH + PANEL;
    const max = clampRailWidth(960, ASK_ROW, reserved);
    expect(max).toBe(ASK_ROW - reserved); // 600
    const chat = ASK_ROW - max - PANEL;
    expect(chat).toBe(MIN_CHAT_WIDTH);
    expect(chat).toBeGreaterThan(100); // never the 72px squeeze
  });

  it("without the reservation the same row squeezes the chat (regression guard)", () => {
    const max = clampRailWidth(960, ASK_ROW); // old behaviour
    const chat = ASK_ROW - max - PANEL;
    expect(chat).toBeLessThan(MIN_CHAT_WIDTH); // 72px — the bug
  });

  it("includes flex gaps in the reservation (the exec page's gap-3)", () => {
    // [rail | gap-3(12px) | chat] on an 848px row ⇒ ceiling 848 - 372 = 476.
    const max = clampRailWidth(960, 848, MIN_CHAT_WIDTH + 12);
    expect(max).toBe(476);
    expect(848 - max - 12).toBe(MIN_CHAT_WIDTH);
  });
});

describe("maxRailWidth", () => {
  it("reserves the chat column's minimum width", () => {
    expect(maxRailWidth(1400)).toBe(Math.min(RAIL_MAX_WIDTH, 1400 - 360));
  });

  it("reserves an explicit reservation when given one", () => {
    expect(maxRailWidth(1248, 648)).toBe(600);
  });
});

describe("stepRailWidth", () => {
  it("grows by one step to the right", () => {
    expect(stepRailWidth(500, 1, 1600)).toBe(500 + RAIL_STEP);
  });

  it("shrinks by one step to the left", () => {
    expect(stepRailWidth(500, -1, 1600)).toBe(500 - RAIL_STEP);
  });

  it("clamps a step at both ends", () => {
    expect(stepRailWidth(RAIL_MIN_WIDTH, -1, 1600)).toBe(RAIL_MIN_WIDTH);
    expect(stepRailWidth(RAIL_MAX_WIDTH, 1, 1600)).toBe(RAIL_MAX_WIDTH);
    expect(stepRailWidth(600, 1, 800)).toBe(440); // container-clamped ceiling
  });

  it("honours the reservation", () => {
    // 1248 row - (360 chat + 288 panel) = 600 ceiling.
    expect(stepRailWidth(590, 1, 1248, MIN_CHAT_WIDTH + 288)).toBe(600);
  });
});
