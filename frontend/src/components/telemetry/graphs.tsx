// Graph panels for the native Telemetry page: metrics (VictoriaMetrics),
// traces (Tempo) and logs (Loki), all read through internal/telemetry.
//
// Every panel resolves exactly one of the five states through
// <TelemetryState>, so a degraded store NAMES itself and a filter that
// excluded everything is not reported as "no data".

import { useMemo } from "react";

import {
  useQueryLogs,
  useQueryMetrics,
  useQueryTraces,
  type TelemetryFilters,
} from "@/api/telemetry";
import { ChartFrame, TimeSeriesChart } from "@/components/ui/chart";
import {
  fmtInt,
  degradedBackends,
  isPermissionDenied,
  TelemetryState,
  type TelemetryStateKind,
} from "@/components/telemetry/states";
import { formatDurationMs } from "@/lib/chart-geometry";
import { cn } from "@/lib/utils";

/** The Orchicon metric families the graphs read (docs/08 §5.2). */
export const TELEMETRY_METRICS = [
  "orchicon_tokens_consumed",
  "orchicon_cost_usd",
  "orchicon_outbox_lag",
];

const METRIC_UNITS: Record<string, string> = {
  orchicon_tokens_consumed: "tokens",
  orchicon_cost_usd: "USD",
  orchicon_outbox_lag: "lag",
};

interface SeriesPointLike {
  timestamp?: { seconds: bigint; nanos: number };
  value: number;
}

/** pointsFromMetric converts a proto series (newest-first) into ascending
 *  chart points. */
export function pointsFromMetric(points: SeriesPointLike[] | undefined) {
  return (points ?? [])
    .map((p) => ({
      t: p.timestamp
        ? Number(p.timestamp.seconds) * 1000 +
          Math.floor(p.timestamp.nanos / 1e6)
        : 0,
      v: p.value,
    }))
    .sort((a, b) => a.t - b.t);
}

function hasFilter(f: TelemetryFilters): boolean {
  return Boolean(f.projectId || f.executionId || f.workItemId || f.search);
}

function panelKind(args: {
  isLoading: boolean;
  error: unknown;
  degraded: { signal: string; reason: string }[];
  empty: boolean;
  filtered: boolean;
}): TelemetryStateKind {
  if (args.isLoading) return "loading";
  if (isPermissionDenied(args.error)) return "denied";
  if (args.error) return "error";
  if (args.degraded.length > 0) return "degraded";
  if (args.empty) return args.filtered ? "empty-filtered" : "empty";
  return "ready";
}

export function MetricsGraphPanel({ filters }: { filters: TelemetryFilters }) {
  const { data, isLoading, error, refetch } = useQueryMetrics({
    metricNames: TELEMETRY_METRICS,
    filters,
  });
  const series = data?.series ?? [];
  const degraded = degradedBackends([
    {
      signal: "metrics",
      degraded: data?.degraded,
      degradedReason: data?.degradedReason,
    },
  ]);
  const kind = panelKind({
    isLoading,
    error,
    degraded,
    empty: series.length === 0,
    filtered: Boolean(filters.projectId),
  });

  return (
    <div className="grid gap-4 lg:grid-cols-3">
      {sparkCharts(series, kind, degraded, error, refetch)}
      <div className="lg:col-span-3">
        <ChartFrame
          title="Metrics"
          description="VictoriaMetrics, tenant + project scoped (no work-item/execution labels)."
          state={
            kind === "ready" ? null : (
              <TelemetryState
                kind={kind}
                signal="metrics"
                degraded={degraded}
                error={error}
                onRetry={() => void refetch()}
                hint="Metrics are emitted by the plane; start it to populate this window."
              />
            )
          }
        >
          <p className="text-sm text-muted-foreground">
            {fmtInt(series.reduce((n, s) => n + (s.points?.length ?? 0), 0))}{" "}
            samples across {series.length} series, charted individually below
            (up to {MAX_SERIES_PER_FAMILY} per metric, busiest first).
          </p>
        </ChartFrame>
      </div>
    </div>
  );
}

/** Identifying labels for a series: the label pairs that distinguish one
 *  series from another. The tenant label is dropped because every series in a
 *  tenant-scoped query carries the same one — it identifies nothing, and
 *  printing it on every chart is noise. */
export function identifyingLabels(labels?: Record<string, string>): string[] {
  if (!labels) return [];
  return Object.entries(labels)
    .filter(
      ([k, v]) => Boolean(v) && k !== "orchicon_tenant_id" && k !== "__name__",
    )
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([k, v]) => `${k}=${v}`);
}

