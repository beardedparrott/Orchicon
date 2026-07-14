import { createRoute, Link } from "@tanstack/react-router";
import { useState } from "react";

import { useListWorkItems } from "@/api/workItems";
import { useListProjects } from "@/api/projects";
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
import type { WorkItem as WorkItemProto } from "@/api/gen/orchicon/api/v1/work_item_pb";

// Work items page (docs/10 §5, docs/02 §2.2). Provides a tree view
// (Epic → Feature → Task → Subtask hierarchy) and a Kanban board
// (status columns). The user selects a project to scope the view.
export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/work-items",
  component: WorkItemsPage,
});

function WorkItemsPage() {
  const { data: projects } = useListProjects();
  const [projectId, setProjectId] = useState<string>("");
  const [view, setView] = useState<"tree" | "board">("tree");

  const activeProjectId = projectId || projects?.[0]?.id || "";

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">Work Items</h1>
          <p className="text-sm text-muted-foreground">
            The work hierarchy: Epic → Feature → Task → Subtask. Dependencies
            form a DAG between items.
          </p>
        </div>
        <div className="flex items-center gap-2">
          {activeProjectId && (
            <Button asChild>
              <Link
                to="/work-items/new"
                search={{ projectId: activeProjectId, parentId: "" }}
              >
                New Work Item
              </Link>
            </Button>
          )}
        </div>
      </div>

      <div className="flex items-center gap-4">
        <select
          className="rounded-md border bg-background px-3 py-1.5 text-sm"
          value={activeProjectId}
          onChange={(e) => setProjectId(e.target.value)}
          disabled={!projects || projects.length === 0}
        >
          {projects && projects.length > 0 ? (
            projects.map((p) => (
              <option key={p.id} value={p.id}>
                {p.name}
              </option>
            ))
          ) : (
            <option value="">No projects available</option>
          )}
        </select>
        <div className="flex rounded-md border">
          <button
            className={cn(
              "px-3 py-1.5 text-sm font-medium transition-colors",
              view === "tree"
                ? "bg-accent text-accent-foreground"
                : "text-muted-foreground hover:bg-accent/50",
            )}
            onClick={() => setView("tree")}
          >
            Tree
          </button>
          <button
            className={cn(
              "px-3 py-1.5 text-sm font-medium transition-colors",
              view === "board"
                ? "bg-accent text-accent-foreground"
                : "text-muted-foreground hover:bg-accent/50",
            )}
            onClick={() => setView("board")}
          >
            Board
          </button>
        </div>
        {activeProjectId && (
          <Button variant="outline" asChild>
            <Link
              to="/work-items/graph"
              search={{ projectId: activeProjectId }}
            >
              Dependency Graph
            </Link>
          </Button>
        )}
      </div>

      {!activeProjectId && (
        <Card>
          <CardHeader>
            <CardTitle>No project selected</CardTitle>
            <CardDescription>
              Create a project first to start adding work items.
            </CardDescription>
          </CardHeader>
        </Card>
      )}

      {activeProjectId && view === "tree" && (
        <TreeView projectId={activeProjectId} />
      )}
      {activeProjectId && view === "board" && (
        <KanbanBoard projectId={activeProjectId} />
      )}
    </div>
  );
}

// TreeView renders the work hierarchy as a collapsible tree
// (docs/02 §2.2). Top-level epics → features → tasks → subtasks.
function TreeView({ projectId }: { projectId: string }) {
  const { data: items, isLoading, error } = useListWorkItems(projectId);

  if (isLoading) {
    return <p className="text-sm text-muted-foreground">Loading…</p>;
  }
  if (error) {
    return (
      <p className="text-sm text-destructive">
        Failed to load work items: {String(error)}
      </p>
    );
  }
  if (!items || items.length === 0) {
    return (
      <Card>
        <CardHeader>
          <CardTitle>No work items yet</CardTitle>
          <CardDescription>
            Create an epic to start building the work hierarchy.
          </CardDescription>
        </CardHeader>
      </Card>
    );
  }

  // Build the tree: epics (parent_id = nil) at the top.
  const epics = items.filter((i) => !i.parentId);
  const childrenOf = (parentId: string) =>
    items.filter((i) => i.parentId === parentId);

  return (
    <div className="space-y-2">
      {epics.map((epic) => (
        <TreeNode
          key={epic.id}
          item={epic}
          childrenOf={childrenOf}
          depth={0}
        />
      ))}
    </div>
  );
}

