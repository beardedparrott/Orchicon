import { useState } from "react";
import { Link, createRoute } from "@tanstack/react-router";
import { z } from "zod";
import { Trash2, SearchX } from "lucide-react";

import { useBatchDeleteExecutions, useListExecutions } from "@/api/executions";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
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

const EXEC_STATUS_OPTIONS = [
  { value: "", label: "Running (default)" },
  { value: "1", label: "Dispatching" },
  { value: "2", label: "Running" },
  { value: "3", label: "Healthy" },
  { value: "4", label: "Stalled" },
  { value: "5", label: "Unhealthy" },
  { value: "6", label: "Terminating" },
  { value: "7", label: "Terminated" },
  { value: "8", label: "Failed to start" },
  { value: "9", label: "Succeeded" },
  { value: "10", label: "Failed" },
];

function ExecutionsPage() {
  const { workflowRunId } = Route.useSearch();
  const [search, setSearch] = useState("");
  const [status, setStatus] = useState(""); // "" defaults to running
  const [sortBy, setSortBy] = useState("created_at");
  const [sortOrder, setSortOrder] = useState("desc");
  const [selected, setSelected] = useState<Set<string>>(new Set());

  const statusFilter = status ? Number(status) as number : undefined;

  const { data: executions, isLoading, error } = useListExecutions({
    workflowRunId,
    search,
    status: statusFilter,
    sortBy,
    sortOrder,
  });
  const batchDelete = useBatchDeleteExecutions();

  const toggleSelect = (id: string) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

  const toggleSelectAll = () => {
    if (!executions) return;
    if (selected.size === executions.length) {
      setSelected(new Set());
    } else {
      setSelected(new Set(executions.map((e) => e.id)));
    }
  };

  const handleBatchDelete = () => {
    if (selected.size === 0) return;
    const count = selected.size;
    if (!window.confirm(`Delete ${count} execution${count === 1 ? "" : "s"}? This will force-stop any that are running.`)) return;
    batchDelete.mutate(Array.from(selected), {
      onSuccess: () => setSelected(new Set()),
    });
  };

  // Default filter: show all running (dispatching, running, healthy, stalled, unhealthy, terminating)
  const resolvedStatus = statusFilter ?? undefined;

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">Executions</h1>
          <p className="text-sm text-muted-foreground">
            Worker executions — concrete invocations of a Worker against a
            Task on a runtime adapter.
          </p>
          {workflowRunId && (
            <p className="mt-1 break-all font-mono text-xs text-muted-foreground/70">
              Filtered by workflow run: {workflowRunId}
            </p>
          )}
        </div>
      </div>

      {/* Filter bar */}
      <div className="flex flex-wrap items-center gap-3">
        <Input
          placeholder="Search worker, task, workflow..."
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          className="max-w-xs"
        />
        <select
          value={status}
          onChange={(e) => setStatus(e.target.value)}
          className="h-9 rounded-md border border-input bg-transparent px-3 text-sm shadow-sm"
        >
          {EXEC_STATUS_OPTIONS.map((opt) => (
            <option key={opt.value} value={opt.value}>{opt.label}</option>
          ))}
        </select>
        <select
          value={sortBy}
          onChange={(e) => setSortBy(e.target.value)}
          className="h-9 rounded-md border border-input bg-transparent px-3 text-sm shadow-sm"
        >
          <option value="created_at">Created</option>
          <option value="status">Status</option>
          <option value="worker_id">Worker</option>
        </select>
        <select
          value={sortOrder}
          onChange={(e) => setSortOrder(e.target.value)}
          className="h-9 rounded-md border border-input bg-transparent px-3 text-sm shadow-sm"
        >
          <option value="desc">Desc</option>
          <option value="asc">Asc</option>
        </select>
        {selected.size > 0 && (
          <Button
            variant="destructive"
            size="sm"
            onClick={handleBatchDelete}
            disabled={batchDelete.isPending}
          >
            <Trash2 className="mr-1 h-3.5 w-3.5" />
            Delete {selected.size} selected
          </Button>
        )}
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
            <CardTitle className="flex items-center gap-2">
              <SearchX className="h-5 w-5 text-muted-foreground" />
              No executions found
            </CardTitle>
          </CardHeader>
        </Card>
      )}

      {executions && executions.length > 0 && (
        <div className="space-y-1">
          {/* Select-all header */}
          <div className="flex items-center gap-2 px-2 py-1">
            <input
              type="checkbox"
              checked={executions.length > 0 && selected.size === executions.length}
              onChange={toggleSelectAll}
              className="h-4 w-4 rounded border-input"
            />
            <span className="text-xs text-muted-foreground">
              {selected.size > 0
                ? `${selected.size} of ${executions.length} selected`
                : `${executions.length} execution${executions.length === 1 ? "" : "s"}`}
            </span>
          </div>

          {executions.map((e) => (
            <div key={e.id} className="group flex items-center gap-2">
              <input
                type="checkbox"
                checked={selected.has(e.id)}
                onChange={() => toggleSelect(e.id)}
                className="ml-2 h-4 w-4 shrink-0 rounded border-input"
              />
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
                      <HealthBadge health={e.healthState} status={e.status} />
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
                  if (window.confirm("Delete this execution?")) {
                    batchDelete.mutate([e.id]);
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

function HealthBadge({ health, status }: { health: number; status: number }) {
  const terminalStatuses = new Set([7, 8, 9, 10]);
  if (terminalStatuses.has(status)) return null;
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
