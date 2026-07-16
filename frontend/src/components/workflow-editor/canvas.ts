// Canvas ↔ StepWire serialization for the workflow editor.
//
// The editor keeps the canvas in React Flow's nodes + edges state. On
// Save we convert to StepWire[] (the JSON shape stored in
// workflow_versions.steps) and POST it. On Load we parse it back into
// nodes + edges.

import { MarkerType, type Edge, type Node } from "reactflow";

import {
  ACCENT_STROKE,
  KIND_ACCENT,
  KIND_TO_STR,
  STR_TO_KIND,
  type StepData,
  type StepWire,
} from "./stepKinds";

// canvasToSteps serializes the editor's nodes + edges into StepWire[].
// The depends_on field is computed from incoming edges: for each node,
// collect the sources of edges where the target is this node.
//
// `position_x` / `position_y` are stored on each step so the editor can
// rehydrate the layout exactly on Load. The backend does not interpret
// positions — it reads `kind` and `depends_on` only.
export function canvasToSteps(nodes: Node<StepData>[], edges: Edge[]): StepWire[] {
  const depsByNode = new Map<string, string[]>();
  for (const n of nodes) depsByNode.set(n.id, []);
  for (const e of edges) {
    if (e.source === e.target) continue;
    depsByNode.get(e.target)?.push(e.source);
  }
  return nodes.map((n) => {
    const d = n.data;
    return {
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
  });
}

// stepsToCanvas parses a workflow version's `steps` JSON into the
// editor's nodes + edges. Malformed JSON is treated as an empty canvas
// (with a console warning in dev).
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
  // Build a kind lookup so edges can be color-coded by source kind.
  const kindByNodeId = new Map<string, number>();
  for (const s of steps) {
    kindByNodeId.set(s.id, STR_TO_KIND[s.kind] ?? 1);
  }
  const edges: Edge[] = [];
  for (const s of steps) {
    for (const dep of s.depends_on ?? []) {
      const srcKind = kindByNodeId.get(dep) ?? 1;
      const accent = KIND_ACCENT[srcKind] ?? "sky";
      edges.push({
        id: `e-${dep}-${s.id}`,
        source: dep,
        target: s.id,
        markerEnd: { type: MarkerType.ArrowClosed },
        animated: true,
        style: { stroke: `var(--kind-${accent})` },
        className: ACCENT_STROKE[accent] ?? "",
      });
    }
  }
  return { nodes, edges };
}
