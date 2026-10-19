// ProjectScopeSelect — the conversations rail's PROJECT picker.
//
// The operator: "When the project list is long, setting a project on the conversation rail goes off the screen
// at the top and you can't scroll through everything."
//
// REPRODUCED IN THE SOURCE, and the mechanism was a placement that could only open one way:
//
//   bottom: `${window.innerHeight - rect.top + 4}px`     // pinned ABOVE the trigger, always
//   max-h-72                                             // a hard 288px cap on the menu
//
// The rail's header sits at the TOP of the page, so opening upward needed a menu's height of room ABOVE it —
// and a long project list fills the 288px cap. Its top therefore landed above y=0, and while the menu scrolls
// internally you cannot scroll to what is outside the viewport. Hence "goes off the screen at the top".
//
// It was correct for the OTHER caller (the composer, pinned to the bottom of its column) and wrong here, which
// is why it survived: the control has two homes and only one of them was tested by eye.
//
// The fix is that the menu measures itself and chooses the side with room, and that its cap is the room that
// actually exists rather than a constant. These are source-contract assertions, per the repo idiom (no jsdom,
// no @testing-library — see SessionGrants.test.tsx, DiffSidebar.railHandle.test.tsx).

import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";

const src = fs.readFileSync(path.join(__dirname, "ProjectScopeSelect.tsx"), "utf8");

/**
 * COMMENT-STRIPPED SOURCE, for every NEGATIVE assertion.
 *
 * This repo has already been bitten by exactly this, and the lesson is recorded in
 * cmd/orchicon/ci_release_workflows_test.go: a test that asserts a string is ABSENT trips on the comment that
 * explains its removal, because the explanation quotes the thing it removed. It happened here on the first run —
 * the note above this fix says the old class was `max-h-72`, and a raw `not.toContain("max-h-72")` failed on that
 * note rather than on any live style.
 *
 * Stripping is the fix rather than rewording, because the comment is worth more than the convenience of a
 * substring search: the next person needs to know WHY a fixed cap was wrong, and they need the test to be
 * looking at code.
 */
function stripComments(s: string): string {
  return s
    .replace(/\/\*[\s\S]*?\*\//g, "")   // block comments
    .replace(/(^|[^:])\/\/.*$/gm, "$1"); // line comments (the [^:] guard keeps https:// urls)
}

const code = stripComments(src);

describe("ProjectScopeSelect menu placement", () => {
  it("chooses the side with room, rather than always opening above the trigger", () => {
    // It reads BOTH rooms and compares them. The old code never looked at roomBelow at all.
    expect(src).toContain("const roomBelow = vh - trigger.bottom - GAP - MARGIN;");
    expect(src).toContain("const roomAbove = trigger.top - GAP - MARGIN;");
    expect(src).toContain("const openDown = roomBelow >= menuH || roomBelow >= roomAbove;");
    // And it acts on the choice: downward sets top, upward sets bottom. A menu that computes a direction and
    // then ignores it is the same bug with more code.
    expect(src).toContain("style.top = `${trigger.bottom + GAP}px`;");
    expect(src).toContain("style.bottom = `${vh - trigger.top + GAP}px`;");
  });

  it("never adopts a side whose room is smaller than the menu, which is what pushed it off the top", () => {
    // The whole defect in one line: the menu's height is capped by the room on the side it chose, and the
    // upward case takes roomAbove. A fixed cap cannot do this — which is why the constant had to go.
    expect(src).toContain("maxHeight: `${Math.max(120, (openDown ? roomBelow : roomAbove))}px`");
  });

  it("has no fixed max-h-72 cap left", () => {
    // The constant cap is the defect itself, not a detail: with a top-of-page trigger, 288px of menu does not
    // fit above it, so the class guaranteed the overflow regardless of placement.
    expect(code).not.toContain("max-h-72");
    expect(code).not.toContain("maxHeight: 288");
  });

  it("measures the real menu height instead of assuming it", () => {
    // The decision needs the menu's own height; assuming a constant is what made the choice wrong before any
    // options had rendered.
    expect(src).toContain("const menuH = menu?.height ??");
    expect(src).toContain("new ResizeObserver(place)");
  });

  it("re-places on scroll so a fixed menu tracks its trigger", () => {
    // `capture: true` matters: the rail list scrolls in an ANCESTOR, and a scroll listener that only hears the
    // window would leave the menu detached from its trigger while the operator scrolls the conversations.
    expect(src).toContain('window.addEventListener("scroll", place, true)');
  });

  it("keeps the trigger reachable and labelled for both of its homes", () => {
    // The rail header and the composer pill share this control; neither may lose its accessible name or its
    // test id, because the rail's own tests and the composer's reach for those.
    expect(src).toContain('data-testid="project-scope-trigger"');
    expect(src).toContain('data-testid="project-scope-menu"');
    expect(src).toContain("aria-expanded={open}");
    expect(src).toContain('aria-haspopup="listbox"');
  });
});
