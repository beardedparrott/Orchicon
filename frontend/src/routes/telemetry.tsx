import { createRoute, Link } from "@tanstack/react-router";
import { useMemo, useState } from "react";

import { useGetUsage, useListProviders } from "@/api/aigateway";
import { useGetDashboard, useQueryLogs, useQueryMetrics, useQueryTraces } from "@/api/telemetry";
import { GRAFANA_UI_URL } from "@/api/clients";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { cn } from "@/lib/utils";
import { Route as rootRoute } from "@/routes/__root";

export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/telemetry",
  component: TelemetryPage,
});

type Tab = "overview" | "traces" | "metrics" | "logs" | "credits";

const TABS: { id: Tab; label: string }[] = [
  { id: "overview", label: "Overview" },
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
          Native OTEL telemetry — traces, metrics, and logs without leaving the Orchicon shell.
          For token and cost breakdown, see{" "}
          <Link to="/cost-explorer" className="text-primary hover:underline">Cost Explorer</Link>.
        </p>
      </div>
      <div className="flex flex-wrap gap-2 border-b border-white/10 pb-px">
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
      {tab === "credits" && <CreditsPanel />}
      {tab === "traces" && <TracesPanel />}
      {tab === "metrics" && <MetricsPanel />}
      {tab === "logs" && <LogsPanel />}
    </div>
  );
}

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
      <Card className="glass-panel">
        <CardHeader>
          <CardTitle>Cost by model</CardTitle>
          <CardDescription>
            Per-model USD spend over the lifetime of the workspace (custom Orchicon roll-up from
            Postgres usage_records — source of truth).
          </CardDescription>
        </CardHeader>
        <CardContent>
          {data?.panels && data.panels.length === 0 && (
            <p className="text-sm text-muted-foreground">
              No usage recorded yet. Run an execution to populate cost telemetry.
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

function TracesPanel() {
  const { data, isLoading } = useQueryTraces();
  const degraded = data?.degraded ?? false;
  return (
    <div className="space-y-6">
      <GrafanaEmbed title="Trace Explorer" degraded={degraded} />
      <Card className="glass-panel">
        <CardHeader>
          <CardTitle>Recent traces</CardTitle>
          <CardDescription>
            Projected from Tempo, scoped to the current tenant (docs/08 §5.1). Open the embedded
            Grafana UI for full drill-down.
          </CardDescription>
        </CardHeader>
        <CardContent className="max-h-[200px] overflow-y-auto">
          {isLoading && <p className="text-sm text-muted-foreground">Loading…</p>}
          {data?.traces && data.traces.length === 0 && !data.degraded && (
            <p className="text-sm text-muted-foreground">No traces yet.</p>
          )}
          {data?.traces && data.traces.length > 0 && (
            <div className="divide-y divide-white/10 rounded-2xl glass-panel overflow-hidden">
              {data.traces.map((t) => (
                <div key={t.traceId} className="flex items-center justify-between px-3 py-2">
                  <div>
                    <div className="font-mono text-xs text-muted-foreground">
                      {t.traceId?.slice(0, 16)}…
                    </div>
                    <div className="text-sm font-medium">{t.rootSpanName || "—"}</div>
                  </div>
                  <div className="text-right text-sm">
                    <div>{fmtInt(t.durationUs ?? 0)} µs</div>
                    <div className="text-xs text-muted-foreground">{t.spanCount ?? 0} spans</div>
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
      <Card className="glass-panel">
        <CardHeader>
          <CardTitle>Recent logs</CardTitle>
          <CardDescription>
            Structured OTel log records carrying trace_id + correlation_id (docs/08 §5.3).
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

function MetricsPanel() {
  const { data, isLoading } = useQueryMetrics({
    metricNames: ["orchicon_tokens_consumed", "orchicon_cost_usd", "orchicon_outbox_lag"],
  });
  const degraded = data?.degraded ?? false;
  const series = data?.series ?? [];
  return (
    <div className="space-y-6">
      <GrafanaEmbed title="Metrics Explorer" degraded={degraded} />
      <Card className="glass-panel">
        <CardHeader>
          <CardTitle>Metric values</CardTitle>
          <CardDescription>Latest samples from VictoriaMetrics.</CardDescription>
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
                const display =
                  name === "orchicon_tokens_consumed"
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
                    <div className="mt-0.5 text-[10px] text-muted-foreground">{pts.length} samples</div>
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
        <StatCard label="Total spent (USD)" value={`$${grandTotal.cost.toFixed(4)}`} loading={isLoading} />
        <StatCard label="Total tokens" value={fmtInt(grandTotal.tokens)} loading={isLoading} />
        <StatCard label="Usage records" value={fmtInt(grandTotal.count)} loading={isLoading} />
      </div>
      {byProvider.size === 0 && !isLoading && (
        <Card className="glass-panel">
          <CardContent className="py-6">
            <p className="text-sm text-muted-foreground text-center">
              No usage records yet. Run an execution to populate credit telemetry.
            </p>
          </CardContent>
        </Card>
      )}
      {byProvider.size > 0 &&
        Array.from(byProvider.entries()).map(([providerId, p]) => {
          const displayName = providerMap.get(providerId) || providerId;
          return (
            <Card key={providerId} className="glass-panel">
              <CardHeader>
                <CardTitle>{displayName}</CardTitle>
                <CardDescription>
                  ${p.totalCost.toFixed(4)} · {fmtInt(p.totalTokens)} tokens · {p.count} records
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
                        ${m.cost.toFixed(4)} · {fmtInt(m.tokens)} tok · {m.count} calls
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

function GrafanaEmbed({ title, degraded }: { title: string; degraded?: boolean }) {
  const src = `${GRAFANA_UI_URL}${grafanaDashboardPath()}`;
  return (
    <Card className="glass-panel">
      <CardHeader>
        <CardTitle>{title} — embedded Grafana</CardTitle>
        <CardDescription>
          Same visual language, inside the Orchicon shell (docs/10 §11). The Grafana UI is proxied
          same-origin.
        </CardDescription>
      </CardHeader>
      <CardContent>
        {degraded ? (
          <div className="flex h-[160px] items-center justify-center rounded-md border border-dashed text-sm text-muted-foreground">
            Telemetry backend is starting up — check back in a moment. The dev stack starts
            automatically with `orchicon start` / `orchicon dev start`.
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

function grafanaDashboardPath(): string {
  return `/d/orchicon-dashboard/orchicon?kiosk&from=now-1h&to=now`;
}

function StatCard({ label, value, loading }: { label: string; value: string; loading?: boolean }) {
  return (
    <Card className="glass-panel">
      <CardHeader className="pb-0">
        <CardDescription className="text-xs">{label}</CardDescription>
      </CardHeader>
      <CardContent className="pb-3 pt-1">
        <div className="text-xl font-semibold">{loading ? "…" : value}</div>
      </CardContent>
    </Card>
  );
}

function fmtInt(n: number | bigint | undefined): string {
  if (n === undefined || n === null) return "0";
  const v = typeof n === "bigint" ? Number(n) : n;
  return v.toLocaleString();
}
