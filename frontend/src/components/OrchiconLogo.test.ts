import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";

// The brand mark: an orca built from six arrows, the arrows in the theme's own ink and the orca in its
// teal. These are SOURCE assertions, like ModelPicker.order.test.tsx, because this project has no
// @testing-library/react or jsdom — the repo's own notes record that components cannot be rendered in
// this suite, so a source assertion is the honest ceiling here.
//
// THE COMMENTS ARE STRIPPED FIRST, and that is not tidiness. The component's own documentation explains
// what it REPLACED — naming the old ring's palette constant and geometry — so an assertion over the raw
// file matches prose and passes or fails for the wrong reason. (The first version of this test did
// exactly that: it failed on a comment that legitimately mentions the removed constant.) Asserting over
// the code means a comment can be as candid as it needs to be, and a commented-out path still counts as
// missing rather than present.
//
// WHAT THEY DEFEND, in order of how easy each would be to lose:
//
//  1. THAT THE ARROWS ADAPT. `currentColor` on the first path is the ONLY reason one component works
//     across 20+ themes. Replace it with a literal and the mark looks right on the theme it was checked
//     against and wrong on the other nineteen — a change nothing else would catch, because the artwork
//     would still be present and the build would still pass.
//  2. THAT THE WHALE DOES NOT. Its colours must stay literal: the eye (#EFF0E0) is drawn on the whale
//     body (#2D8C88), not on the page, so it is legible in every theme precisely because it is fixed.
//     Making it adaptive would put a theme-coloured eye inside a teal whale.
//  3. THAT fillRule IS SET. The whale's outline is self-intersecting; without `evenodd` the overlaps fill
//     in and the shape reads as a blob rather than an orca.
//  4. THAT THE RING IS GONE. The previous mark was a six-segment ring whose arcs used SVG `A` commands.
//     The orca uses only quadratic/cubic curves, so an `A42 42` arc anywhere in the code means the old
//     artwork is back (or was never fully replaced).
//
// The stripper is deliberately naive — block comments, then `//` to end of line. It is sufficient here
// and the file has no URLs or regex literals containing `//`; if one is ever added, this is the line to
// revisit rather than the assertions.
function stripComments(s: string): string {
  return s.replace(/\/\*[\s\S]*?\*\//g, "").replace(/\/\/[^\n]*/g, "");
}

const code = stripComments(fs.readFileSync(path.join(__dirname, "OrchiconLogo.tsx"), "utf8"));

describe("OrchiconMark — the orca, drawn to follow the theme", () => {
  it("draws exactly the five supplied paths, arrows first", () => {
    const decl = code.slice(code.indexOf("const PATHS"), code.indexOf("export function OrchiconMark"));
    const entries = decl.match(/\{ fill: "[^"]*", d: "/g) ?? [];
    expect(entries).toHaveLength(5);

    // Order is load-bearing: the whale parts paint over the arrows.
    expect(decl.indexOf('"currentColor"')).toBeLessThan(decl.indexOf('"#2D8C88"'));
  });

  it("lets the ARROWS inherit the theme ink, and only the arrows", () => {
    // Exactly one adaptive path — the arrows. Everything else is brand colour, deliberately.
    // A named message, because the bare form reports "Target cannot be null or undefined" when there are
    // ZERO matches (String.match returns null, not an empty array) — which says nothing about the theme.
    const adaptive = code.match(/fill: "currentColor"/g) ?? [];
    expect(
      adaptive,
      "the arrows must be filled with currentColor, or the mark stops following the theme (one component, 20+ themes)",
    ).toHaveLength(1);

    // The whale's parts, pinned by value: body, two lighter fins/jaw, eye.
    expect(code).toMatch(/fill: "#2D8C88"/);
    expect(code.match(/fill: "#5CBFA2"/g) ?? [], "the two lighter whale parts (fin and jaw) are missing").toHaveLength(2);
    expect(code).toMatch(/fill: "#EFF0E0"/);
  });

  it("keeps fillRule evenodd — the whale's outline self-intersects", () => {
    expect(code).toMatch(/fillRule="evenodd"/);
  });

  it("carries no remnant of the superseded six-segment ring", () => {
    // The ring's arcs are SVG `A` commands; the orca uses only Q/C curves.
    expect(code).not.toMatch(/A42 42/);
    expect(code).not.toContain("M52.56 8.08");
    // And the constant that existed solely to colour its last segment is gone. If it is reinstated it
    // should be because something IMPORTS it, not to keep a dead export alive.
    expect(code).not.toContain("BRAND_SIGNAL");
  });
});
