// HeadsUpExpandedModal — click-to-expand interrogation for one tile.
//
// A ~90vw/85vh overlay hosting the full SessionChatPane (chat +
// composer via useSendExecutionMessage/useContinueExecutionSession) plus
// compact context/actions (pause/resume/cancel, Tier-2 approvals).
// ESC, backdrop-click, X, and the shrink button all close it WITHOUT
// unmounting the grid (the grid stays mounted underneath; only the
// expanded tile's grid stream is suspended so this modal owns the one
// live subscription while open).
import { useEffect, useState } from "react";
import { Minimize2, Pause, Play, Square, X } from "lucide-react";

import {
  executionKeys,
  useApproveToolCall,
  useCancelExecution,
  useGetExecution,
  useGetExecutionTodos,
  useListPendingApprovals,
  usePauseExecution,
  useResumeExecution,
  useStreamExecutionEvents,
} from "@/api/executions";
import { usageKeys } from "@/api/aigateway";
import { useDebouncedInvalidation } from "@/lib/useDebouncedInvalidation";
import { executionStreamEnabled } from "@/lib/debouncedInvalidation";
import type { StreamStatus } from "@/api/useStream";
import { TodoListCard } from "@/components/executions/ExecutionContextSidebar";
import { SessionChatPane } from "@/components/executions/SessionChatPane";
import { Button } from "@/components/ui/button";
import { LiveDuration } from "@/components/ui/live-duration";
import { cn } from "@/lib/utils";
import type { HeadsUpTileData } from "./headsUp";
import {
  formatCompactTokens,
  useExecutionUsageSummary,
} from "./executionUsage";
import { ExecStatusBadge, StepStatusPill } from "./status";

interface HeadsUpExpandedModalProps {
  tile: HeadsUpTileData;
  onClose: () => void;
}

