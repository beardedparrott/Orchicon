// manage-server.stability.test.ts — the hook-dependency hazard behind the flicker.
//
// REPORTED: "On the server management page it flickers between these two screens over and over
// spastically during an install" — alternating between
//
//   Connecting · port unknown · "could not read the plane's status — Failed to fetch"
//   Connected  · port 8080 · "serving http://localhost:8080/healthz as pid 27612 …"
//
// THE CAUSE IS A DEPENDENCY THAT IS A NEW VALUE EVERY RENDER. planeControl() builds an object literal (see
// lib/desktop.ts httpPlaneControl), and desktopInfo() builds a fresh payload object. Passed straight into an
// effect's dependency array they make the effect re-run after every render; each run starts a fetch, whose
// setState causes the next render. That is an unbounded request loop, and its two outcomes are exactly the
// two screens above.
//
// So there are two things worth pinning, and this file pins both:
//
//   1. THE HAZARD IS REAL — these factories are not referentially stable. If someone makes them stable
//      later, the memoisation in manage-server.tsx becomes merely harmless rather than load-bearing, and
//      this test is what tells them the hazard they were guarding against is gone.
//   2. THE CALL SITE MEMOISES THEM. A future edit that drops useMemo reintroduces the loop, and nothing
//      else in the suite would notice: the page still renders correctly, once.

import { describe, expect, it, beforeEach } from "vitest";
import fs from "node:fs";
import path from "node:path";

import { desktopInfo, planeControl } from "@/lib/desktop";

const g = globalThis as unknown as Record<string, unknown>;
const KEY = "__ORCHICON_DESKTOP__";

const PAYLOAD = {
  desktop: true,
  planeURL: "http://127.0.0.1:8080",
  instance: "dev",
  appTag: "v1.0.0",
  planeTag: "v1.0.0",
  versionMismatch: false,
  controlToken: "tok-123",
};

const manageServerSrc = fs.readFileSync(
  path.join(__dirname, "manage-server.tsx"),
  "utf8",
);

/**
 * code strips comments, because PROSE IS NOT CODE — the same trap these source guards keep hitting.
 *
 * The comments in manage-server.tsx explain this very defect and therefore contain the string
 * `planeControl()`; counting occurrences in the raw file counted the explanation as a second call site and
 * failed a correct implementation. A guard that cannot tell a comment from a call is a guard that punishes
 * documentation.
 */
const code = manageServerSrc
  .replace(/\/\*[\s\S]*?\*\//g, "")   // block comments
  .replace(/(^|[^:])\/\/[^\n]*/g, "$1"); // line comments (the [^:] keeps "https://" intact)

beforeEach(() => {
  const d = Object.getOwnPropertyDescriptor(g, KEY);
  if (d?.configurable) delete g[KEY];
  delete g.go;
  g[KEY] = PAYLOAD;
});

describe("the factories are not referentially stable (the hazard)", () => {
  it("planeControl() returns a different object on every call", () => {
    const a = planeControl();
    const b = planeControl();
    expect(a).not.toBeNull();
    // NOT toBe. This is the whole reason the call site must memoise: an effect with this in its
    // dependency array re-runs on every render, for ever.
    expect(a).not.toBe(b);
  });

  it("desktopInfo() returns a different object on every call", () => {
    expect(desktopInfo()).not.toBe(desktopInfo());
  });
});

describe("manage-server memoises them", () => {
  it("wraps planeControl in useMemo with an empty dependency list", () => {
    // The exact shape matters: useMemo(fn, []) — a non-empty list would let the value change.
    expect(manageServerSrc).toMatch(
      /const plane = useMemo\(\(\) => planeControl\(\), \[\]\)/,
    );
  });

  it("wraps desktopInfo in useMemo too", () => {
    expect(manageServerSrc).toMatch(
      /const info = useMemo\(\(\) => desktopInfo\(\), \[\]\)/,
    );
  });

  it("never calls planeControl() outside a memo", () => {
    // VACUITY GUARD FOR THE PATTERN, not just this one line: any call site added later must memoise too.
    // Every occurrence must be the one inside the useMemo.
    const calls = code.match(/planeControl\(\)/g) ?? [];
    expect(calls.length).toBe(1);
  });

  it("does not import a dependency-free factory into a hook without memoising", () => {
    // The negative assertion that would have caught the bug: no bare `planeControl()` in a deps array.
    expect(code).not.toMatch(/\[plane, instance\][^;]*planeControl/);
  });
});
