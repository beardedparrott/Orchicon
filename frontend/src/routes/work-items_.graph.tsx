// Dependency graph (design §5.2, docs/10, docs/02 §2.2, docs/09 §11).
// A read-only React Flow visualization of the work DAG for a project.
// Nodes are work items; edges are dependencies. Cycles are rejected at
// admission (recursive CTE), so the graph is always a DAG.
//
// Enhanced: Jira-like styling with status-colored nodes, kind-grouped
// layout, interactive controls, legend, auto-refresh, and click-to-navigate.

import { createRoute, useNavigate, useSearch } from "@tanstack/react-router";
import { useCallback, useMemo } from "react";
import { ArrowLeft } from "lucide-react";
import ReactFlow, {
  Background,
  Controls,
  MiniMap,
  type Edge,
  type Node,
  Position,
  useNodesState,
  useEdgesState,
} from "reactflow";

import { useGetDependencyGraph, useListWorkItems } from "@/api/workItems";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import {
  kindMeta,
  statusMeta,
  isTerminal,
} from "@/components/work-items/work-item-meta";
import { cn } from "@/lib/utils";
import { Route as rootRoute } from "@/routes/__root";

import "reactflow/dist/style.css";

export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/work-items/graph",
  component: DependencyGraphPage,
  validateSearch: (search: Record<string, unknown>) => ({
    projectId: (search.projectId as string) ?? "",
  }),
});

