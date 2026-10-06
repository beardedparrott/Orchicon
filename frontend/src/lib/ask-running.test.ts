import { describe, expect, it } from "vitest";
import { runningConvIds } from "./ask-running";

// The bug this pins: the rail row read the POLLED turn_in_flight field ALONE, so a conversation THIS client
// was actively streaming still read as idle on the row beside it.

describe("runningConvIds", () => {
  it("marks a conversation THIS client is streaming, with no server row", () => {
    // The operator's case: the local slot is live, the polled list has not caught up (turnInFlight absent).
    const ids = runningConvIds([{ id: "c1" }], { c1: { isStreaming: true } });
    expect(ids.has("c1")).toBe(true);
  });

  it("marks a turn started in the OTHER client (server flag, no local slot)", () => {
    // AC 13: the union must not regress into a local-only check.
    const ids = runningConvIds([{ id: "c1", turnInFlight: true }], {});
    expect(ids.has("c1")).toBe(true);
  });

  it("does NOT mark an idle conversation, whatever the two halves are absent of", () => {
    const ids = runningConvIds([{ id: "c1" }], { c1: { isStreaming: false } });
    expect(ids.has("c1")).toBe(false);
  });

  it("marks a brand-new local-only turn that is not in the list yet", () => {
    const ids = runningConvIds([], { c9: { isStreaming: true } });
    expect(ids.has("c9")).toBe(true);
  });

  it("tolerates a missing list or stream map", () => {
    expect(runningConvIds(undefined, undefined).size).toBe(0);
    expect(runningConvIds(null, null).size).toBe(0);
  });
});
