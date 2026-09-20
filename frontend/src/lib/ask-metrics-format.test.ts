// The Ask stat strip's pure half. These are the SAME cases the TUI's
// internal/tui/models_test.go asserts, so the two clients cannot drift apart on
// how they report a session.
import { describe, expect, it } from "vitest";

import { UsageRecord } from "@/api/gen/orchicon/api/v1/ai_gateway_pb";
import {
  cacheHitRatio,
  fmtCtx,
  fmtTokens,
  formatAskMetricsLine,
  formatAskMetricsStats,
  summarizeAskMetrics,
} from "@/lib/ask-metrics-format";

// usage builds a record with only the fields the strip reads. Records arrive
// newest-first, so the FIRST argument is the most recent turn.
function usage(r: Partial<UsageRecord>): UsageRecord {
  return new UsageRecord(r as never);
}

describe("summarizeAskMetrics", () => {
  it("sums the session totals and takes context from the NEWEST record", () => {
    const m = summarizeAskMetrics(
      [
        // newest turn: 500 uncached + 400 cached = 900 in context
        usage({ promptTokens: 500n, completionTokens: 100n, totalTokens: 600n, cacheReadTokens: 400n, cacheWriteTokens: 0n, costUsd: 0.005 }),
        // older turn
        usage({ promptTokens: 1000n, completionTokens: 200n, totalTokens: 1200n, cacheReadTokens: 800n, cacheWriteTokens: 100n, costUsd: 0.01 }),
      ],
      "orchicon/anthropic/claude-sonnet-4",
      200000,
    );
    expect(m.tokens).toBe(1800);
    expect(m.cacheRead).toBe(1200);
    expect(m.prompt).toBe(1500);
    expect(m.costUsd).toBeCloseTo(0.015, 6);
    // NOT the sum (900 + 1900) — the latest turn's input is the occupancy.
    expect(m.ctxUsed).toBe(900);
    expect(m.ctxWindow).toBe(200000);
    expect(m.have).toBe(true);
  });

  it("reports nothing when the session has no usage yet", () => {
    const m = summarizeAskMetrics([], "orchicon/a/b", 0);
    expect(m.have).toBe(false);
    expect(m.tokens).toBe(0);
    expect(m.ctxUsed).toBe(0);
  });
});

describe("fmtTokens / fmtCtx", () => {
  it("matches the Go formatter", () => {
    expect(fmtTokens(500)).toBe("500");
    expect(fmtTokens(124000)).toBe("124K");
    expect(fmtTokens(1200000)).toBe("1.2M");
  });

  it("shows occupancy against the window, and never fabricates one", () => {
    expect(fmtCtx(124000, 200000)).toBe("124K/200K");
    expect(fmtCtx(124000, 0)).toBe("124K");
  });
});

describe("cacheHitRatio", () => {
  it("is the cached share of the INPUT side", () => {
    expect(cacheHitRatio(940000, 260000)).toBe("78%");
    expect(cacheHitRatio(100, 0)).toBe("100%");
    expect(cacheHitRatio(0, 500)).toBe("0%");
  });

  it("is omitted (not 0%) when there is no input at all", () => {
    expect(cacheHitRatio(0, 0)).toBeNull();
  });
});

describe("formatAskMetricsLine", () => {
  it("reports the model, context, tokens, cache and cost", () => {
    const line = formatAskMetricsLine(
      summarizeAskMetrics(
        [
          usage({
            promptTokens: 260000n, completionTokens: 1000n, totalTokens: 1200000n,
            cacheReadTokens: 940000n, cacheWriteTokens: 0n, costUsd: 1.2345,
          }),
        ],
        "orchicon/anthropic/claude-sonnet-4",
        200000,
      ),
    );
    expect(line).toContain("orchicon/anthropic/claude-sonnet-4");
    expect(line).toContain("ctx 1.2M/200K"); // newest turn's input side
    expect(line).toContain("1.2M tok");
    expect(line).toContain("cache 78%");
    expect(line).toContain("(940K)");
    expect(line).toContain("$1.2345");
  });

  it("shows the model alone while usage is still pending", () => {
    expect(formatAskMetricsLine(summarizeAskMetrics([], "orchicon/a/b", 0))).toBe("orchicon/a/b");
  });

  it("is empty with no model and no usage", () => {
    expect(formatAskMetricsLine(summarizeAskMetrics([], "", 0))).toBe("");
  });
});

describe("formatAskMetricsStats", () => {
  // The composer renders the model as a clickable CHIP and these numbers as
  // plain text beside it, so the stats half must contain no model at all.
  it("omits the model (it is rendered as a chip, not text)", () => {
    const s = formatAskMetricsStats(
      summarizeAskMetrics(
        [usage({ promptTokens: 10n, completionTokens: 10n, totalTokens: 20n, cacheReadTokens: 0n, costUsd: 0.5 })],
        "orchicon/anthropic/claude-sonnet-4",
        1000,
      ),
    );
    expect(s).not.toContain("orchicon/anthropic/claude-sonnet-4");
    expect(s).toContain("ctx ");
    expect(s).toContain("20 tok");
    expect(s).toContain("$0.5000");
  });

  it("is empty until usage lands", () => {
    expect(formatAskMetricsStats(summarizeAskMetrics([], "a/b/c", 0))).toBe("");
  });

  // line == model + stats, so the chip tooltip still reads as one sentence.
  it("composes back into the flat line", () => {
    const m = summarizeAskMetrics([usage({ promptTokens: 10n, totalTokens: 10n, costUsd: 1 })], "x/y/z", 100);
    const stats = formatAskMetricsStats(m);
    expect(formatAskMetricsLine(m)).toBe(`x/y/z · ${stats}`);
  });
});
