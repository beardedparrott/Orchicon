// -ask-orchicon.activity-line.guard.test.ts — THE ROUTE FACTS THAT LIVE ONLY IN THE ROUTE.
//
// The `-` PREFIX IS LOAD-BEARING (TanStack Router's routeFileIgnorePrefix, the convention every
// other route-side test already follows: -ask-compact-progress, -ask-verb-rotation-wiring,
// -diff-rail-width-wiring, -settings-defaults, -signup, -workers-new.runtime-ref). Without it the
// router plugin treats this file as a ROUTE and prints
// "Route file ... does not export a Route. This file will not be included in the route tree."
// on every `vite build` / dev-server start: a warning this repo had zero of before the file
// existed, and one that makes the real signal unreadable. The suite itself is unaffected either
// way (vitest collects both names) — this is the build, not the test, that cares.
//
// The route cannot be rendered by this repo's test setup (it is a 4000-line TanStack route with
// providers, sockets and queries), so the three facts that exist ONLY as wiring — the gate, the
// turn-in-flight union, and the two heartbeat arms — are pinned as source assertions. This is the
// idiom -ask-verb-rotation-wiring.test.ts and RuntimeImageFailureAlert.test.tsx already use.
import fs from "node:fs";
import path from "node:path";

import { describe, expect, it } from "vitest";

const SRC = fs.readFileSync(path.join(__dirname, "ask-orchicon.tsx"), "utf8");

// Every heartbeat arm is a `chunk.event.case === "heartbeat"` branch. There are two — the dispatch
// stream and the re-dialled watch stream — and BOTH must feed the rotation's clock: a watcher that
// re-attached mid-turn is the client LEAST likely to be holding a stamp.
function heartbeatArms(): string[] {
  const arms: string[] = [];
  const marker = 'chunk.event.case === "heartbeat"';
  let i = SRC.indexOf(marker);
  while (i !== -1) {
    const next = SRC.indexOf("chunk.event.case", i + marker.length);
    arms.push(SRC.slice(i, next === -1 ? undefined : next));
    i = SRC.indexOf(marker, i + 1);
  }
  return arms;
}

describe("ask-orchicon activity line — the route wiring", () => {
  it("AC1 — the OLD gate is gone (the line no longer dies at the first token)", () => {
    // `groupedStream.length === 0` legitimately remains in displayMessages; assert the CONJUNCTION
    // with isThinking, which is the deleted gate — the exact expression that was the bug.
    expect(SRC).not.toContain("isThinking && groupedStream.length === 0");
    expect(SRC).not.toContain("visible until any streaming content arrives");
  });

  it("AC1/AC7 — the new gate is the turn-in-flight union, and the line renders from it", () => {
    expect(SRC).toContain("const turnInFlight = isStreaming || serverTurnInFlight;");
    expect(SRC).toContain("activityLine !== null &&");
    // The line's decision is not inlined in the route; it is the tested lib function.
    expect(SRC).toContain("activityLineFor(activityLineInput)");
  });

  it("AC7 — the 1s ticker runs for the WHOLE turn, not only before the first token", () => {
    expect(SRC).toContain("useNow(turnInFlight ? 1000 : false)");
    // The old condition froze the repaint the moment content arrived — the line would be correct
    // but motionless, which is the same class of bug.
    expect(SRC).not.toContain('(isStreaming && reconnecting && turnProgressing) || isThinking ? 1000 : false');
  });

  it("AC3 — BOTH heartbeat arms feed the rotation's clock", () => {
    const arms = heartbeatArms();
    // Guards the guard: if the second arm is ever merged away, this says so rather than passing
    // vacuously over one arm.
    expect(arms.length).toBe(2);
    for (const arm of arms) {
      expect(arm).toContain("Number(chunk.event.value.serverTimeUnixMs)");
      expect(arm).toContain("serverTimeMs: stamp");
      expect(arm).toContain("serverTimeRecvAt: Date.now()");
      expect(arm).toContain("stamp > 0");
    }
    // ...and the verb comes off the SERVER-derived clock, never a raw local one.
    expect(SRC).toContain("extrapolateServerTime(");
    expect(SRC).not.toContain("activityVerb(Date.now())");
  });

  it("the counter's data source is the ALREADY-POLLED transcript page", () => {
    expect(SRC).toContain("toolCallsFromMessages(");
    expect(SRC).toContain("refetchInterval: isStreaming ? 2000 : false");
  });

  it("AC8 — the old inline bubble tokens moved into the component, they were not reinvented", () => {
    // The route no longer draws its own sky bubble; the shell lives in ActivityLine.tsx (asserted
    // there). A second copy here would be a second source of truth for the same styling.
    expect(SRC).not.toContain("bg-sky-50/20");
  });
});
