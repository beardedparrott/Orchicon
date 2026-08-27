import { createRoute, Link } from "@tanstack/react-router";
import { useMemo, useState } from "react";
import { useQueries } from "@tanstack/react-query";

import { useGetCost, useGetUsage, useGetWorkflowCosts } from "@/api/aigateway";
import { useListProjects } from "@/api/projects";
import { workItemClient } from "@/api/clients";
import { workItemKeys } from "@/api/workItems";
import type { UsageRollup } from "@/api/gen/orchicon/api/v1/ai_gateway_pb";
import { UsageRollup as UsageRollupEnum } from "@/api/gen/orchicon/api/v1/ai_gateway_pb";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { SearchInput } from "@/components/cost-explorer/SearchInput";
import { SortControls } from "@/components/cost-explorer/SortControls";
import { SortableHeader } from "@/components/cost-explorer/SortableHeader";
import {
  filterSummaries,
  filterUsageRecords,
  filterWorkflowAggregates,
  sortSummaries,
  sortUsageRecords,
  sortWorkflowAggregates,
  toggleSort,
  type SortKey,
  type SortState,
  type UsageSortKey,
} from "@/components/cost-explorer/utils";
import { cn } from "@/lib/utils";
import { Route as rootRoute } from "@/routes/__root";

export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/cost-explorer",
  component: CostExplorerPage,
});

function CostExplorerPage() {
  return (
    <div className="space-y-6">
      <nav aria-label="Breadcrumb" className="text-xs text-muted-foreground flex items-center gap-1">
        <Link to="/dashboard" className="hover:text-foreground hover:underline">Overview</Link>
        <span aria-hidden>›</span>
        <span className="text-foreground font-medium">Cost Explorer</span>
      </nav>
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">Cost Explorer</h1>
        <p className="text-sm text-muted-foreground">
          Standalone cost and usage breakdown — fresh vs cache token split, provider/model
          breakdown, and project/work-item attribution. For traces and metrics, see{" "}
          <Link to="/telemetry" className="text-primary hover:underline">Telemetry</Link>.
        </p>
      </div>
      <CostExplorer />
    </div>
  );
}

type RollupMode = UsageRollup | "workflow";

