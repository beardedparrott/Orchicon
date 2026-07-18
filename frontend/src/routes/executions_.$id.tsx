// Execution detail page — the live execution view (docs/10, docs/07
// §3.8, docs/04 §4). Streams telemetry/tool-call/health events in real
// time, provides manual controls (pause/resume/cancel/checkpoint), and
// shows Tier 2 per-tool-call approval requests (docs/05 §7.1).
//
// Layout: a two-column workspace on lg+ — the chat-style runtime session
// on the left, a context sidebar on the right (OpenChamber-style:
// context % bar, message counts, last assistant message, raw event
// timeline). Stacks to a single column on mobile. The legacy admin-panel
// stack of cards was replaced because it buried the live session under
// metadata — the new layout makes the live chat the primary surface
// and the context sidebar the secondary reference.
import { createRoute, useNavigate } from "@tanstack/react-router";
import { useQueryClient } from "@tanstack/react-query";
import { Pause, Play, Square, Save, Trash2, ArrowLeft } from "lucide-react";

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
import { useGetUsage } from "@/api/aigateway";
import { useGetWorkItem } from "@/api/workItems";
import { Markdown } from "@/components/markdown";
import { RuntimeSessionPane } from "@/components/executions/RuntimeSessionPane";
import { ExecutionContextSidebar } from "@/components/executions/ExecutionContextSidebar";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { Route as rootRoute } from "@/routes/__root";

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
  const { data: usage } = useGetUsage({ executionId: id });
  const pauseExec = usePauseExecution();
  const resumeExec = useResumeExecution();
  const cancelExec = useCancelExecution();
  const checkpointNow = useCheckpointNow();
  const deleteExec = useDeleteExecution();

  const navigate = useNavigate();

  // Live event stream (docs/10 §4). Subscribes to
  // StreamExecutionEvents filtered to this execution. onEvent
  // invalidates the detail query so the sidebar's status/cost/duration
  // refreshes as the adapter reports.
  const { events, status } = useStreamExecutionEvents({
    executionId: id,
    onEvent: () => {
      qc.invalidateQueries({ queryKey: executionKeys.detail(id) });
    },
  });

  // Tier 2 pending approvals (docs/05 §7.1).
  const { data: pendingApprovals } = useListPendingApprovals(id);

  if (isLoading) {
    return (
      <div className="flex h-64 items-center justify-center text-sm text-muted-foreground">
        Loading…
      </div>
    );
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
    <div className="space-y-4">
      {/* Compact top action bar — back to list, ID, status pill,
          and the lifecycle actions (pause/resume/cancel/checkpoint/
          delete) as icon buttons. On phones the icon-only buttons
          collapse into a wrap-friendly row; on desktop they sit in
          a single line. */}
      <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
        <div className="flex min-w-0 items-center gap-2">
          <Button
            variant="ghost"
            size="sm"
            onClick={() => navigate({ to: "/executions" })}
            className="shrink-0"
          >
            <ArrowLeft className="h-4 w-4" />
            <span className="ml-1 hidden sm:inline">Back</span>
          </Button>
          <h1 className="text-base font-semibold tracking-tight sm:text-lg">
            Execution
          </h1>
          <span className="hidden truncate font-mono text-xs text-muted-foreground sm:inline">
            {exec.id}
          </span>
          <ExecutionLiveBadge status={exec.status} isLive={status === "open"} />
        </div>
        <div className="flex flex-wrap items-center gap-1.5">
          {isRunning && (
            <IconAction
              icon={Pause}
              label="Pause"
              onClick={() => pauseExec.mutate(id)}
              disabled={pauseExec.isPending}
            />
          )}
          {isPaused && (
            <IconAction
              icon={Play}
              label="Resume"
              onClick={() => resumeExec.mutate(id)}
              disabled={resumeExec.isPending}
            />
          )}
          {isRunning && (
            <IconAction
              icon={Save}
              label="Checkpoint"
              onClick={() => checkpointNow.mutate(id)}
              disabled={checkpointNow.isPending}
            />
          )}
          {!isTerminal && (
            <IconAction
              icon={Square}
              label="Cancel"
              variant="destructive"
              onClick={() => cancelExec.mutate({ id })}
              disabled={cancelExec.isPending}
            />
          )}
          <IconAction
            icon={Trash2}
            label="Delete"
            variant="outline"
            onClick={() => {
              if (
                window.confirm(
                  "Delete this execution? It will be force-stopped if running.",
                )
              ) {
                deleteExec.mutate(id, {
                  onSuccess: () => navigate({ to: "/executions" }),
                });
              }
            }}
            disabled={deleteExec.isPending}
          />
        </div>
      </div>

      {/* Two-column workspace: live chat on the left, context sidebar
          on the right. Stacks on screens narrower than lg. */}
      <div className="grid gap-4 lg:grid-cols-[1fr_320px]">
        <div className="space-y-4 min-w-0">
          {/* Failure card: pulled out of the sidebar so the operator
              sees the error first when something breaks. */}
          {exec.errorMessage && (
            <div className="rounded-xl border border-destructive/50 bg-destructive/10 p-4">
              <div className="mb-1 text-xs font-semibold uppercase tracking-wider text-destructive">
                Error
              </div>
              <Markdown className="text-destructive">{exec.errorMessage}</Markdown>
            </div>
          )}
          {isFailed && !exec.errorMessage && (
            <div className="rounded-xl border border-destructive/50 bg-destructive/10 p-4 text-sm italic text-muted-foreground">
              Execution failed with no additional details.
            </div>
          )}

          {/* Tier 2 pending approval requests (docs/05 §7.1) */}
          {pendingApprovals && pendingApprovals.length > 0 && (
            <ApprovalDialog approvals={pendingApprovals} />
          )}

          {/* Live chat — the primary surface. */}
          <RuntimeSessionPane
            events={events}
            prompt={workItem?.promptContext}
            streamStatus={status}
            storedOutput={exec.output}
          />

          {/* Execution context — kept as a footer card with the
              structured metadata (worker, adapter, task, workflow)
              since that data doesn't fit naturally in the sidebar. */}
          <ExecutionContextFooter exec={exec} />
        </div>

        <ExecutionContextSidebar
          exec={exec}
          events={events}
          usage={usage ?? []}
          streamStatus={status}
        />
      </div>
    </div>
  );
}

