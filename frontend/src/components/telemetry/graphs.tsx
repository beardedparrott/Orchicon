// Graph panels for the native Telemetry page: metrics (VictoriaMetrics),
// traces (Tempo) and logs (Loki), all read through internal/telemetry.
//
// Every panel resolves exactly one of the five states through
// <TelemetryState>, so a degraded store NAMES itself and a filter that
// excluded everything is not reported as "no data".

import { useMemo } from "react";

import { useQueryLogs, useQueryMetrics, useQueryTraces, type TelemetryFilters } from "@/api/telemetry";
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
      t: p.timestamp ? Number(p.timestamp.seconds) * 1000 + Math.floor(p.timestamp.nanos / 1e6) : 0,
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
    { signal: "metrics", degraded: data?.degraded, degradedReason: data?.degradedReason },
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
            {fmtInt(series.reduce((n, s) => n + (s.points?.length ?? 0), 0))} samples across{" "}
            {series.length} series.
          </p>
        </ChartFrame>
      </div>
    </div>
  );
}

function sparkCharts(
  series: { metricName: string; points?: SeriesPointLike[] }[],
  kind: TelemetryStateKind,
  degraded: { signal: string; reason: string }[],
  error: unknown,
  refetch: () => unknown,
) {
  return TELEMETRY_METRICS.map((name, idx) => {
    const found = series.find((s) => s.metricName === name);
    const points = pointsFromMetric(found?.points);
    const unit = METRIC_UNITS[name];
    return (
      <ChartFrame
        key={name}
        title={name}
        description={points.length > 0 ? `Latest ${fmtInt(Math.round(points[points.length - 1].v))} ${unit ?? ""}` : "No samples in this window"}
        state={
          kind === "ready" ? null : (
            <TelemetryState
              kind={kind}
              signal="metrics"
              degraded={degraded}
              error={error}
              onRetry={() => void refetch()}
            />
          )
        }
      >
        <TimeSeriesChart points={points} label={name} unit={unit} colorIndex={idx} />
      </ChartFrame>
    );
  });
}

export function TracesPanel({ filters }: { filters: TelemetryFilters }) {
  const { data, isLoading, error, refetch } = useQueryTraces(filters);
  const traces = data?.traces ?? [];
  const degraded = degradedBackends([
    { signal: "traces", degraded: data?.degraded, degradedReason: data?.degradedReason },
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
            <li key={t.traceId} className="flex items-center justify-between gap-3 px-3 py-2">
              <div className="min-w-0">
                <div className="truncate text-sm font-medium">{t.rootSpanName || "—"}</div>
                <div className="font-mono text-[11px] text-muted-foreground">
                  {(t.traceId ?? "").slice(0, 16)}…
                </div>
              </div>
              <div className="shrink-0 text-right text-sm">
                <div className="tabular-nums">{formatDurationMs(Number(t.durationUs ?? 0) / 1000)}</div>
                <div className="text-[11px] text-muted-foreground">{t.spanCount ?? 0} spans</div>
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
    { signal: "logs", degraded: data?.degraded, degradedReason: data?.degradedReason },
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
          <li key={`${l.traceId ?? "log"}-${i}`} className="rounded-md border px-3 py-2 text-sm">
            <div className="flex items-center justify-between gap-2">
              <span
                className={cn(
                  "rounded px-1.5 py-0.5 font-mono text-[10px] uppercase",
                  SEVERITY_CLASS[l.severity ?? "INFO"] ?? "bg-muted text-muted-foreground",
                )}
              >
                {l.severity || "INFO"}
              </span>
              <span className="font-mono text-[11px] text-muted-foreground">
                {l.timestamp ? new Date(Number(l.timestamp.seconds) * 1000).toLocaleTimeString() : "—"}
              </span>
            </div>
            <p className="mt-1 break-words">{l.body || "—"}</p>
            {l.service ? (
              <p className="mt-1 font-mono text-[10px] text-muted-foreground">{l.service}</p>
            ) : null}
          </li>
        ))}
      </ul>
    </ChartFrame>
  );
}