/** How many individual series one metric family shows before it summarises the
 *  rest. A model-heavy tenant emits dozens; an unbounded wall of charts is the
 *  same information as no charts. */
export const MAX_SERIES_PER_FAMILY = 6;

/** latestValue is the last sample's value (series are newest-first from VM, but
 *  sorting makes that independent of transport order). */
function latestValue(points: { t: number; v: number }[]): number {
  return points.length > 0 ? points[points.length - 1].v : 0;
}

/** familySeries selects EVERY series belonging to one metric family, newest
 *  value first.
 *
 *  This is the function that fixes the dropped-series bug: the panel used to
 *  take `series.find(s => s.metricName === name)`, i.e. the single first match
 *  per family, so a metric emitted per model showed exactly one model and the
 *  page still reported the full series count. Selection and ordering live here
 *  so it can be asserted without a renderer; the cap is a render decision, not
 *  a data one.
 */
export function familySeries<
  T extends {
    metricName: string;
    labels?: Record<string, string>;
    points?: SeriesPointLike[];
  },
>(
  series: T[],
  name: string,
): { id: string; points: { t: number; v: number }[]; latest: number }[] {
  return series
    .filter((s) => s.metricName === name)
    .map((s) => {
      const points = pointsFromMetric(s.points);
      return {
        id: identifyingLabels(s.labels).join(" · "),
        points,
        latest: latestValue(points),
      };
    })
    .sort((a, b) => b.latest - a.latest);
}

function sparkCharts(
  series: {
    metricName: string;
    labels?: Record<string, string>;
    points?: SeriesPointLike[];
  }[],
  kind: TelemetryStateKind,
  degraded: { signal: string; reason: string }[],
  error: unknown,
  refetch: () => unknown,
) {
  const state = (
    <TelemetryState
      kind={kind}
      signal="metrics"
      degraded={degraded}
      error={error}
      onRetry={() => void refetch()}
    />
  );

  // One chart per INDIVIDUAL series, not one per metric family. The previous
  // version took `series.find(...)` — the first series matching each family —
  // so on a tenant where a family is emitted per model, every model but one
  // was silently dropped, and the page still claimed to be showing "M series".
  return TELEMETRY_METRICS.flatMap((name, familyIdx) => {
    const unit = METRIC_UNITS[name];
    const family = series
      .filter((s) => s.metricName === name)
      .map((s) => {
        const points = pointsFromMetric(s.points);
        return {
          id: identifyingLabels(s.labels).join(" · "),
          points,
          latest: latestValue(points),
        };
      })
      // Highest current value first: the top spender is what an operator is
      // looking for, and truncation must never hide it.
      .sort((a, b) => b.latest - a.latest);

    if (family.length === 0) {
      return [
        <ChartFrame
          key={name}
          title={name}
          description="No samples in this window"
          state={kind === "ready" ? null : state}
        >
          <p className="py-6 text-center text-sm text-muted-foreground">
            No samples in this window.
          </p>
        </ChartFrame>,
      ];
    }

    const shown = family.slice(0, MAX_SERIES_PER_FAMILY);
    const frames = shown.map((s, idx) => (
      <ChartFrame
        key={`${name}:${s.id || "aggregate"}`}
        title={s.id || name}
        description={`${name} · latest ${fmtInt(Math.round(s.latest))} ${unit ?? ""}`}
        state={kind === "ready" ? null : state}
      >
        <TimeSeriesChart
          points={s.points}
          label={s.id ? `${name} ${s.id}` : name}
          unit={unit}
          colorIndex={familyIdx + idx}
          heightClass="h-[120px]"
        />
      </ChartFrame>
    ));

    if (family.length > shown.length) {
      frames.push(
        <p
          key={`${name}:more`}
          className="rounded-xl border border-dashed border-white/10 p-3 text-[11px] text-muted-foreground lg:col-span-3"
        >
          Showing the {shown.length} busiest of {family.length} individual
          series for {name}. Narrow the window or scope to a project to see the
          rest.
        </p>,
      );
    }
    return frames;
  });
}

