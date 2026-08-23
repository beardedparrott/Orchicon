import { createRoute } from "@tanstack/react-router";
import { useMemo, useState } from "react";

import { useQueries } from "@tanstack/react-query";

import { useGetCost, useGetUsage, useGetWorkflowCosts, useListProviders } from "@/api/aigateway";
import { useGetDashboard, useQueryLogs, useQueryMetrics, useQueryTraces } from "@/api/telemetry";
import { useListProjects } from "@/api/projects";
import { workItemClient } from "@/api/clients";
import { workItemKeys } from "@/api/workItems";
import { GRAFANA_UI_URL } from "@/api/clients";
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

// Telemetry hub (docs/10 §11): seamlessly embedded Grafana for raw
// traces/metrics/logs exploration (same visual language, inside the
// Orchicon shell — not a separate tool) plus custom Orchicon-specific
// views: cost explorer with Tenant→Project→Task→Execution drill-down,
// and the Orchicon dashboard.
export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/telemetry",
  component: TelemetryPage,
});

type Tab = "overview" | "cost" | "traces" | "metrics" | "logs" | "credits";

const TABS: { id: Tab; label: string }[] = [
  { id: "overview", label: "Overview" },
  { id: "cost", label: "Cost Explorer" },
  { id: "credits", label: "Credits" },
  { id: "traces", label: "Traces (Grafana)" },
  { id: "metrics", label: "Metrics (Grafana)" },
  { id: "logs", label: "Logs (Grafana)" },
];

function TelemetryPage() {
  const [tab, setTab] = useState<Tab>("overview");
  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">Telemetry</h1>
        <p className="text-sm text-muted-foreground">
          Traces, metrics, logs, and cost — explore without leaving the
          Orchicon shell. Raw exploration uses embedded Grafana (Tempo /
          Loki / VictoriaMetrics); cost + the Orchicon dashboard are
          custom views (docs/10 §11).
        </p>
      </div>
      <div className="flex flex-wrap gap-2 border-b pb-px">
        {TABS.map((t) => (
          <button
            key={t.id}
            onClick={() => setTab(t.id)}
            className={cn(
              "rounded-t-md px-3 py-2 text-sm font-medium transition-colors",
              tab === t.id
                ? "border-b-2 border-primary text-foreground"
                : "text-muted-foreground hover:text-foreground",
            )}
          >
            {t.label}
          </button>
        ))}
      </div>
      {tab === "overview" && <OverviewPanel />}
      {tab === "cost" && <CostExplorer />}
      {tab === "credits" && <CreditsPanel />}
      {tab === "traces" && <TracesPanel />}
      {tab === "metrics" && <MetricsPanel />}
      {tab === "logs" && <LogsPanel />}
    </div>
  );
}

// OverviewPanel is the custom Orchicon dashboard: high-level cost +
// usage roll-up + per-model cost breakdown (docs/10 §11). Built custom
// because it's domain-specific; raw exploration uses the embedded
// Grafana UI.
function OverviewPanel() {
  const { data, isLoading } = useGetDashboard();
  const { data: providers } = useListProviders();
  const summary = data?.summary;
  return (
    <div className="space-y-6">
      <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-4">
        <StatCard
          label="Total tokens"
          value={summary ? fmtInt(summary.totalTokens) : "—"}
          loading={isLoading}
        />
        <StatCard
          label="Total cost (USD)"
          value={summary ? `$${summary.totalCostUsd.toFixed(4)}` : "—"}
          loading={isLoading}
        />
        <StatCard
          label="Executions"
          value={summary ? fmtInt(summary.totalExecutions) : "—"}
          loading={isLoading}
        />
        <StatCard
          label="Providers"
          value={providers ? String(providers.length) : "—"}
          loading={isLoading}
        />
      </div>
      <Card>
        <CardHeader>
          <CardTitle>Cost by model</CardTitle>
          <CardDescription>
            Per-model USD spend over the lifetime of the workspace (custom
            Orchicon roll-up from Postgres usage_records — source of
            truth).
          </CardDescription>
        </CardHeader>
        <CardContent>
          {data?.panels && data.panels.length === 0 && (
            <p className="text-sm text-muted-foreground">
              No usage recorded yet. Run an execution to populate cost
              telemetry.
            </p>
          )}
          {data?.panels && data.panels.length > 0 && (
            <div className="space-y-2">
              {data.panels.map((p) => {
                const model = p.labels?.["model"] ?? "unknown";
                const cost = p.points?.[0]?.value ?? 0;
                return (
                  <div
                    key={model}
                    className="flex items-center justify-between rounded-md border px-3 py-2"
                  >
                    <span className="font-mono text-sm">{model}</span>
                    <span className="font-medium">${cost.toFixed(4)}</span>
                  </div>
                );
              })}
            </div>
          )}
        </CardContent>
      </Card>
    </div>
  );
}

