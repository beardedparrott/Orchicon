import { createRoute, Link } from "@tanstack/react-router";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import { useGetUsage, useListProviders } from "@/api/aigateway";
import { setTelemetryAttribution } from "@/api/clients";
import { useListProjects } from "@/api/projects";
import {
  useGetDashboard,
  useGetTelemetryReport,
  type TelemetryFilters as TelemetryFilterSet,
} from "@/api/telemetry";
import { TelemetryReportGroupBy } from "@/api/gen/orchicon/api/v1/telemetry_service_pb";
import {
  LogsPanel,
  MetricsGraphPanel,
  TracesPanel,
} from "@/components/telemetry/graphs";
import { ReportsPanel } from "@/components/telemetry/reports-panel";
import { StatCard, fmtInt } from "@/components/telemetry/states";
import {
  DEFAULT_TELEMETRY_FILTERS,
  TelemetryFilters,
  windowFor,
  type TelemetryFilterValue,
} from "@/components/telemetry/telemetry-filters";
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

type Tab = "overview" | "reports" | "traces" | "metrics" | "logs" | "credits";

const TABS: { id: Tab; label: string }[] = [
  { id: "overview", label: "Overview" },
  { id: "reports", label: "Reports" },
  { id: "traces", label: "Traces" },
  { id: "metrics", label: "Metrics" },
  { id: "logs", label: "Logs" },
  { id: "credits", label: "Credits" },
];

function TelemetryPage() {
  const [tab, setTab] = useState<Tab>("overview");
  const [filters, setFilters] = useState<TelemetryFilterValue>(
    DEFAULT_TELEMETRY_FILTERS,
  );
  const { data: projects } = useListProjects();
  const tabRefs = useRef<Record<string, HTMLButtonElement | null>>({});

  // The resolved window + identity scope every panel queries with.
  const window = useMemo(
    () => windowFor(filters),
    // Recomputed only when the filter inputs change, not on every render.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [filters.range, filters.customStart, filters.customEnd],
  );

  const query: TelemetryFilterSet = useMemo(
    () => ({
      projectId: filters.projectId || undefined,
      executionId: filters.executionId || undefined,
      workItemId: filters.workItemId || undefined,
      search: filters.search || undefined,
      severity: filters.severity || undefined,
      start: window.start,
      end: window.end,
    }),
    [filters, window],
  );

  // Attribution hints for the trace identity filters (see clients.ts).
  useEffect(() => {
    setTelemetryAttribution({
      projectId: query.projectId,
      workItemId: query.workItemId,
      executionId: query.executionId,
    });
  }, [query.projectId, query.workItemId, query.executionId]);

  // Roving-tabindex keyboard model for the tab strip (AC 7): Left/Right move
  // between tabs, Home/End jump to the ends, focus follows selection.
  const onTabKeyDown = useCallback(
    (e: React.KeyboardEvent<HTMLDivElement>) => {
      const idx = TABS.findIndex((t) => t.id === tab);
      let next = idx;
      switch (e.key) {
        case "ArrowRight":
          next = (idx + 1) % TABS.length;
          break;
        case "ArrowLeft":
          next = (idx - 1 + TABS.length) % TABS.length;
          break;
        case "Home":
          next = 0;
          break;
        case "End":
          next = TABS.length - 1;
          break;
        default:
          return;
      }
      e.preventDefault();
      const target = TABS[next];
      setTab(target.id);
      tabRefs.current[target.id]?.focus();
    },
    [tab],
  );

  return (
    <div className="space-y-4">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">Telemetry</h1>
        <p className="text-sm text-muted-foreground">
          Native OTEL telemetry — graphs, reports and search over traces,
          metrics and logs, served by Orchicon itself. For token and cost
          breakdown, see{" "}
          <Link to="/cost-explorer" className="text-primary hover:underline">
            Cost Explorer
          </Link>
          .
        </p>
      </div>

      <TelemetryFilters
        value={filters}
        onChange={setFilters}
        projects={projects}
        disableIdentity={tab === "metrics"}
      />

      <div
        role="tablist"
        aria-label="Telemetry sections"
        onKeyDown={onTabKeyDown}
        className="flex flex-wrap gap-2 border-b border-white/10 pb-px"
      >
        {TABS.map((t) => (
          <button
            key={t.id}
            ref={(el) => {
              tabRefs.current[t.id] = el;
            }}
            role="tab"
            type="button"
            id={`telemetry-tab-${t.id}`}
            aria-selected={tab === t.id}
            aria-controls={`telemetry-panel-${t.id}`}
            tabIndex={tab === t.id ? 0 : -1}
            onClick={() => setTab(t.id)}
            className={cn(
              "rounded-t-md px-3 py-2 text-sm font-medium transition-colors focus-visible:ring-2 focus-visible:ring-ring",
              tab === t.id
                ? "border-b-2 border-primary text-foreground"
                : "text-muted-foreground hover:text-foreground",
            )}
          >
            {t.label}
          </button>
        ))}
      </div>

      <div
        role="tabpanel"
        id={`telemetry-panel-${tab}`}
        aria-labelledby={`telemetry-tab-${tab}`}
        tabIndex={0}
        className="focus-visible:outline-none"
      >
        {tab === "overview" && <OverviewPanel query={query} />}
        {tab === "reports" && <ReportsPanel filters={query} />}
        {tab === "traces" && <TracesPanel filters={query} />}
        {tab === "metrics" && <MetricsGraphPanel filters={query} />}
        {tab === "logs" && <LogsPanel filters={query} />}
        {tab === "credits" && <CreditsPanel />}
      </div>
    </div>
  );
}

