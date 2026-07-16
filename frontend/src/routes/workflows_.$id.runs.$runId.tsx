import { createRoute, useNavigate } from "@tanstack/react-router";
import { useMemo } from "react";
import ReactFlow, {
  Background,
  Controls,
  Handle,
  MarkerType,
  MiniMap,
  Position,
  ReactFlowProvider,
  type Edge,
  type Node,
} from "reactflow";
import { useQueryClient } from "@tanstack/react-query";

import {
  useAbortWorkflow,
  useGetWorkflow,
  useGetWorkflowRun,
  useGetWorkflowStepRuns,
} from "@/api/workflows";
import { useListExecutions } from "@/api/executions";
import { useStreamWorkflowEvents } from "@/api/workflowEvents";
import { workflowKeys } from "@/api/workflows";
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

import "reactflow/dist/style.css";

// Workflow run view (docs/10 §4.1: "Run view overlays live step
// transitions on the same canvas"). Streams workflow events over NATS
// and overlays the step-run status on the editor canvas. A live event
// feed shows step transitions in real-time.
export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/workflows/$id/runs/$runId",
  component: WorkflowRunPage,
});

function WorkflowRunPage() {
  const { id, runId } = Route.useParams();
  return (
    <ReactFlowProvider>
      <RunViewInner workflowId={id} runId={runId} />
    </ReactFlowProvider>
  );
}

const STEP_KIND_LABELS: Record<number, string> = {
  1: "task",
  2: "decision",
  3: "approval",
  4: "parallel",
  5: "recover",
};

const STEP_KIND_COLORS: Record<number, string> = {
  1: "border-blue-400",
  2: "border-amber-400",
  3: "border-yellow-500",
  4: "border-purple-400",
  5: "border-red-400",
};

const STEP_RUN_STATUS_COLORS: Record<number, string> = {
  1: "bg-gray-200 text-gray-700", // pending
  2: "bg-yellow-100 text-yellow-800", // ready
  3: "bg-blue-100 text-blue-800", // running
  4: "bg-green-100 text-green-800", // succeeded
  5: "bg-red-100 text-red-800", // failed
  6: "bg-gray-300 text-gray-600", // skipped
  7: "bg-red-200 text-red-900", // blocked
  8: "bg-amber-100 text-amber-900", // approval_pending
};

