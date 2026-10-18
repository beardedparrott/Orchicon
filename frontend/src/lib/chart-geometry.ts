// Pure chart geometry — no React, no DOM.
//
// The Telemetry page's graphs are hand-rolled SVG (see
// components/ui/chart.tsx) because the repo carries no charting dependency
// and eight hand-tuned themes with no --chart-* variables. Keeping the maths
// here means the parts that are easy to get subtly wrong (flat series, empty
// input, tick rounding, keyboard cursor bounds) are unit-testable without a
// renderer.

export interface ChartPoint {
  /** Timestamp, epoch milliseconds. */
  t: number;
  /** Sample value. */
  v: number;
}

/** Colour slots for multi-series charts, resolved from existing theme tokens. */
export const CHART_SERIES_VARS = [
  "--primary",
  "--ring",
  "--destructive",
  "--muted-foreground",
  "--accent-foreground",
] as const;

/** chartSeries returns a token-derived stroke colour for series `idx`. */
export function chartSeries(idx: number): string {
  const token = CHART_SERIES_VARS[Math.abs(idx) % CHART_SERIES_VARS.length];
  return `hsl(var(${token}))`;
}

/**
 * linearScale maps a value domain onto a pixel range. A degenerate domain
 * (min === max, e.g. a flat series) maps everything to the middle of the
 * range so the line is drawn across the centre instead of collapsing onto an
 * edge or producing NaN.
 */
export function linearScale(
  domain: [number, number],
  range: [number, number],
): (v: number) => number {
  const [dMin, dMax] = domain;
  const [rMin, rMax] = range;
  const dSpan = dMax - dMin;
  if (!Number.isFinite(dSpan) || dSpan === 0) {
    const mid = rMin + (rMax - rMin) / 2;
    return () => mid;
  }
  return (v: number) => rMin + ((v - dMin) / dSpan) * (rMax - rMin);
}

/**
 * boundsOf returns the [min, max] value bounds for a series. A single point
 * or an all-equal series is padded so the line does not sit on an edge.
 */
export function boundsOf(values: number[]): [number, number] {
  if (values.length === 0) return [0, 1];
  let min = values[0];
  let max = values[0];
  for (const v of values) {
    if (!Number.isFinite(v)) continue;
    if (v < min) min = v;
    if (v > max) max = v;
  }
  if (min === max) {
    const pad = Math.abs(min) > 0 ? Math.abs(min) * 0.1 : 1;
    return [min - pad, max + pad];
  }
  return [min, max];
}

function round(n: number): number {
  return Math.round(n * 100) / 100;
}

/**
 * pathFromPoints builds an SVG path ("M x y L x y …") for the series inside a
 * width × height box. Points are ordered by time; returns "" for no data.
 */
export function pathFromPoints(points: ChartPoint[], width: number, height: number): string {
  if (points.length === 0) return "";
  const [vMin, vMax] = boundsOf(points.map((p) => p.v));
  const tMin = points[0].t;
  const tMax = points[points.length - 1].t;
  const x = linearScale([tMin, tMax], [0, width]);
  const y = linearScale([vMin, vMax], [height, 0]);
  return points
    .map((p, i) => `${i === 0 ? "M" : "L"}${round(x(p.t))} ${round(y(p.v))}`)
    .join(" ");
}

/**
 * areaPathFromPoints is pathFromPoints closed down to the baseline, for the
 * filled area under a time-series line.
 */
export function areaPathFromPoints(
  points: ChartPoint[],
  width: number,
  height: number,
): string {
  const line = pathFromPoints(points, width, height);
  if (!line) return "";
  const x = linearScale(
    [points[0].t, points[points.length - 1].t],
    [0, width],
  );
  const firstX = round(x(points[0].t));
  const lastX = round(x(points[points.length - 1].t));
  return `${line} L${lastX} ${height} L${firstX} ${height} Z`;
}

/**
 * niceTicks returns human-friendly axis values within [min, max] (1/2/5 ×
 * 10^n steps), at most `count` of them.
 */
export function niceTicks(min: number, max: number, count = 4): number[] {
  if (!Number.isFinite(min) || !Number.isFinite(max) || count <= 0) return [];
  if (max <= min) return [min];
  const span = max - min;
  const raw = span / count;
  const magnitude = Math.pow(10, Math.floor(Math.log10(raw)));
  // Smallest 1/2/5×10^n step that keeps the tick count at or below `count`,
  // so a "4 ticks" request never draws six.
  const step =
    [1, 2, 5, 10]
      .map((m) => m * magnitude)
      .find((candidate) => span / candidate <= count) ?? 10 * magnitude;
  const out: number[] = [];
  for (let v = Math.ceil(min / step) * step; v <= max + step * 1e-9; v += step) {
    if (out.length > count * 4) break; // defensive: never loop forever
    out.push(Number(v.toPrecision(12)));
  }
  return out;
}

/** clampIndex keeps a keyboard cursor inside a list of `length` items. */
export function clampIndex(i: number, length: number): number {
  if (length <= 0) return -1;
  if (!Number.isFinite(i)) return 0;
  return Math.min(Math.max(Math.trunc(i), 0), length - 1);
}

/** stepIndex moves the cursor by `delta` and clamps to the list bounds. */
export function stepIndex(i: number, delta: number, length: number): number {
  if (length <= 0) return -1;
  return clampIndex(clampIndex(i, length) + delta, length);
}

/** formatCompact renders 1234 → "1.2k", 3400000 → "3.4M". */
export function formatCompact(n: number | undefined | null): string {
  if (n === undefined || n === null || !Number.isFinite(n)) return "—";
  const abs = Math.abs(n);
  if (abs >= 1e9) return `${trim(n / 1e9)}B`;
  if (abs >= 1e6) return `${trim(n / 1e6)}M`;
  if (abs >= 1e3) return `${trim(n / 1e3)}k`;
  return trim(n);
}

function trim(n: number): string {
  const rounded = Math.round(n * 10) / 10;
  return Number.isInteger(rounded) ? String(rounded) : rounded.toFixed(1);
}

/** formatDurationMs renders 1234 → "1.2 s", 95000 → "1m 35s". */
export function formatDurationMs(ms: number | undefined | null): string {
  if (ms === undefined || ms === null || !Number.isFinite(ms) || ms < 0) return "—";
  if (ms < 1000) return `${Math.round(ms)} ms`;
  const totalSeconds = Math.round(ms / 1000);
  if (totalSeconds < 60) return `${trim(ms / 1000)} s`;
  const minutes = Math.floor(totalSeconds / 60);
  const seconds = totalSeconds % 60;
  if (minutes < 60) return seconds === 0 ? `${minutes}m` : `${minutes}m ${seconds}s`;
  const hours = Math.floor(minutes / 60);
  const restMinutes = minutes % 60;
  return restMinutes === 0 ? `${hours}h` : `${hours}h ${restMinutes}m`;
}

/** formatPercent renders a 0..1 ratio as "12.5%". */
export function formatPercent(ratio: number | undefined | null): string {
  if (ratio === undefined || ratio === null || !Number.isFinite(ratio)) return "—";
  return `${trim(ratio * 100)}%`;
}
