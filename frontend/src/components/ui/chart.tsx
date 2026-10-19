// Design-system chart primitives for the native Telemetry page.
//
// Hand-rolled SVG on purpose: the repo has no charting dependency and the
// eight themes define no --chart-* variables, so every colour here comes from
// existing tokens (hsl(var(--primary)) etc.) and re-themes for free. Geometry
// lives in lib/chart-geometry.ts so it is unit-testable without a renderer.
//
// Accessibility: each chart is a focusable role="img" with an aria-label
// summary, Left/Right/Home/End move a cursor across the series and announce
// the focused point through a polite live region, and the full data is
// mirrored in a visually hidden table.

import { useId, useMemo, useState } from "react";

import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import {
  areaPathFromPoints,
  axisTickTimes,
  boundsOf,
  chartSeries,
  clampIndex,
  formatCompact,
  niceTicks,
  pathFromPoints,
  stepIndex,
  type ChartPoint,
} from "@/lib/chart-geometry";
import { cn } from "@/lib/utils";

const VIEW_W = 600;
const VIEW_H = 160;

export interface ChartFrameProps {
  title: string;
  description?: string;
  /** Right-aligned controls (range chips, group-by toggle…). */
  actions?: React.ReactNode;
  /** Rendered instead of `children` when the panel has a state to show. */
  state?: React.ReactNode;
  children?: React.ReactNode;
  className?: string;
}

/** ChartFrame is the shell every graph lives in: title, description, actions,
 *  and a single state slot so no panel invents its own empty/error wording. */
export function ChartFrame({ title, description, actions, state, children, className }: ChartFrameProps) {
  return (
    <Card className={cn("glass-panel", className)}>
      <CardHeader className="flex flex-row items-start justify-between gap-3 pb-2">
        <div className="min-w-0">
          <CardTitle className="truncate text-sm font-medium">{title}</CardTitle>
          {description ? <CardDescription className="text-xs">{description}</CardDescription> : null}
        </div>
        {actions ? <div className="flex shrink-0 flex-wrap items-center gap-2">{actions}</div> : null}
      </CardHeader>
      <CardContent className="pt-2">{state ?? children}</CardContent>
    </Card>
  );
}

export interface TimeSeriesChartProps {
  points: ChartPoint[];
  /** Accessible name of the series (also the live-region prefix). */
  label: string;
  /** Unit suffix for the announced / tabulated values ("tokens", "USD"…). */
  unit?: string;
  /** Format the y-axis and announced values. Defaults to compact numbers. */
  valueFormat?: (v: number) => string;
  colorIndex?: number;
  heightClass?: string;
}

