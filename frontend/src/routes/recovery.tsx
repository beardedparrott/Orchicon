import { Link, createRoute } from "@tanstack/react-router";

import { useListRecoveries } from "@/api/recovery";
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
  path: "/recovery",
  component: RecoveryPage,
});

function RecoveryPage() {
  const { data: recoveries, isLoading, error } = useListRecoveries();

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">Recovery</h1>
        <p className="text-sm text-muted-foreground">
          Recovery workflow executions. When a task fails, recovery runs
          automatically (opt-out, not opt-in — docs/06 §1). Open one to see
          the full narrative.
        </p>
      </div>

      {isLoading && <p className="text-sm text-muted-foreground">Loading…</p>}
      {error && (
        <p className="text-sm text-destructive">
          Failed to load recoveries: {String(error)}
        </p>
      )}

      {recoveries && recoveries.length === 0 && (
        <Card>
          <CardHeader>
            <CardTitle>No recoveries yet</CardTitle>
            <CardDescription>
              When a WorkerExecution fails, the engine creates a
              RecoveryExecution and progresses it through the default
              6-step workflow (capture → summarize → preserve → review →
              plan → resume).
            </CardDescription>
          </CardHeader>
        </Card>
      )}

      {recoveries && recoveries.length > 0 && (
        <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-3">
          {recoveries.map((r) => (
            <Link key={r.id} to="/recovery/$id" params={{ id: r.id }}>
              <Card className="transition-colors hover:bg-accent">
                <CardHeader>
                  <CardTitle className="flex items-center justify-between">
                    <span className="font-mono text-sm">
                      {r.id.slice(0, 12)}…
                    </span>
                    <StatusBadge status={r.status} />
                  </CardTitle>
                  <CardDescription>
                    <span className="font-mono text-xs">
                      task: {r.taskId.slice(0, 12)}… · L{r.level}
                    </span>
                  </CardDescription>
                </CardHeader>
                <CardContent>
                  <p className="text-xs text-muted-foreground">
                    {r.triggerReason} · {r.resumptionPath}
                  </p>
                </CardContent>
              </Card>
            </Link>
          ))}
        </div>
      )}
    </div>
  );
}

function StatusBadge({ status }: { status: number }) {
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
  1: "pending",
  2: "running",
  3: "resumed",
  4: "escalated",
  5: "failed",
  6: "cancelled",
  7: "blocked",
};
const STATUS_STYLES: Record<number, string> = {
  1: "bg-blue-100 text-blue-800",
  2: "bg-indigo-100 text-indigo-800",
  3: "bg-green-100 text-green-800",
  4: "bg-yellow-100 text-yellow-800",
  5: "bg-red-100 text-red-800",
  6: "bg-muted text-muted-foreground",
  7: "bg-orange-100 text-orange-800",
};