function CostExplorer() {
  const [rollup, setRollup] = useState<RollupMode>(UsageRollupEnum.PROJECT);
  const [projectId, setProjectId] = useState("");
  const [taskId, setTaskId] = useState("");
  const [executionId, setExecutionId] = useState("");
  const [search, setSearch] = useState("");
  const [sort, setSort] = useState<SortState<SortKey>>({ key: "cost", dir: "desc" });
  const { data, isLoading, error } = useGetCost({
    rollup: rollup === "workflow" ? UsageRollupEnum.PROJECT : rollup,
    projectId: projectId || undefined,
    taskId: taskId || undefined,
    executionId: executionId || undefined,
  });

  const { data: projects } = useListProjects();

  const projectNameMap = useMemo(() => {
    const m = new Map<string, string>();
    if (projects) for (const p of projects) m.set(p.id, p.name);
    return m;
  }, [projects]);

  const taskIds = useMemo(() => {
    if (!data?.summaries || rollup !== UsageRollupEnum.TASK) return [];
    return [...new Set(data.summaries.map((s) => s.groupKey).filter(Boolean))];
  }, [data, rollup]);

  const taskQueries = useQueries({
    queries: taskIds.map((id) => ({
      queryKey: workItemKeys.detail(id),
      queryFn: async () => {
        const res = await workItemClient.getWorkItem({ id });
        return { id, title: res.workItem?.title || id.slice(0, 12) };
      },
      enabled: !!id,
      staleTime: 5 * 60 * 1000,
    })),
  });

  const taskNameMap = useMemo(() => {
    const m = new Map<string, string>();
    for (const q of taskQueries) {
      if (q.data) m.set(q.data.id, q.data.title);
    }
    return m;
  }, [taskQueries]);

  function scopeLabel(): string | null {
    if (taskId) return `Task: ${taskNameMap.get(taskId) || taskId.slice(0, 12)}`;
    if (projectId) return `Project: ${projectNameMap.get(projectId) || projectId.slice(0, 12)}`;
    return null;
  }

  function handleRowClick(key: string) {
    if (rollup === UsageRollupEnum.PROJECT) {
      setProjectId(key);
      setTaskId("");
      setExecutionId("");
      setRollup(UsageRollupEnum.TASK);
    } else if (rollup === UsageRollupEnum.TASK) {
      setTaskId(key);
      setExecutionId("");
      setRollup(UsageRollupEnum.EXECUTION);
    } else if (rollup === UsageRollupEnum.EXECUTION) {
      setExecutionId(key);
      setRollup(UsageRollupEnum.MODEL);
    }
  }

  function clearScope() {
    setProjectId("");
    setTaskId("");
    setExecutionId("");
    setRollup(UsageRollupEnum.PROJECT);
  }

  function rollbackOneLevel() {
    if (taskId) {
      setTaskId("");
      setExecutionId("");
      setRollup(UsageRollupEnum.TASK);
    } else if (projectId) {
      setProjectId("");
      setRollup(UsageRollupEnum.PROJECT);
    } else {
      clearScope();
    }
  }

  function displayName(s: { groupBy: string; groupKey: string; displayName?: string }): string {
    if (s.displayName) return s.displayName;
    if (s.groupBy === "project") return projectNameMap.get(s.groupKey) || s.groupKey.slice(0, 12);
    if (s.groupBy === "task") return taskNameMap.get(s.groupKey) || s.groupKey.slice(0, 12);
    return s.groupKey.slice(0, 12);
  }

  const visibleSummaries = sortSummaries(
    filterSummaries(data?.summaries ?? [], search, displayName),
    sort,
    displayName,
  );

  return (
    <div className="space-y-6">
      <Card className="glass-panel">
        <CardHeader>
          <CardTitle>Cost drill-down</CardTitle>
          <CardDescription>
            Roll up by a dimension, then drill into a row to narrow scope. Project → Task →
            Execution → Model (docs/10 §11).
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="flex flex-wrap items-center gap-2 rounded-2xl glass-panel p-3 border border-white/10">
            {([
              [UsageRollupEnum.PROJECT, "By Project"],
              [UsageRollupEnum.TASK, "By Task"],
              [UsageRollupEnum.EXECUTION, "By Execution"],
              [UsageRollupEnum.MODEL, "By Model"],
              ["workflow", "By Workflow"],
            ] as [RollupMode, string][]).map(([r, label]) => (
              <button
                key={String(r)}
                onClick={() => {
                  setRollup(r);
                  if (r === UsageRollupEnum.PROJECT) clearScope();
                }}
                className={cn(
                  "rounded-md border px-3 py-1.5 text-sm font-medium",
                  rollup === r
                    ? "border-primary bg-primary text-primary-foreground"
                    : "hover:bg-accent",
                )}
              >
                {label}
              </button>
            ))}
            {rollup !== "workflow" && scopeLabel() && (
              <span className="ml-2 text-xs text-muted-foreground">Scoped to {scopeLabel()}</span>
            )}
            {rollup !== "workflow" && (projectId || taskId) && (
              <>
                <button
                  onClick={rollbackOneLevel}
                  className="rounded-md border px-3 py-1.5 text-sm text-muted-foreground hover:bg-accent"
                >
                  ← Back
                </button>
                <button
                  onClick={clearScope}
                  className="rounded-md border px-3 py-1.5 text-sm text-muted-foreground hover:bg-accent"
                >
                  Clear all
                </button>
              </>
            )}
          </div>
          {rollup === "workflow" ? (
            <WorkflowCostPanel />
          ) : (
            <>
              {error && (
                <p className="text-sm text-destructive">Failed to load cost: {String(error)}</p>
              )}
              {isLoading && <p className="text-sm text-muted-foreground">Loading…</p>}
              {data?.total && (
                <div className="rounded-2xl glass-panel p-3 border border-white/10">
                  <div className="flex items-center justify-between">
                    <span className="text-sm font-medium">Window total</span>
                    <span className="font-medium">
                      ${data.total.costUsd?.toFixed(4) ?? "0.0000"} ·{" "}
                      {fmtSplitHit(
                        toNum(data.total.cacheReadTokens),
                        toNum(data.total.cacheWriteTokens),
                        toNum(data.total.promptTokens),
                      )}{" "}
                      · {fmtInt(data.total.totalTokens ?? 0)} total tok ·{" "}
                      {data.total.executionCount ?? 0} executions
                    </span>
                  </div>
                </div>
              )}
              <div className="flex flex-wrap items-center gap-3 rounded-2xl glass-panel p-3 border border-white/10">
                <SearchInput value={search} onChange={setSearch} placeholder="Search by name…" />
                <SortControls sort={sort} onChange={setSort} />
              </div>
              {data?.summaries && data.summaries.length > 0 && (
                <div className="divide-y divide-white/10 rounded-2xl glass-panel overflow-hidden">
                  {visibleSummaries.map((s) => (
                    <button
                      key={s.groupKey || "unknown"}
                      onClick={() => {
                        if (rollup !== UsageRollupEnum.MODEL) handleRowClick(s.groupKey);
                      }}
                      disabled={rollup === UsageRollupEnum.MODEL}
                      className={cn(
                        "flex w-full items-center justify-between px-3 py-2 text-left",
                        rollup === UsageRollupEnum.MODEL ? "cursor-default" : "hover:bg-accent",
                      )}
                    >
                      <span className="text-sm font-medium">{displayName(s)}</span>
                      <span className="text-sm">
                        ${(s.costUsd ?? 0).toFixed(4)} ·{" "}
                        {fmtSplitHit(
                          toNum(s.cacheReadTokens),
                          toNum(s.cacheWriteTokens),
                          toNum(s.promptTokens),
                        )}{" "}
                        · {fmtInt(s.totalTokens ?? 0)} tok · {s.executionCount ?? 0} execs
                        {s.finishedAt && (
                          <span className="ml-2 text-xs text-muted-foreground">
                            · finished {fmtWhen(s.finishedAt)}
                          </span>
                        )}
                      </span>
                    </button>
                  ))}
                </div>
              )}
              {data?.summaries && data.summaries.length > 0 && visibleSummaries.length === 0 && (
                <p className="text-sm text-muted-foreground">No rows match “{search}”.</p>
              )}
              {data?.summaries && data.summaries.length === 0 && (
                <p className="text-sm text-muted-foreground">
                  No usage records in scope. Run an execution to populate cost.
                </p>
              )}
            </>
          )}
        </CardContent>
      </Card>
      {rollup !== "workflow" && (
        <UsageRecordsTable projectId={projectId} taskId={taskId} executionId={executionId} />
      )}
    </div>
  );
}

