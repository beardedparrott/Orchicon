import { createRoute, useNavigate } from "@tanstack/react-router";
import { useCallback, useEffect, useRef, useState } from "react";
import ReactFlow, {
  Background,
  Controls,
  Handle,
  MarkerType,
  MiniMap,
  Position,
  addEdge,
  useEdgesState,
  useNodesState,
  useReactFlow,
  type Connection,
  type Edge,
  type Node,
  type NodeChange,
  type EdgeChange,
  ReactFlowProvider,
} from "reactflow";

import {
  useAcquireWorkflowEditLock,
  useDeprecateWorkflow,
  useGetWorkflow,
  useGetWorkflowEditLock,
  useListWorkflowVersions,
  usePublishWorkflow,
  useReleaseWorkflowEditLock,
  useUpdateWorkflowVersion,
  useListWorkflowRuns,
  useStartWorkflow,
} from "@/api/workflows";
import { useListWorkers } from "@/api/workers";
import { useListProjects } from "@/api/projects";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import { cn } from "@/lib/utils";
import { Route as rootRoute } from "@/routes/__root";

import "reactflow/dist/style.css";

// Workflow visual editor (docs/10 §5, §5.1, §11: "full visual drag-and-drop
// editor in v0.1"). A React Flow canvas where users drag Workers onto the
// canvas, wire steps together visually, and edit properties inline.
//
// Includes: drag-and-drop, canvas state management, inline property
// editing, undo/redo, and validation. The editor edits the draft
// version's steps; Save persists via UpdateWorkflowVersion, Publish
// transitions draft → published (immutable).
//
// Edit lock (docs/07 §3.3, docs/10 §11): acquired on mount, released on
// unmount. Other users see "currently being edited by [user]" and view
// read-only.
export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/workflows/$id",
  component: WorkflowEditorPage,
});

function WorkflowEditorPage() {
  const { id } = Route.useParams();
  return (
    <ReactFlowProvider>
      <EditorInner workflowId={id} />
    </ReactFlowProvider>
  );
}

// --- step shape (mirrors proto Step / backend workflow.StepWire) ---
interface StepData {
  kind: number; // StepKind enum (1=task,2=decision,3=approval,4=parallel,5=recover)
  name: string;
  ref: string; // worker_id for task steps
  workerVersion: number;
  gatePolicyRef: string;
  config: string; // JSON
}

const STEP_KIND_LABELS: Record<number, string> = {
  1: "task",
  2: "decision",
  3: "approval",
  4: "parallel",
  5: "recover",
};

const STEP_KIND_COLORS: Record<number, string> = {
  1: "border-blue-400 bg-blue-50",
  2: "border-amber-400 bg-amber-50",
  3: "border-yellow-500 bg-yellow-50",
  4: "border-purple-400 bg-purple-50",
  5: "border-red-400 bg-red-50",
};

