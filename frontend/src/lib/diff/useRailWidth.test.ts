import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";

// WIRING TEST for the measurement hook's cadence.
//
// The DECISIONS are pure and tested elsewhere (sideBySideFits, railResize). What can only
// be checked at source level — this repo has no jsdom, so a live ResizeObserver cannot be
// simulated — is that the observer is not rebuilt on every render.
//
// THE BUG THIS PINS. The hook had no dependency array, so every render of the Ask page
// disconnected and rebuilt the observer and forced a layout read to do it. Measured in a
// real browser during a streaming turn: 64 new ResizeObservers and 114
// getBoundingClientRect calls in 20s from ONE node, against 0 at rest. It never threw, so
// no test and no console error would have caught it — the cost was invisible until it was
// counted.
//
// The distinction that must survive: "attach when the node appears" is required (the host
// renders its target conditionally), but it must be keyed on the NODE, not on the render.
describe("useRailWidth", () => {
  const src = fs.readFileSync(path.join(__dirname, "useRailWidth.ts"), "utf8");

  it("keys the observer effect on the observed node, not on every render", () => {
    // The attach effect re-runs each render only to compare ONE ref and return the same
    // value; the observer itself must be keyed.
    expect(src).toMatch(/useLayoutEffect\(\(\) => \{\s*if \(!node\) return;/);
    expect(src).toMatch(/\}, \[node\]\);/);
  });

  it("has no effect that rebuilds the observer without a dependency array", () => {
    // The precise property, not "every effect has deps": the attach effect legitimately has
    // none (it compares one ref). What must not exist is an OBSERVER-BUILDING effect with no
    // deps — that is the shape that rebuilt it on every render.
    const effects =
      src.match(/useLayoutEffect\(\(\) => \{[\s\S]*?\n  \}(?:, \[[^\]]*\])?\);/g) ?? [];
    expect(effects).toHaveLength(2);
    const observerEffects = effects.filter((e) => e.includes("new ResizeObserver("));
    expect(observerEffects).toHaveLength(1);
    expect(observerEffects[0].trimEnd()).toMatch(/\[node\]\);$/);
  });

  it("keeps a single observer per node, not one per render", () => {
    // One construction site, and it is inside the node-keyed effect.
    const constructions = src.match(/new ResizeObserver\(/g) ?? [];
    expect(constructions).toHaveLength(1);
    expect(src).toContain("ro.observe(node)");
    expect(src).not.toContain("ro.observe(ref.current)");
  });

  it("still attaches when the host mounts its target conditionally", () => {
    // The ref identity never changes, so the attach must compare ref.current itself.
    expect(src).toContain("const el = ref.current");
    expect(src).toContain("setNode((prev) => (prev === el ? prev : el))");
  });
});
