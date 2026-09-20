// HeadsUpTile — one DAG step in the run Heads-Up Dashboard.
//
// Summary rules (the tile contract):
//   - ACTIVE tile (stepRun.status === RUNNING): mounts the grid's single
//     execution event stream (see HeadsUpGrid `liveStream`) + polls the
//     durable transcript every 2s, renders a live-tail mini block and a
//     pulsing emerald ring + `live` badge. NEVER raw chunk soup: reasoning
//     chunks are skipped, text is tailed to the last ~600 chars.
//   - DONE tiles: the last durable `kind:text` transcript block
//     (extractLastTextBlock), falling back to the step-run `_summary`
//     head — never streaming chunks.
//   - UPCOMING tiles (no execution): dimmed
//     "Upcoming — not run yet · #N in DAG".
//   - APPROVAL steps: compact approval state + Approve/Reject/Retry.
//   - loop_decision: the loopOutcome tag (mirror of the graph node).
import { useEffect, useMemo } from "react";
import { CheckCircle2, RefreshCw, XCircle } from "lucide-react";
import type { StreamExecutionEventsResponse } from "@/api/gen/orchicon/api/v1/execution_pb";

import { useApproveStep } from "@/api/approvals";
import { useRetryStepRun } from "@/api/workflows";
import { useGetExecutionSession, useStreamExecutionEvents } from "@/api/executions";
import { executionStreamEnabled } from "@/lib/debouncedInvalidation";
import {
  formatCompactTokens,
  useExecutionUsageSummary,
} from "./executionUsage";
import { Button } from "@/components/ui/button";
import { LiveDuration } from "@/components/ui/live-duration";
import { cn } from "@/lib/utils";
import {
  extractLastTextBlock,
  resultSummaryLine,
  tileStatusStyle,
  type HeadsUpTileData,
} from "./headsUp";
import { ExecStatusBadge } from "./status";

interface HeadsUpTileProps {
  tile: HeadsUpTileData;
  runId: string;
  /** Mount the execution event stream. The grid sets this on exactly one
   *  tile (the first running step with an execution) — the perf guard. */
  liveStream: boolean;
  onExpand: (tile: HeadsUpTileData) => void;
}

/** Tail of assistant text from live TELEMETRY events (reasoning skipped). */
function liveTailText(events: StreamExecutionEventsResponse[], maxChars = 600): string {
  const decode = new TextDecoder();
  let out = "";
  for (const resp of events) {
    const evt = resp.event;
    if (!evt || evt.eventType !== 2 || !evt.payload?.length) continue;
    try {
      const p = JSON.parse(decode.decode(evt.payload)) as { text?: unknown };
      if (typeof p.text !== "string" || !p.text) continue;
      if (p.text.startsWith("{")) {
        try {
          const inner = JSON.parse(p.text) as { kind?: unknown; text?: unknown };
          if (inner?.kind === "reasoning") continue; // noise in a mini tail
        } catch {
          /* not JSON — keep as plain text */
        }
      }
      out += p.text;
    } catch {
      /* unparseable event — skip */
    }
  }
  return out.length > maxChars ? "…" + out.slice(-maxChars) : out;
}

/** Compact cost + context line; mounted only when an execution exists so
 *  upcoming tiles never fire an unfiltered usage query. Aggregation mirrors
 *  ExecutionContextSidebar (peak fresh working set + summed cost) so the
 *  tile reads identically to the execution detail page. */
function TileUsage({ executionId }: { executionId: string }) {
  const { workingSet, cost, contextWindow, contextPct, hasRecords } =
    useExecutionUsageSummary(executionId);
  if (!hasRecords) return null;
  return (
    <span className="font-mono text-[11px] text-muted-foreground">
      ${cost.toFixed(4)} · {formatCompactTokens(workingSet)} tokens
      {contextWindow > 0 ? ` · ${contextPct}%` : ""}
    </span>
  );
}