function OverviewPanel({ query }: { query: TelemetryFilterSet }) {
  const { data, isLoading } = useGetDashboard({ projectId: query.projectId });
  const { data: providers } = useListProviders();

  // THE DASHBOARD ANSWERS "DID THE WORK LAND?" TOO, NOT ONLY "WHAT DID IT COST?".
  //
  // GetDashboard is a cost/usage roll-up over usage_records: it can say what the
  // workspace spent and how many executions ran, but not whether any work
  // FINISHED or how it ended — so the landing tab had nothing to say about
  // outcomes at all. The counters come from the same Postgres-only report the
  // Reports tab reads, which also means they keep working while a telemetry
  // store is down (the whole point of that split).
  //
  // Same filters, same window as Reports: two surfaces answering "how much
  // failed" with different windows would be worse than one answering neither.
  const { data: report } = useGetTelemetryReport({
    groupBy: TelemetryReportGroupBy.PROJECT,
    filters: query,
  });
  const outcomes = report?.outcomes;
  const windowLabel = useMemo(() => {
    const start = report?.windowStart;
    const end = report?.windowEnd;
    if (!start || !end) return "in this window";
    const hours = Math.max(
      1,
      Math.round(
        (Number(end.seconds) * 1000 - Number(start.seconds) * 1000) / 3_600_000,
      ),
    );
    return hours === 1 ? "last hour" : `last ${hours}h`;
  }, [report?.windowStart, report?.windowEnd]);

  const summary = data?.summary;
  return (
    <div className="space-y-4">
      <section aria-labelledby="overview-outcomes">
        <h2 id="overview-outcomes" className="mb-2 text-sm font-medium">
          Work outcomes
        </h2>
        <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-4">
          <StatCard
            label="Work completed"
            value={fmtInt(outcomes?.workSucceeded ?? 0)}
            loading={isLoading}
            hint={windowLabel}
          />
          <StatCard
            label="Work failed"
            value={fmtInt(outcomes?.workFailed ?? 0)}
            loading={isLoading}
            hint={windowLabel}
          />
          <StatCard
            label="Work cancelled"
            value={fmtInt(outcomes?.workCancelled ?? 0)}
            loading={isLoading}
            hint={windowLabel}
          />
          <StatCard
            label="Work still open"
            value={fmtInt(outcomes?.workOpen ?? 0)}
            loading={isLoading}
            hint="in flight, not a unit"
          />
        </div>
      </section>

      <Card className="glass-panel">
        <CardHeader className="pb-2">
          <CardTitle className="text-sm">Workflow runs</CardTitle>
          <CardDescription>
            Completed is the success state; aborted is cancelled work, not a
            failure. {windowLabel}.
          </CardDescription>
        </CardHeader>
        <CardContent className="grid grid-cols-2 gap-3 sm:grid-cols-4">
          {[
            { label: "Completed", value: outcomes?.workflowsCompleted ?? 0 },
            { label: "Failed", value: outcomes?.workflowsFailed ?? 0 },
            { label: "Aborted", value: outcomes?.workflowsAborted ?? 0 },
            { label: "Running", value: outcomes?.workflowsRunning ?? 0 },
          ].map((w) => (
            <div key={w.label}>
              <div className="text-xs text-muted-foreground">{w.label}</div>
              <div className="text-lg font-semibold tabular-nums">
                {fmtInt(w.value)}
              </div>
            </div>
          ))}
        </CardContent>
      </Card>

      <section aria-labelledby="overview-cost">
        <h2 id="overview-cost" className="mb-2 text-sm font-medium">
          Cost and usage
        </h2>
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
      </section>
      <Card className="glass-panel">
        <CardHeader>
          <CardTitle>Cost by model</CardTitle>
          <CardDescription>
            Per-model USD spend over the lifetime of the workspace (custom
            Orchicon roll-up from Postgres usage_records — source of truth).
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

function CreditsPanel() {
  // fetchAll: lifetime totals must cover EVERY record. The GetUsage RPC is
  // cursor-paginated (a full page returns next_page_token), so a single
  // request summed only the first pageSize rows and credits read far
  // smaller than reality.
  const { data: usageRecords, isLoading } = useGetUsage({ fetchAll: true });
  const { data: providers } = useListProviders();
  const providerMap = useMemo(() => {
    const m = new Map<string, string>();
    if (providers) for (const p of providers) m.set(p.id, p.name);
    return m;
  }, [providers]);
  const { byProvider, grandTotal } = useMemo(() => {
    const byProvider = new Map<
      string,
      {
        totalCost: number;
        totalTokens: number;
        count: number;
        models: Map<string, { cost: number; tokens: number; count: number }>;
      }
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
    <div className="space-y-4">
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
        <Card className="glass-panel">
          <CardContent className="py-6">
            <p className="text-center text-sm text-muted-foreground">
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
            <Card key={providerId} className="glass-panel">
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