function EditorInner({ workflowId }: { workflowId: string }) {
  const navigate = useNavigate();
  const { data, isLoading, error } = useGetWorkflow(workflowId);
  const { data: versions } = useListWorkflowVersions(workflowId);
  const { data: workers } = useListWorkers();
  const { data: projects } = useListProjects();
  const { data: runs } = useListWorkflowRuns(workflowId);
  const { data: editLock } = useGetWorkflowEditLock(workflowId);
  const acquireLock = useAcquireWorkflowEditLock();
  const releaseLock = useReleaseWorkflowEditLock();
  const updateVersion = useUpdateWorkflowVersion();
  const publishWorkflow = usePublishWorkflow();
  const deprecateWorkflow = useDeprecateWorkflow();
  const startWorkflow = useStartWorkflow();

  const [nodes, setNodes, onNodesChange] = useNodesState([]);
  const [edges, setEdges, onEdgesChange] = useEdgesState([]);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [dirty, setDirty] = useState(false);
  const [validationErrors, setValidationErrors] = useState<string[]>([]);

  // --- undo/redo history ---
  const history = useRef<{ nodes: Node[]; edges: Edge[] }[]>([]);
  const histPtr = useRef(-1);
  const pushHistory = useCallback(
    (n: Node[], e: Edge[]) => {
      // Truncate any redo tail.
      history.current = history.current.slice(0, histPtr.current + 1);
      history.current.push({ nodes: n, edges: e });
      // Cap history at 100 entries.
      if (history.current.length > 100) history.current.shift();
      else histPtr.current = history.current.length - 1;
    },
    [],
  );
  const undo = useCallback(() => {
    if (histPtr.current <= 0) return;
    histPtr.current -= 1;
    const snap = history.current[histPtr.current];
    if (snap) {
      setNodes(snap.nodes);
      setEdges(snap.edges);
      setDirty(true);
    }
  }, [setNodes, setEdges]);
  const redo = useCallback(() => {
    if (histPtr.current >= history.current.length - 1) return;
    histPtr.current += 1;
    const snap = history.current[histPtr.current];
    if (snap) {
      setNodes(snap.nodes);
      setEdges(snap.edges);
      setDirty(true);
    }
  }, [setNodes, setEdges]);

  // --- edit lock lifecycle (docs/07 §3.3) ---
  const [lockActor] = useState(
    () => `user-${Math.random().toString(36).slice(2, 8)}`,
  );
  const [lockAcquired, setLockAcquired] = useState(false);
  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const res = await acquireLock.mutateAsync({
          workflowId,
          actor: lockActor,
        });
        if (!cancelled) setLockAcquired(res.acquired);
      } catch {
        // ignore — read-only mode
      }
    })();
    return () => {
      cancelled = true;
      releaseLock.mutate({ workflowId, actor: lockActor });
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [workflowId]);

  const lockHeldByOther = !!editLock && editLock.heldBy !== lockActor;
  const readOnly = !lockAcquired || lockHeldByOther;

  // --- load steps from the latest draft version into the canvas ---
  const latestVersion = versions && versions.length > 0 ? versions[0] : undefined;
  const loadedRef = useRef<string>("");
  useEffect(() => {
    if (!latestVersion) return;
    // Load once per version id (avoid clobbering in-progress edits).
    if (loadedRef.current === latestVersion.id && !dirty) return;
    const { nodes: n, edges: e } = stepsToCanvas(latestVersion.steps);
    setNodes(n);
    setEdges(e);
    loadedRef.current = latestVersion.id;
    history.current = [{ nodes: n, edges: e }];
    histPtr.current = 0;
    setDirty(false);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [latestVersion?.id]);

  // --- node changes: track position updates for persistence ---
  const handleNodesChange = useCallback(
    (changes: NodeChange[]) => {
      onNodesChange(changes);
      // Mark dirty on position-finalize or removal.
      if (changes.some((c) => c.type === "remove")) {
        const removed = changes
          .filter((c) => c.type === "remove")
          .map((c) => (c as { id: string }).id);
        setEdges((prev) => prev.filter((ed) => !removed.includes(ed.source) && !removed.includes(ed.target)));
        if (selectedId && removed.includes(selectedId)) setSelectedId(null);
        pushHistory(nodes.filter((n) => !removed.includes(n.id)), edges.filter((ed) => !removed.includes(ed.source) && !removed.includes(ed.target)));
        setDirty(true);
      }
    },
    [onNodesChange, setEdges, pushHistory, nodes, edges, selectedId],
  );

  const handleEdgesChange = useCallback(
    (changes: EdgeChange[]) => {
      onEdgesChange(changes);
      if (changes.some((c) => c.type === "remove")) {
        setDirty(true);
        pushHistory(nodes, edges.filter((ed) => !changes.some((c) => c.type === "remove" && c.id === ed.id)));
      }
    },
    [onEdgesChange, pushHistory, nodes, edges],
  );

  const onConnect = useCallback(
    (conn: Connection) => {
      if (conn.source === conn.target) return; // no self-deps
      // Edge source→target means target depends on source.
      setEdges((eds) =>
        addEdge(
          {
            ...conn,
            id: `e-${conn.source}-${conn.target}`,
            markerEnd: { type: MarkerType.ArrowClosed },
            animated: false,
          },
          eds,
        ),
      );
      setDirty(true);
      // push history after state flush
      setTimeout(
        () => pushHistory(nodes, [...edges, { id: `e-${conn.source}-${conn.target}`, source: conn.source!, target: conn.target!, markerEnd: { type: MarkerType.ArrowClosed } }]),
        0,
      );
    },
    [setEdges, pushHistory, nodes, edges],
  );

  // --- drag-and-drop from palette (docs/10 §5.1: WorkerCard draggable) ---
  const rf = useReactFlow();
  const wrapperRef = useRef<HTMLDivElement>(null);

  const onDrop = useCallback(
    (event: React.DragEvent) => {
      event.preventDefault();
      const payload = event.dataTransfer.getData("application/x-workflow-step");
      if (!payload) return;
      const { kind, ref, name } = JSON.parse(payload) as {
        kind: number;
        ref?: string;
        name?: string;
      };
      const position = rf.screenToFlowPosition({
        x: event.clientX,
        y: event.clientY,
      });
      const id = `step-${Math.random().toString(36).slice(2, 10)}`;
      const data: StepData = {
        kind,
        name: name ?? `${STEP_KIND_LABELS[kind] ?? "step"}-${id.slice(5, 9)}`,
        ref: ref ?? "",
        workerVersion: 0,
        gatePolicyRef: "",
        config: "{}",
      };
      const node: Node = {
        id,
        type: "step",
        position,
        data,
      };
      setNodes((nds) => nds.concat(node));
      setDirty(true);
      setTimeout(() => pushHistory([...nodes, node], edges), 0);
      setSelectedId(id);
    },
    [rf, setNodes, pushHistory, nodes, edges],
  );

  // --- inline property editing ---
  const selectedNode = nodes.find((n) => n.id === selectedId) ?? null;
  const updateSelected = (patch: Partial<StepData>) => {
    if (!selectedNode) return;
    setNodes((nds) =>
      nds.map((n) =>
        n.id === selectedNode.id ? { ...n, data: { ...n.data, ...patch } } : n,
      ),
    );
    setDirty(true);
  };

  // --- validation (docs/10 §11: validation is part of the editor) ---
  const validate = useCallback((): string[] => {
    const errs: string[] = [];
    // Each task step must reference a worker.
    for (const n of nodes) {
      const d = n.data as StepData;
      if (d.kind === 1 && !d.ref) {
        errs.push(`Step "${d.name}" is a task but has no Worker reference.`);
      }
      if (!d.name) {
        errs.push(`Step ${n.id} has no name.`);
      }
    }
    // Cycle detection (depends_on must form a DAG). Edges source→target
    // mean target depends on source; a cycle is a closed loop.
    const adj = new Map<string, string[]>();
    for (const n of nodes) adj.set(n.id, []);
    for (const e of edges) {
      // target depends on source → traverse source→target
      adj.get(e.source)?.push(e.target);
    }
    const color = new Map<string, number>(); // 0=white,1=gray,2=black
    const dfs = (u: string): boolean => {
      color.set(u, 1);
      for (const v of adj.get(u) ?? []) {
        const c = color.get(v) ?? 0;
        if (c === 1) return true;
        if (c === 0 && dfs(v)) return true;
      }
      color.set(u, 2);
      return false;
    };
    for (const n of nodes) {
      if ((color.get(n.id) ?? 0) === 0 && dfs(n.id)) {
        errs.push("The step graph contains a cycle (edges must form a DAG).");
        break;
      }
    }
    // Duplicate step ids.
    const ids = new Set<string>();
    for (const n of nodes) {
      if (ids.has(n.id)) errs.push(`Duplicate step id: ${n.id}`);
      ids.add(n.id);
    }
    return errs;
  }, [nodes, edges]);

  useEffect(() => {
    setValidationErrors(validate());
  }, [validate]);

  // --- save (UpdateWorkflowVersion) ---
  const handleSave = async () => {
    if (!latestVersion) return;
    const steps = canvasToSteps(nodes, edges);
    await updateVersion.mutateAsync({
      workflowId,
      steps: JSON.stringify(steps),
      inputs: "{}",
      outputs: "{}",
      recoveryPolicyRef: "",
    });
    setDirty(false);
  };

  const handlePublish = async () => {
    if (dirty) {
      if (!confirm("You have unsaved changes. Save and publish?")) return;
      await handleSave();
    }
    await publishWorkflow.mutateAsync(workflowId);
  };

  const handleStart = async () => {
    if (!data?.workflow) return;
    const projectId = data.workflow.projectId || projects?.[0]?.id || "";
    if (!projectId) {
      alert("This workflow is a tenant template. Start it from a project context, or assign a project.");
      return;
    }
    const run = await startWorkflow.mutateAsync({
      workflowId,
      projectId,
      runContext: "{}",
    });
    navigate({ to: "/workflows/$id/runs/$runId", params: { id: workflowId, runId: run.id } });
  };

  // keyboard shortcuts: undo/redo
  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key === "z" && !e.shiftKey) {
        e.preventDefault();
        undo();
      } else if (
        (e.metaKey || e.ctrlKey) &&
        (e.key === "y" || (e.key === "z" && e.shiftKey))
      ) {
        e.preventDefault();
        redo();
      }
    };
    window.addEventListener("keydown", handler);
    return () => window.removeEventListener("keydown", handler);
  }, [undo, redo]);

  if (isLoading) {
    return <p className="text-sm text-muted-foreground">Loading…</p>;
  }
  if (error) {
    return (
      <p className="text-sm text-destructive">
        Failed to load workflow: {String(error)}
      </p>
    );
  }
  if (!data) return null;

  const wf = data.workflow;
  const isDraft = wf.status === 1;
  const isPublished = wf.status === 2;
  const isDeprecated = wf.status === 3;

  return (
    <div className="flex flex-col gap-4">
      {/* header + actions */}
      <div className="flex items-start justify-between">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">{wf.name}</h1>
          <p className="text-xs text-muted-foreground">
            {wf.projectId ? `project: ${wf.projectId.slice(0, 12)}…` : "tenant template"} ·
            {" "}v{wf.currentVersion || "—"} · status:{" "}
            {WORKFLOW_STATUS_LABELS[wf.status]}
          </p>
        </div>
        <div className="flex flex-wrap gap-2">
          <Button
            variant="outline"
            onClick={undo}
            disabled={readOnly || histPtr.current <= 0}
            title="Undo (Ctrl+Z)"
          >
            Undo
          </Button>
          <Button
            variant="outline"
            onClick={redo}
            disabled={
              readOnly ||
              histPtr.current >= history.current.length - 1
            }
            title="Redo (Ctrl+Shift+Z)"
          >
            Redo
          </Button>
          <Button
            variant="outline"
            onClick={handleSave}
            disabled={readOnly || !isDraft || !dirty || updateVersion.isPending}
          >
            {updateVersion.isPending ? "Saving…" : "Save draft"}
          </Button>
          {isDraft && (
            <Button
              onClick={handlePublish}
              disabled={readOnly || publishWorkflow.isPending || validationErrors.length > 0}
              title={validationErrors.length > 0 ? "Resolve validation errors first" : "Publish (immutable)"}
            >
              {publishWorkflow.isPending ? "Publishing…" : "Publish"}
            </Button>
          )}
          {isPublished && (
            <Button
              variant="outline"
              onClick={() => deprecateWorkflow.mutateAsync(workflowId)}
              disabled={deprecateWorkflow.isPending}
            >
              Deprecate
            </Button>
          )}
          {(isPublished || isDeprecated) && (
            <Button onClick={handleStart} disabled={startWorkflow.isPending}>
              {startWorkflow.isPending ? "Starting…" : "Start run"}
            </Button>
          )}
        </div>
      </div>

      {/* edit lock banner */}
      <EditLockBanner
        lockAcquired={lockAcquired}
        lockHeldByOther={lockHeldByOther}
        heldBy={editLock?.heldBy ?? ""}
      />

      {/* validation errors */}
      {validationErrors.length > 0 && (
        <div className="rounded-md border border-amber-300 bg-amber-50 p-3 text-sm text-amber-800">
          <p className="font-medium">Validation:</p>
          <ul className="ml-4 list-disc">
            {validationErrors.map((e, i) => (
              <li key={i}>{e}</li>
            ))}
          </ul>
        </div>
      )}

      {/* main editor layout: palette | canvas | properties */}
      <div className="grid grid-cols-1 gap-4 lg:grid-cols-[200px_1fr_300px]">
        {/* palette */}
        <Palette workers={workers ?? []} readOnly={readOnly} />

        {/* canvas */}
        <div
          ref={wrapperRef}
          className="h-[600px] rounded-lg border bg-card"
          onDrop={onDrop}
          onDragOver={(e) => {
            e.preventDefault();
            e.dataTransfer.dropEffect = "copy";
          }}
        >
          <ReactFlow
            nodes={nodes}
            edges={edges}
            onNodesChange={handleNodesChange}
            onEdgesChange={handleEdgesChange}
            onConnect={onConnect}
            onNodeClick={(_, n) => setSelectedId(n.id)}
            onPaneClick={() => setSelectedId(null)}
            nodeTypes={{ step: StepNode }}
            fitView
            minZoom={0.2}
            maxZoom={2}
            nodesConnectable={!readOnly}
            nodesDraggable={!readOnly}
            elementsSelectable
          >
            <Background />
            <Controls showInteractive={!readOnly} />
            <MiniMap />
          </ReactFlow>
        </div>

        {/* properties panel */}
        <PropertiesPanel
          node={selectedNode}
          onChange={updateSelected}
          readOnly={readOnly}
        />
      </div>

      {/* version history + runs */}
      <div className="grid gap-4 md:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle>Versions</CardTitle>
            <CardDescription>
              All versions, newest first. A published version is immutable.
            </CardDescription>
          </CardHeader>
          <CardContent>
            {(versions ?? []).length === 0 && (
              <p className="text-sm text-muted-foreground">No versions yet.</p>
            )}
            <div className="space-y-2">
              {(versions ?? []).map((v) => (
                <div
                  key={v.id}
                  className="flex items-center gap-3 rounded-md border p-2 text-sm"
                >
                  <span className="font-mono text-xs font-medium">v{v.version}</span>
                  <VersionStatusBadge status={v.status} />
                  {v.publishedAt && (
                    <span className="text-xs text-muted-foreground">
                      {new Date(Number(v.publishedAt.seconds) * 1000).toLocaleString()}
                    </span>
                  )}
                </div>
              ))}
            </div>
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>Runs</CardTitle>
            <CardDescription>
              Recent runs. Click to view live step transitions.
            </CardDescription>
          </CardHeader>
          <CardContent>
            {(runs ?? []).length === 0 && (
              <p className="text-sm text-muted-foreground">No runs yet.</p>
            )}
            <div className="space-y-2">
              {(runs ?? []).map((r) => (
                <button
                  key={r.id}
                  className="flex w-full items-center gap-3 rounded-md border p-2 text-left text-sm hover:bg-accent"
                  onClick={() =>
                    navigate({
                      to: "/workflows/$id/runs/$runId",
                      params: { id: workflowId, runId: r.id },
                    })
                  }
                >
                  <RunStatusBadge status={r.status} />
                  <span className="font-mono text-xs">{r.id.slice(0, 12)}…</span>
                </button>
              ))}
            </div>
          </CardContent>
        </Card>
      </div>
    </div>
  );
}

