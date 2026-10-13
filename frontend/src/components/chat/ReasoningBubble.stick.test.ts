import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";

// THE RUBBERBAND, ONE LEVEL DOWN.
//
// The reasoning block's own scroller re-pinned itself on every streamed chunk, with no
// notion of the operator having scrolled up inside it: "Auto-scroll the reasoning content
// as it grows during streaming" ran on each `text` change while open, so re-reading an
// earlier step was impossible during a turn — the next token hauled the view back to the
// end of the block. The same defect as the transcript's, seen through a smaller window.
//
// Asserted at source level because this repo's component tests render server-side and a
// live scroll cannot be simulated there; the DECISION itself is pinned as a pure case in
// src/lib/chat-scroll.test.ts (nextStick).
describe("ReasoningBubble — its own live stick", () => {
  const src = fs.readFileSync(
    path.join(__dirname, "ReasoningBubble.tsx"),
    "utf8",
  );

  it("auto-scrolls only while the operator is at the bottom of the block", () => {
    expect(src).toContain("stickRef.current");
    expect(src).toContain("nextStick(metrics, stickRef.current)");
    // The pin must be guarded by the stick, not unconditional.
    expect(src).toContain("open && contentRef.current && stickRef.current");
  });

  it("honours an upward gesture from the first pixel, not once it escapes the margin", () => {
    expect(src).toContain("wheelReleasesStick(e.deltaY)");
    expect(src).toContain("keyReleasesStick(e.key)");
    expect(src).toContain('addEventListener("mousedown"');
  });

  it("observes its own scroll container", () => {
    expect(src).toContain("onScroll={onContentScroll}");
  });
});