function IconAction({
  icon: Icon,
  label,
  onClick,
  disabled,
  variant = "outline",
}: {
  icon: typeof Pause;
  label: string;
  onClick: () => void;
  disabled?: boolean;
  variant?: "outline" | "destructive";
}) {
  return (
    <Button
      variant={variant}
      size="sm"
      onClick={onClick}
      disabled={disabled}
      title={label}
      aria-label={label}
      className="gap-1"
    >
      <Icon className="h-4 w-4" />
      <span className="hidden sm:inline">{label}</span>
    </Button>
  );
}

function ExecutionLiveBadge({
  status,
  isLive,
}: {
  status: number;
  isLive: boolean;
}) {
  const labels: Record<number, string> = {
    1: "Dispatching",
    2: "Running",
    3: "Healthy",
    4: "Stalled",
    5: "Unhealthy",
    6: "Terminating",
    7: "Terminated",
    8: "Failed to start",
    9: "Succeeded",
    10: "Failed",
  };
  const styles: Record<number, string> = {
    1: "bg-blue-100 text-blue-800 dark:bg-blue-950 dark:text-blue-200",
    2: "bg-amber-100 text-amber-800 dark:bg-amber-950 dark:text-amber-200",
    3: "bg-emerald-100 text-emerald-800 dark:bg-emerald-950 dark:text-emerald-200",
    4: "bg-yellow-100 text-yellow-800 dark:bg-yellow-950 dark:text-yellow-200",
    5: "bg-red-100 text-red-800 dark:bg-red-950 dark:text-red-200",
    6: "bg-orange-100 text-orange-800 dark:bg-orange-950 dark:text-orange-200",
    7: "bg-zinc-100 text-zinc-800 dark:bg-zinc-900 dark:text-zinc-200",
    8: "bg-red-200 text-red-900 dark:bg-red-900 dark:text-red-200",
    9: "bg-emerald-100 text-emerald-800 dark:bg-emerald-950 dark:text-emerald-200",
    10: "bg-red-100 text-red-800 dark:bg-red-950 dark:text-red-200",
  };
  const showPulse = isLive && (status === 2 || status === 3);
  return (
    <span
      className={cn(
        "inline-flex shrink-0 items-center gap-1.5 rounded-full px-2 py-0.5 text-xs font-medium",
        styles[status] ?? "bg-muted text-muted-foreground",
      )}
    >
      <span
        className={cn(
          "inline-block h-1.5 w-1.5 rounded-full",
          showPulse ? "bg-emerald-500 animate-pulse" : "bg-current",
        )}
      />
      {labels[status] ?? "unknown"}
    </span>
  );
}