// --- Step node (custom React Flow node) ---
function StepNode({ data, selected }: { data: StepData; selected?: boolean }) {
  const kind = data.kind;
  return (
    <div
      className={cn(
        "min-w-[140px] rounded-md border px-3 py-2 text-center shadow-sm",
        STEP_KIND_COLORS[kind] ?? "border-gray-300 bg-white",
        selected && "ring-2 ring-blue-500",
      )}
    >
      <Handle type="target" position={Position.Left} />
      <div className="text-[10px] font-medium uppercase text-muted-foreground">
        {STEP_KIND_LABELS[kind] ?? "step"}
      </div>
      <div className="truncate text-sm font-semibold">{data.name}</div>
      {data.ref && (
        <div className="truncate text-[10px] font-mono text-muted-foreground">
          {data.ref.slice(0, 18)}
        </div>
      )}
      <Handle type="source" position={Position.Right} />
    </div>
  );
}

// --- Palette (draggable tiles) ---
function Palette({
  workers,
  readOnly,
}: {
  workers: import("@/api/gen/orchicon/api/v1/worker_pb").Worker[];
  readOnly: boolean;
}) {
  const published = workers.filter((w) => w.status === 2);
  const stepKinds = [
    { kind: 2, label: "Decision", desc: "branch" },
    { kind: 3, label: "Approval", desc: "gate" },
    { kind: 4, label: "Parallel", desc: "fan-out" },
    { kind: 5, label: "Recover", desc: "recovery" },
  ];
  return (
    <div className="space-y-3">
      <div>
        <h3 className="mb-2 text-xs font-semibold uppercase text-muted-foreground">
          Workers (drag onto canvas)
        </h3>
        <div className="space-y-2">
          {published.length === 0 && (
            <p className="text-xs text-muted-foreground">
              No published workers. Publish a worker first.
            </p>
          )}
          {published.map((w) => (
            <div
              key={w.id}
              draggable={!readOnly}
              onDragStart={(e) => {
                e.dataTransfer.setData(
                  "application/x-workflow-step",
                  JSON.stringify({ kind: 1, ref: w.id, name: w.name }),
                );
                e.dataTransfer.effectAllowed = "move";
              }}
              className="cursor-grab rounded-md border border-blue-300 bg-blue-50 p-2 text-xs hover:bg-blue-100"
            >
              <div className="font-medium">{w.name}</div>
              <div className="text-[10px] font-mono text-muted-foreground">
                {w.slug}
              </div>
            </div>
          ))}
        </div>
      </div>
      <div>
        <h3 className="mb-2 text-xs font-semibold uppercase text-muted-foreground">
          Step nodes
        </h3>
        <div className="space-y-2">
          {stepKinds.map((s) => (
            <div
              key={s.kind}
              draggable={!readOnly}
              onDragStart={(e) => {
                e.dataTransfer.setData(
                  "application/x-workflow-step",
                  JSON.stringify({ kind: s.kind }),
                );
                e.dataTransfer.effectAllowed = "move";
              }}
              className={cn(
                "cursor-grab rounded-md border p-2 text-xs hover:opacity-80",
                STEP_KIND_COLORS[s.kind] ?? "border-gray-300 bg-white",
              )}
            >
              <div className="font-medium">{s.label}</div>
              <div className="text-[10px] text-muted-foreground">{s.desc}</div>
            </div>
          ))}
        </div>
      </div>
      <p className="text-[10px] text-muted-foreground">
        Draw an edge from A to B to make B depend on A (A runs first).
      </p>
    </div>
  );
}

