// Telemetry query hooks (TanStack Query + Connect-ES, docs/07 §3.9,
// docs/08 §5).
//
// The TelemetryService proxies tenant-scoped queries to Tempo / Loki /
// VictoriaMetrics and answers the Postgres-backed report aggregate. Every
// hook forwards the shared filter set (window + project / execution /
// work-item + free text) and surfaces `degradedReason` beside `degraded`, so
// the UI can name the store that is down (AC 5) instead of drawing an empty
// chart.

import { Timestamp } from "@bufbuild/protobuf";
import { useQuery } from "@tanstack/react-query";

import { telemetryClient } from "@/api/clients";
import { TelemetryReportGroupBy } from "@/api/gen/orchicon/api/v1/telemetry_service_pb";
import type {
  LogRecord,
  MetricSeries,
  Trace,
} from "@/api/gen/orchicon/api/v1/telemetry_pb";

export const telemetryKeys = {
  all: ["telemetry"] as const,
  traces: (f: TelemetryFilterKey) => ["telemetry", "traces", f] as const,
  metrics: (f: TelemetryFilterKey) => ["telemetry", "metrics", f] as const,
  logs: (f: TelemetryFilterKey) => ["telemetry", "logs", f] as const,
  dashboard: (projectId?: string) =>
    ["telemetry", "dashboard", projectId] as const,
  report: (f: TelemetryFilterKey & { groupBy: number }) =>
    ["telemetry", "report", f] as const,
};

/** TelemetryFilters is the shared filter set the whole page drives. */
export interface TelemetryFilters {
  projectId?: string;
  executionId?: string;
  workItemId?: string;
  /** Free-text log search (LogQL line filter). */
  search?: string;
  /** OTel severity text for logs ("ERROR", "WARN", …). */
  severity?: string;
  start?: Date;
  end?: Date;
}

/** Serializable form of the filters, used only as a query key. */
export interface TelemetryFilterKey {
  projectId?: string;
  executionId?: string;
  workItemId?: string;
  search?: string;
  severity?: string;
  startMs?: number;
  endMs?: number;
}

function keyOf(f?: TelemetryFilters): TelemetryFilterKey {
  return {
    projectId: f?.projectId || undefined,
    executionId: f?.executionId || undefined,
    workItemId: f?.workItemId || undefined,
    search: f?.search || undefined,
    severity: f?.severity || undefined,
    startMs: f?.start ? f.start.getTime() : undefined,
    endMs: f?.end ? f.end.getTime() : undefined,
  };
}

function windowTs(f?: TelemetryFilters) {
  return {
    start: f?.start ? Timestamp.fromDate(f.start) : undefined,
    end: f?.end ? Timestamp.fromDate(f.end) : undefined,
  };
}

// Poll while degraded so the view auto-recovers when the backend starts.
function degradedPollInterval(query: {
  state: { data?: unknown };
}): number | false {
  const d = query.state.data as { degraded?: boolean } | undefined;
  return d?.degraded ? 10_000 : false;
}

export function useQueryTraces(filters?: TelemetryFilters) {
  const key = keyOf(filters);
  return useQuery({
    queryKey: telemetryKeys.traces(key),
    queryFn: async () => {
      const { start, end } = windowTs(filters);
      const res = await telemetryClient.queryTraces({
        query: {
          projectId: filters?.projectId ?? "",
          executionId: filters?.executionId ?? "",
          workItemId: filters?.workItemId ?? "",
          start,
          end,
          limit: 50,
        },
      });
      return {
        traces: (res.traces ?? []) as Trace[],
        degraded: res.degraded,
        degradedReason: res.degradedReason,
      };
    },
    refetchInterval: degradedPollInterval,
  });
}

export function useQueryMetrics(opts: {
  metricNames: string[];
  filters?: TelemetryFilters;
}) {
  const key = keyOf(opts.filters);
  return useQuery({
    queryKey: [...telemetryKeys.metrics(key), opts.metricNames],
    queryFn: async () => {
      // Metrics are tenant + project scoped: VictoriaMetrics carries no
      // work-item / execution label (resource_to_telemetry_conversion only
      // emits resource attributes), so those filters are not sent.
      const { start, end } = windowTs(opts.filters);
      const res = await telemetryClient.queryMetrics({
        metricNames: opts.metricNames,
        query: {
          projectId: opts.filters?.projectId ?? "",
          start,
          end,
          limit: 200,
        },
      });
      return {
        series: (res.series ?? []) as MetricSeries[],
        degraded: res.degraded,
        degradedReason: res.degradedReason,
      };
    },
    refetchInterval: degradedPollInterval,
  });
}

export function useQueryLogs(filters?: TelemetryFilters) {
  const key = keyOf(filters);
  return useQuery({
    queryKey: telemetryKeys.logs(key),
    queryFn: async () => {
      const { start, end } = windowTs(filters);
      const res = await telemetryClient.queryLogs({
        query: {
          projectId: filters?.projectId ?? "",
          executionId: filters?.executionId ?? "",
          workItemId: filters?.workItemId ?? "",
          start,
          end,
          limit: 200,
        },
        severity: filters?.severity ?? "",
        search: filters?.search ?? "",
      });
      return {
        logs: (res.logs ?? []) as LogRecord[],
        degraded: res.degraded,
        degradedReason: res.degradedReason,
      };
    },
    refetchInterval: degradedPollInterval,
  });
}

export function useGetDashboard(opts?: { projectId?: string }) {
  return useQuery({
    queryKey: telemetryKeys.dashboard(opts?.projectId),
    queryFn: async () => {
      const res = await telemetryClient.getDashboard({
        projectId: opts?.projectId ?? "",
      });
      return {
        panels: (res.panels ?? []) as MetricSeries[],
        summary: res.summary,
      };
    },
    // Poll so the dashboard advances with live usage records.
    refetchInterval: 5_000,
  });
}

export function useGetTelemetryReport(opts: {
  groupBy: TelemetryReportGroupBy;
  filters?: TelemetryFilters;
}) {
  const key = { ...keyOf(opts.filters), groupBy: opts.groupBy };
  return useQuery({
    queryKey: telemetryKeys.report(key),
    queryFn: async () => {
      const { start, end } = windowTs(opts.filters);
      const res = await telemetryClient.getTelemetryReport({
        groupBy: opts.groupBy,
        projectId: opts.filters?.projectId ?? "",
        executionId: opts.filters?.executionId ?? "",
        workItemId: opts.filters?.workItemId ?? "",
        start,
        end,
      });
      return {
        rows: res.rows ?? [],
        totals: res.totals,
        // outcomes and models are separate dimensions, not more columns on a
        // row: "how much work finished, and how" and "which models did it"
        // are answered independently of the project/workflow grouping. They
        // are passed through here rather than left on the raw response so the
        // panel cannot silently render a blank section when the server starts
        // sending them.
        outcomes: res.outcomes,
        models: res.models ?? [],
        windowStart: res.windowStart,
        windowEnd: res.windowEnd,
      };
    },
    // A report is a snapshot window, not a live feed.
    refetchInterval: false,
  });
}
