// ask-activity-notice.test.ts — THE REGRESSION AND THE PRECEDENCE, asserted without a DOM.
//
// The route cannot be rendered by this repo's test setup, so the DECISION function is where the logic
// lives (src/lib/ask-activity-notice.ts) and this is where it is pinned. Two blocks here must never be
// skipped: the survives-first-content case (AC1 — the whole point) and the escalation ABSENCE (AC5).
import { describe, expect, it, vi } from "vitest";

import { ASK_VERBS, VERB_PERIOD_MS, activityVerb } from "@/lib/ask-verbs";
import {
  NOTICE_WARN_AFTER_MS,
  activityLineAnnouncement,
  activityLineFor,
  activityNoticeText,
  fitActivityNotice,
  silentFor,
  type ActivityLineInput,
} from "@/lib/ask-activity-notice";

const BASE: ActivityLineInput = {
  convId: "c1",
  turnInFlight: true,
  reconnecting: false,
  lastActivityMs: 1_000,
  effectiveServerTimeMs: 0,
  nowMs: 2_000,
  toolCalls: [],
  widthPx: 0,
  cardPending: false,
};

describe("ask-activity-notice — the line survives first content", () => {
  it("AC1 — a turn in flight with text ALREADY streamed still renders the line", () => {
    // The old gate was `isThinking && groupedStream.length === 0`. Model "content already arrived" as a
    // turn that is still in flight with an AGE ELAPSED — the state that used to render nothing at all.
    const line = activityLineFor({ ...BASE, lastActivityMs: 1_000, nowMs: 5_000 });
    expect(line).not.toBeNull();
    expect(line!.startsWith("Orchicon is ")).toBe(true);
  });

  it("AC1 — and it carries the COUNTER in exactly that state (impossible under the old gate)", () => {
    const line = activityLineFor({
      ...BASE,
      lastActivityMs: 1_000,
      nowMs: 5_000,
      toolCalls: [{ toolName: "write", atMs: 4_500 }],
    });
    expect(line).toContain("1 modify · newest call 1s ago");
    expect(line!.startsWith("Orchicon is ")).toBe(true);
  });

  it("AC7 — clears when the turn ends (turnInFlight false)", () => {
    expect(activityLineFor({ ...BASE, turnInFlight: false })).toBeNull();
  });

  it("AC7 — clears when the conversation changes (no conversation, no line)", () => {
    expect(activityLineFor({ ...BASE, convId: "" })).toBeNull();
  });
});

describe("ask-activity-notice — the rotation", () => {
  it("AC3 — same server time, same word (deterministic)", () => {
    const a = activityLineFor({ ...BASE, effectiveServerTimeMs: 8_000 });
    const b = activityLineFor({ ...BASE, effectiveServerTimeMs: 8_000 });
    expect(a).toBe(b);
  });

  it("AC3 — a period apart rotates the word, from the SHARED selector", () => {
    // invariant: a full period apart rotates the word, and the word comes from the SHARED selector
    // (ASK_VERBS / VERB_PERIOD_MS), not a literal — the two clients cannot rotate at different speeds.
    const t0 = 8_000;
    const t1 = 8_000 + VERB_PERIOD_MS; // VERB_PERIOD_MS apart
    expect(activityVerb(t0)).not.toBe(activityVerb(t1));
    expect(activityLineFor({ ...BASE, effectiveServerTimeMs: t0 })).toContain(
      ASK_VERBS[Math.floor(t0 / VERB_PERIOD_MS) % ASK_VERBS.length],
    );
    expect(activityLineFor({ ...BASE, effectiveServerTimeMs: t1 })).toContain(
      ASK_VERBS[Math.floor(t1 / VERB_PERIOD_MS) % ASK_VERBS.length],
    );
  });

  it("AC9 — reduced motion freezes the word but the line is still present", () => {
    vi.stubGlobal("window", { matchMedia: () => ({ matches: true }) });
    try {
      const lines = [0, 4_000, 1_700_000_000_000].map((ts) =>
        activityLineFor({ ...BASE, effectiveServerTimeMs: ts }),
      );
      for (const l of lines) {
        expect(l).not.toBeNull();
        expect(l).toContain(`Orchicon is ${ASK_VERBS[0]}`);
      }
      expect(new Set(lines).size).toBe(1);
    } finally {
      vi.unstubAllGlobals();
    }
  });

  it("AC9 — the announcement does NOT rotate (no screen-reader machine gun)", () => {
    const a = activityLineAnnouncement({ ...BASE, effectiveServerTimeMs: 0 });
    const b = activityLineAnnouncement({ ...BASE, effectiveServerTimeMs: 4_000_000 });
    expect(a).toBe(b);
    expect(a).toBe("Orchicon is working");
  });
});