// --- Properties panel (inline editing) ---
function PropertiesPanel({
  node,
  onChange,
  readOnly,
}: {
  node: Node | null;
  onChange: (patch: Partial<StepData>) => void;
  readOnly: boolean;
}) {
  if (!node) {
    return (
      <Card>
        <CardHeader>
          <CardTitle className="text-base">Properties</CardTitle>
          <CardDescription>
            Select a step to edit its properties. Drag Workers and step
            nodes from the palette onto the canvas.
          </CardDescription>
        </CardHeader>
      </Card>
    );
  }
  const d = node.data as StepData;
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">
          {STEP_KIND_LABELS[d.kind] ?? "step"} properties
        </CardTitle>
        <CardDescription className="font-mono text-xs">{node.id}</CardDescription>
      </CardHeader>
      <CardContent className="space-y-3">
        <div className="space-y-1">
          <Label htmlFor="step-name">Name</Label>
          <Input
            id="step-name"
            value={d.name}
            disabled={readOnly}
            onChange={(e) => onChange({ name: e.target.value })}
          />
        </div>
        {d.kind === 1 && (
          <>
            <div className="space-y-1">
              <Label htmlFor="step-ref">Worker ID</Label>
              <Input
                id="step-ref"
                value={d.ref}
                disabled={readOnly}
                placeholder="worker ULID"
                onChange={(e) => onChange({ ref: e.target.value })}
              />
            </div>
            <div className="space-y-1">
              <Label htmlFor="step-wv">Worker version (0 = latest)</Label>
              <Input
                id="step-wv"
                type="number"
                value={d.workerVersion}
                disabled={readOnly}
                onChange={(e) =>
                  onChange({ workerVersion: Number(e.target.value) })
                }
              />
            </div>
          </>
        )}
        <div className="space-y-1">
          <Label htmlFor="step-gate">Gate policy ref (optional)</Label>
          <Input
            id="step-gate"
            value={d.gatePolicyRef}
            disabled={readOnly}
            placeholder="evaluated before the step runs"
            onChange={(e) => onChange({ gatePolicyRef: e.target.value })}
          />
        </div>
        <div className="space-y-1">
          <Label htmlFor="step-config">Config (JSON)</Label>
          <Textarea
            id="step-config"
            value={d.config}
            disabled={readOnly}
            rows={4}
            className="font-mono text-xs"
            onChange={(e) => onChange({ config: e.target.value })}
          />
        </div>
      </CardContent>
    </Card>
  );
}

