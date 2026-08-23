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
import type { TodoItem } from "@/api/gen/orchicon/api/v1/execution_pb";
import { TodoStatus } from "@/api/gen/orchicon/api/v1/execution_pb";
import { TodoPriority } from "@/api/gen/orchicon/api/v1/execution_pb";
import { useGetExecutionSession, useGetExecutionTodos } from "@/api/executions";
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
  /** Execution id — used to backfill tool/message counts from the durable
   *  session transcript when the live event stream is empty (a completed
   *  session execution viewed after reload). */
  executionId?: string;
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
  executionId,
}: ExecutionContextSidebarProps) {
  const { data: transcript } = useGetExecutionSession(executionId ?? "", Boolean(executionId));
  const execTerminal = exec.status === 7 || exec.status === 8 || exec.status === 9 || exec.status === 10;
  const { data: todos } = useGetExecutionTodos(executionId ?? "", execTerminal ? 0 : 2000);
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
          payload = JSON.parse(new TextDecoder().decode(evt.payload)) as Record<string, unknown>;
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
        case ET.TELEMETRY: {
          let text = payload.text as string | undefined;
          if (text && text.startsWith("{")) {
            try {
              const parsed = JSON.parse(text);
              if (parsed && parsed.kind === "reasoning" && typeof parsed.text === "string") {
                // Streamed reasoning chunk — unwrap for display.
                text = parsed.text;
              }
            } catch {
              /* keep raw */
            }
          }
          if (text) {
            s.assistantCount++;
            s.lastAssistantText = text;
            s.lastAssistantAt = ts;
          }
          break;
        }
        case ET.TOOL_CALL: {
          const toolName = (payload.tool_name as string) || "tool";
          const input = (payload.input as string) || "";
          const output = (payload.output as string) || "";
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
        label: (payload.event_type as string) || "",
      });
      if (s.recentTypes.length > 12) s.recentTypes.shift();
    }
    // A completed session execution viewed after reload has no live event
    // stream; backfill counts from the durable transcript so the sidebar
    // still shows the worker's tool calls and messages.
    if (s.toolCount === 0 && transcript?.length) {
      for (const p of transcript) {
        if (p.kind === "tool_use") {
          s.toolCount++;
          if (!s.lastToolName) s.lastToolName = "tool";
        } else if (p.kind === "text") {
          s.assistantCount++;
        }
      }
    }
    return s;
  }, [events, transcript]);

  // Token usage breakdown from usage_records (AI Gateway dual-write).
  // `total` is the CUMULATIVE re-send sum (each usage_records row is the
  // full per-step_finish request size, so the sum grows with every model
  // call — a transport/spend figure, NOT the model's working set).
  // `peak` is the LARGEST single-step total_tokens — the actual
  // current/peak context window used by the model. The Context card and
  // its % bar key off `peak` (bounded, interpretable); the cumulative
  // figure is surfaced separately and labelled as cumulative.
  const usageBreakdown = useMemo(() => {
    let prompt = 0;
    let completion = 0;
    let cacheRead = 0;
    let cacheWrite = 0;
    let reasoning = 0;
    let total = 0;
    let peak = 0;
    let cost = 0;
    for (const r of usage) {
      prompt += Number(r.promptTokens);
      completion += Number(r.completionTokens);
      cacheRead += Number(r.cacheReadTokens) || 0;
      cacheWrite += Number(r.cacheWriteTokens) || 0;
      reasoning += Number(r.reasoningTokens) || 0;
      total += Number(r.totalTokens);
      const row = Number(r.totalTokens);
      if (row > peak) peak = row;
      cost += Number(r.costUsd);
    }
    return { prompt, completion, cacheRead, cacheWrite, reasoning, total, peak, cost };
  }, [usage]);

  // Context %: the PEAK single-step total_tokens (the model's working set)
  // vs the context window. Falls back to exec.tokenUsage when no usage
  // records exist. This is what makes a long-lived worker never show
  // "context > window" from cumulative re-sends.
  const totalTokens = usageBreakdown.peak || Number(exec.tokenUsage) || 0;
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
        {usageBreakdown.total > totalTokens && (
          <div className="mt-0.5 text-[10px] text-muted-foreground">
            Cumulative input consumed: {fmtNum(usageBreakdown.total)} tokens
          </div>
        )}
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
            {usageBreakdown.cacheRead > 0 && (
              <TokenBar
                label="Cache read"
                count={usageBreakdown.cacheRead}
                pct={rolePct(usageBreakdown.cacheRead)}
                color="bg-emerald-500"
              />
            )}
            {usageBreakdown.cacheWrite > 0 && (
              <TokenBar
                label="Cache write"
                count={usageBreakdown.cacheWrite}
                pct={rolePct(usageBreakdown.cacheWrite)}
                color="bg-teal-500"
              />
            )}
            {usageBreakdown.reasoning > 0 && (
              <TokenBar
                label="Reasoning"
                count={usageBreakdown.reasoning}
                pct={rolePct(usageBreakdown.reasoning)}
                color="bg-amber-500"
              />
            )}
          </div>
        )}
      </div>

      {/* Todo List — the worker's live task list from the latest todowrite
          tool call. Hidden entirely when the worker never recorded one (or
          the execution predates the feature) so the sidebar stays clean. */}
      {todos && todos.length > 0 && (
        <TodoListCard todos={todos} />
      )}

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

