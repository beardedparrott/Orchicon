import { describe, expect, it } from "vitest";
import { formatCompactTokens, summarizeUsage } from "./executionUsage";
import type { UsageRecord } from "@/api/gen/orchicon/api/v1/ai_gateway_pb";

function record(partial: Partial<UsageRecord>): UsageRecord {
  return {
    promptTokens: 0n,
    completionTokens: 0n,
    reasoningTokens: 0n,
    cacheReadTokens: 0n,
    costUsd: 0,
    ...partial,
  } as unknown as UsageRecord;
}

describe("summarizeUsage", () => {
  it("sums cost and takes the peak fresh working set (cache excluded)", () => {
    const out = summarizeUsage([
      record({
        promptTokens: 1000n,
        completionTokens: 200n,
        reasoningTokens: 100n,
        cacheReadTokens: 50000n,
        costUsd: 0.01,
      }),
      record({
        promptTokens: 300n,
        completionTokens: 100n,
        costUsd: 0.0025,
      }),
    ]);
    // Peak fresh row is 1000+200+100 = 1300, NOT inflated by the 50k cache.
    expect(out.workingSet).toBe(1300);
    expect(out.cost).toBeCloseTo(0.0125);
    expect(out.hasRecords).toBe(true);
  });
  it("reports no records on empty input", () => {
    expect(summarizeUsage([])).toEqual({
      workingSet: 0,
      cost: 0,
      hasRecords: false,
    });
    expect(summarizeUsage(undefined).hasRecords).toBe(false);
  });
});

describe("formatCompactTokens", () => {
  it("compacts thousands and millions", () => {
    expect(formatCompactTokens(45)).toBe("45");
    expect(formatCompactTokens(1200)).toBe("1.2k");
    expect(formatCompactTokens(45000)).toBe("45k");
    expect(formatCompactTokens(3100000)).toBe("3.1M");
  });
});