type RollupMode = UsageRollup | "workflow";

// CostExplorer is the custom cost explorer with drill-down
// Project → Task → Execution → Model (docs/10 §11). The drill-down is
// server-validated — the UI reflects state, it does not make policy
// (AGENTS.md invariant #1).
function CostExplorer() {
  const [rollup, setRollup] = useState<RollupMode>(UsageRollupEnum.PROJECT);
  const [projectId, setProjectId] = useState("");
  const [taskId, setTaskId] = useState("");
  const [executionId, setExecutionId] = useState("");
  const [search, setSearch] = useState("");
  // Default matches the backend ORDER BY cost_usd DESC, so the initial view
  // is unchanged until the user sorts. Local per-tab state — never mutates
  // the API query params (docs/10 §11).
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

  // Look up individual work item names for task-level summaries.
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

  // Search + sort over the already-fetched page (no server round-trip).
  // Rows keep their groupKey — drill-down reads it from these same objects,
  // so sorting/filtering never breaks navigation.
  const visibleSummaries = sortSummaries(
    filterSummaries(data?.summaries ?? [], search, displayName),
    sort,
    displayName,
  );

  return (
    <div className="space-y-6">
      <Card>
        <CardHeader>
          <CardTitle>Cost drill-down</CardTitle>
          <CardDescription>
            Roll up by a dimension, then drill into a row to narrow scope.
            Project → Task → Execution → Model (docs/10 §11).
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="flex flex-wrap items-center gap-2">
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
              <span className="ml-2 text-xs text-muted-foreground">
                Scoped to {scopeLabel()}
              </span>
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
                <p className="text-sm text-destructive">
                  Failed to load cost: {String(error)}
                </p>
              )}
              {isLoading && <p className="text-sm text-muted-foreground">Loading…</p>}
              {data?.total && (
                <div className="rounded-md border bg-muted/40 p-3">
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
              <div className="flex flex-wrap items-center gap-3">
                <SearchInput
                  value={search}
                  onChange={setSearch}
                  placeholder="Search by name…"
                />
                <SortControls sort={sort} onChange={setSort} />
              </div>
              {data?.summaries && data.summaries.length > 0 && (
                <div className="divide-y rounded-md border">
                  {visibleSummaries.map((s) => (
                    <button
                      key={s.groupKey || "unknown"}
                      onClick={() => {
                        if (rollup !== UsageRollupEnum.MODEL) handleRowClick(s.groupKey);
                      }}
                      disabled={rollup === UsageRollupEnum.MODEL}
                      className={cn(
                        "flex w-full items-center justify-between px-3 py-2 text-left",
                        rollup === UsageRollupEnum.MODEL
                          ? "cursor-default"
                          : "hover:bg-accent",
                      )}
                    >
                      <span className="text-sm font-medium">
                        {displayName(s)}
                      </span>
                      <span className="text-sm">
                        ${(s.costUsd ?? 0).toFixed(4)} ·{" "}
                        {fmtSplitHit(
                          toNum(s.cacheReadTokens),
                          toNum(s.cacheWriteTokens),
                          toNum(s.promptTokens),
                        )}{" "}
                        · {fmtInt(s.totalTokens ?? 0)} tok ·{" "}
                        {s.executionCount ?? 0} execs
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
                <p className="text-sm text-muted-foreground">
                  No rows match “{search}”.
                </p>
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
        <UsageRecordsTable
          projectId={projectId}
          taskId={taskId}
          executionId={executionId}
        />
      )}
    </div>
  );
}

function statusBadge(status: string | undefined): { label: string; className: string } {
  switch (status) {
    case "completed": return { label: "Succeeded", className: "bg-green-600/10 text-green-600 border-green-600/20" };
    case "failed": return { label: "Failed", className: "bg-red-600/10 text-red-600 border-red-600/20" };
    case "aborted": return { label: "Aborted", className: "bg-yellow-600/10 text-yellow-600 border-yellow-600/20" };
    default: return { label: status || "—", className: "bg-muted text-muted-foreground border-border" };
  }
}

function WorkflowCostPanel() {
  const { data: workflows, isLoading, error } = useGetWorkflowCosts();
  const [expandedWorkflow, setExpandedWorkflow] = useState<string | null>(null);
  const [expandedRun, setExpandedRun] = useState<string | null>(null);
  const [search, setSearch] = useState("");
  const [sort, setSort] = useState<SortState<SortKey>>({ key: "cost", dir: "desc" });

  // Search prunes workflows/runs/workers; the sort orders workflows and the
  // runs within each expanded workflow (worker rows inherit the run order).
  const visible = useMemo(
    () =>
      sortWorkflowAggregates(
        filterWorkflowAggregates(workflows ?? [], search),
        sort,
      ),
    [workflows, search, sort],
  );

  if (isLoading) return <p className="text-sm text-muted-foreground">Loading workflow costs…</p>;
  if (error) return <p className="text-sm text-destructive">Failed to load workflow costs: {String(error)}</p>;
  if (!workflows || workflows.length === 0)
    return <p className="text-sm text-muted-foreground">No workflow costs yet. Run a workflow to populate.</p>;

  return (
    <div className="space-y-2">
      <div className="flex flex-wrap items-center gap-3">
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
            {/* Workflow level (aggregated across all runs) */}
            <button
              onClick={() => setExpandedWorkflow(wfExpanded ? null : wf.workflowId)}
              className="flex w-full items-center justify-between px-3 py-2 text-left hover:bg-accent"
            >
              <span className="text-sm font-medium">
                {wf.workflowName || wf.workflowId?.slice(0, 12)}
              </span>
              <span className="text-sm">
                ${(wf.totalCostUsd ?? 0).toFixed(4)} ·{" "}
                {fmtSplitHit(
                  toNum(wf.cacheReadTokens),
                  toNum(wf.cacheWriteTokens),
                  toNum(wf.promptTokens),
                )}{" "}
                · {fmtInt(wf.totalTokens ?? 0)} tok · {wf.runCount ?? 0} runs · {wf.executionCount ?? 0} execs
                {wf.finishedAt && (
                  <span className="ml-2 text-xs text-muted-foreground">
                    · finished {fmtWhen(wf.finishedAt)}
                  </span>
                )}
              </span>
            </button>

            {/* Runs level */}
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
                          <span className={cn("inline-block rounded border px-1.5 py-0.5 text-[10px] font-medium uppercase leading-none", badge.className)}>
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

                      {/* Worker level inside run */}
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

  // Local per-tab search + sort over the already-fetched page. Default order
  // (by When desc) matches the backend ORDER BY occurred_at DESC.
  const visible = useMemo(
    () => sortUsageRecords(filterUsageRecords(data ?? [], search), sort),
    [data, search, sort],
  );

  return (
    <Card>
      <CardHeader>
        <CardTitle>Recent usage records</CardTitle>
        <CardDescription>
          Postgres source-of-truth rows (docs/08 §5.2). Mirrored to
          VictoriaMetrics as OTel metrics for the embedded Grafana views.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-2">
        <SearchInput
          value={search}
          onChange={setSearch}
          placeholder="Search records…"
        />
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
                    <td className="py-1 pr-3 text-sm">
                      {r.workerName || (r.workerId || "—").slice(0, 12)}
                    </td>
                    <td className="py-1 pr-3 text-xs text-muted-foreground">
                      {r.taskTitle || (r.taskId || "—").slice(0, 12)}
                    </td>
                    <td className="py-1 pr-3">{r.provider || "—"}</td>
                    <td className="py-1 pr-3 font-mono text-xs">
                      {r.model || "—"}
                    </td>
                    <td className="py-1 pr-3 text-right">
                      {fmtInt(r.totalTokens ?? 0)}
                    </td>
                    <td className="py-1 pr-3 text-right">
                      ${(r.costUsd ?? 0).toFixed(4)}
                    </td>
                    <td className="py-1 text-xs text-muted-foreground">
                      {r.occurredAt
                        ? new Date(r.occurredAt.toDate()).toLocaleString()
                        : "—"}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        {data && data.length > 0 && visible.length === 0 && (
          <p className="text-sm text-muted-foreground">
            No records match “{search}”.
          </p>
        )}
      </CardContent>
    </Card>
  );
}

// TracesPanel shows recent traces (projected from Tempo) + the embedded
// Grafana trace explorer for raw drill-down (docs/10 §11).
function TracesPanel() {
  const { data, isLoading } = useQueryTraces();
  const degraded = data?.degraded ?? false;
  return (
    <div className="space-y-6">
      <GrafanaEmbed title="Trace Explorer" degraded={degraded} />
      <Card>
        <CardHeader>
          <CardTitle>Recent traces</CardTitle>
          <CardDescription>
            Projected from Tempo, scoped to the current tenant (docs/08
            §5.1). Open the embedded Grafana UI for full drill-down.
          </CardDescription>
        </CardHeader>
        <CardContent className="max-h-[200px] overflow-y-auto">
          {isLoading && <p className="text-sm text-muted-foreground">Loading…</p>}
          {data?.traces && data.traces.length === 0 && !data.degraded && (
            <p className="text-sm text-muted-foreground">No traces yet.</p>
          )}
          {data?.traces && data.traces.length > 0 && (
            <div className="divide-y rounded-md border">
              {data.traces.map((t) => (
                <div
                  key={t.traceId}
                  className="flex items-center justify-between px-3 py-2"
                >
                  <div>
                    <div className="font-mono text-xs text-muted-foreground">
                      {t.traceId?.slice(0, 16)}…
                    </div>
                    <div className="text-sm font-medium">
                      {t.rootSpanName || "—"}
                    </div>
                  </div>
                  <div className="text-right text-sm">
                    <div>{fmtInt(t.durationUs ?? 0)} µs</div>
                    <div className="text-xs text-muted-foreground">
                      {t.spanCount ?? 0} spans
                    </div>
                  </div>
                </div>
              ))}
            </div>
          )}
        </CardContent>
      </Card>
    </div>
  );
}

function LogsPanel() {
  const { data, isLoading } = useQueryLogs();
  const degraded = data?.degraded ?? false;
  return (
    <div className="space-y-6">
      <GrafanaEmbed title="Log Explorer" degraded={degraded} />
      <Card>
        <CardHeader>
          <CardTitle>Recent logs</CardTitle>
          <CardDescription>
            Structured OTel log records carrying trace_id + correlation_id
            (docs/08 §5.3).
          </CardDescription>
        </CardHeader>
        <CardContent className="max-h-[200px] overflow-y-auto">
          {isLoading && <p className="text-sm text-muted-foreground">Loading…</p>}
          {data?.logs && data.logs.length === 0 && !data.degraded && (
            <p className="text-sm text-muted-foreground">No logs yet.</p>
          )}
          {data?.logs && data.logs.length > 0 && (
            <div className="space-y-2">
              {data.logs.map((l, i) => (
                <div key={i} className="rounded-md border px-3 py-2 text-sm">
                  <div className="flex items-center justify-between">
                    <span className="font-mono text-xs text-muted-foreground">
                      {l.severity || "INFO"}
                    </span>
                    <span className="font-mono text-xs text-muted-foreground">
                      {l.traceId?.slice(0, 12)}…
                    </span>
                  </div>
                  <p className="mt-1">{l.body || "—"}</p>
                </div>
              ))}
            </div>
          )}
        </CardContent>
      </Card>
    </div>
  );
}

// MetricsPanel shows live metric values from VictoriaMetrics plus the
// embedded Grafana metrics explorer for raw drill-down.
function MetricsPanel() {
  const { data, isLoading } = useQueryMetrics({
    metricNames: ["orchicon_tokens_consumed", "orchicon_cost_usd", "orchicon_outbox_lag"],
  });
  const degraded = data?.degraded ?? false;
  const series = data?.series ?? [];

  return (
    <div className="space-y-6">
      <GrafanaEmbed title="Metrics Explorer" degraded={degraded} />
      <Card>
        <CardHeader>
          <CardTitle>Metric values</CardTitle>
          <CardDescription>
            Latest samples from VictoriaMetrics.
          </CardDescription>
        </CardHeader>
        <CardContent>
          {isLoading && <p className="text-sm text-muted-foreground">Loading…</p>}
          {!isLoading && series.length === 0 && !degraded && (
            <p className="text-sm text-muted-foreground">
              No metric data yet. Run an execution to populate metrics.
            </p>
          )}
          {series.length > 0 && (
            <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3 max-h-[200px] overflow-y-auto">
              {series.map((s) => {
                const pts = s.points ?? [];
                const latest = pts[0];
                const name = s.metricName || "unknown";
                const display = name === "orchicon_tokens_consumed"
                  ? `${fmtInt(latest?.value ?? 0)} tokens`
                  : name === "orchicon_cost_usd"
                    ? `$${(latest?.value ?? 0).toFixed(4)}`
                    : name === "orchicon_outbox_lag"
                      ? `${fmtInt(latest?.value ?? 0)} lag`
                      : String(latest?.value ?? 0);
                return (
                  <div key={name} className="rounded-md border p-3">
                    <div className="text-xs text-muted-foreground">{name}</div>
                    <div className="mt-1 text-lg font-semibold">{display}</div>
                    <div className="mt-0.5 text-[10px] text-muted-foreground">
                      {pts.length} samples
                    </div>
                  </div>
                );
              })}
            </div>
          )}
        </CardContent>
      </Card>
    </div>
  );
}

// CreditsPanel shows total credits available and spent by provider, with
// model usage breakdown within each provider. Computed from usage records
// since no dedicated credit-tracking endpoint exists yet.
function CreditsPanel() {
  const { data: usageRecords, isLoading } = useGetUsage({});
  const { data: providers } = useListProviders();

  const providerMap = useMemo(() => {
    const m = new Map<string, string>();
    if (providers) for (const p of providers) m.set(p.id, p.name);
    return m;
  }, [providers]);

  const { byProvider, grandTotal } = useMemo(() => {
    const byProvider = new Map<
      string,
      { totalCost: number; totalTokens: number; count: number; models: Map<string, { cost: number; tokens: number; count: number }> }
    >();
    const grandTotal = { cost: 0, tokens: 0, count: 0 };
    if (usageRecords) {
      for (const r of usageRecords) {
        const provider = r.provider || "unknown";
        const model = r.model || "unknown";
        const cost = Number(r.costUsd ?? 0);
        const tokens = Number(r.totalTokens ?? 0);

        let p = byProvider.get(provider);
        if (!p) {
          p = { totalCost: 0, totalTokens: 0, count: 0, models: new Map() };
          byProvider.set(provider, p);
        }
        p.totalCost += cost;
        p.totalTokens += tokens;
        p.count += 1;

        let m = p.models.get(model);
        if (!m) {
          m = { cost: 0, tokens: 0, count: 0 };
          p.models.set(model, m);
        }
        m.cost += cost;
        m.tokens += tokens;
        m.count += 1;

        grandTotal.cost += cost;
        grandTotal.tokens += tokens;
        grandTotal.count += 1;
      }
    }
    return { byProvider, grandTotal };
  }, [usageRecords]);

  return (
    <div className="space-y-6">
      <div className="grid gap-4 md:grid-cols-3">
        <StatCard
          label="Total spent (USD)"
          value={`$${grandTotal.cost.toFixed(4)}`}
          loading={isLoading}
        />
        <StatCard
          label="Total tokens"
          value={fmtInt(grandTotal.tokens)}
          loading={isLoading}
        />
        <StatCard
          label="Usage records"
          value={fmtInt(grandTotal.count)}
          loading={isLoading}
        />
      </div>
      {byProvider.size === 0 && !isLoading && (
        <Card>
          <CardContent className="py-6">
            <p className="text-sm text-muted-foreground text-center">
              No usage records yet. Run an execution to populate credit
              telemetry.
            </p>
          </CardContent>
        </Card>
      )}
      {byProvider.size > 0 &&
        Array.from(byProvider.entries()).map(([providerId, p]) => {
          const displayName = providerMap.get(providerId) || providerId;
          return (
            <Card key={providerId}>
              <CardHeader>
                <CardTitle>{displayName}</CardTitle>
                <CardDescription>
                  ${p.totalCost.toFixed(4)} · {fmtInt(p.totalTokens)} tokens ·{" "}
                  {p.count} records
                </CardDescription>
              </CardHeader>
              <CardContent>
                <div className="space-y-1">
                  {Array.from(p.models.entries()).map(([model, m]) => (
                    <div
                      key={model}
                      className="flex items-center justify-between rounded-md border px-3 py-2 text-sm"
                    >
                      <span className="font-mono text-xs">{model}</span>
                      <span className="text-xs text-muted-foreground">
                        ${m.cost.toFixed(4)} · {fmtInt(m.tokens)} tok ·{" "}
                        {m.count} calls
                      </span>
                    </div>
                  ))}
                </div>
              </CardContent>
            </Card>
          );
        })}
    </div>
  );
}

// GrafanaEmbed renders the Grafana UI inside the Orchicon shell via a
// same-origin iframe (proxied under /grafana — docs/10 §11: seamless
// embedding, not a separate tool launch). Grafana runs with
// serve_from_sub_path so every URL is generated under /grafana; the
// iframe shares the Orchicon shell's chrome so it feels like one
// platform. When degraded, a placeholder is shown instead of a broken
// iframe loading the SPA fallback (AGENTS.md verification §2).
//
// The embed is a provisioned "Orchicon" dashboard in kiosk mode
// (uid orchicon-dashboard, provisioned in
// deploy/compose/grafana-provisioning/dashboards/orchicon.json) rather
// than a Grafana Explore deep-link: Explore redirects anonymous users to
// the sign-in/home page, whereas dashboards render for the anonymous
// Viewer role out of the box.
function GrafanaEmbed({
  title,
  degraded,
}: {
  title: string;
  degraded?: boolean;
}) {
  const src = `${GRAFANA_UI_URL}${grafanaDashboardPath()}`;
  return (
    <Card>
      <CardHeader>
        <CardTitle>{title} — embedded Grafana</CardTitle>
        <CardDescription>
          Same visual language, inside the Orchicon shell (docs/10 §11).
          The Grafana UI is proxied same-origin.
        </CardDescription>
      </CardHeader>
      <CardContent>
        {degraded ? (
          <div className="flex h-[160px] items-center justify-center rounded-md border border-dashed text-sm text-muted-foreground">
            Telemetry backend is starting up — check back in a moment.
            The dev stack starts automatically with `orchicon start` /
            `orchicon dev start`.
          </div>
        ) : (
          <iframe
            src={src}
            title={title}
            className="h-[640px] w-full rounded-md border"
            sandbox="allow-same-origin allow-scripts allow-forms allow-popups"
          />
        )}
      </CardContent>
    </Card>
  );
}

// grafanaDashboardPath returns the embedded Orchicon dashboard URL
// (kiosk mode hides the Grafana side nav inside the shell).
function grafanaDashboardPath(): string {
  return `/d/orchicon-dashboard/orchicon?kiosk&from=now-1h&to=now`;
}

function StatCard({
  label,
  value,
  loading,
}: {
  label: string;
  value: string;
  loading?: boolean;
}) {
  return (
    <Card>
      <CardHeader className="pb-0">
        <CardDescription className="text-xs">{label}</CardDescription>
      </CardHeader>
      <CardContent className="pb-3 pt-1">
        <div className="text-xl font-semibold">
          {loading ? "…" : value}
        </div>
      </CardContent>
    </Card>
  );
}

function fmtInt(n: number | bigint | undefined): string {
  if (n === undefined || n === null) return "0";
  const v = typeof n === "bigint" ? Number(n) : n;
  return v.toLocaleString();
}

// toNum normalises a proto int64 (bigint) or number to a JS number so the
// cache-hit-ratio math and fmtInt work uniformly across every surface.
function toNum(n: number | bigint | undefined | null): number {
  if (n === undefined || n === null) return 0;
  return typeof n === "bigint" ? Number(n) : n;
}

// fmtWhen renders a proto Timestamp as a locale date-time, or "—" when absent.
function fmtWhen(ts: { seconds?: bigint; nanos?: number } | undefined): string {
  if (!ts) return "—";
  const millis = Number(ts.seconds ?? 0n) * 1000 + Math.floor((ts.nanos ?? 0) / 1e6);
  return new Date(millis).toLocaleString();
}

// cacheHitRatioPct is the canonical platform cache hit ratio (docs/10 §11):
// cache_read ÷ (cache_read + fresh_input), where fresh_input = prompt_tokens.
// cache_write is a separate cost bucket, excluded by definition. Renders 0%
// when there is no cache activity (cache_read + fresh == 0). Mirrors the
// ExecutionContextSidebar cumulative-hit formula so both stay identical.
function cacheHitRatioPct(cacheRead: number, fresh: number): number {
  if (cacheRead + fresh === 0) return 0;
  return Math.round((cacheRead / (cacheRead + fresh)) * 100);
}

// fmtTokenSplit renders the fresh-input vs cache token split for a row:
// "N fresh · N cache-read · N cache-write".
function fmtTokenSplit(cacheRead: number, cacheWrite: number, fresh: number): string {
  return [
    `${fmtInt(fresh)} fresh`,
    `${fmtInt(cacheRead)} cache-read`,
    `${fmtInt(cacheWrite)} cache-write`,
  ].join(" · ");
}

// fmtSplitHit renders the token split plus the cache hit ratio percentage,
// e.g. "1,204 fresh · 32,000 cache-read · 41 cache-write · 96% hit".
function fmtSplitHit(cacheRead: number, cacheWrite: number, fresh: number): string {
  return `${fmtTokenSplit(cacheRead, cacheWrite, fresh)} · ${cacheHitRatioPct(cacheRead, fresh)}% hit`;
}
