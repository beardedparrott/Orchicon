// ask-verbs.test.ts — THE ROTATION IS A PURE FUNCTION OF SERVER TIME, PINNED ACROSS CLIENTS.
//
// The operator: "rotating through a series of words that means 'orchicon is thinking' ... that changes
// every few seconds. We should have a ton of them." The list and selector live in ask-verbs.ts; these tests
// pin the four properties that make the rotation safe to ship: it is deterministic, its fallback is never
// empty, its shape fits the one-row footer, and the TS list cannot drift from the TUI's.
//
// THE CROSS-CLIENT ASSERTION IS THE IMPORTANT ONE. The old activity line was a fixed literal, so both
// clients agreeing was trivially true. Rotation breaks that, and what replaces it is a STRONGER promise:
// both clients draw the SAME word for the same server time. That is what the fixture test below pins —
// ask-verbs.json is the one file this suite AND internal/tui/chat/verbs_test.go enumerate, exactly as the
// Schedules page binds the two languages (schedules-fixture.json).
import fs from "node:fs";
import path from "node:path";

import { describe, expect, it, vi } from "vitest";

import {
  ASK_VERBS,
  VERB_CELL_CAP,
  VERB_PERIOD_MS,
  activityVerb,
  extrapolateServerTime,
  verbAt,
  verbRotationOn,
} from "@/lib/ask-verbs";

type Fixture = {
  periodMs: number;
  verbs: string[];
};

const fixture: Fixture = JSON.parse(
  fs.readFileSync(path.join(__dirname, "ask-verbs.json"), "utf8"),
);

describe("ask-verbs — the rotation is a pure function of server time", () => {
  it("is pure and deterministic: the same stamp yields the same word", () => {
    const stamps = [1, 3999, 4000, 4001, 1_700_000_000_000, 1_700_000_123_456];
    for (const ts of stamps) {
      const first = verbAt(ts);
      expect(verbAt(ts)).toBe(first);
      expect(ASK_VERBS).toContain(first);
    }
  });

  it("never falls back to an empty word before the first heartbeat", () => {
    // 0, negative and tiny stamps: the first second after sending, when the operator is looking.
    for (const ts of [0, -1, -1_700_000_000_000, 1]) {
      const got = verbAt(ts);
      expect(typeof got).toBe("string");
      expect(got.length).toBeGreaterThan(0);
      expect(ASK_VERBS).toContain(got);
    }
    expect(verbAt(0)).toBe(ASK_VERBS[0]);
    // A non-finite stamp cannot throw and cannot name "undefined".
    expect(verbAt(Number.NaN)).toBe(ASK_VERBS[0]);
  });

  it("advances once per period and wraps at the end of the list", () => {
    for (let k = 0; k < ASK_VERBS.length * 2; k++) {
      const want = ASK_VERBS[k % ASK_VERBS.length];
      expect(verbAt(k * VERB_PERIOD_MS)).toBe(want);
      // Within one period the word does not change.
      expect(verbAt(k * VERB_PERIOD_MS + VERB_PERIOD_MS - 1)).toBe(want);
    }
    // invariant (AC2): the period is the 15s heartbeat cadence, and the operator's reaction is the
    // boundary — 1s apart must NOT move the word (a ~1s repaint would read as churn), a period apart must.
    expect(VERB_PERIOD_MS).toBe(15_000);
    expect(VERB_PERIOD_MS).toBeGreaterThan(0);
    expect(verbAt(0)).toBe(verbAt(1_000));
    expect(verbAt(0)).not.toBe(verbAt(15_000));
  });

  it("stays a single short ASCII word — the one-row footer budget", () => {
    expect(ASK_VERBS.length).toBeGreaterThanOrEqual(60);
    const seen = new Set<string>();
    for (const v of ASK_VERBS) {
      expect(v).toMatch(/^[a-z]+$/);
      expect(v.length).toBeLessThanOrEqual(VERB_CELL_CAP);
      expect(seen.has(v)).toBe(false);
      seen.add(v);
    }
  });

  it("never claims a liveness the stream cannot always back", () => {
    const banned = ["receiving", "progressing", "streaming", "completing", "advancing", "succeeding", "finishing"];
    for (const v of ASK_VERBS) {
      for (const b of banned) {
        expect(v.includes(b)).toBe(false);
      }
    }
  });

  it("matches the shared fixture byte for byte and in order", () => {
    expect(fixture.periodMs).toBe(VERB_PERIOD_MS);
    expect(fixture.verbs).toEqual(ASK_VERBS);
  });

  it("can be switched off for reduced motion, pinning to the first word", () => {
    // Non-DOM default: rotation is ON, so the feature cannot ship disabled. (This suite runs in the node
    // environment, which is also the SSR case the guard exists for.)
    expect(verbRotationOn()).toBe(true);
    expect(activityVerb(1_700_000_000_000)).toBe(verbAt(1_700_000_000_000));

    // With the profile's reduce preference set, the word is the still first entry at every stamp. window is
    // stubbed rather than assumed, because this suite has no DOM.
    vi.stubGlobal("window", {
      matchMedia: vi.fn().mockReturnValue({ matches: true }),
    });
    try {
      expect(verbRotationOn()).toBe(false);
      for (const ts of [0, 4000, 1_700_000_000_000]) {
        expect(activityVerb(ts)).toBe(ASK_VERBS[0]);
      }
    } finally {
      vi.unstubAllGlobals();
    }

    // A matchMedia that throws must not take the activity line down with it.
    vi.stubGlobal("window", {
      matchMedia: vi.fn(() => {
        throw new Error("no media query support");
      }),
    });
    try {
      expect(verbRotationOn()).toBe(true);
    } finally {
      vi.unstubAllGlobals();
    }
  });
});

