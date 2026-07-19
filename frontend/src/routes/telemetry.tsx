import { createRoute } from "@tanstack/react-router";
import { useMemo, useState } from "react";

import { useQueries } from "@tanstack/react-query";

import { useGetCost, useGetUsage, useListProviders } from "@/api/aigateway";
import { useGetDashboard, useQueryLogs, useQueryMetrics, useQueryTraces } from "@/api/telemetry";
import { useListProjects } from "@/api/projects";
import { workItemClient } from "@/api/clients";
import { workItemKeys } from "@/api/workItems";
import { SIGNOZ_UI_URL } from "@/api/clients";
import type { UsageRollup } from "@/api/gen/orchicon/api/v1/ai_gateway_pb";
import { UsageRollup as UsageRollupEnum } from "@/api/gen/orchicon/api/v1/ai_gateway_pb";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { cn } from "@/lib/utils";
import { Route as rootRoute } from "@/routes/__root";

// Telemetry hub (docs/10 §11): seamlessly embedded SigNoz for raw
// traces/metrics/logs exploration (same auth, same visual language,
// inside the Orchicon shell — not a separate tool) plus custom
// Orchicon-specific views: cost explorer with Tenant→Project→Task→
// Execution drill-down, and the Orchicon dashboard.
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
  { id: "traces", label: "Traces (SigNoz)" },
  { id: "metrics", label: "Metrics (SigNoz)" },
  { id: "logs", label: "Logs (SigNoz)" },
];

