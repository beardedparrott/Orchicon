// Reports panel — a summarised window, not another chart.
//
// Reports read Postgres (usage_records + worker_executions + workflow_runs)
// through TelemetryService.GetTelemetryReport, so they keep working when a
// telemetry store is down: that is what makes the page's "partial" state
// honest (graphs degraded, report still populated).

import { useMemo, useState } from "react";

import { useGetTelemetryReport, type TelemetryFilters } from "@/api/telemetry";
import { TelemetryReportGroupBy } from "@/api/gen/orchicon/api/v1/telemetry_service_pb";
import { BarList } from "@/components/ui/chart";
import {
  TelemetryState,
  StatCard,
  fmtInt,
  isPermissionDenied,
} from "@/components/telemetry/states";
import { formatCompact, formatDurationMs, formatPercent } from "@/lib/chart-geometry";
import { cn } from "@/lib/utils";

type GroupByKey = "project" | "workflow";
type SortKey = "tokens" | "cost" | "duration" | "failure";

interface RowLike {
  groupKey: string;
  groupName: string;
  totalTokens: bigint;
  costUsd: number;
  executions: number;
  runs: number;
  failedRuns: number;
  durationMsTotal: bigint;
  durationMsAvg: bigint;
}

/** failureRate uses runs when the grouping produced them, else executions —
 *  the same denominator the server uses for totals.failure_rate. */
export function failureRate(runs: number, executions: number, failed: number): number {
  const denom = runs > 0 ? runs : executions;
  return denom > 0 ? failed / denom : 0;
}

