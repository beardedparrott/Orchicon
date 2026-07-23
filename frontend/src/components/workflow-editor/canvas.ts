import { MarkerType, type Edge, type Node } from "reactflow";

import {
  ACCENT_STROKE,
  KIND_ACCENT,
  KIND_TO_STR,
  STR_TO_KIND,
  type StepData,
  type StepWire,
} from "./stepKinds";

export function canvasToSteps(nodes: Node<StepData>[], edges: Edge[]): StepWire[] {
  const depsByNode = new Map<string, string[]>();
  for (const n of nodes) depsByNode.set(n.id, []);
  for (const e of edges) {
    if (e.source === e.target) continue;
    // Skip loop-back edges from loop_decision nodes (source-loop handle).
    // These are visual-only for the re-entry path; adding them as a
    // dependency would prevent the initial dispatch of the target step.
    if (e.sourceHandle === "source-loop") continue;
    depsByNode.get(e.target)?.push(e.source);
  }

  const edgeHandles: Record<string, { sourceHandle?: string; targetHandle?: string }> = {};
  for (const e of edges) {
    if (e.source && e.target) {
      edgeHandles[`e-${e.source}-${e.target}`] = {
        sourceHandle: e.sourceHandle ?? undefined,
        targetHandle: e.targetHandle ?? undefined,
      };
    }
  }

  return nodes.map((n, i) => {
    const d = n.data;
    const wire: StepWire = {
      id: n.id,
      name: d.name,
      kind: KIND_TO_STR(d.kind),
      ref: d.ref,
      worker_version: d.workerVersion,
      depends_on: depsByNode.get(n.id) ?? [],
      gate_policy_ref: d.gatePolicyRef,
      config: d.config,
      position_x: n.position.x,
      position_y: n.position.y,
    };
    if (i === 0 && Object.keys(edgeHandles).length > 0) {
      wire.edge_handles = edgeHandles;
    }
    return wire;
  });
}

export function stepsToCanvas(stepsJson: string): {
  nodes: Node<StepData>[];
  edges: Edge[];
} {
  let steps: StepWire[] = [];
  try {
    steps = JSON.parse(stepsJson || "[]");
  } catch (err) {
    if (import.meta.env.DEV) {
      console.warn("stepsToCanvas: malformed steps JSON, starting empty", err);
    }
    steps = [];
  }

  const nodes: Node<StepData>[] = steps.map((s) => ({
    id: s.id,
    type: "step",
    position: { x: s.position_x, y: s.position_y },
    data: {
      kind: STR_TO_KIND[s.kind] ?? 1,
      name: s.name,
      ref: s.ref,
      workerVersion: s.worker_version,
      gatePolicyRef: s.gate_policy_ref,
      config: s.config,
    },
  }));

  const kindByNodeId = new Map<string, number>();
  for (const s of steps) {
    kindByNodeId.set(s.id, STR_TO_KIND[s.kind] ?? 1);
  }

  let edgeHandles: Record<string, { sourceHandle?: string; targetHandle?: string }> = {};
  for (const s of steps) {
    if (s.edge_handles) {
      edgeHandles = { ...edgeHandles, ...s.edge_handles };
    }
  }

  const edges: Edge[] = [];
  const seen = new Set<string>();
  for (const s of steps) {
    for (const dep of s.depends_on ?? []) {
      const edgeKey = `e-${dep}-${s.id}`;
      seen.add(edgeKey);
      const handles = edgeHandles[edgeKey];
      const srcKind = kindByNodeId.get(dep) ?? 1;
      const accent = KIND_ACCENT[srcKind] ?? "sky";
      edges.push({
        id: edgeKey,
        source: dep,
        target: s.id,
        sourceHandle: handles?.sourceHandle,
        targetHandle: handles?.targetHandle,
        markerEnd: { type: MarkerType.ArrowClosed },
        animated: true,
        style: { stroke: `var(--kind-${accent})` },
        className: ACCENT_STROKE[accent] ?? "",
      });
    }
  }
  // Also restore edges from edge_handles not covered by depends_on
  // (e.g. loop-back edges from loop_decision nodes). Match against known
  // node IDs since step IDs may contain hyphens.
  const nodeIds = new Set(nodes.map((n) => n.id));
  for (const [edgeKey, handles] of Object.entries(edgeHandles)) {
    if (seen.has(edgeKey)) continue;
    for (const srcId of nodeIds) {
      const prefix = `e-${srcId}-`;
      if (edgeKey.startsWith(prefix)) {
        const tgtId = edgeKey.slice(prefix.length);
        if (nodeIds.has(tgtId)) {
          const srcKind = kindByNodeId.get(srcId) ?? 1;
          const accent = KIND_ACCENT[srcKind] ?? "sky";
          edges.push({
            id: edgeKey,
            source: srcId,
            target: tgtId,
            sourceHandle: handles?.sourceHandle,
            targetHandle: handles?.targetHandle,
            markerEnd: { type: MarkerType.ArrowClosed },
            animated: true,
            style: { stroke: `var(--kind-${accent})` },
            className: ACCENT_STROKE[accent] ?? "",
          });
        }
        break;
      }
    }
  }

  return { nodes, edges };
}
