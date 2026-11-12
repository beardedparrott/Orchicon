
import { describe, it, expect } from "vitest";

import { axisTickTimes, type ChartPoint } from "./chart-geometry";

function series(times: number[]): ChartPoint[] {
  return times.map((t, i) => ({ t, v: i }));
}

describe("axisTickTimes", () => {
  it("labels nothing for an empty series", () => {
    expect(axisTickTimes([])).toEqual([]);
  });

  it("anchors the axis on the first and last sample", () => {
    // The X axis is a claim about the range of the data. If it labelled a
    // moment the series does not span, the axis would overstate the window.
    const points = series([1000, 2000, 3000, 4000, 5000]);
    const ticks = axisTickTimes(points);
    expect(ticks[0]).toBe(1000);
    expect(ticks[ticks.length - 1]).toBe(5000);
  });

  it("returns evenly spaced instants", () => {
    const ticks = axisTickTimes(series([0, 1000, 2000, 3000, 4000]), 4);
    expect(ticks).toEqual([0, 1000, 2000, 3000, 4000]);
  });

  it("never returns more than maxSlots + 1 labels", () => {
    // Beyond five labels the timestamps collide at dashboard widths, which is
    // the unreadable axis this whole change exists to remove.
    const many = series(Array.from({ length: 500 }, (_, i) => i * 1000));
    expect(axisTickTimes(many, 4)).toHaveLength(5);
    expect(axisTickTimes(many, 2)).toHaveLength(3);
  });

  it("labels a single sample once instead of dividing by zero", () => {
    expect(axisTickTimes(series([1234]))).toEqual([1234]);
  });

  it("treats a zero-length window as one instant", () => {
    // Every sample at the same millisecond would otherwise produce NaN ticks.
    const ticks = axisTickTimes(series([777, 777, 777]));
    expect(ticks).toEqual([777]);
  });

  it("keeps slots within the number of samples", () => {
    // Three samples cannot honestly produce five distinct instants.
    const ticks = axisTickTimes(series([0, 10, 20]), 4);
    expect(ticks).toHaveLength(3);
    expect(ticks).toEqual([0, 10, 20]);
  });
});
