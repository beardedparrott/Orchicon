// Telemetry query hooks (TanStack Query + Connect-ES, docs/07 §3.9,
// docs/08 §5). The TelemetryService proxies tenant-scoped queries to
// SigNoz/ClickHouse; raw exploration uses the embedded SigNoz UI
// (docs/10 §11), while the Orchicon dashboard is a custom roll-up.

import { useQuery } from "@tanstack/react-query";

import { telemetryClient } from "@/api/clients";
import type { Trace } from "@/api/gen/orchicon/api/v1/telemetry_pb";
import type { MetricSeries } from "@/api/gen/orchicon/api/v1/telemetry_pb";
import type { LogRecord } from "@/api/gen/orchicon/api/v1/telemetry_pb";
import type { DashboardSummary } from "@/api/gen/orchicon/api/v1/telemetry_service_pb";

export const telemetryKeys = {
  all: ["telemetry"] as const,
  traces: (projectId?: string) => ["telemetry", "traces", projectId] as const,
  metrics: (names: string[], projectId?: string) =>
    ["telemetry", "metrics", names, projectId] as const,
  logs: (projectId?: string) => ["telemetry", "logs", projectId] as const,
  dashboard: (projectId?: string) => ["telemetry", "dashboard", projectId] as const,
};

export function useQueryTraces(opts?: {
  projectId?: string;
  executionId?: string;
  correlationId?: string;
  traceId?: string;
}) {
  return useQuery({
    queryKey: telemetryKeys.traces(opts?.projectId),
    queryFn: async () => {
      const res = await telemetryClient.queryTraces({
        query: {
          projectId: opts?.projectId ?? "",
          executionId: opts?.executionId ?? "",
          correlationId: opts?.correlationId ?? "",
          traceId: opts?.traceId ?? "",
          limit: 50,
        },
      });
      return { traces: (res.traces ?? []) as Trace[], degraded: res.degraded };
    },
  });
}

export function useQueryMetrics(opts: {
  metricNames: string[];
  projectId?: string;
}) {
  return useQuery({
    queryKey: telemetryKeys.metrics(opts.metricNames, opts?.projectId),
    queryFn: async () => {
      const res = await telemetryClient.queryMetrics({
        metricNames: opts.metricNames,
        query: { projectId: opts?.projectId ?? "", limit: 100 },
      });
      return { series: (res.series ?? []) as MetricSeries[], degraded: res.degraded };
    },
  });
}

export function useQueryLogs(opts?: { projectId?: string; severity?: string }) {
  return useQuery({
    queryKey: telemetryKeys.logs(opts?.projectId),
    queryFn: async () => {
      const res = await telemetryClient.queryLogs({
        query: { projectId: opts?.projectId ?? "", limit: 100 },
        severity: opts?.severity ?? "",
      });
      return { logs: (res.logs ?? []) as LogRecord[], degraded: res.degraded };
    },
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
        summary: res.summary as DashboardSummary | undefined,
      };
    },
    // Poll so the dashboard advances with live usage records.
    refetchInterval: 5_000,
  });
}
