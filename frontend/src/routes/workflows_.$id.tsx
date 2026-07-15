import { createRoute, useNavigate } from "@tanstack/react-router";
import { useCallback, useEffect, useRef, useState } from "react";
import ReactFlow, {
  Background,
  Controls,
  MarkerType,
  MiniMap,
  ReactFlowProvider,
  addEdge,
  useEdgesState,
  useNodesState,
  useReactFlow,
  type Connection,
  type Edge,
  type Node,
  type NodeChange,
  type EdgeChange,
} from "reactflow";

import {
  useAcquireWorkflowEditLock,
  useDeleteWorkflow,
  useDeprecateWorkflow,
  useGetWorkflow,
  useGetWorkflowEditLock,
  useListWorkflowRuns,
  useListWorkflowVersions,
  usePublishWorkflow,
  useReleaseWorkflowEditLock,
  useStartWorkflow,
  useUpdateWorkflowVersion,
} from "@/api/workflows";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { TooltipProvider } from "@/components/ui/tooltip";
import {
  EditLockBanner,
  RunStatusBadge,
  VersionStatusBadge,
} from "@/components/workflow-editor/EditLockBanner";
import { Palette } from "@/components/workflow-editor/Palette";
import { PropertiesPanel } from "@/components/workflow-editor/PropertiesPanel";
import { StepNode } from "@/components/workflow-editor/StepNode";
import { canvasToSteps, stepsToCanvas } from "@/components/workflow-editor/canvas";
import {
  PALETTE_MIME,
  type PaletteDropPayload,
  type StepData,
} from "@/components/workflow-editor/stepKinds";
import { cn } from "@/lib/utils";
import { Route as rootRoute } from "@/routes/__root";

import "reactflow/dist/style.css";

// Workflow visual editor (docs/10 §5, §5.1, §11: "full visual drag-and-drop
// editor in v0.1"). A React Flow canvas where users drag Workers,
// WorkItems, Policies, and step primitives onto the canvas, wire steps
// together visually, and edit properties inline.
//
// Drag-and-drop (docs/10 §5.1):
//   - Palette tiles use `application/x-orchicon-workflow-step` as the
//     dataTransfer mime key with effectAllowed=copyMove so the browser
//     accepts the drop on any dropEffect the canvas sets.
//   - The drop handler lives on a wrapper div containing ReactFlow
//     because ReactFlow 11.11.x has no native onDrop prop. The wrapper
//     has explicit dragenter/dragover/dragleave handlers so a visible
//     drop-zone highlight engages while dragging.
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

const NODE_TYPES = { step: StepNode };