function DependencyGraphPage() {
  const navigate = useNavigate();
  const search = useSearch({ from: "/work-items_/graph" });
  const { data: graph, isLoading, error, dataUpdatedAt, isFetching } = useGetDependencyGraph(
    search.projectId,
    { refetchInterval: 5_000 },
  );
  const { data: items } = useListWorkItems(search.projectId);

  // Build a map of items by id for quick lookup
  const itemsById = useMemo(
    () => new Map((items ?? []).map((i) => [i.id, i])),
    [items],
  );

  // Convert the server DAG (nodes + edges) into React Flow shapes.
  const layout = useMemo(() => {
    if (!graph) return { nodes: [] as Node[], edges: [] as Edge[] };

    // Layout: group by kind in horizontal lanes with spacing
    const kindY: Record<number, number> = { 1: 0, 2: 200, 3: 400, 4: 600 };
    const kindCount: Record<number, number> = { 1: 0, 2: 0, 3: 0, 4: 0 };

    const rfNodes: Node[] = (graph.nodes ?? []).map((item) => {
      const kind = item.kind;
      const status = item.status;
      const x = kindCount[kind] * 260;
      kindCount[kind] = (kindCount[kind] ?? 0) + 1;

      const meta = kindMeta(kind);
      const sMeta = statusMeta(status);
      const terminal = isTerminal(status);

      return {
        id: item.id,
        data: {
          label: (
            <div className="flex flex-col gap-1 min-w-[160px] max-w-[220px]">
              <div className="flex items-center gap-1.5">
                <span
                  className={cn(
                    "inline-flex h-4 w-4 shrink-0 items-center justify-center rounded text-[10px] font-bold",
                    meta.badge,
                  )}
                >
                  {meta.shortLabel}
                </span>
                <span className={cn("text-xs font-semibold leading-tight line-clamp-2", terminal && "line-through decoration-muted-foreground/50")}>
                  {item.title}
                </span>
              </div>
              <div className="flex items-center gap-1.5">
                <span
                  className={cn(
                    "inline-flex items-center gap-1 rounded-full px-1.5 py-0.5 text-[10px] font-medium",
                    sMeta.pill,
                  )}
                >
                  <span className={cn("h-1 w-1 rounded-full", sMeta.dot)} />
                  {sMeta.label}
                </span>
              </div>
            </div>
          ),
        },
        position: { x, y: kindY[kind] ?? 0 },
        sourcePosition: Position.Right,
        targetPosition: Position.Left,
        className: cn(
          "react-flow__node-default! rounded-lg border-2 bg-card shadow-sm transition-shadow hover:shadow-md",
          `border-l-${meta.dot.replace("bg-", "")}`,
        ),
      };
    });

    const rfEdges: Edge[] = (graph.edges ?? []).map((dep) => ({
      id: dep.id,
      source: dep.fromId,
      target: dep.toId,
      label: depTypeLabel(dep.type),
      labelStyle: { fontSize: 10, fill: "hsl(var(--muted-foreground))" },
      labelBgStyle: { fill: "hsl(var(--card))", fillOpacity: 0.9 },
      labelBgPadding: [4, 2] as [number, number],
      className: edgeClass(dep.type),
      animated: dep.type === 1, // animate "blocks" edges
    }));

    return { nodes: rfNodes, edges: rfEdges };
  }, [graph]);

  const [nodes, setNodes, onNodesChange] = useNodesState(layout.nodes);
  const [edges, setEdges, onEdgesChange] = useEdgesState(layout.edges);

  // Sync nodes/edges when layout changes
  useMemo(() => {
    setNodes(layout.nodes);
    setEdges(layout.edges);
  }, [layout, setNodes, setEdges]);

  const onNodeClick = useCallback(
    (_: React.MouseEvent, node: Node) => {
      navigate({ to: `/work-items/${node.id}` });
    },
    [navigate],
  );

  if (!search.projectId) {
    return (
      <div className="space-y-4">
        <div className="flex items-center gap-2">
          <Button
            variant="ghost"
            size="sm"
            onClick={() => navigate({ to: "/work-items" })}
            className="shrink-0"
          >
            <ArrowLeft className="h-4 w-4" />
            <span className="ml-1 hidden sm:inline">Back</span>
          </Button>
          <h1 className="text-2xl font-semibold tracking-tight">
            Dependency Graph
          </h1>
        </div>
        <Card>
          <CardHeader>
            <CardTitle>No project selected</CardTitle>
          </CardHeader>
          <CardContent>
            <p className="text-sm text-muted-foreground">
              Select a project from the work items page to view its dependency
              graph.
            </p>
          </CardContent>
        </Card>
      </div>
    );
  }

  if (isLoading) {
    return (
      <div className="space-y-4">
        <div className="flex items-center gap-2">
          <Button
            variant="ghost"
            size="sm"
            onClick={() => navigate({ to: "/work-items" })}
            className="shrink-0"
          >
            <ArrowLeft className="h-4 w-4" />
            <span className="ml-1 hidden sm:inline">Back</span>
          </Button>
          <h1 className="text-2xl font-semibold tracking-tight">
            Dependency Graph
          </h1>
        </div>
        <div className="h-[600px] animate-pulse rounded-lg border bg-card/50" />
      </div>
    );
  }

  if (error) {
    return (
      <div className="space-y-4">
        <div className="flex items-center gap-2">
          <Button
            variant="ghost"
            size="sm"
            onClick={() => navigate({ to: "/work-items" })}
            className="shrink-0"
          >
            <ArrowLeft className="h-4 w-4" />
            <span className="ml-1 hidden sm:inline">Back</span>
          </Button>
          <h1 className="text-2xl font-semibold tracking-tight">
            Dependency Graph
          </h1>
        </div>
        <p className="text-sm text-destructive">
          Failed to load dependency graph: {String(error)}
        </p>
      </div>
    );
  }

  const nodeCount = graph?.nodes?.length ?? 0;
  const edgeCount = graph?.edges?.length ?? 0;

  return (
    <div className="flex flex-col gap-4" style={{ height: "calc(100vh - 64px)" }}>
      {/* Header */}
      <div className="flex flex-wrap items-center justify-between gap-3 shrink-0">
        <div className="flex items-center gap-2">
          <Button
            variant="ghost"
            size="sm"
            onClick={() => navigate({ to: "/work-items" })}
            className="shrink-0"
          >
            <ArrowLeft className="h-4 w-4" />
            <span className="ml-1 hidden sm:inline">Back</span>
          </Button>
          <div>
            <h1 className="text-2xl font-semibold tracking-tight">
              Dependency Graph
            </h1>
            <p className="text-xs text-muted-foreground">
              {nodeCount} node{nodeCount !== 1 ? "s" : ""} · {edgeCount} edge{edgeCount !== 1 ? "s" : ""} · Click a node to open
            </p>
          </div>
        </div>
        <div className="flex items-center gap-2">
          <LiveIndicator lastUpdated={dataUpdatedAt} isFetching={isFetching} />
          <Legend />
        </div>
      </div>

      {/* Graph */}
      <div className="flex-1 min-h-0 rounded-lg border bg-card overflow-hidden">
        {nodeCount === 0 ? (
          <div className="flex h-full items-center justify-center">
            <p className="text-sm text-muted-foreground">
              No work items in this project yet. Create items to see the dependency graph.
            </p>
          </div>
        ) : (
          <ReactFlow
            nodes={nodes}
            edges={edges}
            onNodesChange={onNodesChange}
            onEdgesChange={onEdgesChange}
            onNodeClick={onNodeClick}
            fitView
            nodesDraggable
            nodesConnectable={false}
            elementsSelectable
            minZoom={0.1}
            maxZoom={2}
            defaultEdgeOptions={{
              type: "smoothstep",
              style: { strokeWidth: 1.5 },
            }}
          >
            <Background gap={16} />
            <Controls showInteractive={false} />
            <MiniMap
              nodeStrokeWidth={3}
              nodeColor={(n) => {
                const item = itemsById.get(n.id);
                if (!item) return "hsl(var(--muted))";
                const meta = kindMeta(item.kind);
                // Extract the hue from the dot class
                return meta.dot.replace("bg-", "");
              }}
              maskColor="hsl(var(--background) / 0.7)"
              className="!bg-card !border-border"
            />
          </ReactFlow>
        )}
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Live refresh indicator
// ---------------------------------------------------------------------------

function LiveIndicator({
  lastUpdated,
  isFetching,
}: {
  lastUpdated?: number;
  isFetching: boolean;
}) {
  const d = new Date(lastUpdated ?? Date.now());
  const time = d.toLocaleTimeString([], {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    hour12: false,
  });
  return (
    <div
      role="timer"
      aria-label={`Auto-refreshes every 5 seconds. Last refreshed at ${time}`}
      className="flex items-center gap-2 rounded-lg border bg-card px-3 py-2"
    >
      <span
        className={cn(
          "h-2 w-2 animate-pulse rounded-full motion-reduce:animate-none",
          isFetching ? "bg-sky-500" : "bg-emerald-500",
        )}
      />
      <span className="font-mono text-xs font-medium tabular-nums text-foreground">
        Live {time}
      </span>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Legend
// ---------------------------------------------------------------------------

function Legend() {
  return (
    <div className="flex flex-wrap items-center gap-3 rounded-lg border bg-card px-3 py-2 text-xs text-muted-foreground">
      <span className="font-medium text-foreground">Kinds:</span>
      {([1, 2, 3, 4] as const).map((kind) => {
        const meta = kindMeta(kind);
        return (
          <span key={kind} className="flex items-center gap-1">
            <span className={cn("h-2 w-2 rounded-full", meta.dot)} />
            {meta.label}
          </span>
        );
      })}
      <span className="ml-2 border-l pl-3 font-medium text-foreground">Edges:</span>
      <span className="flex items-center gap-1">
        <span className="h-0.5 w-4 bg-red-400" />
        blocks
      </span>
      <span className="flex items-center gap-1">
        <span className="h-0.5 w-4 bg-blue-400" />
        depends_on
      </span>
      <span className="flex items-center gap-1">
        <span className="h-0.5 w-4 border-b border-dashed border-gray-400" />
        relates_to
      </span>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function depTypeLabel(type: number): string {
  const labels: Record<number, string> = {
    1: "blocks",
    2: "depends_on",
    3: "relates_to",
  };
  return labels[type] ?? "unknown";
}

function edgeClass(type: number): string {
  const styles: Record<number, string> = {
    1: "stroke-red-400 stroke-2",
    2: "stroke-blue-400",
    3: "stroke-gray-300 stroke-dashed",
  };
  return styles[type] ?? "";
}
