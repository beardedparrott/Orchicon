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

  it("drives the CARD discovery read from the same cadence, so a missed ask event is recoverable", () => {
    // THE REPORTED BUG: "every time a card was passed, I had to hit refresh on the browser for it to
    // appear." `usePendingAsks` is the discovery half of the card path and had NO interval, so it read once
    // and never again — a card whose stream event was missed stayed invisible until a reload. The guard is
    // on the WIRING, because the mapping above can be perfectly correct while nothing calls it.
    expect(src).toContain("usePendingAsks(activeConvId ?? \"\", {");
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

// --- the CARD discovery read uses the same turn-driven cadence -------------------------------
//
// The operator, mid-session: "every time a card was passed, I had to hit refresh on the browser for it to
// appear."
//
// `usePendingAsks` is the discovery half of the card path — the half that exists so a card does not depend on
// having caught a live stream event. It had NO refetch interval at all, so it read once at mount and never
// again: an ask raised by a turn already in flight reached the client only if the socket delivered it, and
// when that arm missed nothing re-asked until a page reload re-ran the query. These pin the rule the fix
// depends on: while a turn is in flight the discovery read is on the same cadence as the transcript.
describe("card discovery cadence", () => {
  it("polls while a turn is in flight, so a missed stream event is not permanent", () => {
    expect(transcriptPollMs(true)).toBe(TRANSCRIPT_POLL_MS);
  });

  it("stops when no turn is in flight — no idle network churn", () => {
    expect(transcriptPollMs(false)).toBe(false);
  });

  it("is a positive interval, because a zero or negative one would disable polling entirely", () => {
    // `setInterval(fn, 0)` is not "poll as fast as possible" in every runtime, and a 0 value here would
    // silently reintroduce the bug: the read would never repeat.
    expect(TRANSCRIPT_POLL_MS).toBeGreaterThan(0);
  });
});
