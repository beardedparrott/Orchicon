import { createRoute } from "@tanstack/react-router";

import { Route as rootRoute } from "@/routes/__root";

// Dashboard / landing route (docs/10 §5).
export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/",
  component: DashboardPage,
});

function DashboardPage() {
  return (
    <div className="space-y-4">
      <h1 className="text-2xl font-semibold tracking-tight">Dashboard</h1>
      <p className="text-sm text-muted-foreground">
        Orchicon orchestrates autonomous AI work as reliable, observable,
        recoverable, and manageable systems. Orchicon orchestrates.
        Runtimes execute.
      </p>
      <div className="grid gap-4 md:grid-cols-3">
        <div className="rounded-lg border bg-card p-4">
          <div className="text-xs uppercase text-muted-foreground">
            Active Projects
          </div>
          <div className="mt-2 text-3xl font-semibold">—</div>
        </div>
        <div className="rounded-lg border bg-card p-4">
          <div className="text-xs uppercase text-muted-foreground">
            Running Executions
          </div>
          <div className="mt-2 text-3xl font-semibold">—</div>
        </div>
        <div className="rounded-lg border bg-card p-4">
          <div className="text-xs uppercase text-muted-foreground">
            Recoveries (24h)
          </div>
          <div className="mt-2 text-3xl font-semibold">—</div>
        </div>
      </div>
    </div>
  );
}