export function HeadsUpExpandedModal({ tile, onClose }: HeadsUpExpandedModalProps) {
  const execId = tile.execution?.id ?? "";

  // ESC closes; lock body scroll while open.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    window.addEventListener("keydown", onKey);
    const prev = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    return () => {
      window.removeEventListener("keydown", onKey);
      document.body.style.overflow = prev;
    };
  }, [onClose]);

  // The modal owns the live subscription while open (the grid suspended
  // this tile's stream via suspendedStepId — still exactly one stream).
  // Stream events invalidate the detail/session/todos/usage queries so the
  // Context rail and todo list go live while running (mirrors
  // executions_.$id.tsx). Invalidations are coalesced behind a trailing
  // debounce so a token-frequency burst collapses to one batch, and the
  // stream is gated by liveness so terminal executions never hold a
  // connection.
  const scheduleInvalidation = useDebouncedInvalidation([
    executionKeys.detail(execId),
    executionKeys.session(execId),
    executionKeys.todos(execId),
    usageKeys.records(undefined, execId),
  ]);
  // Stream status is mirrored into state so useGetExecution's pollMs can
  // depend on it (the stream hook itself needs isTerminal from exec, which
  // comes from useGetExecution — a cross-hook cycle broken by this state).
  const [streamStatus, setStreamStatus] = useState<StreamStatus>("idle");
  const { data: exec } = useGetExecution(execId, {
    // Pause the 1s detail poll while the event stream is healthy in this
    // tab — the stream is the liveness source then, and the poll's HTTP
    // slot is freed for the heavy transcript fetch. Resumes automatically
    // when the stream drops (closed/error/reconnecting).
    pollMs: streamStatus === "open" ? 0 : 1_000,
  });
  const execStatus = exec?.status ?? tile.execution?.status ?? 0;
  const isRunning = execStatus === 2 || execStatus === 3;
  const isPaused = execStatus === 6;
  const isTerminal =
    execStatus === 7 || execStatus === 8 || execStatus === 9 || execStatus === 10;
  const { events, status } = useStreamExecutionEvents({
    executionId: execId,
    enabled: executionStreamEnabled(execId, isTerminal),
    onEvent: scheduleInvalidation,
  });
  useEffect(() => setStreamStatus(status), [status]);
  const pauseExec = usePauseExecution();
  const resumeExec = useResumeExecution();
  const cancelExec = useCancelExecution();
  const { data: pendingApprovals } = useListPendingApprovals(execId || undefined);
  const approveToolCall = useApproveToolCall();

  // Same cost + context counts as /executions/$id's sidebar (shared hook).
  const { workingSet, cost, contextWindow, contextPct, hasRecords } =
    useExecutionUsageSummary(execId);
  // Worker's todo list: live-polling while non-terminal, static once
  // terminal — hidden when the worker recorded no todos.
  const { data: todos } = useGetExecutionTodos(execId, isTerminal ? 0 : 2000);

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-2 sm:p-4"
      onClick={onClose}
      role="dialog"
      aria-modal="true"
      aria-label={`Step ${tile.stepName}`}
    >
      <div
        className="flex h-[85vh] w-[92vw] max-w-6xl flex-col overflow-hidden rounded-2xl border bg-background shadow-2xl"
        onClick={(e) => e.stopPropagation()}
      >
        {/* header */}
        <div className="flex flex-wrap items-center gap-2 border-b border-border/60 px-4 py-2.5">
          <div className="min-w-0 flex-1">
            <div className="text-[10px] font-medium uppercase tracking-wider text-muted-foreground">
              #{tile.queueIndex} · {tile.stepKindLabel}
            </div>
            <div className="truncate text-base font-semibold">{tile.stepName}</div>
          </div>
          <StepStatusPill status={tile.stepRun?.status ?? 1} />
          {tile.execution && <ExecStatusBadge status={execStatus} />}
          {tile.loopOutcome && (
            <span className="max-w-48 truncate rounded bg-cyan-100 px-1.5 py-0.5 text-[10px] font-medium text-cyan-900 dark:bg-cyan-950/60 dark:text-cyan-100" title={tile.loopOutcome}>
              {tile.loopOutcome}
            </span>
          )}
          <div className="flex items-center gap-1">
            <Button variant="ghost" size="sm" onClick={onClose} title="Shrink back to grid" aria-label="Shrink back to grid">
              <Minimize2 aria-hidden="true" className="h-4 w-4" />
            </Button>
            <Button variant="ghost" size="sm" onClick={onClose} title="Close" aria-label="Close">
              <X aria-hidden="true" className="h-4 w-4" />
            </Button>
          </div>
        </div>

        {/* body: chat + compact context/actions rail */}
        <div className="grid min-h-0 flex-1 gap-3 overflow-hidden p-3 lg:grid-cols-[1fr_280px]">
          <div className="min-h-0 overflow-auto">
            {execId ? (
              <SessionChatPane
                executionId={execId}
                events={events}
                streamStatus={status}
                storedOutput={exec?.output ?? tile.execution?.output}
                workerName={
                  exec?.workerName || tile.execution?.workerName || tile.execution?.workerId
                }
                systemPrompt={exec?.systemPrompt ?? tile.execution?.systemPrompt}
                isRunning={isRunning}
                isTerminal={isTerminal}
              />
            ) : (
              <div className="flex h-full items-center justify-center rounded-2xl border p-8 text-center text-sm text-muted-foreground">
                Upcoming — not run yet · #{tile.queueIndex} in DAG.
                <br />
                The session appears here once the reconciler dispatches this step.
              </div>
            )}
          </div>

          <aside className="min-h-0 space-y-3 overflow-auto">
            <div className="rounded-xl border p-3 text-sm">
              <div className="mb-1 text-xs font-semibold uppercase tracking-wider text-muted-foreground">
                Context
              </div>
              <dl className="space-y-1.5 text-xs">
                <div className="flex justify-between gap-2">
                  <dt className="text-muted-foreground">Worker</dt>
                  <dd className="truncate font-mono">
                    {(exec ?? tile.execution)?.workerName || (exec ?? tile.execution)?.workerId || "—"}
                    {!!(exec ?? tile.execution)?.workerVersion &&
                      ` v${(exec ?? tile.execution)?.workerVersion}`}
                  </dd>
                </div>
                <div className="flex justify-between gap-2">
                  <dt className="text-muted-foreground">Duration</dt>
                  <dd>
                    {tile.stepRun?.startedAt ? (
                      <LiveDuration startedAt={tile.stepRun.startedAt} endedAt={tile.stepRun.endedAt} />
                    ) : (
                      "—"
                    )}
                  </dd>
                </div>
                <div className="flex justify-between gap-2">
                  <dt className="text-muted-foreground">Execution</dt>
                  <dd className="truncate font-mono">{execId ? `${execId.slice(0, 12)}…` : "—"}</dd>
                </div>
                {hasRecords && (
                  <>
                    <div className="flex justify-between gap-2">
                      <dt className="text-muted-foreground">Cost</dt>
                      <dd className="font-mono">${cost.toFixed(4)}</dd>
                    </div>
                    <div className="flex justify-between gap-2">
                      <dt className="text-muted-foreground">Context</dt>
                      <dd className="font-mono">
                        {formatCompactTokens(workingSet)} tokens
                        {contextWindow > 0
                          ? ` · ${contextPct}%`
                          : " · window unknown"}
                      </dd>
                    </div>
                  </>
                )}
              </dl>
              {execId && (
                <Button
                  variant="outline"
                  size="sm"
                  className="mt-2 w-full"
                  onClick={() => {
                    // Tear down this modal's stream before the full page
                    // takes over in the new tab, so only ONE execution
                    // event stream lives across the two tabs (the full
                    // page's). Keeps window.open semantics — the run view
                    // stays preserved in tab A.
                    onClose();
                    window.open(`/executions/${execId}`, "_blank");
                  }}
                >
                  Open full execution page
                </Button>
              )}
            </div>

            {todos && todos.length > 0 && <TodoListCard todos={todos} />}

            {execId && !isTerminal && (
              <div className="flex flex-wrap gap-1.5">
                {isRunning && (
                  <Button size="sm" variant="outline" disabled={pauseExec.isPending} onClick={() => pauseExec.mutate(execId)}>
                    <Pause aria-hidden="true" className="mr-1 h-3 w-3" /> Pause
                  </Button>
                )}
                {isPaused && (
                  <Button size="sm" variant="outline" disabled={resumeExec.isPending} onClick={() => resumeExec.mutate(execId)}>
                    <Play aria-hidden="true" className="mr-1 h-3 w-3" /> Resume
                  </Button>
                )}
                <Button
                  size="sm"
                  variant="destructive"
                  disabled={cancelExec.isPending}
                  onClick={() => cancelExec.mutate({ id: execId })}
                >
                  <Square aria-hidden="true" className="mr-1 h-3 w-3" /> Cancel
                </Button>
              </div>
            )}

            {pendingApprovals && pendingApprovals.length > 0 && (
              <div className="rounded-xl border border-amber-300 bg-amber-50 p-3 dark:bg-amber-950/30">
                <div className="mb-1 text-xs font-semibold uppercase tracking-wider text-amber-800 dark:text-amber-200">
                  Pending approvals ({pendingApprovals.length})
                </div>
                <div className="space-y-2">
                  {pendingApprovals.map((req) => (
                    <div key={req.requestId} className="rounded-md border border-amber-200 bg-amber-100/40 p-2 dark:border-amber-800 dark:bg-amber-950/40">
                      <p className="text-xs font-medium">{req.toolCategory}</p>
                      <div className="mt-1 flex gap-1.5">
                        <Button
                          size="sm"
                          variant="outline"
                          className={cn("h-7 px-2 text-xs")}
                          disabled={approveToolCall.isPending}
                          onClick={() => approveToolCall.mutate({ requestId: req.requestId, approved: true })}
                        >
                          Approve
                        </Button>
                        <Button
                          size="sm"
                          variant="destructive"
                          className="h-7 px-2 text-xs"
                          disabled={approveToolCall.isPending}
                          onClick={() => approveToolCall.mutate({ requestId: req.requestId, approved: false })}
                        >
                          Deny
                        </Button>
                      </div>
                    </div>
                  ))}
                </div>
              </div>
            )}
          </aside>
        </div>
      </div>
    </div>
  );
}