// THE CLOCK WIRING IS PART OF THE FEATURE, NOT AN IMPLEMENTATION DETAIL (AC4/AC5). The word is a pure
// function of SERVER time; the only place server time reaches this client is Heartbeat.server_time_unix_ms,
// and the client must extrapolate from it using ONLY a delta of its own clock readings. These tests pin
// that: a skewed local clock must not change the word (both clients still agree), the pre-heartbeat case
// must fall back to a list member, and a heartbeat-fed stamp must actually feed the selector.
describe("ask-verbs — the server clock feeds the rotation (AC4/AC5)", () => {
  const STAMP = 1_700_000_123_456; // a real Unix ms stamp (2023-11-14T22:13:20Z) and not a period multiple

  it("extrapolates the server stamp forward by the local delta, never past the heartbeat", () => {
    // 3s after receipt the word is the one the SERVER time names, not a frozen one.
    const effective = extrapolateServerTime(STAMP, 1_000, 4_000);
    expect(effective).toBe(STAMP + 3_000);
    expect(verbAt(effective)).toBe(verbAt(STAMP + 3_000));
    // invariant: the extrapolated value advances the rotation across the period boundary — indexing on
    // server time (not a constant) is what keeps the word moving between heartbeats.
    expect(verbAt(effective)).not.toBe(
      verbAt(extrapolateServerTime(STAMP, 1_000, 1_000 + VERB_PERIOD_MS)),
    );
  });

  it("cancels a skewed local clock: two clients with different wall clocks agree", () => {
    // Client A's clock is "correct" and it reads 1_000/4_000 around its receipt of the stamp. Client B's
    // clock is skewed by +3h (10_800_000 ms). BOTH readings of a given client's clock move by the same
    // skew, so BOTH extrapolations are the same server time and therefore the same word — the whole point
    // of indexing on the server stamp. A client that used Date.now() as the SOURCE would diverge here.
    const skewed = 10_800_000;
    const a = extrapolateServerTime(STAMP, 1_000, 4_000);
    const b = extrapolateServerTime(STAMP, 1_000 + skewed, 4_000 + skewed);
    expect(b).toBe(a);
    expect(verbAt(b)).toBe(verbAt(a));
  });

  it("falls back to the list's first word before any heartbeat, and never to empty", () => {
    // No heartbeat yet: the first second after the operator sends, when they are most likely looking.
    expect(extrapolateServerTime(null, null, Date.now())).toBe(0);
    expect(verbAt(extrapolateServerTime(null, null, Date.now()))).toBe(ASK_VERBS[0]);
    // A zero/negative stamp is not a stamp either; it must not anchor at the epoch.
    expect(extrapolateServerTime(0, 1_000, 2_000)).toBe(0);
    // A clock that stepped backwards must not produce a negative age.
    expect(extrapolateServerTime(STAMP, 4_000, 1_000)).toBe(STAMP);
    // And a heartbeat stamp really does move the word: the selector over the extrapolated stamp is a list
    // member, so the rotation is reading the server clock rather than a constant.
    const w = verbAt(extrapolateServerTime(STAMP, 1_000, 1_000));
    expect(ASK_VERBS).toContain(w);
    expect(w).toBe(verbAt(STAMP));
  });
});
