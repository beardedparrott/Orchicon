import { describe, expect, it } from "vitest";
import {
  bottomScrollTop,
  distanceFromBottom,
  isNearBottom,
  isNearTop,
  topScrollTop,
} from "./chat-scroll";

// The transcript's top/bottom controls are the deliverable; the DECISION each one
// makes is this arithmetic. These pin both directions and the live-stick margin.

describe("chat-scroll", () => {
  const atTop = { scrollTop: 0, scrollHeight: 1000, clientHeight: 400 };
  const atBottom = { scrollTop: 600, scrollHeight: 1000, clientHeight: 400 };
  const mid = { scrollTop: 300, scrollHeight: 1000, clientHeight: 400 };

  it("treats the bottom as near-bottom (live stick stays on)", () => {
    expect(isNearBottom(atBottom)).toBe(true);
    expect(distanceFromBottom(atBottom)).toBe(0);
  });

  it("treats a scrolled-up view as NOT near-bottom (live stick releases)", () => {
    expect(isNearBottom(mid)).toBe(false);
    expect(isNearBottom(atTop)).toBe(false);
  });

  it("honours the 80px live-stick margin", () => {
    expect(isNearBottom({ scrollTop: 530, scrollHeight: 1000, clientHeight: 400 })).toBe(true); // 70px
    expect(isNearBottom({ scrollTop: 500, scrollHeight: 1000, clientHeight: 400 })).toBe(false); // 100px
  });

  it("treats the top as near-top (no jump-to-top needed)", () => {
    expect(isNearTop(atTop)).toBe(true);
    expect(isNearTop(mid)).toBe(false);
    expect(isNearTop(atBottom)).toBe(false);
  });

  it("points the two controls at the two ends", () => {
    expect(topScrollTop()).toBe(0);
    expect(bottomScrollTop(atBottom)).toBe(1000);
  });
});
