import { MarkerType, type Edge, type Node } from "reactflow";

import { parse as parseYaml, stringify as stringifyYaml } from "yaml";

import {
  ACCENT_STROKE,
  KIND_ACCENT,
  KIND_TO_STR,
  STR_TO_KIND,
  type StepData,
  type StepWire,
} from "./stepKinds";

// --- Types for the YAML interchange format ---

interface YamlStep {
  name: string;
  kind: string;
  worker?: string;
  worker_version?: number;
  gate_policy?: string;
  config?: Record<string, unknown>;
  connections?: Record<string, string>;
  position?: [number, number];
}

// --- Serializer: canvas → YAML ---

export function canvasToYaml(nodes: Node<StepData>[], edges: Edge[]): string {
  // Build connection map per node: { nodeId -> { "from_top": "SourceName", "to_bottom": "TargetName" } }
  const conns = new Map<string, Record<string, string>>();
  const nameById = new Map<string, string>();
  const idByName = new Map<string, string>();

  for (const n of nodes) {
    nameById.set(n.id, n.data.name);
    idByName.set(n.data.name, n.id);
    conns.set(n.id, {});
  }

  for (const e of edges) {
    if (e.source === e.target) continue;
    const srcName = nameById.get(e.source) ?? "";
    const tgtName = nameById.get(e.target) ?? "";
    if (!srcName || !tgtName) continue;

    // Determine handle positions from edge handle IDs
    // Handle IDs are like "source-bottom" or "target-top"
    const srcPos = posFromHandle(e.sourceHandle, "bottom");
    const tgtPos = posFromHandle(e.targetHandle, "top");

    // Outgoing connection from source
    const outKey = `to_${srcPos}` as const;
    const srcConns = conns.get(e.source)!;
    srcConns[outKey] = tgtName;

    // Incoming connection to target
    const inKey = `from_${tgtPos}` as const;
    const tgtConns = conns.get(e.target)!;
    tgtConns[inKey] = srcName;
  }

  // Build YAML steps preserving node order
  const yamlSteps: YamlStep[] = nodes.map((n) => {
    const d = n.data;
    const step: YamlStep = {
      name: d.name,
      kind: KIND_TO_STR(d.kind),
      position: [n.position.x, n.position.y],
    };

    if (d.ref) step.worker = d.ref;
    if (d.workerVersion) step.worker_version = d.workerVersion;
    if (d.gatePolicyRef) step.gate_policy = d.gatePolicyRef;

    // Parse config JSON into a map for readability
    if (d.config) {
      try {
        const parsed = JSON.parse(d.config);
        if (parsed && typeof parsed === "object") {
          step.config = parsed as Record<string, unknown>;
        }
      } catch { /* keep as omitted */ }
    }

    // Only emit connections if there are any
    const stepConns = conns.get(n.id);
    if (stepConns && Object.keys(stepConns).length > 0) {
      step.connections = stepConns;
    }

    return step;
  });

  return stringifyYaml({ steps: yamlSteps }, {
    lineWidth: 0,
    indent: 2,
    sortMapEntries: false,
  });
}

// --- Deserializer: YAML → canvas nodes and edges ---

