// NATIVE FORM CONTROLS NEED `color-scheme`, AND NO TOKEN CAN STAND IN FOR IT.
//
// The operator's report: "weird rendering on colors for drop downs that make it
// hard to see anything" — the telemetry filter row's <select>s rendered as WHITE
// boxes with near-white text on a dark theme, while the text <input>s beside them
// looked correct.
//
// A native <select>, its popup, scrollbars and carets are painted by the USER
// AGENT. The `glass-input` class was resolving to the correct dark value all
// along; the widget simply ignores author background under the default
// `-webkit-appearance: menulist`. The only lever is `color-scheme`, which tells
// the UA which family of defaults to use.
//
// Asserted here rather than by eye because a missing color-scheme is invisible in
// every other gate: it typechecks, it lints, and it only shows up on a dark theme
// in a real webview.
import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";

const css = fs.readFileSync(path.join(__dirname, "..", "index.css"), "utf8");

/** the body of a top-level rule, by selector text */
function ruleBody(selector: string): string {
  const idx = css.indexOf(selector);
  expect(idx, `index.css declares no \`${selector}\` rule`).toBeGreaterThan(-1);
  const open = css.indexOf("{", idx);
  const close = css.indexOf("}", open);
  return css.slice(open + 1, close);
}

describe("native widget colour scheme", () => {
  it("declares a light default for the root", () => {
    expect(ruleBody(":root {").replace(/\s+/g, "")).toContain("color-scheme:light");
  });

  it("declares dark for the .dark class the theme store toggles", () => {
    // lib/theme-store.ts apply() adds `.dark` for every dark theme, so keying off
    // it means a newly added dark theme inherits the right answer.
    const body = ruleBody("  .dark {").replace(/\s+/g, "");
    expect(body).toContain("color-scheme:dark");
  });

  it("keys the dark answer off the class, not one theme's name", () => {
    // A per-theme list would need updating for every new dark theme and silently
    // regress the one nobody remembered.
    expect(css).not.toMatch(/\[data-theme="[^"]+"\]\s*\{[^}]*color-scheme:\s*dark/);
  });
});
