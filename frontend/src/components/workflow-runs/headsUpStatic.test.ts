// THE HEADS-UP GRID IS STATIC — no streams, no polls, no timers.
//
// The operator, on the third attempt at this view:
//
//	"I need to add looking into orchicon gui not responding if you click heads up mode on a
//	 workflow run. We have tried to do this multiple times now and it's just too much
//	 streaming data. I wonder if we should rethink this. How can we limit the amount of
//	 data? Maybe the individual cards don't stream until you click on one and as soon as you
//	 click away or click the execution button the streaming to the screen stops and is only
//	 happening on the actual execution page itself."
//
// The previous fix (#630) shrank each fetch from ~399 kB to a few kB — a real improvement,
// and not sufficient: a SMALLER payload re-requested every two seconds FROM EVERY TILE is
// still a saturating load while the operator scans a grid of small cards. The problem was
// never only the size; it was that the grid tried to be live N times at once.
//
// So the grid gives up liveness deliberately. A tile is a thumbnail: it fetches its tail
// ONCE for the summary line it displays, and then holds nothing. The EXPANDED tile and the
// execution page own liveness, one stream each, for exactly one execution.
//
// These are SOURCE-LEVEL assertions, which is unusual and deliberate. The failure mode is a
// hook call and a timer inside a small component, and catching a re-introduced
// `setInterval` through the rendered output would need a React Query harness with a mocked
// transport — for an invariant that is fully decidable from the source. This is the cheapest
// place that keeps it true, and it is the third time this view has regressed, so the cost of
// the check is worth paying.

import { readFileSync } from "node:fs";
import { resolve } from "node:path";

import { describe, expect, it } from "vitest";

const read = (rel: string) => readFileSync(resolve(process.cwd(), rel), "utf8");

const TILE = "src/components/workflow-runs/HeadsUpTile.tsx";
const GRID = "src/components/workflow-runs/HeadsUpGrid.tsx";
const MODAL = "src/components/workflow-runs/HeadsUpExpandedModal.tsx";

/** Strip comments so a MENTION of a pattern (in a rationale) is not read as code. */
function code(src: string): string {
  return src
    .replace(/\/\*[\s\S]*?\*\//g, "")
    .replace(/^\s*\/\/.*$/gm, "");
}

describe("a Heads-Up tile is a thumbnail, not a window", () => {
  it("opens NO execution stream", () => {
    expect(code(read(TILE))).not.toContain("useStreamExecutionEvents");
  });

  it("holds NO timer — the 2s poll was the saturating load", () => {
    const c = code(read(TILE));
    expect(c).not.toContain("setInterval");
    expect(c).not.toContain("setTimeout");
    // …and nothing re-fetches after the first paint.
    expect(c).not.toContain("refetch");
  });

  it("still fetches its tail ONCE, for the summary line it shows", () => {
    // The tile is static, not empty: it must still render a summary.
    expect(code(read(TILE))).toContain("useGetExecutionSessionTail");
  });
});

describe("the grid holds no liveness at all", () => {
  it("declares no stream, no timer and no live-step selection", () => {
    const c = code(read(GRID));
    for (const pat of ["useStreamExecutionEvents", "setInterval", "setTimeout", "liveStepId", "liveStream"]) {
      expect(c, `${GRID} still contains ${pat}`).not.toContain(pat);
    }
  });

  it("no tile prop exists to make one live", () => {
    // `liveStream` is gone from the tile's props, so the grid cannot ask for liveness even
    // by accident — a prop is the seam that would let it come back.
    expect(code(read(TILE))).not.toContain("liveStream");
  });

  it("the `suspendedStepId` handoff is gone with it", () => {
    // It existed ONLY to withhold liveness from the expanded step. With no liveness in the
    // grid there is nothing to withhold, so the prop is dead — and a dead prop that looks
    // meaningful is how the next reader concludes the grid still streams.
    expect(code(read(GRID))).not.toContain("suspendedStepId");
  });
});

describe("liveness moved to the one place that should have it", () => {
  it("the expanded modal streams exactly one execution", () => {
    const c = code(read(MODAL));
    expect(c).toContain("useStreamExecutionEvents");
    // One stream, not one per tile: a single call site in the modal.
    expect(c.match(/useStreamExecutionEvents\(/g)?.length).toBe(1);
  });

  it("the route no longer passes a suspended step to the grid", () => {
    const c = code(read("src/routes/workflows_.$id_.runs.$runId.tsx"));
    expect(c).not.toContain("suspendedStepId");
  });
});
