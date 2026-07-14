import { createRoute, useSearch } from "@tanstack/react-router";
import { useMemo } from "react";
import ReactFlow, {
  Background,
  Controls,
  type Edge,
  type Node,
  Position,
} from "reactflow";

import { useGetDependencyGraph } from "@/api/workItems";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Route as rootRoute } from "@/routes/__root";

import "reactflow/dist/style.css";

// Dependency graph (docs/10, docs/02 §2.2, docs/09 §11). A read-only
// React Flow visualization of the work DAG for a project. Nodes are
// work items; edges are dependencies. Cycles are rejected at admission
// (recursive CTE), so the graph is always a DAG.
export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/work-items/graph",
  component: DependencyGraphPage,
  validateSearch: (search: Record<string, unknown>) => ({
    projectId: (search.projectId as string) ?? "",
  }),
});

function DependencyGraphPage() {
  const search = useSearch({ from: "/work-items_/graph" });
  const { data: graph, isLoading, error } = useGetDependencyGraph(
    search.projectId,
  );

  // Convert the server DAG (nodes + edges) into React Flow shapes.
  const { nodes, edges } = useMemo(() => {
    if (!graph) return { nodes: [] as Node[], edges: [] as Edge[] };

    // Layout: group by kind in horizontal lanes.
    const kindY: Record<number, number> = { 1: 0, 2: 150, 3: 300, 4: 450 };
    const kindCount: Record<number, number> = { 1: 0, 2: 0, 3: 0, 4: 0 };

    const rfNodes: Node[] = (graph.nodes ?? []).map((item) => {
      const kind = item.kind;
      const x = kindCount[kind] * 220;
      kindCount[kind] = (kindCount[kind] ?? 0) + 1;
      return {
        id: item.id,
        data: {
          label: (
            <div className="max-w-[200px]">
              <div className="text-xs font-semibold">{item.title}</div>
              <div className="text-xs text-muted-foreground">
                {kindLabel(kind)}
              </div>
            </div>
          ),
        },
        position: { x, y: kindY[kind] ?? 0 },
        sourcePosition: Position.Right,
        targetPosition: Position.Left,
        className: nodeClass(kind),
      };
    });

    const rfEdges: Edge[] = (graph.edges ?? []).map((dep) => ({
      id: dep.id,
      source: dep.fromId,
      target: dep.toId,
      label: depTypeLabel(dep.type),
      className: edgeClass(dep.type),
      animated: dep.type === 1, // animate "blocks" edges
    }));

    return { nodes: rfNodes, edges: rfEdges };
  }, [graph]);

  if (!search.projectId) {
    return (
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
    );
  }

  if (isLoading) {
    return <p className="text-sm text-muted-foreground">Loading graph…</p>;
  }
  if (error) {
    return (
      <p className="text-sm text-destructive">
        Failed to load dependency graph: {String(error)}
      </p>
    );
  }

  return (
    <div className="space-y-4">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">
          Dependency Graph
        </h1>
        <p className="text-sm text-muted-foreground">
          Read-only visualization of the work DAG. Nodes are work items;
          edges are dependencies. Cycles are rejected at admission.
        </p>
      </div>
      <div className="h-[600px] rounded-lg border bg-card">
        <ReactFlow
          nodes={nodes}
          edges={edges}
          fitView
          nodesDraggable={false}
          nodesConnectable={false}
          elementsSelectable
          minZoom={0.2}
          maxZoom={2}
        >
          <Background />
          <Controls showInteractive={false} />
        </ReactFlow>
      </div>
    </div>
  );
}

function kindLabel(kind: number): string {
  const labels: Record<number, string> = {
    1: "epic",
    2: "feature",
    3: "task",
    4: "subtask",
  };
  return labels[kind] ?? "unknown";
}

function nodeClass(kind: number): string {
  const styles: Record<number, string> = {
    1: "react-flow__node-default! bg-purple-50! border-purple-300!",
    2: "react-flow__node-default! bg-indigo-50! border-indigo-300!",
    3: "react-flow__node-default! bg-blue-50! border-blue-300!",
    4: "react-flow__node-default! bg-cyan-50! border-cyan-300!",
  };
  return styles[kind] ?? "";
}

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
