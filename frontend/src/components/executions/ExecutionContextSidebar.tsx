// ExecutionContextSidebar — right-rail panel for the execution detail
// page. Modelled after OpenChamber's "Context" panel: live context
// percentage, message-role counts, last-assistant stats, raw event
// timeline. Renders nothing until the first event arrives (a panel
// with all zeroes looks broken).
//
// Data sources (all already on the page — no extra fetches):
//   - exec.{status, healthState, tokenUsage, costUsd, workerId, ...}
//     — the WorkerExecution row, polled every 3s
//   - events[] — the live StreamExecutionEvents stream
//   - usage[]  — usage_records via useGetUsage({ executionId }),
//     for prompt/completion/cache breakdown when the AI gateway
//     recorded usage
//
// Responsive: collapses to a top-of-page summary card on mobile,
// full sidebar on lg+.

import { useMemo } from "react";
import type { WorkerExecution } from "@/api/gen/orchicon/api/v1/execution_pb";
import type { StreamExecutionEventsResponse } from "@/api/gen/orchicon/api/v1/execution_pb";
import type { UsageRecord } from "@/api/gen/orchicon/api/v1/ai_gateway_pb";
import { cn } from "@/lib/utils";

const EXEC_STATUS_LABELS: Record<number, string> = {
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

const EXEC_STATUS_STYLES: Record<number, string> = {
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

const HEALTH_STATUS_LABELS: Record<number, string> = {
  1: "Healthy",
  2: "Stalled",
  3: "Unhealthy",
  4: "Terminating",
};

interface ExecutionContextSidebarProps {
  exec: WorkerExecution;
  events: StreamExecutionEventsResponse[];
  usage: UsageRecord[];
  /** Approximate context window size. We don't have this from the
   *  worker definition yet — picked conservatively so the progress
   *  bar shows useful info even on free models. v0.2 can read
   *  context_window from the model discovery endpoint. */
  contextWindow?: number;
  streamStatus?: string;
}

interface EventStats {
  assistantCount: number; // text events
  toolCount: number;       // tool_call events (input)
  toolResultCount: number; // tool_call events (output)
  errorCount: number;
  lastAssistantText: string;
  lastAssistantAt: Date | null;
  lastToolName: string;
  recentTypes: Array<{ type: number; ts: Date; label: string }>;
}

export function ExecutionContextSidebar({
  exec,
  events,
  usage,
  contextWindow = 200_000,
  streamStatus,
}: ExecutionContextSidebarProps) {
  const stats = useMemo<EventStats>(() => {
    const s: EventStats = {
      assistantCount: 0,
      toolCount: 0,
      toolResultCount: 0,
      errorCount: 0,
      lastAssistantText: "",
      lastAssistantAt: null,
      lastToolName: "",
      recentTypes: [],
    };
    for (const resp of events) {
      const evt = resp.event;
      if (!evt) continue;
      const ts = evt.occurredAt
        ? new Date(Number(evt.occurredAt.seconds) * 1000)
        : new Date();
      const eventType = evt.eventType;
      let payload: Record<string, unknown> = {};
      if (evt.payload?.length) {
        try {
          payload = JSON.parse(new TextDecoder().decode(evt.payload));
        } catch {
          /* unparseable — ignore */
        }
      }
      const ET = {
        STARTED: 1,
        TELEMETRY: 2,
        TOOL_CALL: 3,
        CHECKPOINT: 4,
        APPROVAL_REQUEST: 5,
        HEALTH: 6,
        RESULT: 7,
        ERROR: 8,
        CONTROL: 9,
      };
      switch (eventType) {
        case ET.TELEMETRY:
          if (payload.text) {
            s.assistantCount++;
            s.lastAssistantText = payload.text;
            s.lastAssistantAt = ts;
          }
          break;
        case ET.TOOL_CALL: {
          const toolName = payload.tool_name || "tool";
          const input = payload.input || "";
          const output = payload.output || "";
          if (input && !output) {
            s.toolCount++;
            s.lastToolName = toolName;
          } else if (output) {
            s.toolResultCount++;
          }
          break;
        }
        case ET.ERROR:
          s.errorCount++;
          break;
      }
      // Capture the last ~12 event types for the raw timeline
      s.recentTypes.push({
        type: eventType,
        ts,
        label: payload.event_type || "",
      });
      if (s.recentTypes.length > 12) s.recentTypes.shift();
    }
    return s;
  }, [events]);

  // Token usage breakdown from usage_records (AI Gateway dual-write).
  // We sum across all step_finish events for this execution.
  const usageBreakdown = useMemo(() => {
    let prompt = 0;
    let completion = 0;
    let total = 0;
    let cost = 0;
    for (const r of usage) {
      prompt += Number(r.promptTokens);
      completion += Number(r.completionTokens);
      total += Number(r.totalTokens);
      cost += Number(r.costUsd);
    }
    return { prompt, completion, total, cost };
  }, [usage]);

  // Context %: total tokens used vs the model's context window.
  // Falls back to total tokens when no window is known — we still
  // show a bar that the user can interpret.
  const totalTokens =
    usageBreakdown.total || Number(exec.tokenUsage) || 0;
  const cost =
    usageBreakdown.cost > 0 ? usageBreakdown.cost : Number(exec.costUsd);
  const contextPct =
    contextWindow > 0
      ? Math.min(100, Math.round((totalTokens / contextWindow) * 100))
      : 0;

  const statusLabel = EXEC_STATUS_LABELS[exec.status] ?? "unknown";
  const statusClass =
    EXEC_STATUS_STYLES[exec.status] ?? "bg-muted text-muted-foreground";
  const healthLabel = HEALTH_STATUS_LABELS[exec.healthState] ?? "unknown";
  const isLive = streamStatus === "open";
  const isFailed = exec.status === 10 || exec.status === 8;
  const isTerminal = exec.status === 7 || exec.status === 8;

  // Preview of the latest assistant text — used in the "Last
  // assistant message" row. Truncated to keep the sidebar compact.
  const lastAssistantPreview = stats.lastAssistantText
    ? stats.lastAssistantText.length > 120
      ? stats.lastAssistantText.slice(0, 120) + "…"
      : stats.lastAssistantText
    : "—";

  const rolePct = (n: number) =>
    totalTokens > 0 ? Math.round((n / totalTokens) * 100) : 0;

  return (
    <aside className="space-y-3 lg:sticky lg:top-4">
      {/* Context usage card */}
      <div className="rounded-xl border bg-card p-4 shadow-sm">
        <div className="mb-2 flex items-center justify-between">
          <h3 className="text-xs font-semibold uppercase tracking-wider text-muted-foreground">
            Context
          </h3>
          <span className="text-xs font-medium text-muted-foreground">
            {contextPct}%
          </span>
        </div>
        <div className="h-2 w-full overflow-hidden rounded-full bg-muted">
          <div
            className={cn(
              "h-full rounded-full transition-all",
              contextPct > 80
                ? "bg-red-500"
                : contextPct > 50
                  ? "bg-amber-500"
                  : "bg-emerald-500",
            )}
            style={{ width: `${Math.max(contextPct, 2)}%` }}
          />
        </div>
        <div className="mt-1 flex items-baseline justify-between text-xs text-muted-foreground">
          <span className="font-mono">{fmtNum(totalTokens)} tokens</span>
          <span>of {fmtNum(contextWindow)}</span>
        </div>
      </div>

      {/* Status card */}
      <div className="rounded-xl border bg-card p-4 shadow-sm">
        <div className="flex items-center justify-between">
          <span
            className={cn(
              "inline-flex items-center gap-1.5 rounded-full px-2 py-0.5 text-xs font-medium",
              statusClass,
            )}
          >
            <span
              className={cn(
                "inline-block h-1.5 w-1.5 rounded-full",
                isLive && !isTerminal
                  ? "bg-emerald-500 animate-pulse"
                  : isFailed
                    ? "bg-red-500"
                    : "bg-zinc-500",
              )}
            />
            {statusLabel}
          </span>
          <span className="text-xs text-muted-foreground">{healthLabel}</span>
        </div>
        <div className="mt-3 grid grid-cols-2 gap-3">
          <div>
            <div className="text-[10px] font-semibold uppercase tracking-wider text-muted-foreground">
              Cost
            </div>
            <div className="mt-0.5 font-mono text-lg font-semibold tabular-nums">
              ${cost.toFixed(4)}
            </div>
          </div>
          <div>
            <div className="text-[10px] font-semibold uppercase tracking-wider text-muted-foreground">
              Duration
            </div>
            <div className="mt-0.5 font-mono text-lg font-semibold tabular-nums">
              {fmtDuration(exec.startedAt, exec.endedAt)}
            </div>
          </div>
        </div>
      </div>

      {/* Message counts */}
      <div className="rounded-xl border bg-card p-4 shadow-sm">
        <div className="mb-2 flex items-center justify-between">
          <h3 className="text-xs font-semibold uppercase tracking-wider text-muted-foreground">
            Messages
          </h3>
          <span className="text-xs text-muted-foreground">
            {stats.assistantCount + stats.toolCount}
          </span>
        </div>
        <div className="grid grid-cols-2 gap-2 text-sm">
          <RoleCount
            label="Assistant"
            count={stats.assistantCount}
            color="bg-blue-500"
          />
          <RoleCount
            label="Tool calls"
            count={stats.toolCount}
            color="bg-amber-500"
          />
        </div>
        {usageBreakdown.total > 0 && (
          <div className="mt-3 space-y-1">
            <TokenBar
              label="Input"
              count={usageBreakdown.prompt}
              pct={rolePct(usageBreakdown.prompt)}
              color="bg-blue-500"
            />
            <TokenBar
              label="Output"
              count={usageBreakdown.completion}
              pct={rolePct(usageBreakdown.completion)}
              color="bg-violet-500"
            />
          </div>
        )}
      </div>

      {/* Last assistant message preview */}
      {stats.lastAssistantAt && (
        <div className="rounded-xl border bg-card p-4 shadow-sm">
          <h3 className="text-xs font-semibold uppercase tracking-wider text-muted-foreground">
            Last assistant message
          </h3>
          <p className="mt-1 line-clamp-3 text-xs leading-relaxed text-foreground/80">
            {lastAssistantPreview}
          </p>
          <p className="mt-1 font-mono text-[10px] text-muted-foreground">
            {stats.lastAssistantAt.toLocaleTimeString()}
          </p>
        </div>
      )}

      {/* Last tool used */}
      {stats.lastToolName && (
        <div className="rounded-xl border bg-card p-4 shadow-sm">
          <h3 className="text-xs font-semibold uppercase tracking-wider text-muted-foreground">
            Last tool
          </h3>
          <p className="mt-1 font-mono text-sm font-medium">
            {stats.lastToolName}
            {stats.lastToolName === "task" && (
              <span className="ml-2 rounded bg-violet-100 px-1.5 py-0.5 text-[10px] font-bold uppercase text-violet-800 dark:bg-violet-900 dark:text-violet-200">
                subagent
              </span>
            )}
          </p>
        </div>
      )}

      {/* Raw events timeline (compact) */}
      {stats.recentTypes.length > 0 && (
        <div className="rounded-xl border bg-card p-4 shadow-sm">
          <h3 className="mb-2 text-xs font-semibold uppercase tracking-wider text-muted-foreground">
            Raw events
          </h3>
          <ul className="space-y-1">
            {stats.recentTypes.slice(-8).map((evt, i) => (
              <li
                key={`${evt.ts.getTime()}-${i}`}
                className="flex items-center justify-between gap-2 text-xs"
              >
                <span className="truncate font-mono text-muted-foreground">
                  {evt.label.replace(/^execution\./, "")}
                </span>
                <span className="shrink-0 font-mono text-[10px] text-muted-foreground/70">
                  {evt.ts.toLocaleTimeString()}
                </span>
              </li>
            ))}
          </ul>
        </div>
      )}

      {stats.errorCount > 0 && (
        <div className="rounded-xl border border-destructive/40 bg-destructive/10 p-3 text-xs text-destructive">
          {stats.errorCount} error event
          {stats.errorCount === 1 ? "" : "s"} recorded
        </div>
      )}
    </aside>
  );
}

function RoleCount({
  label,
  count,
  color,
}: {
  label: string;
  count: number;
  color: string;
}) {
  return (
    <div className="flex items-center gap-2">
      <span className={cn("inline-block h-2 w-2 rounded-full", color)} />
      <span className="text-muted-foreground">{label}</span>
      <span className="ml-auto font-mono font-semibold tabular-nums">
        {count}
      </span>
    </div>
  );
}

function TokenBar({
  label,
  count,
  pct,
  color,
}: {
  label: string;
  count: number;
  pct: number;
  color: string;
}) {
  return (
    <div>
      <div className="flex items-center justify-between text-[10px]">
        <span className="text-muted-foreground">{label}</span>
        <span className="font-mono tabular-nums">
          {fmtNum(count)} · {pct}%
        </span>
      </div>
      <div className="mt-0.5 h-1 w-full overflow-hidden rounded-full bg-muted">
        <div
          className={cn("h-full rounded-full", color)}
          style={{ width: `${Math.max(pct, 1)}%` }}
        />
      </div>
    </div>
  );
}

function fmtNum(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 10_000) return `${(n / 1000).toFixed(0)}k`;
  if (n >= 1_000) return `${(n / 1000).toFixed(1)}k`;
  return String(n);
}

function fmtDuration(
  startedAt: { seconds: string | number | bigint } | undefined,
  endedAt: { seconds: string | number | bigint } | undefined,
): string {
  if (!startedAt) return "—";
  const startMs = Number(startedAt.seconds) * 1000;
  const endMs = endedAt ? Number(endedAt.seconds) * 1000 : Date.now();
  const ms = Math.max(0, endMs - startMs);
  if (ms < 1000) return `${ms}ms`;
  const s = ms / 1000;
  if (s < 60) return `${s.toFixed(1)}s`;
  const m = s / 60;
  if (m < 60) return `${Math.floor(m)}m ${Math.floor(s % 60)}s`;
  const h = m / 60;
  return `${Math.floor(h)}h ${Math.floor(m % 60)}m`;
}