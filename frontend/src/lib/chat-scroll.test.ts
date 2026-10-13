import { describe, expect, it } from "vitest";
import {
  DOWNWARD_KEYS,
  atBottom,
  UPWARD_KEYS,
  bottomScrollTop,
  distanceFromBottom,
  hasArrived,
  isNearBottom,
  isNearTop,
  keyReleasesStick,
  nextStick,
  topScrollTop,
  wheelReleasesStick,
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

// ---------------------------------------------------------------- the live stick
//
// THE OPERATOR: "When you scroll up or hit home to go to the top, the auto refresh of the
// live conversations brings you right back down. This should only do this if you are
// actively at the bottom."
//
// The container made that decision by TIMING — it ignored any scroll event arriving within
// 200ms of its own auto-scroll. Two consequences, and these tests pin both: an event it
// ignored was an event whose operator intent it never recorded, so the stick stayed ON; and
// a smooth scroll away from the tail passes back THROUGH the near-bottom margin, so its
// in-flight positions read as "at the bottom" and re-armed the stick the gesture had just
// released. A ScrollDrive replaces the window: a scroll is attributed by WHERE it is.

describe("chat-scroll — the live stick", () => {
  // A conversation longer than its viewport, resting at the tail.
  const tail = { scrollTop: 600, scrollHeight: 1000, clientHeight: 400 };
  // Parked in the middle: reading history, NOT at the bottom.
  const reading = { scrollTop: 300, scrollHeight: 1000, clientHeight: 400 };
  // A position that is still inside the 80px margin — the band a jump passes through.
  const nearTail = { scrollTop: 550, scrollHeight: 1000, clientHeight: 400 };

  it("releases the stick at a position away from the bottom", () => {
    expect(isNearBottom(reading)).toBe(false);
  });

  it("keeps the stick at the bottom, and in the margin", () => {
    expect(isNearBottom(tail)).toBe(true);
    expect(isNearBottom(nearTail)).toBe(true); // 50px from the bottom
  });

  it("treats a scroll at the end we drove to as OURS, arrived", () => {
    expect(hasArrived(tail, "bottom")).toBe(true);
    expect(hasArrived({ ...tail, scrollTop: 0 }, "top")).toBe(true);
  });

  it("treats a scroll short of the end we drove to as still OURS — an animation in flight", () => {
    // THIS IS THE "JUMP TO TOP" CASE. The operator jumped to the top; the animation is
    // passing back through the near-bottom margin. Reading `isNearBottom` here would say
    // "the operator is at the bottom" and re-arm the stick, and the next streamed chunk
    // would drag them back down. The drive says: that position is not where I sent it,
    // so it is not the operator's to change anything.
    expect(hasArrived(nearTail, "top")).toBe(false);
    expect(hasArrived(reading, "top")).toBe(false);
    expect(isNearBottom(nearTail)).toBe(true); // ...which is exactly why it must be ignored
  });

  it("drives toward an END, not a pixel — so content arriving cannot stale it", () => {
    // The content grew while the jump was in flight. A recorded position would now be
    // unreachable and the drive would never complete; an end is still true.
    const grown = { scrollTop: 1000, scrollHeight: 1400, clientHeight: 400 };
    expect(hasArrived(grown, "bottom")).toBe(true);
    // ...and the same for a top drive on a conversation that shrank.
    const shrunk = { scrollTop: 0, scrollHeight: 200, clientHeight: 400 };
    expect(hasArrived(shrunk, "top")).toBe(true);
  });

  it("RE-ACQUIRES only at the ACTUAL tail, not in the margin", () => {
    // THE RUBBERBAND. The operator: "the GUI is still rubberbanding to the bottom of the
    // page."
    //
    // A deliberate upward gesture releases the stick, and the scroll that gesture caused is
    // still inside the 80px margin. A margin-based position test therefore re-arms the stick
    // immediately — the release is undone by the very motion it produced, and the next
    // chunk snaps the view back to the tail. On a trackpad, whose wheel events are a few
    // pixels each, no gesture can get outside the margin at all, so the view is pinned
    // however hard the operator scrolls.
    expect(atBottom(nearTail)).toBe(false); // inside the margin, but NOT at the tail
    expect(nextStick(nearTail, false)).toBe(false); // ...so it must not re-arm

    // And it DOES re-acquire the moment they reach the end.
    expect(nextStick(tail, false)).toBe(true);
  });

  it("keeps FOLLOWING through the margin, so slack never breaks the follow", () => {
    // The other half of the asymmetry, and the reason both tests exist: while already
    // following, the same margin position must KEEP following — requiring the exact tail
    // would drop the follow on any rounding or tiny content change.
    expect(nextStick(nearTail, true)).toBe(true);
    expect(nextStick(tail, true)).toBe(true);
    // And leaving the margin still releases.
    expect(nextStick(reading, true)).toBe(false);
  });

  it("absorbs sub-pixel rounding at the tail", () => {
    // A browser's scrollTop is a float after a device-pixel-ratio scale, so a view that is
    // visually at the tail can measure a fraction short of it.
    expect(atBottom({ ...tail, scrollTop: 599.4 })).toBe(true);
    expect(atBottom({ ...tail, scrollTop: 590 })).toBe(false);
  });

  it("reads an upward gesture as intent, whether it is a wheel or a key", () => {
    expect(wheelReleasesStick(-1)).toBe(true);
    expect(wheelReleasesStick(-120)).toBe(true);
    expect(wheelReleasesStick(120)).toBe(false); // downward: the position decides
    expect(keyReleasesStick("PageUp")).toBe(true);
    expect(keyReleasesStick("Home")).toBe(true);
    expect(keyReleasesStick("ArrowUp")).toBe(true);
    expect(keyReleasesStick("End")).toBe(false);
    expect(keyReleasesStick("ArrowDown")).toBe(false);
    // The two directions are named in one place each, so neither can drift.
    for (const k of UPWARD_KEYS) expect(keyReleasesStick(k)).toBe(true);
    for (const k of DOWNWARD_KEYS) expect(keyReleasesStick(k)).toBe(false);
  });
});