function TreeNode({
  item,
  childrenOf,
  depth,
}: {
  item: WorkItemProto;
  childrenOf: (parentId: string) => WorkItemProto[];
  depth: number;
}) {
  const [expanded, setExpanded] = useState(depth < 2);
  const children = childrenOf(item.id);
  const hasChildren = children.length > 0;

  return (
    <div>
      <div
        className="flex items-center gap-2 rounded-md border p-2 hover:bg-accent/50"
        style={{ marginLeft: `${depth * 20}px` }}
      >
        {hasChildren ? (
          <button
            onClick={() => setExpanded(!expanded)}
            className="text-xs text-muted-foreground hover:text-foreground"
          >
            {expanded ? "▼" : "▶"}
          </button>
        ) : (
          <span className="w-3" />
        )}
        <KindBadge kind={item.kind} />
        <Link
          to="/work-items/$id"
          params={{ id: item.id }}
          className="flex-1 truncate text-sm font-medium hover:underline"
        >
          {item.title}
        </Link>
        <StatusPill status={item.status} />
      </div>
      {expanded &&
        children.map((child) => (
          <TreeNode
            key={child.id}
            item={child}
            childrenOf={childrenOf}
            depth={depth + 1}
          />
        ))}
    </div>
  );
}

// KanbanBoard renders work items grouped by status in columns
// (docs/02 §2.2 lifecycle states).
function KanbanBoard({ projectId }: { projectId: string }) {
  const { data: items, isLoading, error } = useListWorkItems(projectId);

  if (isLoading) {
    return <p className="text-sm text-muted-foreground">Loading…</p>;
  }
  if (error) {
    return (
      <p className="text-sm text-destructive">
        Failed to load: {String(error)}
      </p>
    );
  }
  if (!items || items.length === 0) {
    return <p className="text-sm text-muted-foreground">No work items.</p>;
  }

  const columns = [
    { status: 1, label: "Pending" },
    { status: 2, label: "Ready" },
    { status: 3, label: "Assigned" },
    { status: 4, label: "Running" },
    { status: 6, label: "Succeeded" },
    { status: 7, label: "Failed" },
    { status: 8, label: "Cancelled" },
  ];

  return (
    <div className="grid gap-4 md:grid-cols-4 lg:grid-cols-7">
      {columns.map((col) => {
        const colItems = items.filter((i) => i.status === col.status);
        return (
          <div key={col.status} className="space-y-2">
            <div className="flex items-center justify-between">
              <h3 className="text-sm font-semibold">{col.label}</h3>
              <span className="text-xs text-muted-foreground">
                {colItems.length}
              </span>
            </div>
            {colItems.map((item) => (
              <Link
                key={item.id}
                to="/work-items/$id"
                params={{ id: item.id }}
              >
                <Card className="transition-colors hover:bg-accent">
                  <CardContent className="p-3">
                    <KindBadge kind={item.kind} />
                    <p className="mt-1 text-sm font-medium line-clamp-2">
                      {item.title}
                    </p>
                  </CardContent>
                </Card>
              </Link>
            ))}
            {colItems.length === 0 && (
              <p className="text-xs text-muted-foreground">—</p>
            )}
          </div>
        );
      })}
    </div>
  );
}

function KindBadge({ kind }: { kind: number }) {
  const labels: Record<number, string> = {
    1: "E",
    2: "F",
    3: "T",
    4: "S",
  };
  const styles: Record<number, string> = {
    1: "bg-purple-100 text-purple-800",
    2: "bg-indigo-100 text-indigo-800",
    3: "bg-blue-100 text-blue-800",
    4: "bg-cyan-100 text-cyan-800",
  };
  return (
    <span
      className={cn(
        "inline-flex h-5 w-5 items-center justify-center rounded text-xs font-bold",
        styles[kind] ?? "bg-muted text-muted-foreground",
      )}
    >
      {labels[kind] ?? "?"}
    </span>
  );
}

function StatusPill({ status }: { status: number }) {
  const labels: Record<number, string> = {
    1: "pending",
    2: "ready",
    3: "assigned",
    4: "running",
    5: "checkpointing",
    6: "succeeded",
    7: "failed",
    8: "cancelled",
    9: "recovering",
  };
  const styles: Record<number, string> = {
    1: "bg-gray-100 text-gray-700",
    2: "bg-blue-100 text-blue-800",
    3: "bg-yellow-100 text-yellow-800",
    4: "bg-green-100 text-green-800",
    5: "bg-orange-100 text-orange-800",
    6: "bg-green-600 text-white",
    7: "bg-red-100 text-red-800",
    8: "bg-gray-200 text-gray-600",
    9: "bg-orange-600 text-white",
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
