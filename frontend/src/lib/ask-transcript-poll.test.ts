import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";
import { TRANSCRIPT_POLL_MS, transcriptPollMs } from "./ask-transcript-poll";

// ask-transcript-poll.test.ts — THE CADENCE, AND THE WIRING THAT WAS WRONG.
//
// The cadence itself is a one-line mapping, so the interesting assertion is the SECOND half: the
// route cannot be rendered by this setup, so a caller that went back to gating on this client's own
// stream slot would leave a mapping test green while reintroducing the reported bug — the ledger
// frozen, the ticker running, "the timer just continues counting up and never resets on the next
// newest call". So the route is read as TEXT, the idiom mode-toggle-source.test.ts already uses here
// for exactly this reason (the wiring a unit test cannot see).

describe("transcriptPollMs", () => {
  it("polls while a turn is in flight", () => {
    expect(transcriptPollMs(true)).toBe(TRANSCRIPT_POLL_MS);
    expect(TRANSCRIPT_POLL_MS).toBeGreaterThan(0);
  });

  it("does not poll once the turn settles — no idle network churn", () => {
    expect(transcriptPollMs(false)).toBe(false);
  });
});

describe("the transcript query in ask-orchicon", () => {
  const src = fs.readFileSync(
    path.join(__dirname, "..", "routes", "ask-orchicon.tsx"),
    "utf8",
  );

  it("drives the cadence from the turn, not from this client's stream slot", () => {
    expect(src).toContain("@/lib/ask-transcript-poll");
    expect(src).toContain("refetchInterval: transcriptPollMs(turnInFlight)");
  });

  it("no longer gates the poll on isStreaming — the half-turn gate that caused it", () => {
    // THE REGRESSION, caught literally: `{ refetchInterval: isStreaming ? 2000 : false }`.
    expect(src).not.toContain("refetchInterval: isStreaming ? 2000 : false");
    expect(src).not.toContain("transcriptPollMs(isStreaming)");
  });

  it("declares the union ABOVE the query that reads it (the TDZ rule this route documents)", () => {
    // A `const` used above its declaration is a TDZ error the production build rejects, which is
    // why the union had to be hoisted rather than referenced in place.
    const union = src.indexOf("  const turnInFlight = isStreaming || serverTurnInFlight;");
    const query = src.indexOf("refetchInterval: transcriptPollMs(turnInFlight)");
    expect(union).toBeGreaterThan(-1);
    expect(query).toBeGreaterThan(-1);
    expect(union).toBeLessThan(query);
  });
});
