// The GUI half of the rotating activity verb's CLOCK WIRING.
//
// The word itself is a pure function of server time (src/lib/ask-verbs.ts, pinned to the TUI's list by
// ask-verbs.json). What cannot be unit-tested from the library — and is exactly where the feature dies
// silently — is the ROUTE's wiring: if a heartbeat arm forgets to store server_time_unix_ms, the line is
// stuck on the fallback word forever; if the render reads Date.now() as the source, two clients with
// skewed clocks disagree, which is the one thing server-stamping exists to prevent.
//
// The repo idiom for route-level contracts with no jsdom is a source assertion (see
// -diff-rail-width-wiring.test.ts / -ask-compact-progress.test.ts), so this test reads the route source
// and asserts the two load-bearing facts: BOTH heartbeat arms feed the clock, and the render composes
// the word from the server-derived value.
import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";

const src = fs.readFileSync(path.join(__dirname, "ask-orchicon.tsx"), "utf8");

// Every heartbeat arm is a `chunk.event.case === "heartbeat"` branch. There are two — the dispatch stream
// and the re-dialled watch stream — and BOTH must feed the rotation's clock: a watcher that re-attached
// mid-turn is the client least likely to be holding a stamp.
function heartbeatArms(): string[] {
  const arms: string[] = [];
  const marker = 'chunk.event.case === "heartbeat"';
  let i = src.indexOf(marker);
  while (i !== -1) {
    // The arm's body: from the heartbeat case to the next `else if (chunk.event.case`.
    const next = src.indexOf("chunk.event.case", i + marker.length);
    arms.push(src.slice(i, next === -1 ? undefined : next));
    i = src.indexOf(marker, i + 1);
  }
  return arms;
}

describe("ask-orchicon rotating verb — the heartbeat feeds the clock (GUI half of AC4)", () => {
  it("has two heartbeat arms, and each records the server stamp with its receipt instant", () => {
    const arms = heartbeatArms();
    // Guards the guard: if the second arm is ever merged away, this test says so rather than passing
    // vacuously over one arm.
    expect(arms.length).toBe(2);
    for (const arm of arms) {
      // The stamp comes off the wire and is coerced (it is a bigint in the generated type).
      expect(arm).toContain("Number(chunk.event.value.serverTimeUnixMs)");
      // ...and is stored WITH the local instant it arrived, which is the delta's other half.
      expect(arm).toContain("serverTimeMs: stamp");
      expect(arm).toContain("serverTimeRecvAt: Date.now()");
      // A zero/absent stamp must not be stored (it would anchor the rotation at the epoch).
      expect(arm).toContain("stamp > 0");
    }
  });

  it("renders the word from the server-derived clock, not from Date.now()", () => {
    expect(src).toContain("Orchicon is {activityVerb(effectiveServerTimeMs)}");
    // effectiveServerTimeMs is the SERVER stamp extrapolated by a delta of our own clock readings — the
    // helper that guarantees a skewed local clock cancels out (both properties asserted in
    // ask-verbs.test.ts).
    expect(src).toContain("extrapolateServerTime(");
    expect(src).toContain("activeStream?.serverTimeMs ?? null");
    expect(src).toContain("activeStream?.serverTimeRecvAt ?? null");
    // The verb must never be rendered straight off a raw local clock.
    expect(src).not.toContain("activityVerb(Date.now())");
  });

  it("keeps a repaint tick alive while the rotation is on screen", () => {
    // The verb is animation: without a tick between the 15s heartbeats it would freeze for a quarter of a
    // minute. The existing useNow ticker is widened to also run while isThinking.
    expect(src).toMatch(/isThinking\s*\?\s*1000\s*:\s*false/);
  });
});
