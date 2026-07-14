import { Link, createRoute } from "@tanstack/react-router";

import { useListWorkers } from "@/api/workers";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { cn } from "@/lib/utils";
import { Route as rootRoute } from "@/routes/__root";

// Worker catalog (docs/10 §5, docs/05 §3). Fetches via Connect-ES +
// TanStack Query; the UI reflects server state only (AGENTS.md
// invariant #1). Workers are tenant-owned, reusable, versioned
// execution profiles (docs/05 §1).
export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/workers",
  component: WorkersPage,
});

function WorkersPage() {
  const { data: workers, isLoading, error } = useListWorkers();

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">Workers</h1>
          <p className="text-sm text-muted-foreground">
            Reusable, versioned execution profiles. A Worker declares what is
            permitted; the adapter advertises what is possible.
          </p>
        </div>
        <Button asChild>
          <Link to="/workers/new">New Worker</Link>
        </Button>
      </div>

      {isLoading && <p className="text-sm text-muted-foreground">Loading…</p>}
      {error && (
        <p className="text-sm text-destructive">
          Failed to load workers: {String(error)}
        </p>
      )}

      {workers && workers.length === 0 && (
        <Card>
          <CardHeader>
            <CardTitle>No workers yet</CardTitle>
            <CardDescription>
              Create a worker to define a reusable execution profile with
              permissions, budgets, and a system prompt.
            </CardDescription>
          </CardHeader>
        </Card>
      )}

      {workers && workers.length > 0 && (
        <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-3">
          {workers.map((w) => (
            <Link key={w.id} to="/workers_/$id" params={{ id: w.id }}>
              <Card className="transition-colors hover:bg-accent">
                <CardHeader>
                  <CardTitle className="flex items-center justify-between">
                    <span className="truncate">{w.name}</span>
                    <WorkerStatusBadge status={w.status} />
                  </CardTitle>
                  <CardDescription className="font-mono text-xs">
                    {w.slug}
                  </CardDescription>
                </CardHeader>
                <CardContent>
                  {w.purpose && (
                    <p className="text-xs text-muted-foreground line-clamp-2">
                      {w.purpose}
                    </p>
                  )}
                  <div className="mt-2 flex items-center gap-3 text-xs text-muted-foreground">
                    <span>v{w.currentVersion}</span>
                  </div>
                </CardContent>
              </Card>
            </Link>
          ))}
        </div>
      )}
    </div>
  );
}

// WorkerStatusBadge renders a colored pill for the worker lifecycle
// state (docs/05 §4).
function WorkerStatusBadge({ status }: { status: number }) {
  const label = STATUS_LABELS[status] ?? "unknown";
  return (
    <span
      className={cn(
        "rounded-full px-2 py-0.5 text-xs font-medium",
        STATUS_STYLES[status] ?? "bg-muted text-muted-foreground",
      )}
    >
      {label}
    </span>
  );
}

const STATUS_LABELS: Record<number, string> = {
  1: "draft",
  2: "published",
  3: "deprecated",
  4: "retired",
};

const STATUS_STYLES: Record<number, string> = {
  1: "bg-blue-100 text-blue-800",
  2: "bg-green-100 text-green-800",
  3: "bg-yellow-100 text-yellow-800",
  4: "bg-gray-200 text-gray-700",
};
