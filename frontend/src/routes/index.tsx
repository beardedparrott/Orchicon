import { createRoute } from "@tanstack/react-router";
import { useMemo } from "react";

import { Route as rootRoute } from "@/routes/__root";
import { useListProjects } from "@/api/projects";
import { useListExecutions } from "@/api/executions";
import { useListRecoveries } from "@/api/recovery";
import { useGetCost, useGetUsage, useListOpenCodeModels } from "@/api/aigateway";
import { UsageRollup } from "@/api/gen/orchicon/api/v1/ai_gateway_pb";
import { cn } from "@/lib/utils";

export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/",
  component: DashboardPage,
});

function DashboardPage() {
  const { data: projects } = useListProjects();
  const { data: executions } = useListExecutions({});
  const { data: recoveries } = useListRecoveries({});
  const { data: costData } = useGetCost({ rollup: UsageRollup.PROJECT });
  const { data: usageRecords } = useGetUsage({});
  const { data: models } = useListOpenCodeModels();

  const activeProjects = projects?.length ?? 0;

  const runningExecs = useMemo(() => {
    if (!executions) return 0;
    const active = new Set([1, 2, 3, 4, 5]); // dispatching → unhealthy
    return executions.filter((e) => active.has(e.status)).length;
  }, [executions]);

  const failedExecs = useMemo(() => {
    if (!executions) return 0;
    return executions.filter((e) => e.status === 10).length; // failed
  }, [executions]);

  const succeededExecs = useMemo(() => {
    if (!executions) return 0;
    return executions.filter((e) => e.status === 9).length; // succeeded
  }, [executions]);

  const recentCost = costData?.total?.costUsd ?? 0;
  const recentTokens = costData?.total?.totalTokens ?? 0;

  const activeRecoveries = useMemo(() => {
    if (!recoveries) return 0;
    return recoveries.filter((r) => r.status === 2).length; // running
  }, [recoveries]);

  const modelCount = models?.length ?? 0;
  const totalExecs = executions?.length ?? 0;

  const providerSet = useMemo(() => {
    if (!usageRecords) return new Set<string>();
    return new Set(usageRecords.map((r) => r.provider).filter(Boolean));
  }, [usageRecords]);

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">Dashboard</h1>
        <p className="text-sm text-muted-foreground">
          Orchicon orchestrates autonomous AI work as reliable, observable,
          recoverable, and manageable systems.
        </p>
      </div>

      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <Tile label="Active Projects" value={String(activeProjects)} />
        <Tile label="Available Models" value={String(modelCount)} />
        <Tile label="Total Executions" value={fmtInt(totalExecs)} />
        <Tile label="Providers Used" value={String(providerSet.size)} />
      </div>

      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <Tile label="Running Now" value={String(runningExecs)} accent />
        <Tile label="Succeeded" value={fmtInt(succeededExecs)} className="text-green-600" />
        <Tile label="Failed" value={fmtInt(failedExecs)} className="text-red-600" />
        <Tile label="Active Recoveries" value={String(activeRecoveries)} className="text-yellow-600" />
      </div>

      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <Tile label="Total Cost (USD)" value={`$${recentCost.toFixed(4)}`} />
        <Tile label="Total Tokens" value={fmtInt(recentTokens)} />
        <Tile label="Usage Records" value={fmtInt(usageRecords?.length ?? 0)} />
        <Tile label="Projects" value={String(activeProjects)} />
      </div>
    </div>
  );
}

function Tile({
  label,
  value,
  accent,
  className,
}: {
  label: string;
  value: string;
  accent?: boolean;
  className?: string;
}) {
  return (
    <div className={cn("rounded-lg border bg-card p-4", accent && "border-primary/30")}>
      <div className="text-xs uppercase text-muted-foreground">{label}</div>
      <div className={cn("mt-2 text-3xl font-semibold", className)}>{value}</div>
    </div>
  );
}

function fmtInt(n: number | bigint | undefined): string {
  if (n === undefined || n === null) return "0";
  const v = typeof n === "bigint" ? Number(n) : n;
  return v.toLocaleString();
}
