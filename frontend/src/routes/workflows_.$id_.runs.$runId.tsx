import { createRoute, useNavigate } from "@tanstack/react-router";
import { useEffect, useMemo, useState } from "react";
import { CheckCircle2, XCircle } from "lucide-react";
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
import type { Timestamp } from "@bufbuild/protobuf";
import type { WorkflowStepRun } from "@/api/gen/orchicon/api/v1/workflow_pb";
import { StepKind, StepRunStatus } from "@/api/gen/orchicon/api/v1/workflow_pb";

import { useApproveStep } from "@/api/approvals";
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
import { Badge } from "@/components/ui/badge";
import { ACCENT_STROKE, KIND_ACCENT } from "@/components/workflow-editor/stepKinds";
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
  1: "worker",
  2: "conditional",
  3: "approval",
  4: "parallel",
  5: "recover",
  6: "work_item",
  7: "project",
  8: "loop_decision",
  9: "policy",
};

const STEP_KIND_COLORS: Record<number, string> = {
  1: "border-sky-400",
  2: "border-amber-400",
  3: "border-yellow-500",
  4: "border-violet-400",
  5: "border-rose-400",
  6: "border-emerald-400",
  7: "border-indigo-400",
  8: "border-cyan-400",
  9: "border-amber-400",
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
  const { data: runExecs } = useListExecutions({ workflowRunId: runId, sortOrder: "asc" });
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
  // step-run status on each node. Mirrors the editor's stepsToCanvas so
  // loop-back edges, source/target handles, and per-kind accent colors
  // are preserved on the run view.
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
      edge_handles?: Record<string, { sourceHandle?: string; targetHandle?: string }>;
    }[] = [];
    try {
      steps = JSON.parse(stepsJson);
    } catch {
      steps = [];
    }
    const kindStrToNum: Record<string, number> = {
      task: 1, decision: 2, approval: 3, parallel: 4, recover: 5,
      work_item: 6, project: 7, loop_decision: 8, policy: 9,
    };
    // Per-kind accent tokens. KIND_ACCENT maps the proto kind to a
    // tailwind color name; ACCENT_STROKE maps that name to the
    // tailwind class applied to the SVG edge. We use BOTH the
    // className (so the tailwind stroke-*-400 utility draws the
    // line — required when var(--kind-*) is undefined in the SVG
    // context) AND the CSS-var style (so dark/light theme tokens
    // can override without retouching every edge).
    const kindAccent: Record<number, string> = {
      1: KIND_ACCENT[1] ?? "sky",
      2: KIND_ACCENT[2] ?? "amber",
      3: KIND_ACCENT[3] ?? "yellow",
      4: KIND_ACCENT[4] ?? "violet",
      5: KIND_ACCENT[5] ?? "rose",
      6: KIND_ACCENT[6] ?? "emerald",
      7: KIND_ACCENT[7] ?? "indigo",
      8: KIND_ACCENT[8] ?? "cyan",
      9: KIND_ACCENT[9] ?? "amber",
    };
    // Map each step_id → active step run (ignoring superseded rows so
    // a loop_decision re-ask that replaced the prior run doesn't
    // shadow the new run's status).
    const statusByStep = new Map<string, number>();
    const loopOutcomeByStep = new Map<string, string>();
    const sortByIteration = (a: { iteration: number }, b: { iteration: number }) =>
      b.iteration - a.iteration;
    type StepRunT = NonNullable<typeof stepRuns>[number];
    const latestByStep = new Map<string, StepRunT>();
    const sortedStepRuns = [...(stepRuns ?? [])].sort(sortByIteration);
    for (const sr of sortedStepRuns) {
      if (sr.supersededBy) continue;
      latestByStep.set(sr.stepId, sr);
    }
    for (const [stepID, sr] of latestByStep) {
      statusByStep.set(stepID, sr.status);
      // For loop_decision steps, surface the recorded outcome so the
      // operator can see at a glance whether the decision looped
      // back, re-asked, or accepted (recover-pending surfaces as
      // "recover pending" since the step's result holds a status
      // hint).
      if (sr.stepKind === 8) {
        try {
          const parsed = sr.result ? JSON.parse(sr.result) : null;
          if (parsed && typeof parsed === "object") {
            if (parsed.loop === "re-ask") loopOutcomeByStep.set(stepID, "re-ask reviewer");
            else if (parsed.loop) loopOutcomeByStep.set(stepID, `loop → ${String(parsed.loop).slice(0, 16)}`);
            else if (parsed.decision) loopOutcomeByStep.set(stepID, `decision: ${String(parsed.decision)}`);
            else if (sr.status === 4) loopOutcomeByStep.set(stepID, "decision made");
            else if (sr.status === 3) loopOutcomeByStep.set(stepID, "evaluating…");
            else if (sr.status === 5) loopOutcomeByStep.set(stepID, "max iterations reached");
          } else if (sr.status === 4) {
            loopOutcomeByStep.set(stepID, "decision made");
          } else if (sr.status === 5) {
            loopOutcomeByStep.set(stepID, "max iterations reached");
          }
        } catch {
          /* ignore malformed result */
        }
      }
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
          stepId: s.id,
          loopOutcome: loopOutcomeByStep.get(s.id),
        },
      };
    });
    const edges: Edge[] = [];
    const edgeHandles: Record<string, { sourceHandle?: string; targetHandle?: string }> = {};
    for (const s of steps) {
      if (s.edge_handles) Object.assign(edgeHandles, s.edge_handles);
    }
    const nodeIds = new Set(nodes.map((n) => n.id));
    const kindById = new Map<string, number>();
    for (const n of nodes) kindById.set(n.id, (n.data as { kind: number }).kind);
    const seen = new Set<string>();
    for (const s of steps) {
      for (const dep of s.depends_on ?? []) {
        const edgeKey = `e-${dep}-${s.id}`;
        seen.add(edgeKey);
        const handles = edgeHandles[edgeKey];
        const srcKind = kindById.get(dep) ?? 1;
        const accent = kindAccent[srcKind] ?? "sky";
        edges.push({
          id: edgeKey,
          source: dep,
          target: s.id,
          sourceHandle: handles?.sourceHandle,
          targetHandle: handles?.targetHandle,
          markerEnd: { type: MarkerType.ArrowClosed },
          animated: statusByStep.get(s.id) === 3,
          // className is the visible stroke (Tailwind stroke-*-400
          // utility). The style.stroke is the same color via a CSS
          // variable so dark/light themes can override it. Without
          // the className, an undefined --kind-${accent} CSS var
          // makes the SVG stroke "none" and the edge is invisible.
          className: ACCENT_STROKE[accent] ?? "",
          style: { stroke: `var(--kind-${accent})` },
        });
      }
    }
    // Restore loop-back edges from edge_handles not covered by depends_on
    // (e.g. loop_decision source-loop / source-success handles). Match
    // by node-id prefix against known node IDs (mirrors canvas.ts:120-145).
    for (const [edgeKey, handles] of Object.entries(edgeHandles)) {
      if (seen.has(edgeKey)) continue;
      for (const srcId of nodeIds) {
        const prefix = `e-${srcId}-`;
        if (edgeKey.startsWith(prefix)) {
          const tgtId = edgeKey.slice(prefix.length);
          if (nodeIds.has(tgtId)) {
            const srcKind = kindById.get(srcId) ?? 1;
            const accent = kindAccent[srcKind] ?? "sky";
            edges.push({
              id: edgeKey,
              source: srcId,
              target: tgtId,
              sourceHandle: handles?.sourceHandle,
              targetHandle: handles?.targetHandle,
              markerEnd: { type: MarkerType.ArrowClosed },
              // Loop-back edges animate when the loop decision is
              // actively routing (status 3 = running) so the user
              // can see the cycle in motion.
              animated: statusByStep.get(srcId) === 3,
              className: ACCENT_STROKE[accent] ?? "",
              style: { stroke: `var(--kind-${accent})` },
            });
          }
          break;
        }
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
              {(stepRuns ?? []).map((sr) => {
                // Step rows that already have a worker execution
                // become buttons — click through to the live execution
                // session pane (the user expects this in the "Workflows
                // → click execution → see live chat" flow).
                // Rows without an execution yet (still waiting for the
                // task reconciler to dispatch) stay non-interactive so
                // they don't look clickable when they aren't.
                const clickable = !!sr.workerExecutionId;
                const Row = clickable ? "button" : "div";
                return (
                  <Row
                    key={sr.id}
                    type={clickable ? "button" : undefined}
                    className={cn(
                      "flex w-full items-center gap-3 rounded-md border p-2 text-left text-sm",
                      clickable &&
                        "hover:bg-accent focus:outline-none focus-visible:ring-2 focus-visible:ring-ring",
                    )}
                    onClick={
                      clickable
                        ? () =>
                            window.open(
                              `/executions/${sr.workerExecutionId!}`,
                              "_blank",
                            )
                        : undefined
                    }
                    aria-label={
                      clickable
                        ? `Open execution ${sr.workerExecutionId}`
                        : undefined
                    }
                  >
                    <StepStatusPill status={sr.status} />
                    <div className="min-w-0 flex-1">
                      <div className="flex items-center gap-2">
                        <span className="font-medium">{sr.stepName || sr.stepId}</span>
                        <span className="text-xs text-muted-foreground">
                          {STEP_KIND_LABELS[sr.stepKind] ?? "step"}
                        </span>
                        <LiveDuration startedAt={sr.startedAt} endedAt={sr.endedAt} />
                      </div>
                      {sr.result && (() => {
                        try {
                          const r = JSON.parse(sr.result);
                          const parts: string[] = [];
                          if (r._decision) parts.push(r._decision);
                          if (r._worker) parts.push(`by ${r._worker}`);
                          if (r._summary) {
                            const line = r._summary.split("\n")[0].slice(0, 120);
                            parts.push(line);
                          }
                          if (Array.isArray(r._touched_files) && r._touched_files.length > 0) {
                            parts.push(`${r._touched_files.length} file${r._touched_files.length !== 1 ? "s" : ""}`);
                          }
                          if (parts.length === 0) return null;
                          return (
                            <div className="mt-1 text-xs text-muted-foreground">
                              {parts.join(" · ")}
                            </div>
                          );
                        } catch { return null; }
                      })()}
                    </div>
                    {sr.workerExecutionId && (
                      <span className="shrink-0 font-mono text-xs text-muted-foreground">
                        exec: {sr.workerExecutionId.slice(0, 12)}…
                      </span>
                    )}
                  </Row>
                );
              })}
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
              .map((sr) => {
                if (sr.stepKind === StepKind.APPROVAL && (sr.status === StepRunStatus.APPROVAL_PENDING || sr.status === StepRunStatus.SUCCEEDED)) {
                  // APPROVAL step — show the approval panel inline.
                  return <ApprovalStepCard key={sr.id} stepRun={sr} />;
                }
                return (
                  <div key={sr.id} className="flex items-center gap-3 rounded-md border p-2 text-sm text-muted-foreground">
                    <StepStatusPill status={sr.status} />
                    <span className="font-medium">{sr.stepName || sr.stepId.slice(0, 12)}</span>
                    <span className="text-xs text-muted-foreground/60">waiting for dispatch…</span>
                  </div>
                );
              })}
            {/* Actual WorkerExecutions */}
            {(runExecs ?? []).length === 0 && (stepRuns ?? []).filter((sr) => !sr.workerExecutionId).length === 0 && (
              <p className="text-sm text-muted-foreground">No executions yet.</p>
            )}
            {(runExecs ?? []).map((ex) => (
              <button
                key={ex.id}
                className="flex w-full items-center gap-3 rounded-md border p-2 text-left text-sm hover:bg-accent"
                onClick={() =>
                  window.open(`/executions/${ex.id}`, "_blank")
                }
              >
                <ExecStatusBadge status={ex.status} />
                <span className="font-medium min-w-0 truncate">
                  {ex.workflowName
                    ? `${ex.workflowName} — ${workerLabel(ex.workerId)}${ex.iteration > 0 ? ` (Loop #${ex.iteration})` : ""}`
                    : `${workerLabel(ex.workerId)}${ex.iteration > 0 ? ` (loop #${ex.iteration})` : ""}`}
                </span>
                <span className="font-mono text-xs text-muted-foreground shrink-0">{ex.id.slice(0, 12)}…</span>
                <LiveDuration startedAt={ex.startedAt} endedAt={ex.endedAt} />
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

// --- Approval step card (inline approve/reject panel for the run view) ---

function ApprovalStepCard({ stepRun }: { stepRun: WorkflowStepRun }) {
  const [reason, setReason] = useState("");
  const [showContext, setShowContext] = useState(false);
  const approveMutation = useApproveStep();

  // Parse the review context from the step run result.
  let context: {
    upstreamWorker?: string;
    upstreamSummary?: string;
    upstreamFiles?: string[];
    ac?: string;
  } = {};
  let decision = "";
  try {
    const result = JSON.parse(stepRun.result ?? "{}");
    if (result._review_context) context = result._review_context;
    decision = result._decision ?? "";
  } catch {}

  const isPending = stepRun.status === StepRunStatus.APPROVAL_PENDING;
  const isResolved = stepRun.status === StepRunStatus.SUCCEEDED;

  return (
    <div className="rounded-md border border-yellow-300 bg-yellow-50 p-3 text-sm dark:border-yellow-800 dark:bg-yellow-950/30">
      <div className="flex items-center justify-between">
        <div className="flex items-center gap-2">
          <Badge variant="outline" className="bg-amber-100 text-amber-900">
            {isPending ? "Approval pending" : isResolved ? (decision === "approved" ? "Approved" : "Rejected") : "Unknown"}
          </Badge>
          <span className="font-medium">{stepRun.stepName || stepRun.stepId.slice(0, 12)}</span>
        </div>
        <button
          className="text-xs text-muted-foreground hover:underline"
          onClick={() => setShowContext(!showContext)}
        >
          {showContext ? "Hide context" : "Show context"}
        </button>
      </div>

      {showContext && context.upstreamSummary && (
        <div className="mt-2 space-y-2">
          {context.upstreamWorker && (
            <p className="text-xs text-muted-foreground">
              From worker: <span className="font-medium">{context.upstreamWorker}</span>
            </p>
          )}
          <div className="rounded-md bg-white/50 p-2 dark:bg-black/10">
            <p className="text-xs font-medium text-muted-foreground">Upstream summary</p>
            <p className="mt-0.5 text-sm whitespace-pre-wrap">{context.upstreamSummary}</p>
          </div>
          {context.upstreamFiles && context.upstreamFiles.length > 0 && (
            <div>
              <p className="text-xs font-medium text-muted-foreground">Files changed</p>
              <div className="mt-0.5 flex flex-wrap gap-1">
                {context.upstreamFiles.map((f) => (
                  <code key={f} className="rounded bg-muted px-1 py-0.5 text-xs">{f}</code>
                ))}
              </div>
            </div>
          )}
          {context.ac && (
            <div>
              <p className="text-xs font-medium text-muted-foreground">Acceptance criteria</p>
              <p className="mt-0.5 text-sm whitespace-pre-wrap">{context.ac}</p>
            </div>
          )}
        </div>
      )}

      {/* Approve/Reject buttons for pending steps */}
      {isPending && (
        <div className="mt-2 space-y-2 border-t border-yellow-200 pt-2 dark:border-yellow-800">
          <textarea
            placeholder="Reason / feedback (optional)..."
            className="w-full rounded-md border bg-white px-2 py-1.5 text-sm dark:bg-black/10"
            rows={2}
            value={reason}
            onChange={(e) => setReason(e.target.value)}
          />
          <div className="flex gap-2">
            <Button
              size="sm"
              variant="default"
              onClick={() => approveMutation.mutate({ stepRunId: stepRun.id, approved: true, reason, reviewedBy: "" })}
              disabled={approveMutation.isPending}
            >
              <CheckCircle2 className="mr-1 h-4 w-4" />
              Approve
            </Button>
            <Button
              size="sm"
              variant="destructive"
              onClick={() => approveMutation.mutate({ stepRunId: stepRun.id, approved: false, reason, reviewedBy: "" })}
              disabled={approveMutation.isPending}
            >
              <XCircle className="mr-1 h-4 w-4" />
              Reject
            </Button>
          </div>
        </div>
      )}

      {/* Show result for resolved steps */}
      {isResolved && decision !== "" && (
        <div className="mt-2 border-t border-yellow-200 pt-2 text-xs dark:border-yellow-800">
          <span className="text-muted-foreground">Decision: </span>
          <span className={cn("font-medium", decision === "approved" ? "text-emerald-600" : "text-red-600")}>
            {decision}
          </span>
        </div>
      )}
    </div>
  );
}

// --- Run step node (overlays step-run status on the canvas) ---
function RunStepNode({
  data,
}: {
  data: {
    kind: number;
    name: string;
    ref: string;
    runStatus: number;
    stepId?: string;
    loopOutcome?: string;
    loopBranch?: string;
  };
}) {
  const statusColor = STEP_RUN_STATUS_COLORS[data.runStatus] ?? "bg-gray-200";
  const statusLabel = STEP_RUN_STATUS_LABELS[data.runStatus] ?? "pending";
  const isLoopDecision = data.kind === 8;
  // For loop_decision steps, surface the outcome the reconciler
  // recorded on the step run's result JSON (e.g. "loop: PR Reviewer"
  // or "re-ask"). This is the visual the operator asked for: a
  // clear indicator that the decision fired and where the run went.
  const loopTag = (() => {
    if (!isLoopDecision) return null;
    if (data.loopOutcome) return data.loopOutcome;
    if (data.runStatus === 4) return "decision made";
    if (data.runStatus === 3) return "evaluating…";
    return null;
  })();
  return (
    <div
      className={cn(
        "relative min-w-[160px] rounded-md border px-3 py-2 text-center shadow-sm",
        STEP_KIND_COLORS[data.kind] ?? "border-gray-300",
      )}
    >
      <Handle
        type="target"
        id="target-left"
        position={Position.Left}
        className="!h-2.5 !w-2.5 !border-2 !border-background !bg-emerald-400"
      />
      <Handle
        type="target"
        id="target-top"
        position={Position.Top}
        className="!h-2 !w-2 !border-2 !border-background !bg-emerald-400"
      />
      <div className="text-[10px] font-medium uppercase text-muted-foreground">
        {STEP_KIND_LABELS[data.kind] ?? "step"}
      </div>
      <div className="truncate text-sm font-semibold" title={data.name}>
        {data.name}
      </div>
      <div className={cn("mt-1 inline-block rounded-full px-2 py-0.5 text-[10px] font-medium", statusColor)}>
        {statusLabel}
      </div>
      {loopTag && (
        <div
          className="mt-1 inline-block max-w-full truncate rounded bg-cyan-100 px-1.5 py-0.5 text-[10px] font-medium text-cyan-900 dark:bg-cyan-950/60 dark:text-cyan-100"
          title={loopTag}
        >
          {loopTag}
        </div>
      )}
      {isLoopDecision ? (
        <>
          <Handle
            type="source"
            id="source-success"
            position={Position.Bottom}
            className="!h-2.5 !w-2.5 !border-2 !border-background !bg-emerald-500"
          />
          <span className="pointer-events-none absolute -bottom-4 left-1/2 -translate-x-1/2 text-[9px] font-medium text-emerald-600 dark:text-emerald-400">
            success
          </span>
          <Handle
            type="source"
            id="source-loop"
            position={Position.Right}
            className="!h-2.5 !w-2.5 !border-2 !border-background !bg-rose-500"
          />
          <span className="pointer-events-none absolute -right-9 top-1/2 -translate-y-1/2 text-[9px] font-medium text-rose-600 dark:text-rose-400">
            loop
          </span>
        </>
      ) : (
        <>
          <Handle
            type="source"
            id="source-right"
            position={Position.Right}
            className="!h-2.5 !w-2.5 !border-2 !border-background !bg-amber-500"
          />
          <Handle
            type="source"
            id="source-bottom"
            position={Position.Bottom}
            className="!h-2 !w-2 !border-2 !border-background !bg-amber-500"
          />
        </>
      )}
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

function workerLabel(id: string): string {
  const prefix = "w_se_";
  if (id.startsWith(prefix)) {
    const rest = id.slice(prefix.length);
    return rest
      .split("_")
      .map((w) => (w.length > 0 ? w[0].toUpperCase() + w.slice(1) : w))
      .join(" ");
  }
  return id;
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

function formatDuration(seconds: number): string {
  if (seconds < 1) return "<1s";
  if (seconds < 60) return `${Math.round(seconds * 10) / 10}s`;
  const m = Math.floor(seconds / 60);
  const s = Math.round(seconds % 60);
  return `${m}m ${s}s`;
}

function LiveDuration({ startedAt, endedAt }: { startedAt?: Timestamp | null; endedAt?: Timestamp | null }) {
  const [elapsed, setElapsed] = useState(0);

  useEffect(() => {
    const startMs = startedAt ? Number(startedAt.seconds) * 1000 + (startedAt.nanos ?? 0) / 1_000_000 : 0;
    if (!startMs) { setElapsed(0); return; }

    if (endedAt) {
      const endMs = Number(endedAt.seconds) * 1000 + (endedAt.nanos ?? 0) / 1_000_000;
      setElapsed((endMs - startMs) / 1000);
      return;
    }

    const tick = () => setElapsed((Date.now() - startMs) / 1000);
    tick();
    const id = setInterval(tick, 1000);
    return () => clearInterval(id);
  }, [startedAt, endedAt]);

  if (!startedAt) return null;
  return (
    <span className="font-mono text-xs text-muted-foreground shrink-0">
      {formatDuration(elapsed)}
    </span>
  );
}

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
