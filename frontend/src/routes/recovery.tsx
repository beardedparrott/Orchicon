import { Link, createRoute } from "@tanstack/react-router";
import { useState } from "react";
import { Trash2, SearchX } from "lucide-react";

import { useBatchCancelRecoveries, useListRecoveries } from "@/api/recovery";
import type { RecoveryStatus } from "@/api/gen/orchicon/api/v1/recovery_pb";
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

export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/recovery",
  component: RecoveryPage,
});

function RecoveryPage() {
  const [status, setStatus] = useState("all");
  const [selected, setSelected] = useState<Set<string>>(new Set());

  const statusFilter: RecoveryStatus | undefined =
    status === "all" ? undefined : (Number(status) as RecoveryStatus);

  const { data: recoveries, isLoading, error } = useListRecoveries({
    status: statusFilter,
  });
  const batchCancel = useBatchCancelRecoveries();

  const toggleSelect = (id: string) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

  const toggleSelectAll = () => {
    if (!recoveries) return;
    if (selected.size === recoveries.length) {
      setSelected(new Set());
    } else {
      setSelected(new Set(recoveries.map((r) => r.id)));
    }
  };

  const handleBatchCancel = () => {
    if (selected.size === 0) return;
    const count = selected.size;
    if (!window.confirm(`Cancel ${count} recover${count === 1 ? "y" : "ies"}?`)) return;
    batchCancel.mutate(Array.from(selected), {
      onSuccess: () => setSelected(new Set()),
    });
  };

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">Recovery</h1>
          <p className="text-sm text-muted-foreground">
            Recovery workflow executions. When a task fails, recovery runs
            automatically (opt-out, not opt-in — docs/06 §1). Open one to see
            the full narrative.
          </p>
        </div>
      </div>

      <div className="flex flex-wrap items-center gap-3">
        <select
          value={status}
          onChange={(e) => setStatus(e.target.value)}
          className="h-9 rounded-md border border-input bg-transparent px-3 text-sm shadow-sm"
        >
          <option value="all">All statuses</option>
          <option value="1">Pending</option>
          <option value="2">Running</option>
          <option value="3">Resumed</option>
          <option value="4">Escalated</option>
          <option value="5">Failed</option>
          <option value="6">Cancelled</option>
          <option value="7">Blocked</option>
        </select>
        {selected.size > 0 && (
          <Button
            variant="destructive"
            size="sm"
            onClick={handleBatchCancel}
            disabled={batchCancel.isPending}
          >
            <Trash2 className="mr-1 h-3.5 w-3.5" />
            Cancel {selected.size} selected
          </Button>
        )}
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
            <CardTitle className="flex items-center gap-2">
              <SearchX className="h-5 w-5 text-muted-foreground" />
              No recoveries yet
            </CardTitle>
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
        <>
          <div className="flex items-center gap-2 px-2 py-1">
            <input
              type="checkbox"
              checked={recoveries.length > 0 && selected.size === recoveries.length}
              onChange={toggleSelectAll}
              className="h-4 w-4 rounded border-input"
            />
            <span className="text-xs text-muted-foreground">
              {selected.size > 0
                ? `${selected.size} of ${recoveries.length} selected`
                : `${recoveries.length} recover${recoveries.length === 1 ? "y" : "ies"}`}
            </span>
          </div>
          <div className="space-y-1">
            {recoveries.map((r) => (
              <div key={r.id} className="group flex items-center gap-2">
                <input
                  type="checkbox"
                  checked={selected.has(r.id)}
                  onChange={() => toggleSelect(r.id)}
                  className="ml-2 h-4 w-4 shrink-0 rounded border-input"
                />
                <Link to="/recovery/$id" params={{ id: r.id }} className="min-w-0 flex-1">
                  <Card className="transition-colors hover:bg-accent">
                    <CardContent className="flex flex-col gap-2 p-4 sm:flex-row sm:items-center sm:justify-between">
                      <div className="flex min-w-0 items-center gap-3">
                        <StatusBadge status={r.status} />
                        <div className="min-w-0 flex-1 overflow-hidden">
                          <p className="truncate text-sm font-medium font-mono">
                            {r.id.slice(0, 12)}…
                          </p>
                          <p className="break-all font-mono text-xs text-muted-foreground">
                            task: {r.taskId.slice(0, 12)}… · L{r.level}
                          </p>
                        </div>
                      </div>
                      <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-muted-foreground sm:shrink-0">
                        <span>{r.triggerReason}</span>
                        <span>{r.resumptionPath}</span>
                      </div>
                    </CardContent>
                  </Card>
                </Link>
                <button
                  onClick={() => {
                    if (window.confirm("Cancel this recovery?")) {
                      batchCancel.mutate([r.id]);
                    }
                  }}
                  className="opacity-0 group-hover:opacity-100 rounded px-1.5 py-0.5 text-xs font-medium text-muted-foreground hover:text-destructive hover:bg-accent transition-all shrink-0"
                  title="Cancel recovery"
                >
                  ✕
                </button>
              </div>
            ))}
          </div>
        </>
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