function timeLabel(t: number): string {
  return new Date(t).toLocaleTimeString(undefined, {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

/** axisTimeLabel is the X-axis form: minute precision, and a date once the
 *  window is longer than a day (otherwise every label reads 00:00–23:00 twice
 *  over and the axis tells the operator nothing). */
function axisTimeLabel(t: number, spanMs: number): string {
  const d = new Date(t);
  if (spanMs > 24 * 60 * 60 * 1000) {
    return d.toLocaleString(undefined, {
      month: "short",
      day: "numeric",
      hour: "2-digit",
      minute: "2-digit",
    });
  }
  return d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });
}

/** TimeSeriesChart renders one series as an area + line with a keyboard cursor. */
export function TimeSeriesChart({
  points,
  label,
  unit,
  valueFormat = formatCompact,
  colorIndex = 0,
  heightClass = "h-[160px]",
}: TimeSeriesChartProps) {
  const [cursor, setCursor] = useState<number>(-1);
  const tableId = useId();
  const color = chartSeries(colorIndex);

  const values = useMemo(() => points.map((p) => p.v), [points]);
  const [vMin, vMax] = useMemo(() => {
    if (values.length === 0) return [0, 1] as const;
    let lo = values[0];
    let hi = values[0];
    for (const v of values) {
      if (v < lo) lo = v;
      if (v > hi) hi = v;
    }
    return [lo, hi] as const;
  }, [values]);

  const line = useMemo(() => pathFromPoints(points, VIEW_W, VIEW_H), [points]);
  const area = useMemo(() => areaPathFromPoints(points, VIEW_W, VIEW_H), [points]);
  const ticks = useMemo(() => niceTicks(vMin, vMax, 4), [vMin, vMax]);

  const active = cursor >= 0 && cursor < points.length ? points[cursor] : undefined;
  const summary =
    points.length === 0
      ? `${label}: no data in this window`
      : `${label}: ${points.length} points, latest ${valueFormat(points[points.length - 1].v)}${unit ? ` ${unit}` : ""}`;

  function onKeyDown(e: React.KeyboardEvent<SVGSVGElement>) {
    if (points.length === 0) return;
    const start = cursor < 0 ? points.length - 1 : cursor;
    let next = start;
    switch (e.key) {
      case "ArrowLeft":
        next = stepIndex(start, -1, points.length);
        break;
      case "ArrowRight":
        next = stepIndex(start, 1, points.length);
        break;
      case "Home":
        next = 0;
        break;
      case "End":
        next = clampIndex(points.length - 1, points.length);
        break;
      case "Escape":
        setCursor(-1);
        return;
      default:
        return;
    }
    e.preventDefault();
    setCursor(next);
  }

  const cursorX =
    active && points.length > 1
      ? ((active.t - points[0].t) / (points[points.length - 1].t - points[0].t)) * VIEW_W
      : VIEW_W;
  const [cMin, cMax] = boundsOf(values);
  const cursorY = active ? VIEW_H - ((active.v - cMin) / (cMax - cMin || 1)) * VIEW_H : 0;

  // Axis labels are DOM text, never <text> inside the plot SVG. The plot is
  // preserveAspectRatio="none" so the path fills any container width, and
  // that scale is NON-UNIFORM: x stretches to the container while y stays
  // 1:1. SVG text inherits that transform and renders horizontally stretched
  // and vertically squashed — the "squished font" operators reported on the
  // graph's x coordinates. vectorEffect only rescues strokes, so there is no
  // SVG-side fix. DOM text is laid out after the scale and cannot distort.
  const yLabels = ticks.map((tick) => {
    const ratio = vMax === vMin ? 0.5 : (tick - vMin) / (vMax - vMin);
    return { value: tick, topPct: (1 - ratio) * 100 };
  });

  const xLabels = useMemo(() => {
    const instants = axisTickTimes(points);
    if (instants.length === 0) return [];
    const span = points[points.length - 1].t - points[0].t;
    return instants.map((t) => axisTimeLabel(t, span));
  }, [points]);

  return (
    <div className="w-full">
      <div className="flex w-full gap-2">
        {/* Y axis: a fixed gutter so labels never sit on top of the series. */}
        <div className={cn("relative w-10 shrink-0", heightClass)} aria-hidden="true">
          {yLabels.map((l) => (
            <span
              key={l.value}
              className="absolute right-0 -translate-y-1/2 text-[10px] leading-none tabular-nums text-muted-foreground"
              style={{ top: `${l.topPct}%` }}
            >
              {valueFormat(l.value)}
            </span>
          ))}
        </div>
        <div className={cn("relative min-w-0 flex-1", heightClass)}>
          <svg
            role="img"
            aria-label={summary}
            aria-describedby={tableId}
            tabIndex={0}
            viewBox={`0 0 ${VIEW_W} ${VIEW_H}`}
            preserveAspectRatio="none"
            onKeyDown={onKeyDown}
            className="h-full w-full rounded-md focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
          >
            {ticks.map((tick) => {
              const ratio = vMax === vMin ? 0.5 : (tick - vMin) / (vMax - vMin);
              const y = VIEW_H - ratio * VIEW_H;
              return (
                <line
                  key={tick}
                  x1={0}
                  x2={VIEW_W}
                  y1={y}
                  y2={y}
                  stroke="hsl(var(--border))"
                  strokeWidth={1}
                  strokeDasharray="2 4"
                  vectorEffect="non-scaling-stroke"
                />
              );
            })}
            {area ? <path d={area} fill={color} fillOpacity={0.12} stroke="none" /> : null}
            {line ? (
              <path
                d={line}
                fill="none"
                stroke={color}
                strokeWidth={2}
                strokeLinejoin="round"
                strokeLinecap="round"
                vectorEffect="non-scaling-stroke"
              />
            ) : null}
          </svg>
          {/* The cursor dot is DOM too: a <circle> in the non-uniform plot is
              scaled into an ellipse, so it would be drawn as a smear. */}
          {active ? (
            <span
              aria-hidden="true"
              className="pointer-events-none absolute h-2 w-2 -translate-x-1/2 -translate-y-1/2 rounded-full"
              style={{
                left: `${(cursorX / VIEW_W) * 100}%`,
                top: `${(cursorY / VIEW_H) * 100}%`,
                backgroundColor: color,
                boxShadow: "0 0 0 1.5px hsl(var(--background))",
              }}
            />
          ) : null}
        </div>
      </div>
      <div
        className="ml-12 mt-1 flex justify-between gap-2 text-[10px] leading-none tabular-nums text-muted-foreground"
        aria-hidden="true"
      >
        {xLabels.map((l, i) => (
          <span key={`${l}-${i}`} className="truncate">
            {l}
          </span>
        ))}
      </div>
      <p aria-live="polite" className="mt-1 text-[11px] text-muted-foreground">
        {active
          ? `${timeLabel(active.t)} — ${valueFormat(active.v)}${unit ? ` ${unit}` : ""}`
          : points.length === 0
            ? "No samples"
            : `Latest ${valueFormat(points[points.length - 1].v)}${unit ? ` ${unit}` : ""} · use ←/→ to inspect`}
      </p>
      <table id={tableId} className="sr-only">
        <caption>{label}</caption>
        <thead>
          <tr>
            <th scope="col">Time</th>
            <th scope="col">Value{unit ? ` (${unit})` : ""}</th>
          </tr>
        </thead>
        <tbody>
          {points.map((p) => (
            <tr key={p.t}>
              <td>{timeLabel(p.t)}</td>
              <td>{valueFormat(p.v)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

export interface BarListItem {
  key: string;
  label: string;
  value: number;
  /** Optional trailing note (e.g. "12 executions"). */
  note?: string;
}

export interface BarListProps {
  items: BarListItem[];
  valueFormat?: (v: number) => string;
  colorIndex?: number;
}

/** BarList is the horizontal breakdown primitive (cost by project, etc.). */
export function BarList({ items, valueFormat = formatCompact, colorIndex = 0 }: BarListProps) {
  const max = items.reduce((m, i) => Math.max(m, i.value), 0);
  const color = chartSeries(colorIndex);
  return (
    <ul className="space-y-1.5">
      {items.map((item) => {
        const pct = max > 0 ? Math.max(2, (item.value / max) * 100) : 0;
        return (
          <li key={item.key} className="text-sm">
            <div className="flex items-center justify-between gap-2">
              <span className="truncate" title={item.label}>
                {item.label}
              </span>
              <span className="shrink-0 tabular-nums text-muted-foreground">
                {valueFormat(item.value)}
                {item.note ? <span className="ml-2 text-xs">{item.note}</span> : null}
              </span>
            </div>
            <div className="mt-1 h-1.5 w-full overflow-hidden rounded-full bg-muted" aria-hidden="true">
              <div className="h-full rounded-full" style={{ width: `${pct}%`, backgroundColor: color }} />
            </div>
          </li>
        );
      })}
    </ul>
  );
}

/** Sparkline is the inline trend next to a stat value. */
export function Sparkline({ points, colorIndex = 0 }: { points: ChartPoint[]; colorIndex?: number }) {
  const line = useMemo(() => pathFromPoints(points, 100, 24), [points]);
  if (!line) {
    return <div className="h-6 w-full" aria-hidden="true" />;
  }
  return (
    <svg
      viewBox="0 0 100 24"
      preserveAspectRatio="none"
      className="h-6 w-full"
      aria-hidden="true"
      focusable="false"
    >
      <path
        d={line}
        fill="none"
        stroke={chartSeries(colorIndex)}
        strokeWidth={1.5}
        vectorEffect="non-scaling-stroke"
      />
    </svg>
  );
}
