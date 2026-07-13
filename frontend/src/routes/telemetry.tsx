import { createRoute } from "@tanstack/react-router";
import { useState } from "react";

import { useGetCost, useGetUsage, useListProviders } from "@/api/aigateway";
import { useGetDashboard, useQueryLogs, useQueryTraces } from "@/api/telemetry";
import { SIGNOZ_UI_URL } from "@/api/clients";
import type { UsageRollup } from "@/api/gen/orchicon/api/v1/ai_gateway_pb";
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

type Tab = "overview" | "cost" | "traces" | "metrics" | "logs";

const TABS: { id: Tab; label: string }[] = [
  { id: "overview", label: "Overview" },
  { id: "cost", label: "Cost Explorer" },
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
      {tab === "traces" && <TracesPanel />}
      {tab === "metrics" && <SigNozEmbed title="Metrics" />}
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
// Tenant → Project → Task → Execution (docs/10 §11). The drill-down is
// server-validated — the UI reflects state, it does not make policy
// (AGENTS.md invariant #1).
function CostExplorer() {
  const [rollup, setRollup] = useState<UsageRollup>(1); // PROJECT
  const [projectId, setProjectId] = useState<string>("");
  const [taskId, setTaskId] = useState<string>("");
  const { data, isLoading, error } = useGetCost({
    rollup,
    projectId: projectId || undefined,
    taskId: taskId || undefined,
  });
  return (
    <div className="space-y-6">
      <Card>
        <CardHeader>
          <CardTitle>Cost drill-down</CardTitle>
          <CardDescription>
            Roll up by a dimension, then drill into a row to narrow scope.
            Tenant → Project → Task → Execution (docs/10 §11).
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="flex flex-wrap gap-2">
            {([
              [1, "Project"],
              [2, "Task"],
              [3, "Execution"],
              [4, "Model"],
            ] as [UsageRollup, string][]).map(([r, label]) => (
              <button
                key={r}
                onClick={() => setRollup(r)}
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
            {(projectId || taskId) && (
              <button
                onClick={() => {
                  setProjectId("");
                  setTaskId("");
                }}
                className="rounded-md border px-3 py-1.5 text-sm text-muted-foreground hover:bg-accent"
              >
                Clear scope
              </button>
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
                    if (rollup === 1) setProjectId(s.groupKey);
                    else if (rollup === 2) setTaskId(s.groupKey);
                  }}
                  className="flex w-full items-center justify-between px-3 py-2 text-left hover:bg-accent"
                >
                  <span className="font-mono text-sm">
                    {s.groupKey || "—"}
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
      <UsageRecordsTable projectId={projectId} taskId={taskId} />
    </div>
  );
}

function UsageRecordsTable({
  projectId,
  taskId,
}: {
  projectId?: string;
  taskId?: string;
}) {
  const { data, isLoading } = useGetUsage({ projectId, taskId });
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
          {data?.degraded && (
            <p className="text-sm text-yellow-700">
              SigNoz backend unavailable — showing degraded (empty) results.
              Start the dev stack (`make up`) to explore traces.
            </p>
          )}
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
      <SigNozEmbed title="Trace Explorer" path="/trace" />
    </div>
  );
}

function LogsPanel() {
  const { data, isLoading } = useQueryLogs();
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
          {data?.degraded && (
            <p className="text-sm text-yellow-700">
              SigNoz backend unavailable — degraded results.
            </p>
          )}
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
      <SigNozEmbed title="Log Explorer" path="/logs" />
    </div>
  );
}

// SigNozEmbed renders the SigNoz UI inside the Orchicon shell via a
// same-origin iframe (proxied under /signoz in dev — docs/10 §11:
// seamless embedding, not a separate tool launch). The iframe shares
// the Orchicon shell's chrome so it feels like one platform.
function SigNozEmbed({ title, path = "" }: { title: string; path?: string }) {
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
        <iframe
          src={src}
          title={title}
          className="h-[640px] w-full rounded-md border"
          sandbox="allow-same-origin allow-scripts allow-forms allow-popups"
        />
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
