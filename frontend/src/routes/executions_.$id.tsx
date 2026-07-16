import { createRoute, useNavigate } from "@tanstack/react-router";
import { useQueryClient } from "@tanstack/react-query";

import {
  useGetExecution,
  useStreamExecutionEvents,
  usePauseExecution,
  useResumeExecution,
  useCancelExecution,
  useCheckpointNow,
  useDeleteExecution,
  useListPendingApprovals,
  useApproveToolCall,
} from "@/api/executions";
import { executionKeys } from "@/api/executions";
import { useGetWorkItem } from "@/api/workItems";
import { RuntimeSessionPane } from "@/components/executions/RuntimeSessionPane";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Route as rootRoute } from "@/routes/__root";

// Execution detail — the live execution view (docs/10, docs/07 §3.8,
// docs/04 §4). Streams telemetry/tool-call/health events in real-time,
// provides manual controls (pause/resume/cancel/checkpoint), and shows
// Tier 2 per-tool-call approval requests (docs/05 §7.1).
export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/executions/$id",
  component: ExecutionDetailPage,
});

function ExecutionDetailPage() {
  const { id } = Route.useParams();
  const qc = useQueryClient();

  const { data: exec, isLoading, error } = useGetExecution(id);
  const { data: workItem } = useGetWorkItem(exec?.taskId ?? "");
  const pauseExec = usePauseExecution();
  const resumeExec = useResumeExecution();
  const cancelExec = useCancelExecution();
  const checkpointNow = useCheckpointNow();
  const deleteExec = useDeleteExecution();

  const navigate = useNavigate();

  // Live event stream (docs/10 §4). Subscribes to
  // StreamExecutionEvents filtered to this execution.
  const { events, status } = useStreamExecutionEvents({
    executionId: id,
    onEvent: () => {
      qc.invalidateQueries({ queryKey: executionKeys.detail(id) });
    },
  });

  // Tier 2 pending approvals (docs/05 §7.1).
  const { data: pendingApprovals } = useListPendingApprovals(id);

  if (isLoading) {
    return <p className="text-sm text-muted-foreground">Loading…</p>;
  }
  if (error) {
    return (
      <p className="text-sm text-destructive">
        Failed to load execution: {String(error)}
      </p>
    );
  }
  if (!exec) return null;

  const isRunning = exec.status === 2 || exec.status === 3;
  const isPaused = exec.status === 6;
  const isTerminal = exec.status === 7 || exec.status === 8;
  const isFailed = exec.status === 10 || exec.status === 8;

  return (
    <div className="space-y-6">
      <div className="flex items-start justify-between">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">
            Execution
          </h1>
          <p className="font-mono text-xs text-muted-foreground">{exec.id}</p>
        </div>
        <div className="flex gap-2">
          {isRunning && (
            <Button
              variant="outline"
              onClick={() => pauseExec.mutate(id)}
              disabled={pauseExec.isPending}
            >
              {pauseExec.isPending ? "Pausing…" : "Pause"}
            </Button>
          )}
          {isPaused && (
            <Button
              variant="outline"
              onClick={() => resumeExec.mutate(id)}
              disabled={resumeExec.isPending}
            >
              {resumeExec.isPending ? "Resuming…" : "Resume"}
            </Button>
          )}
          {isRunning && (
            <Button
              variant="outline"
              onClick={() => checkpointNow.mutate(id)}
              disabled={checkpointNow.isPending}
            >
              Checkpoint
            </Button>
          )}
          {!isTerminal && (
            <Button
              variant="destructive"
              onClick={() => cancelExec.mutate({ id })}
              disabled={cancelExec.isPending}
            >
              {cancelExec.isPending ? "Cancelling…" : "Cancel"}
            </Button>
          )}
          <Button
            variant="outline"
            onClick={() => {
              if (window.confirm("Delete this execution? It will be force-stopped if running.")) {
                deleteExec.mutate(id, {
                  onSuccess: () => navigate({ to: "/executions" }),
                });
              }
            }}
            disabled={deleteExec.isPending}
          >
            {deleteExec.isPending ? "Deleting…" : "Delete"}
          </Button>
        </div>
      </div>

      <div className="flex flex-wrap gap-3">
        <Card className="flex-1 min-w-[140px]">
          <CardHeader className="p-3">
            <CardDescription className="text-[11px]">Status</CardDescription>
            <CardTitle className="text-sm break-all">
              <ExecStatusLabel status={exec.status} />
            </CardTitle>
          </CardHeader>
        </Card>
        <Card className="flex-1 min-w-[140px]">
          <CardHeader className="p-3">
            <CardDescription className="text-[11px]">Health</CardDescription>
            <CardTitle className="text-sm">
              <HealthLabel health={exec.healthState} />
            </CardTitle>
          </CardHeader>
        </Card>
        <Card className="flex-1 min-w-[140px]">
          <CardHeader className="p-3">
            <CardDescription className="text-[11px]">Token usage</CardDescription>
            <CardTitle className="text-sm">{Number(exec.tokenUsage)}</CardTitle>
          </CardHeader>
        </Card>
        <Card className="flex-1 min-w-[140px]">
          <CardHeader className="p-3">
            <CardDescription className="text-[11px]">Cost (USD)</CardDescription>
            <CardTitle className="text-sm">${exec.costUsd.toFixed(4)}</CardTitle>
          </CardHeader>
        </Card>
      </div>

      {/* Error message (shown on failure) */}
      {exec.errorMessage && (
        <Card className="border-destructive/50">
          <CardHeader className="p-3">
            <CardDescription className="text-[11px] text-destructive">Error</CardDescription>
            <CardTitle className="text-sm font-mono text-destructive whitespace-pre-wrap">
              {exec.errorMessage}
            </CardTitle>
          </CardHeader>
        </Card>
      )}
      {isFailed && !exec.errorMessage && (
        <Card className="border-destructive/50">
          <CardHeader className="p-3">
            <CardDescription className="text-[11px] text-destructive">Error</CardDescription>
            <CardTitle className="text-sm text-muted-foreground italic">
              Execution failed with no additional details.
            </CardTitle>
          </CardHeader>
        </Card>
      )}

      {/* Worker + adapter + workflow info */}
      <Card>
        <CardHeader className="p-3">
          <CardTitle className="text-sm">Execution context</CardTitle>
        </CardHeader>
        <CardContent className="p-3 pt-0">
          <div className="flex flex-col gap-3 text-sm">
            <div>
              <span className="text-[10px] font-semibold uppercase text-muted-foreground">
                Worker
              </span>
              <p className="font-mono break-all">{exec.workerId}</p>
              <p className="text-muted-foreground">v{exec.workerVersion}</p>
            </div>
            <div>
              <span className="text-[10px] font-semibold uppercase text-muted-foreground">
                Adapter
              </span>
              <p className="font-mono break-all">{exec.adapterId || "—"}</p>
            </div>
            <div>
              <span className="text-[10px] font-semibold uppercase text-muted-foreground">
                Task
              </span>
              <p className="font-mono break-all">{exec.taskId}</p>
            </div>
            <div>
              <span className="text-[10px] font-semibold uppercase text-muted-foreground">
                Workflow
              </span>
              {exec.workflowRunId ? (
                <>
                  <p className="font-mono break-all">{exec.workflowName || exec.workflowRunId}</p>
                  {exec.workflowStepId && <p className="text-muted-foreground">step: {exec.workflowStepId}</p>}
                </>
              ) : (
                <p className="text-muted-foreground">—</p>
              )}
            </div>
          </div>
        </CardContent>
      </Card>

      {/* Runtime session pane — live model output + tool calls */}
      <RuntimeSessionPane
        events={events}
        prompt={workItem?.promptContext}
        streamStatus={status}
      />

      {/* Tier 2 pending approval requests (docs/05 §7.1) */}
      {pendingApprovals && pendingApprovals.length > 0 && (
        <ApprovalDialog approvals={pendingApprovals} />
      )}

      {/* Live event feed (docs/10 §4, docs/04 §4) */}
      <Card>
        <CardHeader>
          <div className="flex items-center justify-between">
            <div>
              <CardTitle>Live Events</CardTitle>
              <CardDescription>
                Real-time execution telemetry streamed via NATS.
              </CardDescription>
            </div>
            <StreamStatusBadge status={status} />
          </div>
        </CardHeader>
        <CardContent>
          {events.length === 0 ? (
            <p className="text-sm text-muted-foreground">
              No events yet. Waiting for telemetry…
            </p>
          ) : (
            <div className="max-h-96 space-y-2 overflow-auto">
              {events.map((resp, i) => {
                const evt = resp.event;
                if (!evt) return null;
                return (
                  <div
                    key={`${evt.eventId}-${i}`}
                    className="flex items-start gap-3 rounded-md border p-3 text-sm"
                  >
                    <EventIcon eventType={evt.eventType} />
                    <div className="flex-1">
                      <span className="font-medium">
                        <EventTypeLabel eventType={evt.eventType} />
                      </span>
                      {evt.payload && evt.payload.length > 0 && (
                        <pre className="mt-1 max-h-40 overflow-auto rounded bg-muted p-2 text-xs text-muted-foreground">
                          {formatPayload(evt.payload)}
                        </pre>
                      )}
                    </div>
                    <span className="text-xs text-muted-foreground">
                      {evt.occurredAt
                        ? new Date(
                            Number(evt.occurredAt.seconds) * 1000,
                          ).toLocaleTimeString()
                        : "--:--:--"}
                    </span>
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
    </div>
  );
}

// ApprovalDialog renders pending Tier 2 tool-call approval requests
// (docs/05 §7.1, docs/07 §3.8). The human approves or denies each
// request; the decision is routed to the adapter via the Execute
// bistream.
function ApprovalDialog({
  approvals,
}: {
  approvals: import("@/api/gen/orchicon/api/v1/execution_pb").ApprovalRequest[];
}) {
  const approveToolCall = useApproveToolCall();
  return (
    <Card className="border-yellow-300">
      <CardHeader>
        <CardTitle className="text-yellow-800">
          ⚠ Pending Tool-Call Approvals
        </CardTitle>
        <CardDescription>
          Tier 2 per-tool-call gating (docs/05 §7.1). The worker's
          gated_tools requires human approval for these calls.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-3">
        {approvals.map((req) => (
          <div
            key={req.requestId}
            className="rounded-md border border-yellow-200 bg-yellow-50 p-3"
          >
            <div className="flex items-start justify-between">
              <div>
                <p className="text-sm font-medium">
                  {req.toolCategory}
                </p>
                {req.detail && req.detail.length > 0 && (
                  <pre className="mt-1 max-h-40 overflow-auto rounded bg-white p-2 text-xs">
                    {formatPayload(req.detail)}
                  </pre>
                )}
              </div>
              <div className="flex gap-2">
                <Button
                  size="sm"
                  variant="outline"
                  onClick={() =>
                    approveToolCall.mutate({
                      requestId: req.requestId,
                      approved: true,
                    })
                  }
                  disabled={approveToolCall.isPending}
                >
                  Approve
                </Button>
                <Button
                  size="sm"
                  variant="destructive"
                  onClick={() =>
                    approveToolCall.mutate({
                      requestId: req.requestId,
                      approved: false,
                    })
                  }
                  disabled={approveToolCall.isPending}
                >
                  Deny
                </Button>
              </div>
            </div>
          </div>
        ))}
      </CardContent>
    </Card>
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
    <span className={`text-xs font-medium ${colors[status] ?? ""}`}>
      ● {labels[status] ?? status}
    </span>
  );
}

function ExecStatusLabel({ status }: { status: number }) {
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
  const colors: Record<number, string> = {
    9: "text-emerald-600 font-semibold",
    10: "text-red-600 font-semibold",
  };
  return <span className={colors[status] ?? ""}>{labels[status] ?? "unknown"}</span>;
}

function HealthLabel({ health }: { health: number }) {
  const labels: Record<number, string> = {
    1: "healthy",
    2: "stalled",
    3: "unhealthy",
    4: "terminating",
  };
  return <span className="capitalize">{labels[health] ?? "unknown"}</span>;
}

function EventIcon({ eventType }: { eventType: number }) {
  const icons: Record<number, string> = {
    1: "▶",
    2: "📊",
    3: "🔧",
    4: "💾",
    5: "⚠",
    6: "❤",
    7: "✓",
    8: "✗",
    9: "⏯",
  };
  return <span className="text-sm">{icons[eventType] ?? "•"}</span>;
}

function EventTypeLabel({ eventType }: { eventType: number }) {
  const labels: Record<number, string> = {
    1: "started",
    2: "telemetry",
    3: "tool_call",
    4: "checkpoint",
    5: "approval_request",
    6: "health",
    7: "result",
    8: "error",
    9: "control",
  };
  return <span className="capitalize">{labels[eventType] ?? "unknown"}</span>;
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
