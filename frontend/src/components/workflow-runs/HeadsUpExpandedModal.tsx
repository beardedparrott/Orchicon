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
//   - the stream is gated on liveness (executionStreamEnabled), so a finished execution holds nothing.

import { useEffect, useState } from "react";
import { X } from "lucide-react";

import { executionKeys, useGetExecution, useStreamExecutionEvents } from "@/api/executions";
import { usageKeys } from "@/api/aigateway";
import { useDebouncedInvalidation } from "@/lib/useDebouncedInvalidation";
import { executionStreamEnabled } from "@/lib/debouncedInvalidation";
import type { StreamStatus } from "@/api/useStream";
import { SessionChatPane } from "@/components/executions/SessionChatPane";
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
          <Button variant="ghost" size="icon" onClick={onClose} aria-label="Close">
            <X className="h-4 w-4" aria-hidden="true" />
          </Button>
        </div>
        <div className="min-h-0 flex-1 overflow-y-auto p-3">
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
      </div>
    </div>
  );
}
