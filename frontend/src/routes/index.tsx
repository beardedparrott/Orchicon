import { createRoute } from "@tanstack/react-router";
import { useMemo } from "react";

import { Route as rootRoute } from "@/routes/__root";
import { useListProjects } from "@/api/projects";
import { useListExecutions } from "@/api/executions";
import { useListRecoveries } from "@/api/recovery";
import { useGetCost, useListOpenCodeModels } from "@/api/aigateway";
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
  const { data: models } = useListOpenCodeModels();

  const activeProjects = projects?.length ?? 0;

  const runningExecs = useMemo(() => {
    if (!executions) return 0;
    const active = new Set([1, 2, 3, 4, 5]);
    return executions.filter((e) => active.has(e.status)).length;
  }, [executions]);

  const totalCost = costData?.total?.costUsd ?? 0;
  const totalTokens = costData?.total?.totalTokens ?? 0;

  const activeRecoveries = useMemo(() => {
    if (!recoveries) return 0;
    return recoveries.filter((r) => r.status === 2).length;
  }, [recoveries]);

  const totalRecoveries = recoveries?.length ?? 0;

  const modelCount = models?.length ?? 0;
  const totalExecs = executions?.length ?? 0;

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">Dashboard</h1>
        <p className="text-sm text-muted-foreground">
          High-level overview of the Orchicon control plane.
        </p>
      </div>

      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <Tile label="Active Projects" value={String(activeProjects)} description="Total projects in the tenant" />
        <Tile label="Total Executions" value={fmtInt(totalExecs)} description="Worker executions (all time)" />
        <Tile label="Running Executions" value={String(runningExecs)} description="Executions in progress" accent />
        <Tile label="Available Models" value={String(modelCount)} description="Models from opencode CLI" />
      </div>

      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <Tile label="Total Spend (USD)" value={`$${totalCost.toFixed(4)}`} description="Lifetime AI model cost" />
        <Tile label="Total Tokens" value={fmtInt(totalTokens)} description="Lifetime token consumption" />
        <Tile label="Active Recoveries" value={String(activeRecoveries)} description="Ongoing recovery workflows" className="text-yellow-600" />
        <Tile label="Total Recoveries" value={fmtInt(totalRecoveries)} description="Recoveries triggered (all time)" />
      </div>
    </div>
  );
}

function Tile({
  label,
  value,
  description,
  accent,
  className,
}: {
  label: string;
  value: string;
  description?: string;
  accent?: boolean;
  className?: string;
}) {
  return (
    <div className={cn("rounded-lg border bg-card p-4", accent && "border-primary/30")}>
      <div className="text-xs uppercase text-muted-foreground">{label}</div>
      <div className={cn("mt-2 text-3xl font-semibold", className)}>{value}</div>
      {description && <div className="mt-1 text-xs text-muted-foreground">{description}</div>}
    </div>
  );
}

function fmtInt(n: number | bigint | undefined): string {
  if (n === undefined || n === null) return "0";
  const v = typeof n === "bigint" ? Number(n) : n;
  return v.toLocaleString();
}