export function HeadsUpTile({ tile, runId, liveStream, onExpand }: HeadsUpTileProps) {
  const execId = tile.execution?.id ?? "";
  const { data: session, refetch: refetchSession } = useGetExecutionSession(
    execId,
    Boolean(execId),
  );
  // Liveness gate: a terminal execution (7/8/9/10) never holds a stream
  // connection, even when it is the grid's designated live tile.
  const execStatus = tile.execution?.status ?? 0;
  const isTerminal =
    execStatus === 7 || execStatus === 8 || execStatus === 9 || execStatus === 10;
  const { events } = useStreamExecutionEvents({
    executionId: execId,
    enabled: liveStream && executionStreamEnabled(execId, isTerminal),
  });

  // Running tile: re-pull the durable transcript every 2s (the runner
  // flushes on that cadence) so the summary converges even between
  // stream bursts.
  useEffect(() => {
    if (!liveStream || !execId) return;
    const t = window.setInterval(() => void refetchSession(), 2000);
    return () => window.clearInterval(t);
  }, [liveStream, execId, refetchSession]);

  const status = tileStatusStyle(tile.stepRun?.status ?? 1);
  const expandable = Boolean(tile.execution);
  const summary = useMemo(() => {
    if (tile.isUpcoming) return "";
    return (
      extractLastTextBlock(session, 600) || resultSummaryLine(tile.stepRun)
    );
  }, [session, tile.stepRun, tile.isUpcoming]);
  const liveTail = liveStream ? liveTailText(events) : "";

  const Container = expandable ? "button" : "div";
  return (
    <Container
      type={expandable ? "button" : undefined}
      onClick={expandable ? () => onExpand(tile) : undefined}
      aria-label={expandable ? `Expand step ${tile.stepName}` : undefined}
      className={cn(
        "flex min-h-[190px] flex-col gap-2 rounded-xl border bg-card p-3 text-left shadow-sm",
        tile.isActive && "ring-2 ring-emerald-500 shadow-lg",
        tile.isUpcoming && "opacity-60",
        expandable && "cursor-pointer hover:border-primary/50 focus:outline-none focus-visible:ring-2 focus-visible:ring-ring",
      )}
    >
      <div className="flex items-start justify-between gap-2">
        <div className="min-w-0">
          <div className="text-[10px] font-medium uppercase tracking-wider text-muted-foreground">
            #{tile.queueIndex} · {tile.stepKindLabel}
          </div>
          <div className="truncate text-sm font-semibold" title={tile.stepName}>
            {tile.stepName}
          </div>
        </div>
        <div className="flex shrink-0 flex-col items-end gap-1">
          <span
            className={cn(
              "rounded-full px-2 py-0.5 text-xs font-medium",
              status.className,
            )}
          >
            {status.label}
          </span>
          {tile.isActive && (
            <span className="inline-flex items-center gap-1 rounded-full bg-emerald-500 px-2 py-0.5 text-[10px] font-bold uppercase text-white">
              <span className="inline-block h-1.5 w-1.5 animate-pulse rounded-full bg-white" />
              live
            </span>
          )}
        </div>
      </div>

      {tile.execution && (
        <div className="flex flex-wrap items-center gap-x-2 gap-y-1 text-xs">
          <span className="truncate font-medium">
            {tile.execution.workerName || tile.execution.workerId}
            {!!tile.execution.workerVersion && (
              <span className="text-muted-foreground"> v{tile.execution.workerVersion}</span>
            )}
          </span>
          <ExecStatusBadge status={tile.execution.status} />
        </div>
      )}

      <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
        {tile.stepRun?.startedAt && (
          <LiveDuration startedAt={tile.stepRun.startedAt} endedAt={tile.stepRun.endedAt} />
        )}
        {execId && <TileUsage executionId={execId} />}
      </div>

      {tile.loopOutcome && (
        <div
          className="inline-block max-w-full truncate self-start rounded bg-cyan-100 px-1.5 py-0.5 text-[10px] font-medium text-cyan-900 dark:bg-cyan-950/60 dark:text-cyan-100"
          title={tile.loopOutcome}
        >
          {tile.loopOutcome}
        </div>
      )}

      <div className="min-h-[3rem] flex-1">
        {tile.isUpcoming ? (
          <p className="text-xs italic text-muted-foreground">
            Upcoming — not run yet · #{tile.queueIndex} in DAG
          </p>
        ) : tile.stepKind === 3 ? (
          <ApprovalTilePanel stepRun={tile.stepRun} runId={runId} summary={summary} />
        ) : liveStream ? (
          <div className="space-y-1">
            {liveTail ? (
              <pre className="max-h-28 overflow-hidden whitespace-pre-wrap break-words font-mono text-[11px] leading-snug text-foreground/90">
                {liveTail}
              </pre>
            ) : (
              <p className="text-xs italic text-muted-foreground">
                {summary || "Worker starting — waiting for first output…"}
              </p>
            )}
          </div>
        ) : (
          <p className="line-clamp-6 max-h-36 overflow-hidden whitespace-pre-wrap break-words text-xs leading-relaxed text-foreground/85">
            {summary || "No summary yet."}
          </p>
        )}
      </div>

      <div className="flex items-center justify-between border-t border-border/50 pt-1.5">
        <span className="font-mono text-[10px] text-muted-foreground/70">
          {execId ? `exec ${execId.slice(0, 12)}…` : tile.stepRun ? `step ${tile.stepRun.id.slice(0, 12)}…` : "no step run"}
        </span>
        {expandable && (
          <span className="text-[10px] font-medium text-muted-foreground">expand ⤢</span>
        )}
      </div>
    </Container>
  );
}