function statusBadge(status: string | undefined): { label: string; className: string } {
  switch (status) {
    case "completed":
      return { label: "Succeeded", className: "bg-green-600/10 text-green-600 border-green-600/20" };
    case "failed":
      return { label: "Failed", className: "bg-red-600/10 text-red-600 border-red-600/20" };
    case "aborted":
      return { label: "Aborted", className: "bg-yellow-600/10 text-yellow-600 border-yellow-600/20" };
    default:
      return { label: status || "—", className: "bg-muted text-muted-foreground border-border" };
  }
}

function WorkflowCostPanel() {
  const { data: workflows, isLoading, error } = useGetWorkflowCosts();
  const [expandedWorkflow, setExpandedWorkflow] = useState<string | null>(null);
  const [expandedRun, setExpandedRun] = useState<string | null>(null);
  const [search, setSearch] = useState("");
  const [sort, setSort] = useState<SortState<SortKey>>({ key: "cost", dir: "desc" });

  const visible = useMemo(
    () => sortWorkflowAggregates(filterWorkflowAggregates(workflows ?? [], search), sort),
    [workflows, search, sort],
  );

  if (isLoading) return <p className="text-sm text-muted-foreground">Loading workflow costs…</p>;
  if (error)
    return <p className="text-sm text-destructive">Failed to load workflow costs: {String(error)}</p>;
  if (!workflows || workflows.length === 0)
    return <p className="text-sm text-muted-foreground">No workflow costs yet. Run a workflow to populate.</p>;

  return (
    <div className="space-y-2">
      <div className="flex flex-wrap items-center gap-3 rounded-2xl glass-panel p-3 border border-white/10">
        <SearchInput
          value={search}
          onChange={setSearch}
          placeholder="Search workflows, runs, workers…"
        />
        <SortControls sort={sort} onChange={setSort} />
      </div>
      {visible.length === 0 && (
        <p className="text-sm text-muted-foreground">No workflows match “{search}”.</p>
      )}
      {visible.map((wf) => {
        const wfExpanded = expandedWorkflow === wf.workflowId;
        return (
          <div key={wf.workflowId} className="rounded-md border">
            <button
              onClick={() => setExpandedWorkflow(wfExpanded ? null : wf.workflowId)}
              className="flex w-full items-center justify-between px-3 py-2 text-left hover:bg-accent"
            >
              <span className="text-sm font-medium">{wf.workflowName || wf.workflowId?.slice(0, 12)}</span>
              <span className="text-sm">
                ${(wf.totalCostUsd ?? 0).toFixed(4)} ·{" "}
                {fmtSplitHit(toNum(wf.cacheReadTokens), toNum(wf.cacheWriteTokens), toNum(wf.promptTokens))}{" "}
                · {fmtInt(wf.totalTokens ?? 0)} tok · {wf.runCount ?? 0} runs ·{" "}
                {wf.executionCount ?? 0} execs
                {wf.finishedAt && (
                  <span className="ml-2 text-xs text-muted-foreground">
                    · finished {fmtWhen(wf.finishedAt)}
                  </span>
                )}
              </span>
            </button>
            {wfExpanded && wf.runs && wf.runs.length > 0 && (
              <div className="border-t divide-y">
                {wf.runs.map((run) => {
                  const runExpanded = expandedRun === run.workflowRunId;
                  const badge = statusBadge(run.runStatus);
                  return (
                    <div key={run.workflowRunId}>
                      <button
                        onClick={() => setExpandedRun(runExpanded ? null : run.workflowRunId)}
                        className="flex w-full items-center justify-between px-5 py-2 text-left hover:bg-accent/50"
                      >
                        <span className="flex items-center gap-2">
                          <span
                            className={cn(
                              "inline-block rounded border px-1.5 py-0.5 text-[10px] font-medium uppercase leading-none",
                              badge.className,
                            )}
                          >
                            {badge.label}
                          </span>
                          <span className="text-xs font-medium truncate">
                            {run.workItemName || run.workflowRunId?.slice(0, 12)}
                          </span>
                          {run.workItemName && (
                            <span className="text-xs font-mono text-muted-foreground">
                              {run.workflowRunId?.slice(0, 12)}
                            </span>
                          )}
                        </span>
                        <span className="text-xs text-muted-foreground">
                          ${(run.totalCostUsd ?? 0).toFixed(4)} ·{" "}
                          {fmtSplitHit(
                            toNum(run.cacheReadTokens),
                            toNum(run.cacheWriteTokens),
                            toNum(run.promptTokens),
                          )}{" "}
                          · {fmtInt(run.totalTokens ?? 0)} tok · {run.executionCount ?? 0} execs
                          {run.finishedAt && (
                            <span className="ml-2 text-xs text-muted-foreground">
                              · finished {fmtWhen(run.finishedAt)}
                            </span>
                          )}
                        </span>
                      </button>
                      {runExpanded && run.workers && run.workers.length > 0 && (
                        <div className="border-t divide-y bg-muted/20">
                          {run.workers.map((worker) => (
                            <div
                              key={worker.workerId || "unknown"}
                              className="flex items-center justify-between px-7 py-2 text-sm"
                            >
                              <div className="min-w-0 flex-1">
                                <div className="font-medium truncate">
                                  {worker.workerName || worker.workerId || "Unknown worker"}
                                </div>
                                <div className="text-xs text-muted-foreground">
                                  {worker.executionCount ?? 0} execution{(worker.executionCount ?? 0) !== 1 ? "s" : ""}
                                </div>
                              </div>
                              <div className="text-right shrink-0 ml-4">
                                <div className="text-xs font-medium">
                                  ${(worker.totalCostUsd ?? 0).toFixed(4)}
                                </div>
                                <div className="text-xs text-muted-foreground">
                                  {fmtInt(worker.totalTokens ?? 0)} tok total ·{" "}
                                  {fmtSplitHit(
                                    toNum(worker.cacheReadTokens),
                                    toNum(worker.cacheWriteTokens),
                                    toNum(worker.promptTokens),
                                  )}{" "}
                                  · {fmtInt(worker.completionTokens ?? 0)} out
                                </div>
                              </div>
                            </div>
                          ))}
                        </div>
                      )}
                    </div>
                  );
                })}
              </div>
            )}
          </div>
        );
      })}
    </div>
  );
}