function EditorInner({ workflowId }: { workflowId: string }) {
  const navigate = useNavigate();
  const { data, isLoading, error } = useGetWorkflow(workflowId);
  const { data: versions } = useListWorkflowVersions(workflowId);
  const { data: runs } = useListWorkflowRuns(workflowId);
  const { data: editLock } = useGetWorkflowEditLock(workflowId);
  const acquireLock = useAcquireWorkflowEditLock();
  const releaseLock = useReleaseWorkflowEditLock();
  const updateVersion = useUpdateWorkflowVersion();
  const publishWorkflow = usePublishWorkflow();
  const deprecateWorkflow = useDeprecateWorkflow();
  const startWorkflow = useStartWorkflow();
  const deleteMutation = useDeleteWorkflow();

  const [nodes, setNodes, onNodesChange] = useNodesState(
    [] as Node<StepData>[],
  );
  const [edges, setEdges, onEdgesChange] = useEdgesState([] as Edge[]);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [dirty, setDirty] = useState(false);
  const [validationErrors, setValidationErrors] = useState<string[]>([]);
  const [dropActive, setDropActive] = useState(false);

  // --- undo/redo history ---
  const history = useRef<{ nodes: Node<StepData>[]; edges: Edge[] }[]>([]);
  const histPtr = useRef(-1);
  const pushHistory = useCallback(
    (n: Node<StepData>[], e: Edge[]) => {
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
      if (changes.some((c) => c.type === "remove")) {
        const removed = changes
          .filter((c) => c.type === "remove")
          .map((c) => (c as { id: string }).id);
        setEdges((prev) =>
          prev.filter(
            (ed) => !removed.includes(ed.source) && !removed.includes(ed.target),
          ),
        );
        if (selectedId && removed.includes(selectedId)) setSelectedId(null);
        pushHistory(
          nodes.filter((n) => !removed.includes(n.id)),
          edges.filter(
            (ed) => !removed.includes(ed.source) && !removed.includes(ed.target),
          ),
        );
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
        pushHistory(
          nodes,
          edges.filter(
            (ed) => !changes.some((c) => c.type === "remove" && c.id === ed.id),
          ),
        );
      }
    },
    [onNodesChange, pushHistory, nodes, edges],
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
      setTimeout(
        () =>
          pushHistory(nodes, [
            ...edges,
            {
              id: `e-${conn.source}-${conn.target}`,
              source: conn.source!,
              target: conn.target!,
              markerEnd: { type: MarkerType.ArrowClosed },
            },
          ]),
        0,
      );
    },
    [setEdges, pushHistory, nodes, edges],
  );

  // --- drag-and-drop from palette ---
  const rf = useReactFlow();
  const dragCounter = useRef(0);

  const onDragEnter = useCallback((e: React.DragEvent) => {
    // dragenter/leave fire for every child element entered; the counter
    // is the standard pattern to detect a stable "over the drop zone"
    // state without flicker.
    if (e.dataTransfer.types.includes(PALETTE_MIME)) {
      dragCounter.current += 1;
      e.preventDefault();
      setDropActive(true);
    }
  }, []);
  const onDragLeave = useCallback((e: React.DragEvent) => {
    if (e.dataTransfer.types.includes(PALETTE_MIME)) {
      dragCounter.current = Math.max(0, dragCounter.current - 1);
      if (dragCounter.current === 0) setDropActive(false);
    }
  }, []);
  const onDragOver = useCallback((e: React.DragEvent) => {
    if (e.dataTransfer.types.includes(PALETTE_MIME)) {
      e.preventDefault();
      e.dataTransfer.dropEffect = "copy";
    }
  }, []);
  const onDrop = useCallback(
    (event: React.DragEvent) => {
      event.preventDefault();
      dragCounter.current = 0;
      setDropActive(false);
      const payload = event.dataTransfer.getData(PALETTE_MIME);
      if (!payload) return;
      let parsed: PaletteDropPayload;
      try {
        parsed = JSON.parse(payload) as PaletteDropPayload;
      } catch {
        return;
      }
      const { kind, name, ref, workerId, workItemId, policyId } = parsed;
      // screenToFlowPosition falls back to clientX/clientY if the
      // viewport is not yet initialized (the function returns the input
      // unchanged when its internal domNode ref is null). Guard the
      // values anyway so we never place a node at NaN.
      const raw = rf.screenToFlowPosition({
        x: event.clientX,
        y: event.clientY,
      });
      const position = {
        x: Number.isFinite(raw?.x) ? raw.x : 100,
        y: Number.isFinite(raw?.y) ? raw.y : 100,
      };
      const id = `step-${Math.random().toString(36).slice(2, 10)}`;
      const config = workItemId
        ? JSON.stringify({
            work_item_id: workItemId,
            // Pre-populate the step with the work item's metadata so the
            // properties panel has useful defaults even before the next
            // server round-trip.
            work_item_title: name,
          })
        : "{}";
      const data: StepData = {
        kind,
        name: name ?? `step-${id.slice(5, 9)}`,
        ref: ref ?? "",
        workerVersion: workerId ? 0 : 0,
        gatePolicyRef: policyId ?? "",
        config,
      };
      const node: Node<StepData> = {
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
    for (const n of nodes) {
      const d = n.data;
      if (d.kind === 1 && !d.ref && !/work_item_id/.test(d.config || "")) {
        errs.push(
          `Step "${d.name || n.id}" is a task but has no Worker or work item reference.`,
        );
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
      if (!window.confirm("You have unsaved changes. Save and publish?")) return;
      await handleSave();
    }
    await publishWorkflow.mutateAsync(workflowId);
  };

  const handleStart = async () => {
    if (!data?.workflow) return;
    const projectId = data.workflow.projectId;
    if (!projectId) {
      window.alert(
        "This workflow is a tenant template. Start it from a project context, or assign a project.",
      );
      return;
    }
    const run = await startWorkflow.mutateAsync({
      workflowId,
      projectId,
      runContext: "{}",
    });
    navigate({
      to: "/workflows/$id/runs/$runId",
      params: { id: workflowId, runId: run.id },
    });
  };

  // keyboard shortcuts: undo/redo + delete selected
  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      const target = e.target as HTMLElement | null;
      const inField =
        target &&
        (target.tagName === "INPUT" ||
          target.tagName === "TEXTAREA" ||
          target.isContentEditable);
      if (inField) return;
      if ((e.metaKey || e.ctrlKey) && e.key === "z" && !e.shiftKey) {
        e.preventDefault();
        undo();
      } else if (
        (e.metaKey || e.ctrlKey) &&
        (e.key === "y" || (e.key === "z" && e.shiftKey))
      ) {
        e.preventDefault();
        redo();
      } else if ((e.key === "Delete" || e.key === "Backspace") && selectedId) {
        e.preventDefault();
        setNodes((nds) => nds.filter((n) => n.id !== selectedId));
        setEdges((eds) =>
          eds.filter((ed) => ed.source !== selectedId && ed.target !== selectedId),
        );
        setSelectedId(null);
        setDirty(true);
      }
    };
    window.addEventListener("keydown", handler);
    return () => window.removeEventListener("keydown", handler);
  }, [undo, redo, selectedId, setNodes, setEdges]);

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
  const projectId = wf.projectId;

  return (
    <TooltipProvider delayDuration={250}>
      <div className="flex flex-col gap-4">
        {/* header + actions */}
        <div className="flex items-start justify-between">
          <div>
            <h1 className="text-2xl font-semibold tracking-tight">{wf.name}</h1>
            <p className="text-xs text-muted-foreground">
              {projectId ? `project: ${projectId.slice(0, 12)}…` : "tenant template"} ·
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
                disabled={
                  readOnly || publishWorkflow.isPending || validationErrors.length > 0
                }
                title={
                  validationErrors.length > 0
                    ? "Resolve validation errors first"
                    : "Publish (immutable)"
                }
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
            <Button
              variant="destructive"
              onClick={() => {
                if (
                  window.confirm(
                    "Permanently delete this workflow and all its versions and runs? This cannot be undone.",
                  )
                ) {
                  deleteMutation.mutate(workflowId, {
                    onSuccess: () => navigate({ to: "/workflows" }),
                  });
                }
              }}
              disabled={deleteMutation.isPending}
            >
              {deleteMutation.isPending ? "Deleting…" : "Delete"}
            </Button>
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
          <div className="rounded-md border border-amber-300 bg-amber-50 p-3 text-sm text-amber-800 dark:border-amber-800 dark:bg-amber-950/40 dark:text-amber-200">
            <p className="font-medium">Validation:</p>
            <ul className="ml-4 list-disc">
              {validationErrors.map((e, i) => (
                <li key={i}>{e}</li>
              ))}
            </ul>
          </div>
        )}

        {/* main editor layout: palette | canvas | properties */}
        <div className="grid grid-cols-1 gap-4 lg:grid-cols-[240px_1fr_300px]">
          <Palette projectId={projectId} readOnly={readOnly} />

          {/* canvas */}
          <div
            className={cn(
              "relative h-[640px] rounded-lg border bg-card transition-colors",
              dropActive &&
                "border-primary bg-primary/5 ring-2 ring-primary/30 ring-offset-1",
            )}
            onDragEnter={onDragEnter}
            onDragOver={onDragOver}
            onDragLeave={onDragLeave}
            onDrop={onDrop}
          >
            <ReactFlow
              nodes={nodes}
              edges={edges}
              onNodesChange={handleNodesChange}
              onEdgesChange={handleEdgesChange}
              onConnect={onConnect}
              onNodeClick={(_, n) => setSelectedId(n.id)}
              onPaneClick={() => setSelectedId(null)}
              nodeTypes={NODE_TYPES}
              fitView
              minZoom={0.2}
              maxZoom={2}
              nodesConnectable={!readOnly}
              nodesDraggable={!readOnly}
              elementsSelectable
              proOptions={{ hideAttribution: true }}
            >
              <Background gap={20} size={1} />
              <Controls showInteractive={!readOnly} />
              <MiniMap
                pannable
                zoomable
                className="!bg-background/80 !border-border"
              />
            </ReactFlow>
            {dropActive && (
              <div
                className="pointer-events-none absolute inset-0 z-10 flex items-center justify-center"
                aria-hidden
              >
                <div className="rounded-md border-2 border-dashed border-primary bg-primary/10 px-6 py-3 text-sm font-medium text-primary shadow-sm">
                  Drop to add step
                </div>
              </div>
            )}
            {nodes.length === 0 && !dropActive && (
              <div className="pointer-events-none absolute inset-0 flex items-center justify-center text-sm text-muted-foreground">
                Drag a tile from the palette to begin.
              </div>
            )}
          </div>

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
    </TooltipProvider>
  );
}

const WORKFLOW_STATUS_LABELS: Record<number, string> = {
  1: "draft",
  2: "published",
  3: "deprecated",
};
