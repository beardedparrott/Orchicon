// HeadsUpTile — one DAG step in the run Heads-Up Dashboard.
//
// Summary rules (the tile contract). THE TILE IS STATIC — it holds no stream and no
// timer. See the rationale above the tail fetch for why the grid gave up liveness; the
// short version is that N timers behind a grid of thumbnails is what made this view hang.
//   - ACTIVE tile (stepRun.status === RUNNING): the last durable `kind:text` transcript
//     block (extractLastTextBlock), falling back to a "waiting for first output" line —
//     plus the pulsing ring + `live` badge, which are facts about the RUN, not liveness.
//   - DONE tiles: the last durable `kind:text` transcript block
//     (extractLastTextBlock), falling back to the step-run `_summary`
//     head — never streaming chunks.
//   - UPCOMING tiles (no execution): dimmed
//     "Upcoming — not run yet · #N in DAG".
//   - APPROVAL steps: compact approval state + Approve/Reject/Retry.
//   - loop_decision: the loopOutcome tag (mirror of the graph node).
import { useMemo } from "react";
import { CheckCircle2, RefreshCw, XCircle } from "lucide-react";

import { useApproveStep } from "@/api/approvals";
import { useRetryStepRun } from "@/api/workflows";
import { useGetExecutionSessionTail } from "@/api/executions";
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

export function HeadsUpTile({ tile, runId }: HeadsUpTileProps) {
  const execId = tile.execution?.id ?? "";
  // THE TAIL, NOT THE TRANSCRIPT. This tile renders one line — the last thing the worker
  // said (extractLastTextBlock scans backwards and stops at the first text part). Asking the
  // shared transcript hook for limit=10000 downloaded ~399 kB per tile per refetch to read a
  // ~34-byte block; on a many-step run, re-fetched twice a second, that is what hung the UI.
  // See useGetExecutionSessionTail.
  const { data: session } = useGetExecutionSessionTail(execId, Boolean(execId));
  // A TILE IS A THUMBNAIL, NOT A WINDOW. It holds NO stream and NO poll: it fetches the
  // tail ONCE (for its summary line) and then stays put.
  //
  // WHAT THIS REPLACES, and why reducing the payload was not enough. This tile used to hold
  // an execution event stream (for the one "active" tile) plus a 2s transcript poll (for
  // EVERY tile) — so opening the grid started N timers and a stream, all of them re-reading
  // and re-rendering while the operator looked at a grid of small cards. The earlier fix
  // shrank each fetch from 399 kB to a few kB, but a smaller payload re-requested every two
  // seconds from every tile is still a saturating load, and it is what made this view hang.
  //
  // LIVENESS WAS NEVER THE POINT HERE. An operator scanning a run wants to know which step
  // is where — the status chip and the last summary line answer that. Watching output
  // advance is what the EXPANDED tile and the execution page are for, and both own their own
  // stream for exactly one execution. So the grid gives up liveness deliberately: no stream
  // here, no timer here, nothing updating off-screen.
  const status = tileStatusStyle(tile.stepRun?.status ?? 1);
  const expandable = Boolean(tile.execution);
  const summary = useMemo(() => {
    if (tile.isUpcoming) return "";
    return (
      extractLastTextBlock(session, 600) || resultSummaryLine(tile.stepRun)
    );
  }, [session, tile.stepRun, tile.isUpcoming]);
  // A TILE OPENS THE EXECUTION PAGE — it does not expand into a live view.
  //
  // It used to open HeadsUpExpandedModal, which hosted the full SessionChatPane: a live event
  // STREAM plus five fetches, mounted on top of a run route that already polls. Clicking from
  // there to the execution page then left that stream alive behind the page being opened, on an
  // HTTP/1.1 origin with ~6 connections — which is the non-responsive page the operator hit
  // ("as soon as I clicked 'launch live execution page'"). The modal's only unique offering was
  // hosting the transcript, which is the execution page's own job, and the tile already carries
  // inline Approve/Reject/Retry for approval steps — so removing it costs no capability.
  //
  // An ANCHOR rather than a JS handler: a real link gives middle-click and cmd-click for free,
  // works before hydration, and cannot silently break the way a handler can.
  const Container = expandable ? "a" : "div";
  const href = expandable ? `/executions/${execId}` : undefined;
  return (
    <Container
      {...(expandable ? { href, target: "_blank", rel: "noopener noreferrer" } : {})}
      aria-label={expandable ? `Open the execution page for step ${tile.stepName}` : undefined}
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
          {/* NO "live" CHIP. It used to pulse here, and it read as "this card is updating" —
              which this card cannot do: it holds no stream and no poll, so between a step's
              writes nothing on it moves. The STATUS PILL above already says "running", which is
              the true statement about the run; a pulsing chip beside it only implied the card was
              live too. The operator called the pairing a misnomer, and this was half of it. */}
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
            {tile.upcomingReason || `Upcoming — not run yet · #${tile.queueIndex} in DAG`}
          </p>
        ) : tile.stepKind === 3 ? (
          <ApprovalTilePanel stepRun={tile.stepRun} runId={runId} summary={summary} />
        ) : tile.isActive ? (
          // SAY WHAT IS TRUE. This used to read "Worker starting — waiting for first output…",
          // which the operator flagged as "a misnomer" — and he was right, for a reason that is
          // about THIS card rather than about the run: the tile is a static snapshot (no stream,
          // no poll — see above), so nothing will appear here however long you wait. A message
          // promising output on a card that cannot produce it is the card lying about itself.
          //
          // The truth is: the RUN is still going, and this card is a snapshot of what has been
          // written to the step run SO FAR. The action that actually shows progress is opening
          // it, so the message names that instead of a wait.
          <p className="text-xs italic text-muted-foreground">
            {summary || "Running — no summary yet. Open it for live output."}
          </p>
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
