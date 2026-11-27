// HeadsUpExpandedModal — the quick-look window for one running step.
//
// THIS WAS REMOVED ONCE, AND THE REASON IT WAS REMOVED NO LONGER HOLDS.
//
// The modal hosted the full SessionChatPane: a live StreamExecutionEvents subscription plus fetches,
// mounted on top of a run route that already polls. On an HTTP/1.1 origin with ~6 connections per origin,
// clicking through to the execution page left that stream alive BEHIND the page being opened, and the
// operator got a page that never became responsive ("as soon as I clicked 'launch live execution page'").
// The cost was never the modal. It was that EVERY open execution view created its own JetStream consumer
// fed the entire `orchicon.events.execution.>` firehose and discarded other executions' events in Go.
//
// That is fixed at the source (internal/execution/hub.go): ONE plane-wide consumer, per-execution
// in-process fan-out, and the stream ENDS at terminal status. A modal now costs an in-process attach and
// one socket, and it releases both on unmount or when the step finishes.
//
// THE RULES THAT KEEP IT CHEAP, all deliberate:
//   - the caller renders it only while open, so a closed modal costs nothing;
//   - it is ONE execution, never a grid — the tile and grid stay static thumbnails, and
//     headsUpStatic.test.ts still forbids streams and timers in them;
//   - closing it unmounts the pane, and useStream aborts on unmount, so the socket is released rather
//     than left behind the page the operator navigates to next;
//   - the stream is gated on liveness (executionStreamEnabled), so a finished execution holds nothing;
//   - the STATISTICS PANEL (ExecutionContextSidebar, the same one /executions/$id renders) is fed by the
//     events this modal's ONE stream already delivers plus two plain queries (usage records, the work
//     item's declared context window). It opens no subscription of its own, and the usage query is the
//     one scheduleInvalidation already refreshes;
//   - "Open full page" is a real anchor to /executions/<id> in a NEW TAB, like the tile's: it cannot
//     leave this modal's socket alive behind the page being opened, which was the original complaint.

import { useEffect, useState } from "react";
import { ExternalLink, X } from "lucide-react";

import { executionKeys, useGetExecution, useStreamExecutionEvents } from "@/api/executions";
import { usageKeys, useGetUsage } from "@/api/aigateway";
import { useGetWorkItem } from "@/api/workItems";
import { useDebouncedInvalidation } from "@/lib/useDebouncedInvalidation";
import { executionStreamEnabled } from "@/lib/debouncedInvalidation";
import type { StreamStatus } from "@/api/useStream";
import { SessionChatPane } from "@/components/executions/SessionChatPane";
import { ExecutionContextSidebar } from "@/components/executions/ExecutionContextSidebar";
import { Button } from "@/components/ui/button";
import { LiveDuration } from "@/components/ui/live-duration";
import type { HeadsUpTileData } from "./headsUp";
import { ExecStatusBadge } from "./status";

export interface HeadsUpExpandedModalProps {
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

  // Stream events invalidate the detail/session/usage queries so the pane and its context go live while
  // running. Coalesced behind a trailing debounce so a token-frequency burst collapses to one batch.
  const scheduleInvalidation = useDebouncedInvalidation([
    executionKeys.detail(execId),
    executionKeys.session(execId),
    usageKeys.records(undefined, execId),
  ]);

  // Stream status is mirrored into state so useGetExecution's pollMs can depend on it (the stream hook
  // needs isTerminal from exec, which comes from useGetExecution — a cross-hook cycle broken here).
  const [streamStatus, setStreamStatus] = useState<StreamStatus>("idle");
  const { data: exec } = useGetExecution(execId, {
    pollMs: streamStatus === "open" ? 0 : 2_000,
  });
  const execStatus = exec?.status ?? tile.execution?.status ?? 0;
  const isRunning = execStatus === 2 || execStatus === 3;
  const isTerminal = execStatus === 7 || execStatus === 8 || execStatus === 9 || execStatus === 10;

  const { events, status } = useStreamExecutionEvents({
    executionId: execId,
    enabled: executionStreamEnabled(execId, isTerminal),
    onEvent: scheduleInvalidation,
  });
  useEffect(() => setStreamStatus(status), [status]);

  // The statistics panel's inputs — the same two the full page gives it. The work item's declared context
  // window is the fallback when the model is not in opencode model discovery (custom local providers).
  const { data: usage } = useGetUsage({ executionId: execId }, !!execId);
  const { data: workItem } = useGetWorkItem(exec?.taskId ?? "");
  const declaredContextWindow =
    workItem?.contextWindow && Number(workItem.contextWindow) > 0
      ? Number(workItem.contextWindow)
      : undefined;

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
        <div className="flex flex-wrap items-center gap-2 border-b border-border/60 px-4 py-2.5">
          <div className="min-w-0 flex-1">
            <div className="text-[10px] font-medium uppercase tracking-wider text-muted-foreground">
              {tile.stepKindLabel}
            </div>
            <div className="truncate text-base font-semibold">{tile.stepName}</div>
          </div>
          <ExecStatusBadge status={execStatus} />
          {isRunning ? <LiveDuration startedAt={exec?.startedAt} /> : null}
          {execId ? (
            <Button variant="outline" size="sm" asChild>
              <a
                href={`/executions/${execId}`}
                target="_blank"
                rel="noopener noreferrer"
                aria-label={`Open the full execution page for ${tile.stepName} in a new tab`}
              >
                <ExternalLink className="mr-1.5 h-3.5 w-3.5" aria-hidden="true" />
                Open full page
              </a>
            </Button>
          ) : null}
          <Button variant="ghost" size="icon" onClick={onClose} aria-label="Close">
            <X className="h-4 w-4" aria-hidden="true" />
          </Button>
        </div>
        {/* Two columns from `lg` up, each scrolling on its own (chat left, statistics right — the
            /executions/$id arrangement); stacked below it, where the whole body scrolls instead. */}
        <div className="grid min-h-0 flex-1 gap-3 overflow-y-auto p-3 lg:grid-cols-[minmax(0,1fr)_320px] lg:overflow-hidden">
          <div className="min-h-0 min-w-0 lg:overflow-y-auto">
            <SessionChatPane
              executionId={execId}
              events={events}
              streamStatus={status}
              storedOutput={exec?.output}
              workerName={exec?.workerName || exec?.workerId}
              systemPrompt={exec?.systemPrompt}
              isRunning={isRunning}
              isTerminal={isTerminal}
            />
          </div>
          <div className="min-h-0 lg:overflow-y-auto">
            {exec ? (
              <ExecutionContextSidebar
                exec={exec}
                events={events}
                usage={usage ?? []}
                contextWindow={declaredContextWindow}
                streamStatus={status}
                executionId={execId}
              />
            ) : (
              // Not yet loaded (or no execution behind this tile): say so instead of an empty column.
              <div
                className="rounded-2xl glass-panel p-4 text-sm text-muted-foreground"
                role="status"
              >
                {execId ? "Loading statistics…" : "No execution has started for this step."}
              </div>
            )}
          </div>
        </div>
      </div>
    </div>
  );
}
