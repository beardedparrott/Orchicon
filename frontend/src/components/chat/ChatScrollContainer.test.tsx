import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";

// WIRING TEST — a pure helper nothing calls is inert, and that is the exact failure this
// change is about (the container's scroll decisions were made in a way that read the
// component's own auto-scroll as the operator's intent).
//
// The DECISIONS are pinned in src/lib/chat-scroll.test.ts. What is pinned here is that the
// container actually consults them, and — most importantly — that the TIME WINDOW is gone.
// That window was the bug: it suppressed the operator's own scroll whenever it landed
// within 200ms of an auto-scroll, and during a streaming reply that is most of the time,
// so the live stick stayed on and the next chunk dragged them back to the tail.
//
// Asserted as the ABSENCE of the old mechanism, because a source-level test is the only
// level this repo can check it at (the component tests render server-side, so a live
// scroll cannot be simulated) and a regression here would otherwise be silent.
describe("ChatScrollContainer", () => {
  const src = fs.readFileSync(
    path.join(__dirname, "ChatScrollContainer.tsx"),
    "utf8",
  );

  it("no longer decides intent from WHEN a scroll arrived", () => {
    expect(src).not.toContain("lastAutoScrollRef");
    expect(src).not.toContain("Date.now()");
  });

  it("consults the drive so its own in-flight scrolls are not read as intent", () => {
    expect(src).toContain("hasArrived(metrics, driveRef.current)");
    expect(src).toContain('driveRef.current = "bottom"');
    expect(src).toContain('driveRef.current = "top"');
  });

  it("releases the stick on the operator's own upward gestures", () => {
    expect(src).toContain("wheelReleasesStick(e.deltaY)");
    expect(src).toContain("keyReleasesStick(e.key)");
    expect(src).toContain("stickRef.current = false");
    // A scrollbar drag begins with one of these and then produces scroll events no other
    // gesture explains.
    expect(src).toContain('addEventListener("mousedown"');
  });

  it("re-acquires the stick only at the ACTUAL tail, not in the margin", () => {
    // The rubberband guard (see nextStick in chat-scroll.ts): the decision must go through
    // the shared function, not be re-derived here as a bare near-bottom test — which is
    // exactly the shape that pinned the operator to the tail.
    expect(src).toContain("nextStick(metrics, stickRef.current)");
  });

  it("keeps the two controls and the live-stick plumbing", () => {
    expect(src).toContain("jump to top");
    expect(src).toContain("jump to latest");
    expect(src).toContain("overflow-y-auto");
    expect(src).toContain("onScroll={onScroll}");
  });
});