// --- Edit lock banner (docs/07 §3.3) ---
function EditLockBanner({
  lockAcquired,
  lockHeldByOther,
  heldBy,
}: {
  lockAcquired: boolean;
  lockHeldByOther: boolean;
  heldBy: string;
}) {
  if (lockAcquired) {
    return (
      <div className="rounded-md border border-green-200 bg-green-50 p-3 text-sm text-green-800">
        ● Edit lock acquired — you can edit this workflow. Save persists
        the draft; Publish makes it immutable.
      </div>
    );
  }
  if (lockHeldByOther) {
    return (
      <div className="rounded-md border border-yellow-200 bg-yellow-50 p-3 text-sm text-yellow-800">
        ⏳ Currently being edited by{" "}
        <span className="font-mono">{heldBy}</span> — viewing read-only.
        The lock expires automatically on disconnect.
      </div>
    );
  }
  return null;
}

// --- canvas ↔ steps serialization ---

// stepsToStepWires converts the canvas (nodes + edges) into the StepWire
// shape stored in workflow_versions.steps.
interface StepWire {
  id: string;
  name: string;
  kind: string;
  ref: string;
  worker_version: number;
  depends_on: string[];
  gate_policy_ref: string;
  config: string;
  position_x: number;
  position_y: number;
}

