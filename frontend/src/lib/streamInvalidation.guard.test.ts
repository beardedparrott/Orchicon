import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";

// The guard for a bug class that has now shipped twice, in two different views.
//
// A live event stream whose `onEvent` invalidates a query SYNCHRONOUSLY, per
// event, keeps a refetch permanently in flight. A burst then saturates the
// browser's ~6-connection per-origin HTTP/1.1 budget and HANGS THE WHOLE UI —
// which is what the operator reported: "the Heads Up view causes the entire web
// GUI to hang up because it is processing a large stream", a white screen on
// every page.
//
// It was fixed for the execution streams (executions_.$id.tsx) via
// lib/useDebouncedInvalidation, and then MISSED for the run route's workflow
// stream — and because the run route IS the Heads Up view, the symptom came
// straight back on the one view that mounts it.
//
// HeadsUpExpandedModal USED TO BE LISTED HERE and is now deleted: it hosted a
// second live stream on the run route, which is one of the things that made the
// view hang. The run view's tiles are static and a tile links to the execution
// page, so there is no second stream left to guard.
//
// WHY A SOURCE ASSERTION: the defect is a WIRING OMISSION, and nothing about the
// resulting code is wrong in isolation — the invalidation is valid, the stream is
// valid, only their combination is fatal. A runtime test would have to drive a
// real event burst through a mounted route to see it. This mirrors the repo's
// existing source-assertion guard (ModelPicker.test.tsx) for the same reason.

const SRC = path.join(__dirname, "..");

/** Every file that subscribes to a live event stream. */
const STREAMING_FILES = [
  "routes/workflows_.$id_.runs.$runId.tsx",
  "routes/executions_.$id.tsx",
  "routes/projects_.$id.tsx",
];

function read(rel: string): string {
  return fs.readFileSync(path.join(SRC, rel), "utf8");
}

/** True when an inline `onEvent: () => { … }` reaches invalidateQueries. */
function hasInlineInvalidatingOnEvent(src: string): boolean {
  const needle = "onEvent: () => {";
  for (let i = src.indexOf(needle); i >= 0; i = src.indexOf(needle, i + 1)) {
    // A generous window: the handler body of a wiring hook is short, and a wider
    // window can only produce a false POSITIVE, which is the safe direction here.
    if (src.slice(i, i + 400).includes("invalidateQueries")) {
      return true;
    }
  }
  return false;
}

describe("live-stream invalidations are coalesced (the UI-hang guard)", () => {
  for (const rel of STREAMING_FILES) {
    it(`${rel} does not invalidate synchronously per event`, () => {
      const src = read(rel);
      expect(
        hasInlineInvalidatingOnEvent(src),
        "an inline onEvent calls invalidateQueries directly: a burst of events " +
          "then keeps a refetch permanently in flight and hangs the whole UI. " +
          "Route it through useDebouncedInvalidation instead.",
      ).toBe(false);
    });

    it(`${rel} uses useDebouncedInvalidation`, () => {
      const src = read(rel);
      expect(src).toContain("useDebouncedInvalidation");
      expect(src).toMatch(/onEvent:\s*schedule\w*Invalidation/);
    });
  }

  it("the run route — the Heads Up view — is among them", () => {
    // Named explicitly, because this is the one that was missed: the Heads Up
    // view mounts routes/workflows_.$id_.runs.$runId.tsx, so a regression here
    // reproduces the operator's white screen exactly.
    const src = read("routes/workflows_.$id_.runs.$runId.tsx");
    expect(src).toContain("workflowKeys.run(runId)");
    expect(src).toContain("workflowKeys.stepRuns(runId)");
    expect(src).toMatch(/useDebouncedInvalidation\(\[/);
  });
});
