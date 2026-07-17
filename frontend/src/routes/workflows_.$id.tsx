import { createRoute, useNavigate } from "@tanstack/react-router";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
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
  useAbortWorkflow,
  useAcquireWorkflowEditLock,
  useCreateWorkflowVersion,
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
import { DeletableEdge } from "@/components/workflow-editor/DeletableEdge";
import { canvasToSteps, stepsToCanvas } from "@/components/workflow-editor/canvas";
import {
  ACCENT_STROKE,
  KIND_ACCENT,
  PALETTE_MIME,
  STEP_KIND,
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
const EDGE_TYPES = { default: DeletableEdge };

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
  const abortWorkflow = useAbortWorkflow();
  const deleteMutation = useDeleteWorkflow();
  const createVersion = useCreateWorkflowVersion();

  const [nodes, setNodes, onNodesChange] = useNodesState(
    [] as Node<StepData>[],
  );
  const [edges, setEdges, onEdgesChange] = useEdgesState([] as Edge[]);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [dirty, setDirty] = useState(false);
  const [validationErrors, setValidationErrors] = useState<string[]>([]);
  const [dropActive, setDropActive] = useState(false);

  // Resolve project from canvas PROJECT connector nodes. Must be before
  // the early returns (hooks cannot be conditional).
  const resolvedProjectId = useMemo(() => {
    for (const n of nodes) {
      if (n.data.kind === STEP_KIND.PROJECT) {
        const cfg = parseConfig(n.data.config);
        if (typeof cfg.project_id === "string" && cfg.project_id) {
          return cfg.project_id;
        }
      }
    }
    return "";
  }, [nodes]);

  // PR D: listen for delete events from StepNode's hover-× button.
  // The node dispatches a CustomEvent on window; we remove the node
  // + its connected edges and push history. Equivalent to pressing
  // Del/Backspace on a selected node, but discoverable.
  useEffect(() => {
    const onDeleteNode = (e: Event) => {
      const detail = (e as CustomEvent<{ id: string }>).detail;
      if (!detail?.id) return;
      const removed = detail.id;
      setNodes((nds) => nds.filter((n) => n.id !== removed));
      setEdges((eds) =>
        eds.filter((ed) => ed.source !== removed && ed.target !== removed),
      );
      setSelectedId(null);
      setDirty(true);
    };
    const onDeleteEdge = (e: Event) => {
      const detail = (e as CustomEvent<{ id: string }>).detail;
      if (!detail?.id) return;
      setEdges((eds) => eds.filter((ed) => ed.id !== detail.id));
      setDirty(true);
    };
    window.addEventListener("orchicon:delete-node", onDeleteNode as EventListener);
    window.addEventListener("orchicon:delete-edge", onDeleteEdge as EventListener);
    return () => {
      window.removeEventListener("orchicon:delete-node", onDeleteNode as EventListener);
      window.removeEventListener("orchicon:delete-edge", onDeleteEdge as EventListener);
    };
  }, []);

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
  const latestIsDraft = latestVersion?.status === 1; // WorkflowVersionStatus.DRAFT
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

  // --- version restore: load an older version's steps into the canvas ---
  const restoreVersion = useCallback(
    (v: { id: string; steps: string; version: number }) => {
      if (loadedRef.current === v.id) return; // already on canvas
      if (!window.confirm(`Load v${v.version} into the editor? Any unsaved changes will be lost.`)) return;
      const { nodes: n, edges: e } = stepsToCanvas(v.steps);
      setNodes(n);
      setEdges(e);
      loadedRef.current = v.id;
      history.current = [{ nodes: n, edges: e }];
      histPtr.current = 0;
      setDirty(true);
    },
    [setNodes, setEdges],
  );

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
      // Color the edge by the source node's step kind for visual flow.
      const srcNode = nodes.find((n) => n.id === conn.source);
      const srcKind = srcNode?.data.kind ?? 1;
      const accent = KIND_ACCENT[srcKind] ?? "sky";
      const edgeStyle = { stroke: `var(--kind-${accent})` };
      const edgeClass = ACCENT_STROKE[accent] ?? "";
      setEdges((eds) =>
        addEdge(
          {
            ...conn,
            id: `e-${conn.source}-${conn.target}`,
            markerEnd: { type: MarkerType.ArrowClosed },
            animated: true,
            style: edgeStyle,
            className: edgeClass,
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
              sourceHandle: conn.sourceHandle,
              targetHandle: conn.targetHandle,
              markerEnd: { type: MarkerType.ArrowClosed },
              animated: true,
              style: edgeStyle,
              className: edgeClass,
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
      const { kind, name } = parsed;
      const raw = rf.screenToFlowPosition({
        x: event.clientX,
        y: event.clientY,
      });
      const position = {
        x: Number.isFinite(raw?.x) ? raw.x : 100,
        y: Number.isFinite(raw?.y) ? raw.y : 100,
      };
      const id = `step-${Math.random().toString(36).slice(2, 10)}`;
      const initialConfig =
        kind === STEP_KIND.RECOVER
          ? JSON.stringify({ strategy: "summarize_restart" })
          : "{}";
      const data: StepData = {
        kind,
        name: name ?? `step-${id.slice(5, 9)}`,
        ref: "",
        workerVersion: 0,
        gatePolicyRef: "",
        config: initialConfig,
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

  // --- validation ---
  // Per-kind required bindings:
  //   task (1)      → worker ref
  //   work_item (6) → config.work_item_id
  //   project (7)   → config.project_id
  //   policy (8)    → gatePolicyRef
  //   recover (5)   → config.strategy
  //   decision/approval/parallel → no required binding
  const validate = useCallback((): string[] => {
    const errs: string[] = [];
    for (const n of nodes) {
      const d = n.data;
      const cfg = parseConfig(d.config);
      if (d.kind === STEP_KIND.TASK && !d.ref) {
        errs.push(
          `Step "${d.name || n.id}" is a worker but has no Worker selected.`,
        );
      }
      if (d.kind === STEP_KIND.WORK_ITEM && !cfg.work_item_id) {
        errs.push(
          `Step "${d.name || n.id}" is a work item but has no work item selected.`,
        );
      }
      if (d.kind === STEP_KIND.PROJECT && !cfg.project_id) {
        errs.push(
          `Step "${d.name || n.id}" is a project but has no project selected.`,
        );
      }
      if (d.kind === STEP_KIND.POLICY && !d.gatePolicyRef) {
        errs.push(
          `Step "${d.name || n.id}" is a policy but has no policy selected.`,
        );
      }
      if (d.kind === STEP_KIND.RECOVER && !cfg.strategy) {
        errs.push(
          `Step "${d.name || n.id}" is a recovery step but has no strategy selected.`,
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
    const run = await startWorkflow.mutateAsync({
      workflowId,
      projectId: effectiveProjectId,
      runContext: "{}",
    });
    navigate({
      to: "/workflows/$id/runs/$runId",
      params: { id: workflowId, runId: run.id },
    });
  };

  // PR D: inline Stop / Abort for the most recent running or pending
  // run. The latest run is whichever row has the highest
  // (createdAt) — runs are listed newest-first by the API.
  const latestRun = (runs ?? [])[0];
  const latestRunActive = latestRun
    ? latestRun.status === 1 /* pending */ || latestRun.status === 2 /* running */
    : false;
  const handleStop = async () => {
    if (!latestRun) return;
    if (
      !window.confirm(
        `Stop run ${latestRun.id.slice(0, 12)}…? Aborting halts all queued and running steps; the run is marked aborted (terminal).`,
      )
    ) {
      return;
    }
    await abortWorkflow.mutateAsync({ runId: latestRun.id, reason: "user clicked Stop" });
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
  const isPublished = wf.status === 2;
  const isDeprecated = wf.status === 3;
  // Use the resolved project from canvas nodes if available, falling
  // back to the workflow-level projectId for compatibility with
  // existing workflows that set it at creation time.
  const effectiveProjectId = resolvedProjectId || wf.projectId;

  return (
    <TooltipProvider delayDuration={250}>
      <div className="flex flex-col gap-4">
        {/* header + actions */}
        <div className="flex items-start justify-between">
          <div>
            <h1 className="text-2xl font-semibold tracking-tight">{wf.name}</h1>
            <p className="text-xs text-muted-foreground">
              {effectiveProjectId ? `project: ${effectiveProjectId.slice(0, 12)}…` : "tenant template"} ·
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
              disabled={readOnly || !latestIsDraft || !dirty || updateVersion.isPending}
            >
              {updateVersion.isPending ? "Saving…" : "Save draft"}
            </Button>
            {latestIsDraft && (
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
            {(isPublished || isDeprecated) && (
              <Button
                variant="outline"
                onClick={async () => {
                  const note = window.prompt("Version note (optional):");
                  if (note === null) return; // cancelled
                  await createVersion.mutateAsync({ workflowId, versionNote: note });
                }}
                disabled={createVersion.isPending}
              >
                {createVersion.isPending ? "Creating…" : "New version"}
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
            {/* PR D: inline Stop for the most recent active run. The
                button is destructive (Stop halts steps + marks the run
                aborted) so it's a red outline. Disabled when the run
                has already terminal'd. */}
            {latestRunActive && latestRun && (
              <Button
                variant="destructive"
                onClick={handleStop}
                disabled={abortWorkflow.isPending}
                title={`Stop run ${latestRun.id.slice(0, 12)}…`}
              >
                {abortWorkflow.isPending ? "Stopping…" : `Stop ${latestRun.id.slice(0, 12)}…`}
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
          <Palette readOnly={readOnly} />

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
              edgeTypes={EDGE_TYPES}
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
            projectId={effectiveProjectId}
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
                    <button
                      type="button"
                      className="ml-auto rounded px-1.5 py-0.5 text-[11px] font-medium text-muted-foreground hover:bg-accent hover:text-accent-foreground"
                      title="Load this version into the editor"
                      onClick={() => restoreVersion(v)}
                    >
                      Restore
                    </button>
                  </div>
                ))}
              </div>
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <CardTitle>Runs</CardTitle>
              <CardDescription>
                Recent runs. Click to view live step transitions and executions.
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

// parseConfig defensively reads a step's config JSON. Returns {} for
// empty / malformed input. Used by the validator and the properties
// panel to extract work_item_id / project_id without re-parsing JSON.
function parseConfig(config: string): Record<string, unknown> {
  if (!config) return {};
  try {
    const parsed = JSON.parse(config);
    if (parsed && typeof parsed === "object") return parsed as Record<string, unknown>;
  } catch {
    /* fall through */
  }
  return {};
}