function TelemetryPage() {
  const [tab, setTab] = useState<Tab>("overview");
  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">Telemetry</h1>
        <p className="text-sm text-muted-foreground">
          Traces, metrics, logs, and cost — explore without leaving the
          Orchicon shell. Raw exploration uses embedded SigNoz; cost + the
          Orchicon dashboard are custom views (docs/10 §11).
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
// SigNoz UI.
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
            Per-model USD spend in the dashboard window (custom Orchicon
            roll-up from Postgres usage_records — source of truth).
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

// CostExplorer is the custom cost explorer with drill-down
// Project → Task → Execution → Model (docs/10 §11). The drill-down is
// server-validated — the UI reflects state, it does not make policy
// (AGENTS.md invariant #1).
function CostExplorer() {
  const [rollup, setRollup] = useState<UsageRollup>(UsageRollupEnum.PROJECT);
  const [projectId, setProjectId] = useState("");
  const [taskId, setTaskId] = useState("");
  const [executionId, setExecutionId] = useState("");
  const { data, isLoading, error } = useGetCost({
    rollup,
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

  function displayName(s: { groupBy: string; groupKey: string }): string {
    if (s.groupBy === "project") return projectNameMap.get(s.groupKey) || s.groupKey.slice(0, 12);
    if (s.groupBy === "task") return taskNameMap.get(s.groupKey) || s.groupKey.slice(0, 12);
    if (s.groupBy === "execution") return s.groupKey.slice(0, 12);
    return s.groupKey;
  }

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
            ] as [UsageRollup, string][]).map(([r, label]) => (
              <button
                key={r}
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
            {scopeLabel() && (
              <span className="ml-2 text-xs text-muted-foreground">
                Scoped to {scopeLabel()}
              </span>
            )}
            {(projectId || taskId) && (
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
                  {fmtInt(data.total.totalTokens ?? 0)} tokens ·{" "}
                  {data.total.executionCount ?? 0} executions
                </span>
              </div>
            </div>
          )}
          {data?.summaries && data.summaries.length > 0 && (
            <div className="divide-y rounded-md border">
              {data.summaries.map((s) => (
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
                    {fmtInt(s.totalTokens ?? 0)} tok ·{" "}
                    {s.executionCount ?? 0} execs
                  </span>
                </button>
              ))}
            </div>
          )}
          {data?.summaries && data.summaries.length === 0 && (
            <p className="text-sm text-muted-foreground">
              No usage records in scope. Run an execution to populate cost.
            </p>
          )}
        </CardContent>
      </Card>
      <UsageRecordsTable
        projectId={projectId}
        taskId={taskId}
        executionId={executionId}
      />
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
  return (
    <Card>
      <CardHeader>
        <CardTitle>Recent usage records</CardTitle>
        <CardDescription>
          Postgres source-of-truth rows (docs/08 §5.2). Mirrored to
          ClickHouse as OTel metrics for the embedded SigNoz views.
        </CardDescription>
      </CardHeader>
      <CardContent>
        {isLoading && <p className="text-sm text-muted-foreground">Loading…</p>}
        {data && data.length === 0 && (
          <p className="text-sm text-muted-foreground">No usage records yet.</p>
        )}
        {data && data.length > 0 && (
          <div className="overflow-x-auto">
            <table className="w-full text-sm">
              <thead className="text-left text-muted-foreground">
                <tr>
                  <th className="py-1 pr-3">Execution</th>
                  <th className="py-1 pr-3">Provider</th>
                  <th className="py-1 pr-3">Model</th>
                  <th className="py-1 pr-3 text-right">Tokens</th>
                  <th className="py-1 pr-3 text-right">Cost (USD)</th>
                  <th className="py-1">When</th>
                </tr>
              </thead>
              <tbody className="divide-y">
                {data.map((r) => (
                  <tr key={r.id}>
                    <td className="py-1 pr-3 font-mono text-xs">
                      {(r.executionId || "—").slice(0, 12)}
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
      </CardContent>
    </Card>
  );
}

// TracesPanel shows recent traces (projected from SigNoz) + the embedded
// SigNoz trace explorer for raw drill-down (docs/10 §11).
function TracesPanel() {
  const { data, isLoading } = useQueryTraces();
  const degraded = data?.degraded ?? false;
  return (
    <div className="space-y-6">
      <Card>
        <CardHeader>
          <CardTitle>Recent traces</CardTitle>
          <CardDescription>
            Projected from SigNoz/ClickHouse, scoped to the current tenant
            (docs/08 §5.1). Open the embedded SigNoz UI for full drill-down.
          </CardDescription>
        </CardHeader>
        <CardContent>
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
      <SigNozEmbed title="Trace Explorer" path="/trace" degraded={degraded} />
    </div>
  );
}

function LogsPanel() {
  const { data, isLoading } = useQueryLogs();
  const degraded = data?.degraded ?? false;
  return (
    <div className="space-y-6">
      <Card>
        <CardHeader>
          <CardTitle>Recent logs</CardTitle>
          <CardDescription>
            Structured OTel log records carrying trace_id + correlation_id
            (docs/08 §5.3).
          </CardDescription>
        </CardHeader>
        <CardContent>
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
      <SigNozEmbed title="Log Explorer" path="/logs" degraded={degraded} />
    </div>
  );
}

// MetricsPanel shows live metric values from ClickHouse plus the
// embedded SigNoz metrics explorer for raw drill-down.
function MetricsPanel() {
  const { data, isLoading } = useQueryMetrics({
    metricNames: ["orchicon_tokens_consumed", "orchicon_cost_usd", "orchicon_outbox_lag"],
  });
  const degraded = data?.degraded ?? false;
  const series = data?.series ?? [];

  return (
    <div className="space-y-6">
      <Card>
        <CardHeader>
          <CardTitle>Metric values</CardTitle>
          <CardDescription>
            Latest samples from ClickHouse (signoz_metrics.samples_v4).
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
            <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
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
      <SigNozEmbed title="Metrics Explorer" degraded={degraded} />
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

// SigNozEmbed renders the SigNoz UI inside the Orchicon shell via a
// same-origin iframe (proxied under /signoz in dev — docs/10 §11:
// seamless embedding, not a separate tool launch). The iframe shares
// the Orchicon shell's chrome so it feels like one platform.
// When degraded, a placeholder is shown instead of a broken iframe
// loading the SPA fallback (AGENTS.md verification §2).
function SigNozEmbed({
  title,
  path = "",
  degraded,
}: {
  title: string;
  path?: string;
  degraded?: boolean;
}) {
  const src = `${SIGNOZ_UI_URL}${path}`;
  return (
    <Card>
      <CardHeader>
        <CardTitle>{title} — embedded SigNoz</CardTitle>
        <CardDescription>
          Same auth, same visual language, inside the Orchicon shell
          (docs/10 §11). The SigNoz UI is proxied same-origin.
        </CardDescription>
      </CardHeader>
      <CardContent>
        {degraded ? (
          <div className="flex h-[160px] items-center justify-center rounded-md border border-dashed text-sm text-muted-foreground">
            SigNoz is starting up — check back in a moment. The dev stack
            starts automatically with `orchicon start` / `orchicon dev start`.
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
      <CardHeader className="pb-2">
        <CardDescription>{label}</CardDescription>
      </CardHeader>
      <CardContent>
        <div className="text-2xl font-semibold">
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