describe("ask-activity-notice — precedence", () => {
  it("AC4 — reconnecting outranks the line (null, not a counter)", () => {
    const input = {
      ...BASE,
      reconnecting: true,
      toolCalls: [{ toolName: "write", atMs: 1_500 }],
    };
    // The existing "Connection interrupted — still working…" / "Turn stalled — …" banner keeps its
    // slot: a counter that outranks "the connection is gone" would claim a liveness the plane cannot
    // deliver. turnProgressing === false (the stalled state) is carried by `reconnecting` in the
    // route — `isStreaming && reconnecting` gates the banner — so this case covers it too.
    expect(activityLineFor(input)).toBeNull();
  });

  it("AC5 — escalation outranks the counter, and the counter is ABSENT not merely outranked", () => {
    const fiveWrites = Array.from({ length: 5 }, (_, i) => ({
      toolName: "write",
      atMs: 55_000 + i,
    }));
    const line = activityLineFor({
      ...BASE,
      lastActivityMs: 0,
      nowMs: 60_000,
      toolCalls: fiveWrites,
    })!;
    expect(line).toContain("no output for 60s");
    expect(line).not.toContain("modify");
    // ...and the re-dial band at 35s names the watchdog's verdict.
    expect(
      activityLineFor({ ...BASE, lastActivityMs: 0, nowMs: 40_000, toolCalls: fiveWrites }),
    ).toContain("the stream will re-attach if it stays silent");
  });

  it("AC5 — the 25s boundary: the counter is present just below it and gone at it", () => {
    const calls = [{ toolName: "write", atMs: 1 }];
    const justUnder = activityLineFor({
      ...BASE,
      lastActivityMs: 0,
      nowMs: NOTICE_WARN_AFTER_MS - 1,
      toolCalls: calls,
    })!;
    expect(justUnder).toContain("modify");
    const atWarn = activityLineFor({
      ...BASE,
      lastActivityMs: 0,
      nowMs: NOTICE_WARN_AFTER_MS,
      toolCalls: calls,
    })!;
    expect(atWarn).not.toContain("modify");
    expect(atWarn).toContain(`no output for ${NOTICE_WARN_AFTER_MS / 1000}s`);
  });

  it("permission-denied — a pending card drops the counter but keeps the line", () => {
    const input = {
      ...BASE,
      cardPending: true,
      toolCalls: [{ toolName: "write", atMs: 1_500 }],
    };
    const line = activityLineFor(input)!;
    expect(line).not.toBeNull();
    expect(line).not.toContain("modify");
    expect(activityLineAnnouncement(input)).toContain("permission request is waiting");
  });

  it("the age band only fires once the silence is worth stating", () => {
    // silent <= 0 or < 1s: the bare line, never an absurd "0s ago".
    expect(activityLineFor({ ...BASE, lastActivityMs: null, nowMs: 5_000 })).not.toContain(
      "last activity",
    );
    expect(
      activityLineFor({ ...BASE, lastActivityMs: 4_999, nowMs: 5_000 }),
    ).not.toContain("last activity");
    expect(
      activityLineFor({ ...BASE, lastActivityMs: 1_000, nowMs: 5_000 }),
    ).toContain("last activity 4s ago");
  });

  it("silentFor clamps, never a negative age", () => {
    expect(silentFor(null, 1_000)).toBe(0);
    expect(silentFor(2_000, 1_000)).toBe(0);
    expect(silentFor(1_000, 4_000)).toBe(3_000);
  });
});

describe("ask-activity-notice — resize degrades like the TUI", () => {
  const verb = "Orchicon is thinking…";

  it("width 0 is UNBOUNDED (not yet measured), and a fitting line is untouched", () => {
    const line = `${verb} · 3 modifies · 1 read · newest call 4s ago`;
    expect(fitActivityNotice(line, verb, 0)).toBe(line);
    expect(fitActivityNotice(line, verb, 1_000)).toBe(line);
  });

  it("STEP 1 — the summary goes first", () => {
    const line = `${verb} · 3 modifies · 1 read · newest call 4s ago`;
    expect(fitActivityNotice(line, verb, [...verb].length)).toBe(verb);
  });

  it("STEP 2 — the escalation band goes next, the verb survives", () => {
    const line = `${verb} · no output for 35s — the stream will re-attach if it stays silent`;
    expect(fitActivityNotice(line, verb, [...verb].length)).toBe(verb);
  });

  it("STEP 3 — narrower than the verb: a non-empty rune prefix, never empty", () => {
    const out = fitActivityNotice(verb, verb, 5);
    expect(out.length).toBeGreaterThan(0);
    expect(verb.startsWith(out)).toBe(true);
    expect([...out].length).toBe(5);
    expect(fitActivityNotice(verb, verb, -1)).toBe(verb); // still unbounded
  });

  it("a measured narrow pane (widthPx) drops the counter before the verb", () => {
    const line = activityLineFor({
      ...BASE,
      lastActivityMs: 1_000,
      nowMs: 5_000,
      toolCalls: [{ toolName: "write", atMs: 4_500 }],
      widthPx: 0,
    })!;
    expect(line).toContain("modify");
    // 8 px per cell: 160 px is 20 cells — enough for the verb, not for the summary.
    const narrow = activityLineFor({
      ...BASE,
      lastActivityMs: 1_000,
      nowMs: 5_000,
      toolCalls: [{ toolName: "write", atMs: 4_500 }],
      widthPx: 160,
    })!;
    expect(narrow).not.toContain("modify");
    expect(narrow.startsWith("Orchicon is ")).toBe(true);
  });

  it("activityNoticeText is total over its bands", () => {
    expect(activityNoticeText(0, 0, "", 0)).toBe(`Orchicon is ${ASK_VERBS[0]}…`);
    expect(activityNoticeText(5_000, 0, "1 read · newest call 1s ago", 0)).toBe(
      `Orchicon is ${ASK_VERBS[0]}… · 1 read · newest call 1s ago`,
    );
    expect(activityNoticeText(30_000, 0, "1 read · newest call 1s ago", 0)).toBe(
      `Orchicon is ${ASK_VERBS[0]}… · no output for 30s`,
    );
  });

  it("the announcement reports counted work, and no counters when there is none", () => {
    expect(activityLineAnnouncement(BASE)).toBe("Orchicon is working");
    expect(
      activityLineAnnouncement({ ...BASE, toolCalls: [{ toolName: "bash", atMs: 1_500 }] }),
    ).toBe("Orchicon is working · 1 bash");
  });
});
