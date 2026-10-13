import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";

// THE SAME BUG, IN THE SIBLING SURFACE — and this file exists because it was here.
//
// The execution session chat carried its OWN copy of the transcript's stick logic: the
// near-bottom arithmetic with its own magic 80, and a 200ms window to "ignore our own
// scroll events". That copy is how it inherited the same defect, and the window was open
// often enough to matter because this pane flushes every ~2s while running.
//
// So the assertions are (a) that the shared module's decisions are what this pane uses,
// and (b) that the local copy is gone. Duplication is what caused the drift; a source-level
// check is the only level available here (component tests render server-side, so a live
// scroll cannot be simulated), and a regression would otherwise be invisible.
describe("SessionChatPane — the live stick", () => {
  const src = fs.readFileSync(
    path.join(__dirname, "SessionChatPane.tsx"),
    "utf8",
  );

  it("uses the SHARED decisions rather than its own copy of the arithmetic", () => {
    expect(src).toContain("isNearBottom(metrics)");
    expect(src).toContain("hasArrived(metrics, driveRef.current)");
    expect(src).toContain("bottomScrollTop(el)");
    // The hand-rolled arithmetic and its magic number must not come back.
    expect(src).not.toContain("scrollHeight - el.scrollTop - el.clientHeight < 80");
    expect(src).not.toContain("< 80");
  });

  it("no longer decides intent from WHEN a scroll arrived", () => {
    expect(src).not.toContain("lastAutoScrollRef");
  });

  it("releases the stick on the operator's own upward gestures", () => {
    expect(src).toContain("wheelReleasesStick(e.deltaY)");
    expect(src).toContain("keyReleasesStick(e.key)");
    expect(src).toContain('addEventListener("mousedown"');
  });

  it("drives the smooth jump to an END, so the animation cannot release the stick mid-flight", () => {
    expect(src).toContain('driveRef.current = "bottom"');
  });
});