function kindNumToString(k: number): string {
  return STEP_KIND_LABELS[k] ?? "task";
}

function canvasToSteps(nodes: Node[], edges: Edge[]): StepWire[] {
  // depends_on: for each node, the set of source ids of incoming edges
  // (edges where target == node id). Edge source→target means target
  // depends on source.
  const depsByNode = new Map<string, string[]>();
  for (const n of nodes) depsByNode.set(n.id, []);
  for (const e of edges) {
    if (e.source === e.target) continue;
    depsByNode.get(e.target)?.push(e.source);
  }
  return nodes.map((n) => {
    const d = n.data as StepData;
    return {
      id: n.id,
      name: d.name,
      kind: kindNumToString(d.kind),
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

function stepsToCanvas(stepsJson: string): { nodes: Node[]; edges: Edge[] } {
  let steps: StepWire[] = [];
  try {
    steps = JSON.parse(stepsJson || "[]");
  } catch {
    steps = [];
  }
  const kindStrToNum: Record<string, number> = {
    task: 1,
    decision: 2,
    approval: 3,
    parallel: 4,
    recover: 5,
  };
  const nodes: Node[] = steps.map((s) => ({
    id: s.id,
    type: "step",
    position: { x: s.position_x, y: s.position_y },
    data: {
      kind: kindStrToNum[s.kind] ?? 1,
      name: s.name,
      ref: s.ref,
      workerVersion: s.worker_version,
      gatePolicyRef: s.gate_policy_ref,
      config: s.config,
    } as StepData,
  }));
  const edges: Edge[] = [];
  for (const s of steps) {
    for (const dep of s.depends_on ?? []) {
      // dep is a dependency of s → edge dep→s.
      edges.push({
        id: `e-${dep}-${s.id}`,
        source: dep,
        target: s.id,
        markerEnd: { type: MarkerType.ArrowClosed },
      });
    }
  }
  return { nodes, edges };
}

// --- small badges ---

function VersionStatusBadge({ status }: { status: number }) {
  const labels: Record<number, string> = {
    1: "draft",
    2: "published",
    3: "deprecated",
  };
  const styles: Record<number, string> = {
    1: "bg-blue-100 text-blue-800",
    2: "bg-green-100 text-green-800",
    3: "bg-yellow-100 text-yellow-800",
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

function RunStatusBadge({ status }: { status: number }) {
  const labels: Record<number, string> = {
    1: "pending",
    2: "running",
    3: "completed",
    4: "failed",
    5: "aborted",
    6: "paused",
  };
  const styles: Record<number, string> = {
    1: "bg-gray-200 text-gray-700",
    2: "bg-blue-100 text-blue-800",
    3: "bg-green-100 text-green-800",
    4: "bg-red-100 text-red-800",
    5: "bg-gray-300 text-gray-700",
    6: "bg-yellow-100 text-yellow-800",
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

const WORKFLOW_STATUS_LABELS: Record<number, string> = {
  1: "draft",
  2: "published",
  3: "deprecated",
};