export function ReportsPanel({ filters }: { filters: TelemetryFilters }) {
  const [groupBy, setGroupBy] = useState<GroupByKey>("project");
  const [sortKey, setSortKey] = useState<SortKey>("cost");

  const { data, isLoading, error, refetch } = useGetTelemetryReport({
    groupBy:
      groupBy === "workflow"
        ? TelemetryReportGroupBy.WORKFLOW
        : TelemetryReportGroupBy.PROJECT,
    filters,
  });

  const rows: RowLike[] = useMemo(
    () =>
      (data?.rows ?? []).map((r) => ({
        groupKey: r.groupKey,
        groupName: r.groupName || r.groupKey || "—",
        totalTokens: r.totalTokens,
        costUsd: r.costUsd,
        executions: r.executions,
        runs: r.runs,
        failedRuns: r.failedRuns,
        durationMsTotal: r.durationMsTotal,
        durationMsAvg: r.durationMsAvg,
      })),
    [data],
  );

  const sorted = useMemo(() => {
    const withRate = rows.map((r) => ({
      ...r,
      rate: failureRate(r.runs, r.executions, r.failedRuns),
    }));
    withRate.sort((a, b) => {
      switch (sortKey) {
        case "tokens":
          return Number(b.totalTokens - a.totalTokens);
        case "duration":
          return Number(b.durationMsTotal - a.durationMsTotal);
        case "failure":
          return b.rate - a.rate;
        default:
          return b.costUsd - a.costUsd;
      }
    });
    return withRate;
  }, [rows, sortKey]);

  const totals = data?.totals;
  const totalsRate = failureRate(
    totals?.runs ?? 0,
    totals?.executions ?? 0,
    totals?.failedRuns ?? 0,
  );

  const kind = isLoading
    ? "loading"
    : isPermissionDenied(error)
      ? "denied"
      : error
        ? "error"
        : rows.length === 0
          ? "empty"
          : "ready";

  const groupToggle = (
    <div className="flex rounded-md border" role="group" aria-label="Group reports by">
      {(["project", "workflow"] as GroupByKey[]).map((key) => (
        <button
          key={key}
          type="button"
          aria-pressed={groupBy === key}
          onClick={() => setGroupBy(key)}
          className={cn(
            "px-3 py-1 text-xs font-medium capitalize transition-colors focus-visible:ring-2 focus-visible:ring-ring",
            groupBy === key
              ? "bg-accent text-accent-foreground"
              : "text-muted-foreground hover:bg-accent/50",
            key === "project" ? "rounded-l-md" : "rounded-r-md",
          )}
        >
          {key}
        </button>
      ))}
    </div>
  );

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="grid flex-1 gap-3 sm:grid-cols-2 lg:grid-cols-4">
          <StatCard
            label="Tokens"
            value={formatCompact(Number(totals?.totalTokens ?? 0))}
            loading={isLoading}
            hint="window total"
          />
          <StatCard
            label="Cost (USD)"
            value={`$${Number(totals?.costUsd ?? 0).toFixed(4)}`}
            loading={isLoading}
            hint="window total"
          />
          <StatCard
            label="Avg duration"
            value={formatDurationMs(Number(totals?.durationMsAvg ?? 0))}
            loading={isLoading}
            hint="per finished unit"
          />
          <StatCard
            label="Failure rate"
            value={formatPercent(totalsRate)}
            loading={isLoading}
            hint={`${fmtInt(totals?.failedRuns ?? 0)} failed of ${fmtInt(
              (totals?.runs ?? 0) > 0 ? totals?.runs : totals?.executions,
            )}`}
          />
        </div>
        <div className="flex items-center gap-2">{groupToggle}</div>
      </div>

      {kind !== "ready" ? (
        <TelemetryState
          kind={kind}
          signal="reports"
          error={error}
          onRetry={() => void refetch()}
          hint="Reports read Postgres, so they keep working while a telemetry store is down. Widen the window to see earlier work."
        />
      ) : (
        <div className="grid gap-4 lg:grid-cols-2">
          <div className="rounded-xl glass-panel p-4">
            <h3 className="mb-2 text-sm font-medium">Cost share</h3>
            <BarList
              items={sorted.slice(0, 8).map((r) => ({
                key: r.groupKey,
                label: r.groupName,
                value: r.costUsd,
                note: `${fmtInt(r.executions)} exec`,
              }))}
              valueFormat={(v) => `$${v.toFixed(4)}`}
            />
          </div>
          <div className="overflow-x-auto rounded-xl glass-panel p-4">
            <h3 className="mb-2 text-sm font-medium">
              {groupBy === "project" ? "Per project" : "Per workflow"}
            </h3>
            <table className="w-full text-left text-sm">
              <caption className="sr-only">Telemetry report sorted by {sortKey}</caption>
              <thead>
                <tr className="text-[11px] uppercase text-muted-foreground">
                  <th scope="col" className="py-1 pr-2 font-medium">
                    {groupBy === "project" ? "Project" : "Workflow"}
                  </th>
                  <th scope="col" className="py-1 pr-2 font-medium">
                    <SortButton active={sortKey === "tokens"} onClick={() => setSortKey("tokens")}>
                      Tokens
                    </SortButton>
                  </th>
                  <th scope="col" className="py-1 pr-2 font-medium">
                    <SortButton active={sortKey === "cost"} onClick={() => setSortKey("cost")}>
                      Cost
                    </SortButton>
                  </th>
                  <th scope="col" className="py-1 pr-2 font-medium">
                    <SortButton active={sortKey === "duration"} onClick={() => setSortKey("duration")}>
                      Avg time
                    </SortButton>
                  </th>
                  <th scope="col" className="py-1 font-medium">
                    <SortButton active={sortKey === "failure"} onClick={() => setSortKey("failure")}>
                      Fail
                    </SortButton>
                  </th>
                </tr>
              </thead>
              <tbody className="divide-y divide-white/10">
                {sorted.map((r) => (
                  <tr key={r.groupKey}>
                    <td className="py-1.5 pr-2">
                      <span className="line-clamp-1" title={r.groupName}>
                        {r.groupName}
                      </span>
                    </td>
                    <td className="py-1.5 pr-2 tabular-nums">{formatCompact(Number(r.totalTokens))}</td>
                    <td className="py-1.5 pr-2 tabular-nums">${r.costUsd.toFixed(4)}</td>
                    <td className="py-1.5 pr-2 tabular-nums">{formatDurationMs(Number(r.durationMsAvg))}</td>
                    <td className="py-1.5 tabular-nums">{formatPercent(r.rate)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}
    </div>
  );
}

function SortButton({
  active,
  onClick,
  children,
}: {
  active: boolean;
  onClick: () => void;
  children: React.ReactNode;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      aria-pressed={active}
      className={cn(
        "inline-flex items-center gap-0.5 rounded px-1 py-0.5 uppercase transition-colors focus-visible:ring-2 focus-visible:ring-ring",
        active ? "text-foreground" : "text-muted-foreground hover:text-foreground",
      )}
    >
      {children}
      {active ? <span aria-hidden="true">▾</span> : null}
    </button>
  );
}