function RunViewInner({ workflowId, runId }: { workflowId: string; runId: string }) {
  const navigate = useNavigate();
  const qc = useQueryClient();
  const { data: wfData } = useGetWorkflow(workflowId);
  const { data: run, isLoading, error } = useGetWorkflowRun(runId);
  const { data: stepRuns } = useGetWorkflowStepRuns(runId);
  const { data: runExecs } = useListExecutions({ workflowRunId: runId });
  const abortRun = useAbortWorkflow();

  // Live event stream (docs/10 §4). Subscribes to StreamWorkflowEvents
  // filtered to this run; invalidates the run + step-runs queries so the
  // canvas and feed refresh on each transition.
  const { events, status } = useStreamWorkflowEvents({
    workflowRunId: runId,
    onEvent: () => {
      qc.invalidateQueries({ queryKey: workflowKeys.run(runId) });
      qc.invalidateQueries({ queryKey: workflowKeys.stepRuns(runId) });
    },
  });

  // Build the canvas from the published version's steps, overlaying the
  // step-run status on each node.
  const { nodes, edges } = useMemo(() => {
    const stepsJson = wfData?.latestVersion?.steps ?? "[]";
    let steps: {
      id: string;
      name: string;
      kind: string;
      ref: string;
      depends_on: string[];
      position_x: number;
      position_y: number;
    }[] = [];
    try {
      steps = JSON.parse(stepsJson);
    } catch {
      steps = [];
    }
    const kindStrToNum: Record<string, number> = {
      task: 1, decision: 2, approval: 3, parallel: 4, recover: 5,
    };
    const statusByStep = new Map<string, number>();
    for (const sr of stepRuns ?? []) {
      statusByStep.set(sr.stepId, sr.status);
    }
    const nodes: Node[] = steps.map((s) => {
      const runStatus = statusByStep.get(s.id) ?? 1; // pending default
      return {
        id: s.id,
        type: "runStep",
        position: { x: s.position_x, y: s.position_y },
        data: {
          kind: kindStrToNum[s.kind] ?? 1,
          name: s.name,
          ref: s.ref,
          runStatus,
        },
      };
    });
    const edges: Edge[] = [];
    for (const s of steps) {
      for (const dep of s.depends_on ?? []) {
        edges.push({
          id: `e-${dep}-${s.id}`,
          source: dep,
          target: s.id,
          markerEnd: { type: MarkerType.ArrowClosed },
          animated: statusByStep.get(s.id) === 3, // animate running steps
        });
      }
    }
    return { nodes, edges };
  }, [wfData?.latestVersion?.steps, stepRuns]);

  if (isLoading) {
    return <p className="text-sm text-muted-foreground">Loading run…</p>;
  }
  if (error) {
    return (
      <p className="text-sm text-destructive">
        Failed to load run: {String(error)}
      </p>
    );
  }
  if (!run) return null;

  const isTerminal = run.status === 3 || run.status === 4 || run.status === 5;

  return (
    <div className="space-y-4">
      <div className="flex items-start justify-between">
        <div>
          <button
            className="text-xs text-muted-foreground hover:text-foreground"
            onClick={() => navigate({ to: "/workflows/$id", params: { id: workflowId } })}
          >
            ← back to editor
          </button>
          <h1 className="text-2xl font-semibold tracking-tight">Workflow Run</h1>
          <p className="font-mono text-xs text-muted-foreground">{run.id}</p>
          <p className="text-xs text-muted-foreground">
            workflow v{run.workflowVersion} · status:{" "}
            <RunStatusBadge status={run.status} />
            {run.currentStep && (
              <> · current step: <span className="font-mono">{run.currentStep}</span></>
            )}
          </p>
        </div>
        <div className="flex gap-2">
          {!isTerminal && (
            <Button
              variant="destructive"
              onClick={() => abortRun.mutateAsync({ runId })}
              disabled={abortRun.isPending}
            >
              {abortRun.isPending ? "Aborting…" : "Abort"}
            </Button>
          )}
        </div>
      </div>

      {/* run canvas with live step transitions */}
      <div className="h-[600px] rounded-lg border bg-card">
        <ReactFlow
          nodes={nodes}
          edges={edges}
          nodeTypes={{ runStep: RunStepNode }}
          fitView
          minZoom={0.2}
          maxZoom={2}
          nodesDraggable={false}
          nodesConnectable={false}
        >
          <Background />
          <Controls showInteractive={false} />
          <MiniMap />
        </ReactFlow>
      </div>

      {/* live event feed */}
      <Card>
        <CardHeader>
          <div className="flex items-center justify-between">
            <div>
              <CardTitle>Live Step Transitions</CardTitle>
              <CardDescription>
                Real-time workflow events streamed via NATS.
              </CardDescription>
            </div>
            <StreamStatusBadge status={status} />
          </div>
        </CardHeader>
        <CardContent>
          {events.length === 0 && (stepRuns ?? []).length === 0 ? (
            <p className="text-sm text-muted-foreground">
              No events yet. Waiting for the reconciler to progress the DAG…
            </p>
          ) : (
            <div className="max-h-96 space-y-2 overflow-auto">
              {/* Render recent streamed events; also seed with the current
                  step-run snapshot when the stream is empty (e.g. on a
                  reconnect). */}
              {events.length === 0 &&
                (stepRuns ?? []).map((sr) => (
                  <div
                    key={sr.id}
                    className="flex items-start gap-3 rounded-md border p-3 text-sm"
                  >
                    <StepStatusPill status={sr.status} />
                    <div className="flex-1">
                      <span className="font-medium">{sr.stepName || sr.stepId}</span>
                      <span className="ml-2 text-xs text-muted-foreground">
                        {STEP_KIND_LABELS[sr.stepKind] ?? "step"}
                      </span>
                    </div>
                  </div>
                ))}
              {events.map((resp, i) => {
                const evt = resp.event;
                if (!evt) return null;
                return (
                  <div
                    key={`${evt.eventType}-${resp.sequence}-${i}`}
                    className="flex items-start gap-3 rounded-md border p-3 text-sm"
                  >
                    <EventDot eventType={evt.eventType} />
                    <div className="flex-1">
                      <span className="font-medium">{evt.eventType.replace("workflow.", "")}</span>
                      {evt.stepId && (
                        <span className="ml-2 font-mono text-xs text-muted-foreground">
                          step: {evt.stepId}
                        </span>
                      )}
                      {evt.payload && evt.payload.length > 0 && (
                        <pre className="mt-1 max-h-32 overflow-auto rounded bg-muted p-2 text-xs text-muted-foreground">
                          {formatPayload(evt.payload)}
                        </pre>
                      )}
                    </div>
                    <span className="text-xs text-muted-foreground">
                      #{String(resp.sequence)}
                    </span>
                  </div>
                );
              })}
            </div>
          )}
        </CardContent>
      </Card>

      {/* edit lock indicator (docs/07 §3.3): the run view is read-only,
          but show whether an edit lock is held so the user knows the
          editor state. */}
      <Card>
        <CardHeader>
          <CardTitle className="text-base">Step runs</CardTitle>
          <CardDescription>
            Current snapshot of step-run status (refreshes on each event).
          </CardDescription>
        </CardHeader>
        <CardContent>
          {(stepRuns ?? []).length === 0 ? (
            <p className="text-sm text-muted-foreground">No step runs yet.</p>
          ) : (
            <div className="space-y-2">
              {(stepRuns ?? []).map((sr) => (
                <div
                  key={sr.id}
                  className="flex items-center gap-3 rounded-md border p-2 text-sm"
                >
                  <StepStatusPill status={sr.status} />
                  <span className="font-medium">{sr.stepName || sr.stepId}</span>
                  <span className="text-xs text-muted-foreground">
                    {STEP_KIND_LABELS[sr.stepKind] ?? "step"}
                  </span>
                  {sr.workerExecutionId && (
                    <span className="font-mono text-xs text-muted-foreground">
                      exec: {sr.workerExecutionId.slice(0, 12)}…
                    </span>
                  )}
                </div>
              ))}
            </div>
          )}
        </CardContent>
      </Card>

      {/* associated worker executions + pending step runs */}
      <Card>
        <CardHeader>
          <CardTitle className="text-base">Executions</CardTitle>
          <CardDescription>
            Worker executions spawned by this run. Step runs pending dispatch
            appear immediately; executor links appear once the reconciler
            creates them (auto-refreshes).
          </CardDescription>
        </CardHeader>
        <CardContent>
          <div className="space-y-2">
            {/* Pending step runs (no workerExecutionId yet) */}
            {(stepRuns ?? [])
              .filter((sr) => !sr.workerExecutionId)
              .map((sr) => (
                <div key={sr.id} className="flex items-center gap-3 rounded-md border p-2 text-sm text-muted-foreground">
                  <StepStatusPill status={sr.status} />
                  <span className="font-medium">{sr.stepName || sr.stepId.slice(0, 12)}</span>
                  <span className="text-xs text-muted-foreground/60">waiting for dispatch…</span>
                </div>
              ))}
            {/* Actual WorkerExecutions */}
            {(runExecs ?? []).length === 0 && (stepRuns ?? []).filter((sr) => !sr.workerExecutionId).length === 0 && (
              <p className="text-sm text-muted-foreground">No executions yet.</p>
            )}
            {(runExecs ?? []).map((ex) => (
              <button
                key={ex.id}
                className="flex w-full items-center gap-3 rounded-md border p-2 text-left text-sm hover:bg-accent"
                onClick={() =>
                  navigate({
                    to: "/executions/$id",
                    params: { id: ex.id },
                  })
                }
              >
                <ExecStatusBadge status={ex.status} />
                <span className="font-medium min-w-0 truncate">{ex.workflowName || ex.workerId}</span>
                <span className="font-mono text-xs text-muted-foreground shrink-0">{ex.id.slice(0, 12)}…</span>
                {ex.startedAt && (
                  <span className="ml-auto text-xs text-muted-foreground shrink-0">
                    {new Date(Number(ex.startedAt.seconds) * 1000).toLocaleTimeString()}
                  </span>
                )}
              </button>
            ))}
          </div>
        </CardContent>
      </Card>
    </div>
  );
}