function UsageRecordsTable({
  projectId,
  taskId,
  executionId,
}: {
  projectId?: string;
  taskId?: string;
  executionId?: string;
}) {
  const { data, isLoading } = useGetUsage({ projectId, taskId, executionId });
  const [search, setSearch] = useState("");
  const [sort, setSort] = useState<SortState<UsageSortKey>>({ key: "when", dir: "desc" });
  const visible = useMemo(
    () => sortUsageRecords(filterUsageRecords(data ?? [], search), sort),
    [data, search, sort],
  );
  return (
    <Card className="glass-panel">
      <CardHeader>
        <CardTitle>Recent usage records</CardTitle>
        <CardDescription>
          Postgres source-of-truth rows (docs/08 §5.2). Mirrored to VictoriaMetrics as OTel metrics
          for the embedded Grafana views.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-2">
        <SearchInput value={search} onChange={setSearch} placeholder="Search records…" />
        {isLoading && <p className="text-sm text-muted-foreground">Loading…</p>}
        {data && data.length === 0 && (
          <p className="text-sm text-muted-foreground">No usage records yet.</p>
        )}
        {data && data.length > 0 && (
          <div className="overflow-x-auto">
            <table className="w-full text-sm">
              <thead className="text-left text-muted-foreground">
                <tr>
                  <SortableHeader label="Worker" sortKey="worker" activeKey={sort.key} dir={sort.dir} onSort={(k) => setSort(toggleSort(sort, k))} />
                  <SortableHeader label="Task" sortKey="task" activeKey={sort.key} dir={sort.dir} onSort={(k) => setSort(toggleSort(sort, k))} />
                  <SortableHeader label="Provider" sortKey="provider" activeKey={sort.key} dir={sort.dir} onSort={(k) => setSort(toggleSort(sort, k))} />
                  <SortableHeader label="Model" sortKey="model" activeKey={sort.key} dir={sort.dir} onSort={(k) => setSort(toggleSort(sort, k))} />
                  <SortableHeader label="Tokens" sortKey="tokens" align="right" activeKey={sort.key} dir={sort.dir} onSort={(k) => setSort(toggleSort(sort, k))} />
                  <SortableHeader label="Cost (USD)" sortKey="cost" align="right" activeKey={sort.key} dir={sort.dir} onSort={(k) => setSort(toggleSort(sort, k))} />
                  <SortableHeader label="When" sortKey="when" activeKey={sort.key} dir={sort.dir} onSort={(k) => setSort(toggleSort(sort, k))} />
                </tr>
              </thead>
              <tbody className="divide-y">
                {visible.map((r) => (
                  <tr key={r.id}>
                    <td className="py-1 pr-3 text-sm">{r.workerName || (r.workerId || "—").slice(0, 12)}</td>
                    <td className="py-1 pr-3 text-xs text-muted-foreground">
                      {r.taskTitle || (r.taskId || "—").slice(0, 12)}
                    </td>
                    <td className="py-1 pr-3">{r.provider || "—"}</td>
                    <td className="py-1 pr-3 font-mono text-xs">{r.model || "—"}</td>
                    <td className="py-1 pr-3 text-right">{fmtInt(r.totalTokens ?? 0)}</td>
                    <td className="py-1 pr-3 text-right">${(r.costUsd ?? 0).toFixed(4)}</td>
                    <td className="py-1 text-xs text-muted-foreground">
                      {r.occurredAt ? new Date(r.occurredAt.toDate()).toLocaleString() : "—"}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        {data && data.length > 0 && visible.length === 0 && (
          <p className="text-sm text-muted-foreground">No records match “{search}”.</p>
        )}
      </CardContent>
    </Card>
  );
}

function fmtInt(n: number | bigint | undefined): string {
  if (n === undefined || n === null) return "0";
  const v = typeof n === "bigint" ? Number(n) : n;
  return v.toLocaleString();
}

function toNum(n: number | bigint | undefined | null): number {
  if (n === undefined || n === null) return 0;
  return typeof n === "bigint" ? Number(n) : n;
}

function fmtWhen(ts: { seconds?: bigint; nanos?: number } | undefined): string {
  if (!ts) return "—";
  const millis = Number(ts.seconds ?? 0n) * 1000 + Math.floor((ts.nanos ?? 0) / 1e6);
  return new Date(millis).toLocaleString();
}

function cacheHitRatioPct(cacheRead: number, fresh: number): number {
  if (cacheRead + fresh === 0) return 0;
  return Math.round((cacheRead / (cacheRead + fresh)) * 100);
}

function fmtTokenSplit(cacheRead: number, cacheWrite: number, fresh: number): string {
  return [
    `${fmtInt(fresh)} fresh`,
    `${fmtInt(cacheRead)} cache-read`,
    `${fmtInt(cacheWrite)} cache-write`,
  ].join(" · ");
}

function fmtSplitHit(cacheRead: number, cacheWrite: number, fresh: number): string {
  return `${fmtTokenSplit(cacheRead, cacheWrite, fresh)} · ${cacheHitRatioPct(cacheRead, fresh)}% hit`;
}
