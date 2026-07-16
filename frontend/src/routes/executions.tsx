import { Link, createRoute } from "@tanstack/react-router";
import { z } from "zod";

import { useDeleteExecution, useListExecutions } from "@/api/executions";
import {
  Card,
  CardContent,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { cn } from "@/lib/utils";
import { Route as rootRoute } from "@/routes/__root";

const executionsSearchSchema = z.object({
  workflowRunId: z.string().optional(),
});

export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/executions",
  validateSearch: executionsSearchSchema,
  component: ExecutionsPage,
});

function ExecutionsPage() {
  const { workflowRunId } = Route.useSearch();
  const { data: executions, isLoading, error } = useListExecutions({ workflowRunId });
  const deleteExec = useDeleteExecution();

  return (
    <div className="mx-auto max-w-6xl space-y-6">
      <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
        <div className="min-w-0">
          <h1 className="text-2xl font-semibold tracking-tight">Executions</h1>
          <p className="text-sm text-muted-foreground">
            Worker executions — concrete invocations of a Worker against a
            Task on a runtime adapter. Click through to view live telemetry.
          </p>
          {workflowRunId && (
            <p className="mt-1 break-all font-mono text-xs text-muted-foreground/70">
              Filtered by workflow run: {workflowRunId}
            </p>
          )}
        </div>
      </div>

      {isLoading && <p className="text-sm text-muted-foreground">Loading…</p>}
      {error && (
        <p className="text-sm text-destructive">
          Failed to load executions: {String(error)}
        </p>
      )}

      {executions && executions.length === 0 && (
        <Card>
          <CardHeader>
            <CardTitle>No executions yet</CardTitle>
          </CardHeader>
        </Card>
      )}

      {executions && executions.length > 0 && (
        <div className="space-y-2">
          {executions.map((e) => (
            <div key={e.id} className="group flex items-center gap-2">
              <Link
                to="/executions/$id"
                params={{ id: e.id }}
                className="min-w-0 flex-1"
              >
                <Card className="transition-colors hover:bg-accent">
                  <CardContent className="flex flex-col gap-2 p-4 sm:flex-row sm:items-center sm:justify-between">
                    <div className="flex min-w-0 items-center gap-3">
                      <ExecStatusBadge status={e.status} />
                      <div className="min-w-0 flex-1 overflow-hidden">
                        <p className="truncate text-sm font-medium">
                          {e.workflowName || e.workerId} v{e.workerVersion}
                        </p>
                        <p className="break-all font-mono text-xs text-muted-foreground">
                          {e.id}
                        </p>
                      </div>
                    </div>
                    <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-muted-foreground sm:shrink-0">
                      <HealthBadge health={e.healthState} />
                      {Number(e.tokenUsage) > 0 && (
                        <span className="font-mono tabular-nums">
                          {Number(e.tokenUsage).toLocaleString()} tokens
                        </span>
                      )}
                      {Number(e.costUsd) > 0 && (
                        <span className="font-mono tabular-nums">
                          ${Number(e.costUsd).toFixed(4)}
                        </span>
                      )}
                      {e.startedAt && (
                        <span>
                          {new Date(
                            Number(e.startedAt.seconds) * 1000,
                          ).toLocaleTimeString()}
                        </span>
                      )}
                    </div>
                  </CardContent>
                </Card>
              </Link>
              <button
                onClick={() => {
                  if (window.confirm("Delete this execution? This will force-stop it if running.")) {
                    deleteExec.mutate(e.id);
                  }
                }}
                className="opacity-0 group-hover:opacity-100 rounded px-1.5 py-0.5 text-xs font-medium text-muted-foreground hover:text-destructive hover:bg-accent transition-all shrink-0"
                title="Delete execution"
              >
                ✕
              </button>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

function ExecStatusBadge({ status }: { status: number }) {
  const labels: Record<number, string> = {
    1: "dispatching",
    2: "running",
    3: "healthy",
    4: "stalled",
    5: "unhealthy",
    6: "terminating",
    7: "terminated",
    8: "failed_to_start",
    9: "succeeded",
    10: "failed",
  };
  const styles: Record<number, string> = {
    1: "bg-blue-100 text-blue-800",
    2: "bg-green-100 text-green-800",
    3: "bg-green-600 text-white",
    4: "bg-yellow-100 text-yellow-800",
    5: "bg-red-100 text-red-800",
    6: "bg-orange-100 text-orange-800",
    7: "bg-gray-200 text-gray-700",
    8: "bg-red-600 text-white",
    9: "bg-emerald-100 text-emerald-800",
    10: "bg-red-700 text-white",
  };
  return (
    <span
      className={cn(
        "rounded-full px-2 py-0.5 text-xs font-medium",
        styles[status] ?? "bg-muted text-muted-foreground",
      )}
    >
      {labels[status] ?? "unknown"}
    </span>
  );
}

function HealthBadge({ health }: { health: number }) {
  const labels: Record<number, string> = {
    1: "healthy",
    2: "stalled",
    3: "unhealthy",
    4: "terminating",
  };
  const styles: Record<number, string> = {
    1: "text-green-600",
    2: "text-yellow-600",
    3: "text-red-600",
    4: "text-orange-600",
  };
  return <span className={cn("text-xs font-medium", styles[health] ?? "")}>● {labels[health] ?? "unknown"}</span>;
}