function ExecutionContextFooter({
  exec,
}: {
  exec: import("@/api/gen/orchicon/api/v1/execution_pb").WorkerExecution;
}) {
  const items = [
    { label: "Worker", value: `${exec.workerId} v${exec.workerVersion}` },
    { label: "Adapter", value: exec.adapterId || "—" },
    { label: "Task", value: exec.taskId },
    {
      label: "Workflow",
      value: exec.workflowRunId
        ? `${exec.workflowName || exec.workflowRunId}${exec.workflowStepId ? ` · step ${exec.workflowStepId}` : ""}`
        : "—",
    },
  ];
  return (
    <div className="rounded-xl border bg-card p-4 shadow-sm">
      <div className="mb-3 flex items-center justify-between">
        <h3 className="text-xs font-semibold uppercase tracking-wider text-muted-foreground">
          Execution context
        </h3>
        <span className="font-mono text-[10px] text-muted-foreground/70 sm:hidden">
          {exec.id}
        </span>
      </div>
      <dl className="grid grid-cols-1 gap-3 text-sm sm:grid-cols-2">
        {items.map((item) => (
          <div key={item.label}>
            <dt className="text-[10px] font-semibold uppercase tracking-wider text-muted-foreground">
              {item.label}
            </dt>
            <dd className="mt-0.5 break-all font-mono text-xs">{item.value}</dd>
          </div>
        ))}
      </dl>
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
    <div className="rounded-xl border border-amber-300 bg-amber-50 p-4 dark:bg-amber-950/30">
      <div className="mb-1 text-xs font-semibold uppercase tracking-wider text-amber-800 dark:text-amber-200">
        Pending Tool-Call Approvals
      </div>
      <p className="mb-3 text-xs text-amber-700 dark:text-amber-300">
        Tier 2 per-tool-call gating (docs/05 §7.1). The worker's
        gated_tools requires human approval for these calls.
      </p>
      <div className="space-y-2">
        {approvals.map((req) => (
          <div
            key={req.requestId}
            className="rounded-md border border-amber-200 bg-amber-100/40 p-3 dark:border-amber-800 dark:bg-amber-950/40"
          >
            <div className="flex flex-col gap-2 sm:flex-row sm:items-start sm:justify-between">
              <div className="min-w-0 flex-1">
                <p className="text-sm font-medium">{req.toolCategory}</p>
                {req.detail && req.detail.length > 0 && (
                  <pre className="mt-1 max-h-32 overflow-auto rounded bg-background/60 p-2 text-xs">
                    {formatPayload(req.detail)}
                  </pre>
                )}
              </div>
              <div className="flex shrink-0 gap-2">
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
      </div>
    </div>
  );
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