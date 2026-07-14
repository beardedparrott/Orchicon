import { Link, createRoute } from "@tanstack/react-router";

import { useListWorkflows } from "@/api/workflows";
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
  const { data: workflows, isLoading, error } = useListWorkflows();

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

      {isLoading && <p className="text-sm text-muted-foreground">Loading…</p>}
      {error && (
        <p className="text-sm text-destructive">
          Failed to load workflows: {String(error)}
        </p>
      )}

      {workflows && workflows.length === 0 && (
        <Card>
          <CardHeader>
            <CardTitle>No workflows yet</CardTitle>
            <CardDescription>
              Create a workflow and open the visual editor to drag Workers
              onto a canvas, wire step dependencies, and run the DAG.
            </CardDescription>
          </CardHeader>
        </Card>
      )}

      {workflows && workflows.length > 0 && (
        <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-3">
          {workflows.map((w) => (
            <Link key={w.id} to="/workflows_/$id" params={{ id: w.id }}>
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
          ))}
        </div>
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
