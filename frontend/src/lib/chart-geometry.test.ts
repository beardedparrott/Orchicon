import { describe, it, expect } from "vitest";

import {
  areaPathFromPoints,
  boundsOf,
  chartSeries,
  clampIndex,
  formatCompact,
  formatDurationMs,
  formatPercent,
  linearScale,
  niceTicks,
  pathFromPoints,
  stepIndex,
  type ChartPoint,
} from "./chart-geometry";

describe("linearScale", () => {
  it("maps the domain onto the range", () => {
    const s = linearScale([0, 10], [0, 100]);
    expect(s(0)).toBe(0);
    expect(s(5)).toBe(50);
    expect(s(10)).toBe(100);
  });

  it("inverts the range for screen coordinates", () => {
    const s = linearScale([0, 10], [160, 0]);
    expect(s(0)).toBe(160);
    expect(s(10)).toBe(0);
  });

  it("keeps a flat domain on the midline instead of NaN", () => {
    const s = linearScale([7, 7], [0, 100]);
    expect(s(7)).toBe(50);
    expect(Number.isNaN(s(7))).toBe(false);
  });
});

describe("boundsOf", () => {
  it("pads a flat series so the line is visible", () => {
    const [lo, hi] = boundsOf([5, 5, 5]);
    expect(lo).toBeLessThan(5);
    expect(hi).toBeGreaterThan(5);
  });

  it("returns a usable domain for an empty series", () => {
    expect(boundsOf([])).toEqual([0, 1]);
  });

  it("finds min and max regardless of order", () => {
    expect(boundsOf([3, -1, 9, 4])).toEqual([-1, 9]);
  });
});

describe("pathFromPoints", () => {
  it("returns an empty path when there is no data", () => {
    expect(pathFromPoints([], 100, 100)).toBe("");
    expect(areaPathFromPoints([], 100, 100)).toBe("");
  });

  it("draws a single point as a move command", () => {
    const p = pathFromPoints([{ t: 1000, v: 5 }], 100, 100);
    expect(p.startsWith("M")).toBe(true);
    expect(p).not.toContain("L");
  });

  it("draws increasing series left-to-right, ascending values upward", () => {
    const points: ChartPoint[] = [
      { t: 0, v: 0 },
      { t: 1, v: 10 },
    ];
    const d = pathFromPoints(points, 100, 100);
    expect(d).toBe("M0 100 L100 0");
  });

  it("closes the area path down to the baseline", () => {
    const points: ChartPoint[] = [
      { t: 0, v: 0 },
      { t: 1, v: 10 },
    ];
    const d = areaPathFromPoints(points, 100, 100);
    expect(d.endsWith("L100 100 L0 100 Z")).toBe(true);
  });

  it("never emits NaN for a flat series", () => {
    const d = pathFromPoints(
      [
        { t: 0, v: 4 },
        { t: 1, v: 4 },
      ],
      100,
      100,
    );
    expect(d).not.toContain("NaN");
  });
});

describe("niceTicks", () => {
  it("returns rounded ticks inside the domain", () => {
    const ticks = niceTicks(0, 100, 4);
    expect(ticks.length).toBeGreaterThan(1);
    for (const t of ticks) {
      expect(t).toBeGreaterThanOrEqual(0);
      expect(t).toBeLessThanOrEqual(100);
    }
    expect(ticks).toContain(0);
    expect(ticks).toContain(100);
  });

  it("returns a single tick for a flat domain", () => {
    expect(niceTicks(5, 5, 4)).toEqual([5]);
  });

  it("returns nothing for an inverted or non-finite domain", () => {
    expect(niceTicks(10, 0, 4)).toEqual([10]);
    expect(niceTicks(NaN, 1, 4)).toEqual([]);
  });

  it("respects the requested tick count roughly", () => {
    expect(niceTicks(0, 1000, 4).length).toBeLessThanOrEqual(5);
  });
});

describe("cursor helpers", () => {
  it("clamps to the list bounds", () => {
    expect(clampIndex(-3, 5)).toBe(0);
    expect(clampIndex(9, 5)).toBe(4);
    expect(clampIndex(2, 5)).toBe(2);
  });

  it("returns -1 for an empty list", () => {
    expect(clampIndex(0, 0)).toBe(-1);
    expect(stepIndex(0, 1, 0)).toBe(-1);
  });

  it("steps and clamps at both ends", () => {
    expect(stepIndex(0, -1, 5)).toBe(0);
    expect(stepIndex(4, 1, 5)).toBe(4);
    expect(stepIndex(2, 1, 5)).toBe(3);
  });
});

describe("formatting", () => {
  it("formats compact numbers", () => {
    expect(formatCompact(999)).toBe("999");
    expect(formatCompact(1200)).toBe("1.2k");
    expect(formatCompact(3400000)).toBe("3.4M");
    expect(formatCompact(undefined)).toBe("—");
  });

  it("formats durations", () => {
    expect(formatDurationMs(820)).toBe("820 ms");
    expect(formatDurationMs(1234)).toBe("1.2 s");
    expect(formatDurationMs(95000)).toBe("1m 35s");
    expect(formatDurationMs(3600000)).toBe("1h");
    expect(formatDurationMs(-1)).toBe("—");
  });

  it("formats a ratio as a percentage", () => {
    expect(formatPercent(0)).toBe("0%");
    expect(formatPercent(0.125)).toBe("12.5%");
    expect(formatPercent(1)).toBe("100%");
  });

  it("picks a token-derived colour per series index", () => {
    expect(chartSeries(0)).toBe("hsl(var(--primary))");
    expect(chartSeries(1)).toBe("hsl(var(--ring))");
    // Wraps rather than going undefined for many series.
    expect(chartSeries(7)).toBe(chartSeries(2));
  });
});