/** Compact approval state + inline Approve/Reject/Retry for a tile. */
function ApprovalTilePanel({
  stepRun,
  runId,
  summary,
}: {
  stepRun: HeadsUpTileData["stepRun"];
  runId: string;
  summary: string;
}) {
  const approve = useApproveStep();
  const retry = useRetryStepRun();
  if (!stepRun) return <p className="text-xs italic text-muted-foreground">No step run yet.</p>;
  const isPending = stepRun.status === 8;
  let decision = "";
  try {
    const r = JSON.parse(stepRun.result ?? "{}") as { _decision?: unknown };
    if (typeof r._decision === "string") decision = r._decision;
  } catch {
    /* ignore */
  }
  return (
    <div
      className="space-y-1.5 rounded-md border border-yellow-300/60 bg-yellow-50/50 p-2 dark:border-yellow-800/60 dark:bg-yellow-950/20"
      onClick={(e) => e.stopPropagation()}
    >
      <div className="text-[10px] font-bold uppercase tracking-wider text-amber-700 dark:text-amber-300">
        {isPending ? "Approval pending" : decision ? `Decision: ${decision}` : "Approval step"}
      </div>
      {summary && (
        <p className="line-clamp-3 text-xs text-foreground/80">{summary}</p>
      )}
      {isPending && (
        <div className="flex flex-wrap gap-1">
          <Button
            size="sm"
            className="h-7 px-2 text-xs"
            disabled={approve.isPending}
            onClick={() =>
              approve.mutate({ stepRunId: stepRun.id, approved: true, reason: "", reviewedBy: "" })
            }
          >
            <CheckCircle2 aria-hidden="true" className="mr-1 h-3 w-3" />
            Approve
          </Button>
          <Button
            size="sm"
            variant="destructive"
            className="h-7 px-2 text-xs"
            disabled={approve.isPending}
            onClick={() =>
              approve.mutate({ stepRunId: stepRun.id, approved: false, reason: "", reviewedBy: "" })
            }
          >
            <XCircle aria-hidden="true" className="mr-1 h-3 w-3" />
            Reject
          </Button>
          <Button
            size="sm"
            variant="outline"
            className="h-7 px-2 text-xs"
            disabled={retry.isPending}
            onClick={() => retry.mutate({ stepRunId: stepRun.id, workflowRunId: runId })}
          >
            <RefreshCw aria-hidden="true" className="mr-1 h-3 w-3" />
            Retry
          </Button>
        </div>
      )}
    </div>
  );
}