// --- Run step node (overlays step-run status on the canvas) ---
function RunStepNode({ data }: { data: { kind: number; name: string; ref: string; runStatus: number } }) {
  const statusColor = STEP_RUN_STATUS_COLORS[data.runStatus] ?? "bg-gray-200";
  return (
    <div
      className={cn(
        "min-w-[140px] rounded-md border px-3 py-2 text-center shadow-sm",
        STEP_KIND_COLORS[data.kind] ?? "border-gray-300",
      )}
    >
      <Handle type="target" position={Position.Left} />
      <div className="text-[10px] font-medium uppercase text-muted-foreground">
        {STEP_KIND_LABELS[data.kind] ?? "step"}
      </div>
      <div className="truncate text-sm font-semibold">{data.name}</div>
      <div className={cn("mt-1 inline-block rounded-full px-2 py-0.5 text-[10px] font-medium", statusColor)}>
        {RUN_STATUS_LABELS[data.runStatus] ?? "pending"}
      </div>
      <Handle type="source" position={Position.Right} />
    </div>
  );
}

// Handle is imported from reactflow; referenced by RunStepNode to render
// source/target handles on the canvas nodes.

// --- badges + helpers ---

function RunStatusBadge({ status }: { status: number }) {
  return (
    <span
      className={cn(
        "rounded-full px-2 py-0.5 text-xs font-medium",
        RUN_STATUS_STYLES[status] ?? "bg-muted text-muted-foreground",
      )}
    >
      {RUN_STATUS_LABELS[status] ?? "unknown"}
    </span>
  );
}