export function TracesPanel({ filters }: { filters: TelemetryFilters }) {
  const { data, isLoading, error, refetch } = useQueryTraces(filters);
  const traces = data?.traces ?? [];
  const degraded = degradedBackends([
    {
      signal: "traces",
      degraded: data?.degraded,
      degradedReason: data?.degradedReason,
    },
  ]);
  const kind = panelKind({
    isLoading,
    error,
    degraded,
    empty: traces.length === 0,
    filtered: hasFilter(filters),
  });

  // Trace durations over time — the only two dimensions Tempo gives us
  // without a per-span read.
  const points = useMemo(
    () =>
      traces
        .map((t) => ({
          t: t.startTime ? Number(t.startTime.seconds) * 1000 : 0,
          v: Number(t.durationUs ?? 0) / 1000,
        }))
        .sort((a, b) => a.t - b.t),
    [traces],
  );

  return (
    <div className="space-y-4">
      <ChartFrame
        title="Trace duration"
        description="Root spans from Tempo (ms). Identity filters ride orchicon.project_id / work_item_id / execution_id."
        state={
          kind === "ready" ? null : (
            <TelemetryState
              kind={kind}
              signal="traces"
              degraded={degraded}
              error={error}
              onRetry={() => void refetch()}
              hint="Nothing ran in this window, or the identity filters excluded every trace."
            />
          )
        }
      >
        <TimeSeriesChart points={points} label="Trace duration" unit="ms" />
      </ChartFrame>

      {kind === "ready" ? (
        <ul className="divide-y divide-white/10 overflow-hidden rounded-2xl glass-panel">
          {traces.map((t) => (
            <li
              key={t.traceId}
              className="flex items-center justify-between gap-3 px-3 py-2"
            >
              <div className="min-w-0">
                <div className="truncate text-sm font-medium">
                  {t.rootSpanName || "—"}
                </div>
                <div className="font-mono text-[11px] text-muted-foreground">
                  {(t.traceId ?? "").slice(0, 16)}…
                </div>
              </div>
              <div className="shrink-0 text-right text-sm">
                <div className="tabular-nums">
                  {formatDurationMs(Number(t.durationUs ?? 0) / 1000)}
                </div>
                <div className="text-[11px] text-muted-foreground">
                  {t.spanCount ?? 0} spans
                </div>
              </div>
            </li>
          ))}
        </ul>
      ) : null}
    </div>
  );
}

const SEVERITY_CLASS: Record<string, string> = {
  ERROR: "bg-destructive/15 text-destructive",
  FATAL: "bg-destructive/25 text-destructive",
  WARN: "bg-accent text-accent-foreground",
  INFO: "bg-muted text-muted-foreground",
};

export function LogsPanel({ filters }: { filters: TelemetryFilters }) {
  const { data, isLoading, error, refetch } = useQueryLogs(filters);
  const logs = data?.logs ?? [];
  const degraded = degradedBackends([
    {
      signal: "logs",
      degraded: data?.degraded,
      degradedReason: data?.degradedReason,
    },
  ]);
  const kind = panelKind({
    isLoading,
    error,
    degraded,
    empty: logs.length === 0,
    filtered: hasFilter(filters) || Boolean(filters.severity),
  });

  return (
    <ChartFrame
      title="Logs"
      description="Structured OTel log records from Loki, carrying trace_id + correlation_id."
      state={
        kind === "ready" ? null : (
          <TelemetryState
            kind={kind}
            signal="logs"
            degraded={degraded}
            error={error}
            onRetry={() => void refetch()}
            hint="Clear the text search / severity / identity filters, or widen the window."
          />
        )
      }
    >
      <ul className="max-h-[420px] space-y-2 overflow-y-auto">
        {logs.map((l, i) => (
          <li
            key={`${l.traceId ?? "log"}-${i}`}
            className="rounded-md border px-3 py-2 text-sm"
          >
            <div className="flex items-center justify-between gap-2">
              <span
                className={cn(
                  "rounded px-1.5 py-0.5 font-mono text-[10px] uppercase",
                  SEVERITY_CLASS[l.severity ?? "INFO"] ??
                    "bg-muted text-muted-foreground",
                )}
              >
                {l.severity || "INFO"}
              </span>
              <span className="font-mono text-[11px] text-muted-foreground">
                {l.timestamp
                  ? new Date(
                      Number(l.timestamp.seconds) * 1000,
                    ).toLocaleTimeString()
                  : "—"}
              </span>
            </div>
            <p className="mt-1 break-words">{l.body || "—"}</p>
            {l.service ? (
              <p className="mt-1 font-mono text-[10px] text-muted-foreground">
                {l.service}
              </p>
            ) : null}
          </li>
        ))}
      </ul>
    </ChartFrame>
  );
}
