import { useState } from "react";
import { Link, createRoute } from "@tanstack/react-router";
import { Trash2, SearchX } from "lucide-react";

import { useBatchDeleteWorkflows, useListWorkflows } from "@/api/workflows";
import { WorkflowStatus } from "@/api/gen/orchicon/api/v1/workflow_pb";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";
import { Route as rootRoute } from "@/routes/__root";

// Workflows list (docs/10 §5, docs/02 §2.4). Fetches via Connect-ES +
// TanStack Query; the UI reflects server state only (AGENTS.md
// invariant #1). Workflows are composable execution plans — project-
// scoped or tenant-level templates.
export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/workflows",
  component: WorkflowsPage,
});

function WorkflowsPage() {
  const [search, setSearch] = useState("");
  const [status, setStatus] = useState("all");
  const [sortBy, setSortBy] = useState("created_at");
  const [sortOrder, setSortOrder] = useState("asc");
  const [selected, setSelected] = useState<Set<string>>(new Set());

  const statusFilter =
    status === "all" ? undefined : (Number(status) as WorkflowStatus);

  const { data: workflows, isLoading, error } = useListWorkflows({
    search,
    status: statusFilter,
    sortBy,
    sortOrder,
  });
  const batchDelete = useBatchDeleteWorkflows();

  const toggleSelect = (id: string) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

  const toggleSelectAll = () => {
    if (!workflows) return;
    if (selected.size === workflows.length) {
      setSelected(new Set());
    } else {
      setSelected(new Set(workflows.map((w) => w.id)));
    }
  };

  const handleBatchDelete = () => {
    if (selected.size === 0) return;
    const count = selected.size;
    if (!window.confirm(`Delete ${count} workflow${count === 1 ? "" : "s"}? This cannot be undone.`)) return;
    batchDelete.mutate(Array.from(selected), {
      onSuccess: () => setSelected(new Set()),
    });
  };

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">Workflows</h1>
          <p className="text-sm text-muted-foreground">
            Composable execution plans. Drag Workers onto a canvas, wire
            steps together, and run the DAG.
          </p>
        </div>
        <Button asChild>
          <Link to="/workflows/new">New Workflow</Link>
        </Button>
      </div>

      <div className="flex flex-wrap items-center gap-4">
        <Input
          placeholder="Search workflows…"
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          className="max-w-xs"
        />
        <select
          value={status}
          onChange={(e) => setStatus(e.target.value)}
          className="h-9 rounded-md border border-input bg-transparent px-3 text-sm shadow-sm"
        >
          <option value="all">All</option>
          <option value="1">Draft</option>
          <option value="2">Published</option>
          <option value="3">Deprecated</option>
        </select>
        <select
          value={sortBy}
          onChange={(e) => setSortBy(e.target.value)}
          className="h-9 rounded-md border border-input bg-transparent px-3 text-sm shadow-sm"
        >
          <option value="created_at">Created</option>
          <option value="name">Name</option>
          <option value="status">Status</option>
        </select>
        <select
          value={sortOrder}
          onChange={(e) => setSortOrder(e.target.value)}
          className="h-9 rounded-md border border-input bg-transparent px-3 text-sm shadow-sm"
        >
          <option value="asc">Asc</option>
          <option value="desc">Desc</option>
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
          Failed to load workflows: {String(error)}
        </p>
      )}

      {workflows && workflows.length === 0 && (
        <Card>
          <CardHeader>
            <CardTitle className="flex items-center gap-2">
              <SearchX className="h-5 w-5 text-muted-foreground" />
              No workflows yet
            </CardTitle>
            <CardDescription>
              Create a workflow and open the visual editor to drag Workers
              onto a canvas, wire step dependencies, and run the DAG.
            </CardDescription>
          </CardHeader>
        </Card>
      )}

      {workflows && workflows.length > 0 && (
        <>
          <div className="flex items-center gap-2 px-2 py-1">
            <input
              type="checkbox"
              checked={workflows.length > 0 && selected.size === workflows.length}
              onChange={toggleSelectAll}
              className="h-4 w-4 rounded border-input"
            />
            <span className="text-xs text-muted-foreground">
              {selected.size > 0
                ? `${selected.size} of ${workflows.length} selected`
                : `${workflows.length} workflow${workflows.length === 1 ? "" : "s"}`}
            </span>
          </div>
          <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-3">
            {workflows.map((w) => (
              <div key={w.id} className="group relative">
                <input
                  type="checkbox"
                  checked={selected.has(w.id)}
                  onChange={() => toggleSelect(w.id)}
                  className="absolute left-3 top-3 z-10 h-4 w-4 rounded border-input"
                />
                <Link to="/workflows/$id" params={{ id: w.id }}>
                  <Card className="transition-colors hover:bg-accent">
                    <CardHeader>
                      <CardTitle className="flex items-center justify-between">
                        <span className="truncate">{w.name}</span>
                        <StatusBadge status={w.status} />
                      </CardTitle>
                      <CardDescription>
                        {w.projectId ? (
                          <span className="font-mono text-xs">
                            project: {w.projectId.slice(0, 10)}…
                          </span>
                        ) : (
                          <span className="text-xs italic">tenant template</span>
                        )}
                      </CardDescription>
                    </CardHeader>
                    <CardContent>
                      <p className="text-xs text-muted-foreground">
                        v{w.currentVersion || "— (draft)"}
                      </p>
                    </CardContent>
                  </Card>
                </Link>
                <button
                  onClick={() => {
                    if (window.confirm("Delete this workflow?")) {
                      batchDelete.mutate([w.id]);
                    }
                  }}
                  className="absolute right-3 top-3 opacity-0 group-hover:opacity-100 rounded px-1.5 py-0.5 text-xs font-medium text-muted-foreground hover:text-destructive hover:bg-accent transition-all"
                  title="Delete workflow"
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

// StatusBadge renders a colored pill for the workflow lifecycle state
// (docs/02 §2.4).
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
  1: "draft",
  2: "published",
  3: "deprecated",
};

const STATUS_STYLES: Record<number, string> = {
  1: "bg-blue-100 text-blue-800",
  2: "bg-green-100 text-green-800",
  3: "bg-yellow-100 text-yellow-800",
};