function StepStatusPill({ status }: { status: number }) {
  return (
    <span
      className={cn(
        "rounded-full px-2 py-0.5 text-xs font-medium",
        STEP_RUN_STATUS_COLORS[status] ?? "bg-gray-200 text-gray-700",
      )}
    >
      {STEP_RUN_STATUS_LABELS[status] ?? "pending"}
    </span>
  );
}

function StreamStatusBadge({ status }: { status: string }) {
  const colors: Record<string, string> = {
    idle: "text-muted-foreground",
    connecting: "text-yellow-600",
    open: "text-green-600",
    reconnecting: "text-yellow-600",
    closed: "text-muted-foreground",
    error: "text-destructive",
  };
  const labels: Record<string, string> = {
    idle: "idle",
    connecting: "connecting…",
    open: "live",
    reconnecting: "reconnecting…",
    closed: "disconnected",
    error: "error",
  };
  return (
    <span className={cn("text-xs font-medium", colors[status] ?? "")}>
      ● {labels[status] ?? status}
    </span>
  );
}

function EventDot({ eventType }: { eventType: string }) {
  if (eventType.includes("succeeded")) return <span className="text-sm text-green-600">✓</span>;
  if (eventType.includes("failed")) return <span className="text-sm text-red-600">✗</span>;
  if (eventType.includes("ready")) return <span className="text-sm text-yellow-600">●</span>;
  if (eventType.includes("started") || eventType.includes("running")) return <span className="text-sm text-blue-600">▶</span>;
  if (eventType.includes("blocked")) return <span className="text-sm text-red-700">⛔</span>;
  if (eventType.includes("approval")) return <span className="text-sm text-amber-600">⚠</span>;
  return <span className="text-sm text-muted-foreground">•</span>;
}

function formatPayload(data: Uint8Array): string {
  try {
    const text = new TextDecoder().decode(data);
    try {
      return JSON.stringify(JSON.parse(text), null, 2);
    } catch {
      return text;
    }
  } catch {
    return `${data.length} bytes`;
  }
}

const RUN_STATUS_LABELS: Record<number, string> = {
  1: "pending",
  2: "running",
  3: "completed",
  4: "failed",
  5: "aborted",
  6: "paused",
};

const RUN_STATUS_STYLES: Record<number, string> = {
  1: "bg-gray-200 text-gray-700",
  2: "bg-blue-100 text-blue-800",
  3: "bg-green-100 text-green-800",
  4: "bg-red-100 text-red-800",
  5: "bg-gray-300 text-gray-700",
  6: "bg-yellow-100 text-yellow-800",
};

const STEP_RUN_STATUS_LABELS: Record<number, string> = {
  1: "pending",
  2: "ready",
  3: "running",
  4: "succeeded",
  5: "failed",
  6: "skipped",
  7: "blocked",
  8: "approval_pending",
};

function ExecStatusBadge({ status }: { status: number }) {
  const labels: Record<number, string> = {
    1: "dispatching",
    2: "running",
    3: "healthy",
    4: "stalled",
    5: "unhealthy",
    6: "terminating",
    7: "terminated",
    8: "failed_to_start",
    9: "succeeded",
    10: "failed",
  };
  const styles: Record<number, string> = {
    1: "bg-blue-100 text-blue-800",
    2: "bg-green-100 text-green-800",
    3: "bg-green-600 text-white",
    4: "bg-yellow-100 text-yellow-800",
    5: "bg-red-100 text-red-800",
    6: "bg-orange-100 text-orange-800",
    7: "bg-gray-200 text-gray-700",
    8: "bg-red-600 text-white",
    9: "bg-emerald-100 text-emerald-800",
    10: "bg-red-700 text-white",
  };
  return (
    <span className={cn("rounded-full px-2 py-0.5 text-xs font-medium", styles[status] ?? "bg-muted text-muted-foreground")}>
      {labels[status] ?? "unknown"}
    </span>
  );
}