// TodoListCard — the worker's live task list from the latest todowrite
// tool call. Renders an X/Y completed counter, a thin progress bar (mirroring
// the Context card), and per-status item rows. Called only when the worker
// actually recorded a todo list (non-empty).
function TodoListCard({ todos }: { todos: TodoItem[] }) {
  const completed = todos.filter(
    (t) => t.status === TodoStatus.COMPLETED,
  ).length;
  const total = todos.length;
  const pct = total > 0 ? Math.round((completed / total) * 100) : 0;
  return (
    <div className="rounded-xl border bg-card p-4 shadow-sm">
      <div className="mb-2 flex items-center justify-between">
        <h3 className="text-xs font-semibold uppercase tracking-wider text-muted-foreground">
          Todo List
        </h3>
        <span className="text-xs font-medium text-muted-foreground">
          {completed}/{total}
        </span>
      </div>
      <div className="h-1.5 w-full overflow-hidden rounded-full bg-muted">
        <div
          className={cn(
            "h-full rounded-full transition-all",
            pct === 100 ? "bg-emerald-500" : "bg-blue-500",
          )}
          style={{ width: `${Math.max(pct, 2)}%` }}
        />
      </div>
      <ul className="mt-3 space-y-1.5">
        {todos.map((todo, i) => (
          <TodoRow key={i} todo={todo} />
        ))}
      </ul>
    </div>
  );
}

// TodoRow renders a single todo item with a status indicator: completed
// (checked + strikethrough), in_progress (pulsing dot + highlighted),
// pending (unchecked circle), cancelled (dimmed/struck). UNSPECIFIED status
// renders as pending (forward-compatible).
function TodoRow({ todo }: { todo: TodoItem }) {
  const status = todo.status;
  const isCompleted = status === TodoStatus.COMPLETED;
  const isInProgress = status === TodoStatus.IN_PROGRESS;
  const isCancelled = status === TodoStatus.CANCELLED;
  const priority =
    todo.priority === TodoPriority.HIGH
      ? "high"
      : todo.priority === TodoPriority.MEDIUM
        ? "med"
        : todo.priority === TodoPriority.LOW
          ? "low"
          : "";
  return (
    <li className="flex items-start gap-2 text-sm">
      <span
        className={cn(
          "mt-0.5 flex h-4 w-4 shrink-0 items-center justify-center rounded-full border",
          isCompleted && "border-emerald-500 bg-emerald-500 text-white",
          isInProgress && "border-amber-500 bg-amber-100 dark:bg-amber-950",
          !isCompleted && !isInProgress && !isCancelled && "border-muted-foreground/40",
          isCancelled && "border-muted-foreground/30 bg-muted",
        )}
      >
        {isCompleted && (
          <svg
            className="h-3 w-3"
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            strokeWidth={3}
            strokeLinecap="round"
            strokeLinejoin="round"
          >
            <path d="M20 6 9 17l-5-5" />
          </svg>
        )}
        {isInProgress && (
          <span className="h-2 w-2 rounded-full bg-amber-500 animate-pulse" />
        )}
      </span>
      <span
        className={cn(
          "min-w-0 flex-1 leading-snug",
          isCompleted && "text-muted-foreground line-through",
          isInProgress && "font-medium text-foreground",
          isCancelled && "text-muted-foreground/60 line-through",
          !isCompleted && !isInProgress && !isCancelled && "text-foreground/80",
        )}
      >
        {todo.content}
        {priority && (
          <span className="ml-1.5 text-[10px] font-medium uppercase tracking-wide text-muted-foreground/70">
            {priority}
          </span>
        )}
      </span>
    </li>
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