export function yamlToCanvas(yamlStr: string): { nodes: Node<StepData>[]; edges: Edge[] } {
  const doc = parseYaml(yamlStr);
  if (!doc || typeof doc !== "object" || !("steps" in doc)) {
    throw new Error("YAML must contain a top-level 'steps' key");
  }
  const rawSteps = doc.steps;
  if (!Array.isArray(rawSteps)) {
    throw new Error("'steps' must be an array");
  }

  // Parse YAML steps into StepWire format
  const wires: StepWire[] = [];
  const nameToId = new Map<string, string>();

  for (let i = 0; i < rawSteps.length; i++) {
    const s = rawSteps[i] as YamlStep;
    if (!s.name) throw new Error(`Step ${i + 1} is missing 'name'`);

    const slug = s.name.toLowerCase().replace(/[^a-z0-9]+/g, "_").replace(/^_|_$/g, "");
    const id = `step_${slug}`;
    nameToId.set(s.name, id);

    const [posX, posY] = s.position ?? [100, 100 + i * 150];

    const wire: StepWire = {
      id,
      name: s.name,
      kind: s.kind,
      ref: s.worker ?? "",
      worker_version: s.worker_version ?? 0,
      depends_on: [],
      gate_policy_ref: s.gate_policy ?? "",
      config: "{}",
      position_x: posX,
      position_y: posY,
    };

    if (s.config) {
      wire.config = JSON.stringify(s.config);
    }

    wires.push(wire);
  }

  // Resolve connections → depends_on + edge_handles
  // Also build a reverse map: id → connections
  const edgeHandles: Record<string, { sourceHandle?: string; targetHandle?: string }> = {};

  for (const s of rawSteps as YamlStep[]) {
    if (!s.connections) continue;
    const tgtId = nameToId.get(s.name);
    if (!tgtId) continue;

    for (const [key, val] of Object.entries(s.connections)) {
      const srcName = val as string;
      const srcId = nameToId.get(srcName);
      if (!srcId) continue;
      const tgtWire = wires.find((w) => w.id === tgtId);
      if (!tgtWire) continue;

      // `to_<pos>`: outgoing from this node to `val`
      // `from_<pos>`: incoming to this node from `val`
      if (key.startsWith("to_")) {
        const pos = key.replace("to_", "");
        const edgeKey = `e-${tgtId}-${srcId}`;
        edgeHandles[edgeKey] = {
          sourceHandle: handleFromPos(pos),
          targetHandle: undefined,
        };
        // The target needs the dependency too
        const srcWire = wires.find((w) => w.id === srcId);
        if (srcWire && !srcWire.depends_on.includes(tgtId)) {
          srcWire.depends_on.push(tgtId);
        }
      } else if (key.startsWith("from_")) {
        const pos = key.replace("from_", "");
        const edgeKey = `e-${srcId}-${tgtId}`;
        edgeHandles[edgeKey] = {
          sourceHandle: undefined,
          targetHandle: handleFromPos(pos),
        };
        tgtWire.depends_on.push(srcId);
      }
    }
  }

  // Attach edge_handles to the first wire (matches canvas.ts convention)
  if (Object.keys(edgeHandles).length > 0 && wires.length > 0) {
    wires[0].edge_handles = edgeHandles;
  }

  // Build canvas nodes from wires
  const nodes: Node<StepData>[] = wires.map((s) => ({
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

  // Build canvas edges from wires + edge_handles
  const kindByNodeId = new Map<string, number>();
  for (const w of wires) kindByNodeId.set(w.id, STR_TO_KIND[w.kind] ?? 1);

  const allEdgeHandles: Record<string, { sourceHandle?: string; targetHandle?: string }> = {};
  for (const w of wires) {
    if (w.edge_handles) Object.assign(allEdgeHandles, w.edge_handles);
  }

  const edges: Edge[] = [];
  for (const w of wires) {
    if (!w.depends_on) continue;
    for (const dep of w.depends_on) {
      const edgeKey = `e-${dep}-${w.id}`;
      const handles = allEdgeHandles[edgeKey];
      const srcKind = kindByNodeId.get(dep) ?? 1;
      const accent = KIND_ACCENT[srcKind] ?? "sky";
      edges.push({
        id: edgeKey,
        source: dep,
        target: w.id,
        sourceHandle: handles?.sourceHandle,
        targetHandle: handles?.targetHandle,
        markerEnd: { type: MarkerType.ArrowClosed },
        animated: true,
        style: { stroke: `var(--kind-${accent})` },
        className: ACCENT_STROKE[accent] ?? "",
      });
    }
  }

  return { nodes, edges };
}

// --- Helpers ---

// Map React Flow handle IDs to position names and back
const POS_FROM_HANDLE: Record<string, string> = {
  "source-bottom": "bottom",
  "source-top": "top",
  "source-left": "left",
  "source-right": "right",
  "target-bottom": "bottom",
  "target-top": "top",
  "target-left": "left",
  "target-right": "right",
};

function posFromHandle(handleId: string | undefined | null, fallback: string): string {
  if (!handleId) return fallback;
  return POS_FROM_HANDLE[handleId] ?? fallback;
}

function handleFromPos(pos: string): string | undefined {
  const map: Record<string, string> = {
    top: "source-top",
    bottom: "source-bottom",
    left: "source-left",
    right: "source-right",
  };
  return map[pos];
}
