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
import { useListOpenCodeModels } from "@/api/aigateway";
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
  /** Optional override for the model's context window. When omitted, the
   *  panel resolves the real window from the model that actually ran (usage
   *  records' provider+model → model discovery limits.context). If it can't
   *  be resolved, the panel shows the working set without a fabricated
   *  percentage/denominator rather than guessing. */
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
  contextWindow,
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

  // Resolve the REAL model context window from the model that actually ran.
  // The usage records carry provider+model; we match that against model
  // discovery's opencode model and take limits.context. When the panel
  // received an explicit contextWindow prop (or the model can't be
  // resolved), fall back to that / unknown rather than fabricating a window.
  const { data: models } = useListOpenCodeModels();
  const resolvedContextWindow = useMemo(() => {
    if (contextWindow && contextWindow > 0) return contextWindow;
    if (!models || models.length === 0) return 0;
    // Pick the model from the most recent usage record (provider+model).
    const latest = usage[usage.length - 1];
    if (!latest?.provider || !latest?.model) return 0;
    const ref = `${latest.provider}/${latest.model}`;
    const found =
      models.find((m) => m.modelRef === ref) ??
      models.find((m) => m.id === latest.model);
    const ctx = found?.limits?.context ? Number(found.limits.context) : 0;
    return ctx > 0 ? ctx : 0;
  }, [contextWindow, models, usage]);

  // Token usage breakdown from usage_records (AI Gateway dual-write).
  // `workingSet` is the PEAK single-step FRESH token count
  // (prompt+completion+reasoning, cache EXCLUDED) — the largest working set
  // the model actually held in a single step. It is bounded and
  // interpretable, and it is what the Context % bar is measured against the
  // (real) context window. Cache reads are re-sends of already-counted
  // tokens, so they are deliberately NOT part of the working set — they live
  // in the cumulative cache-transport section instead.
  //
  // `entered` is the CUMULATIVE re-send sum (each usage_records row is the
  // full per-step_finish request size, so the sum grows with every model
  // call — a transport/spend figure, NOT the model's working set). It is
  // surfaced separately as the cumulative bar.
  const usageBreakdown = useMemo(() => {
    let prompt = 0;
    let completion = 0;
    let cacheRead = 0;
    let cacheWrite = 0;
    let reasoning = 0;
    let entered = 0;
    let workingSet = 0;
    let cost = 0;
    let cumCacheRead = 0;
    let cumCacheWrite = 0;
    let cumPrompt = 0;
    let peakPrompt = 0;
    let peakCompletion = 0;
    let peakReasoning = 0;
    for (const r of usage) {
      const p = Number(r.promptTokens) || 0;
      const c = Number(r.completionTokens) || 0;
      const rsn = Number(r.reasoningTokens) || 0;
      const cr = Number(r.cacheReadTokens) || 0;
      const cw = Number(r.cacheWriteTokens) || 0;
      prompt += p;
      completion += c;
      cacheRead += cr;
      cacheWrite += cw;
      reasoning += rsn;
      entered += Number(r.totalTokens) || 0;
      cost += Number(r.costUsd);
      cumCacheRead += cr;
      cumCacheWrite += cw;
      cumPrompt += p;
      // Fresh working set excludes cache reads (re-sends), matching the
      // budget gate semantics — the model really held p+c+rsn distinct
      // tokens in this step.
      const fresh = p + c + rsn;
      if (fresh > workingSet) {
        workingSet = fresh;
        peakPrompt = p;
        peakCompletion = c;
        peakReasoning = rsn;
      }
    }
    // Peak-row FRESH buckets (coherent with `workingSet`).
    const peakBuckets = {
      prompt: peakPrompt,
      completion: peakCompletion,
      reasoning: peakReasoning,
    };
    return { prompt, completion, cacheRead, cacheWrite, reasoning, entered, workingSet, cost, peakBuckets, cumCacheRead, cumCacheWrite, cumPrompt };
  }, [usage]);

  // Cache hit-rate: fraction of new input that was served from cache
  // (cumulative reads vs cumulative fresh input). A real, interpretable
  // figure for the cache-transport bar.
  const cacheHitRate =
    usageBreakdown.cumCacheRead + usageBreakdown.cumPrompt > 0
      ? Math.round(
          (usageBreakdown.cumCacheRead /
            (usageBreakdown.cumCacheRead + usageBreakdown.cumPrompt)) *
            100,
        )
      : 0;

  // Working set is the PEAK FRESH single-step token count. Falls back to
  // exec.tokenUsage when no usage records exist (a legacy single number).
  const totalTokens = usageBreakdown.workingSet || Number(exec.tokenUsage) || 0;
  const cost =
    usageBreakdown.cost > 0 ? usageBreakdown.cost : Number(exec.costUsd);
  // The % bar only exists when the RESOLVED context window is known. When
  // it isn't, we show the working set with a "window unknown" label instead
  // of a fabricated percentage.
  const effectiveContextWindow = resolvedContextWindow;
  const contextPct =
    effectiveContextWindow > 0
      ? Math.min(100, Math.round((totalTokens / effectiveContextWindow) * 100))
      : 0;
  const cumulativePct =
    effectiveContextWindow > 0
      ? Math.min(
          100,
          Math.round((usageBreakdown.entered / effectiveContextWindow) * 100),
        )
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

  // Token % bars key off the PEAK FRESH row buckets (prompt+completion+
  // reasoning, cache excluded) and the cumulative sums — both measured
  // against the resolved total context window (capped at 100%). Cache
  // reads/writes are NOT baked in; they're shown separately in the
  // cumulative cache-transport section.
  const peakBuckets = usageBreakdown.peakBuckets;
  const rolePctVsWindow = (n: number) =>
    effectiveContextWindow > 0
      ? Math.min(100, Math.round((n / effectiveContextWindow) * 100))
      : 0;

  return (
    <aside className="space-y-3 lg:sticky lg:top-4">
      {/* Context usage card */}
      <div className="rounded-2xl glass-panel p-4">
        <div className="mb-2 flex items-center justify-between">
          <h3 className="text-xs font-semibold uppercase tracking-wider text-muted-foreground">
            Context
          </h3>
          <span className="text-xs font-medium text-muted-foreground">
            {effectiveContextWindow > 0 ? `${contextPct}%` : "—"}
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
          <span>
            {effectiveContextWindow > 0 ? (
              <>peak working set / {fmtNum(effectiveContextWindow)} window</>
            ) : (
              "working set (model window unknown)"
            )}
          </span>
        </div>
        {usageBreakdown.entered > 0 && (
          <div className="mt-2 border-t pt-1.5">
            <div className="h-2 w-full overflow-hidden rounded-full bg-muted">
              <div
                className={cn(
                  "h-full rounded-full transition-all",
                  cumulativePct > 80
                    ? "bg-red-500"
                    : cumulativePct > 50
                      ? "bg-amber-500"
                      : "bg-emerald-500",
                )}
                style={{ width: `${Math.max(cumulativePct, 2)}%` }}
              />
            </div>
            <div className="mt-1 flex items-baseline justify-between text-xs text-muted-foreground">
              <span className="font-mono">
                {fmtNum(usageBreakdown.entered)} tokens
              </span>
              <span>
                {effectiveContextWindow > 0 ? (
                  <>cumulative / {fmtNum(effectiveContextWindow)} window</>
                ) : (
                  "cumulative (model window unknown)"
                )}
              </span>
            </div>
          </div>
        )}
      </div>

      {/* Status card */}
      <div className="rounded-2xl glass-panel p-4">
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
      <div className="rounded-2xl glass-panel p-4">
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
        {usageBreakdown.entered > 0 && (
          <div className="mt-3 space-y-2">
            <div>
              <div className="mb-1 text-[10px] text-muted-foreground">
                Context window (input/output)
              </div>
              <TokenBar
                label="Input"
                count={peakBuckets.prompt}
                pct={rolePctVsWindow(peakBuckets.prompt)}
                color="bg-blue-500"
              />
              <TokenBar
                label="Output"
                count={peakBuckets.completion}
                pct={rolePctVsWindow(peakBuckets.completion)}
                color="bg-violet-500"
              />
              {peakBuckets.reasoning > 0 && (
                <TokenBar
                  label="Reasoning"
                  count={peakBuckets.reasoning}
                  pct={rolePctVsWindow(peakBuckets.reasoning)}
                  color="bg-amber-500"
                />
              )}
            </div>
            <div className="border-t border-dashed pt-1.5">
              <div className="mb-1 text-[10px] text-muted-foreground">
                Cumulative (input/output)
              </div>
              <TokenBar
                label="Input"
                count={usageBreakdown.prompt}
                pct={rolePctVsWindow(usageBreakdown.prompt)}
                color="bg-blue-500"
              />
              <TokenBar
                label="Output"
                count={usageBreakdown.completion}
                pct={rolePctVsWindow(usageBreakdown.completion)}
                color="bg-violet-500"
              />
              {usageBreakdown.reasoning > 0 && (
                <TokenBar
                  label="Reasoning"
                  count={usageBreakdown.reasoning}
                  pct={rolePctVsWindow(usageBreakdown.reasoning)}
                  color="bg-amber-500"
                />
              )}
            </div>
            {/* Cache transport is a SEPARATE figure from the working set —
                it's cumulative re-sends of already-counted tokens, so it
                gets its own bar + hit-rate %, not a slice of the mix. */}
            {(usageBreakdown.cumCacheRead > 0 || usageBreakdown.cumCacheWrite > 0) && (
              <div className="border-t border-dashed pt-1.5">
                <div className="mb-1 flex items-center justify-between text-[10px]">
                  <span className="font-medium uppercase tracking-wider text-muted-foreground">
                    Cache transport (cumulative)
                  </span>
                  {cacheHitRate > 0 && (
                    <span className="font-mono font-medium text-emerald-700 dark:text-emerald-600">
                      {cacheHitRate}% hit rate
                    </span>
                  )}
                </div>
                {usageBreakdown.cumCacheRead > 0 && (
                  <TokenBar
                    label="Cache read (re-sent)"
                    count={usageBreakdown.cumCacheRead}
                    pct={cacheHitRate}
                    color="bg-emerald-500"
                  />
                )}
                {usageBreakdown.cumCacheWrite > 0 && (
                  <TokenBar
                    label="Cache write"
                    count={usageBreakdown.cumCacheWrite}
                    pct={
                      usageBreakdown.cumCacheWrite +
                        usageBreakdown.cumPrompt >
                      0
                        ? Math.round(
                            (usageBreakdown.cumCacheWrite /
                              (usageBreakdown.cumCacheWrite +
                                usageBreakdown.cumPrompt)) *
                              100,
                          )
                        : 0
                    }
                    color="bg-teal-500"
                  />
                )}
              </div>
            )}
            {/* Cumulative totals — always shown when any usage was recorded.
                Cache read/write and reasoning are separate cost buckets and
                are deliberately NOT folded into the input/output mix, so
                they get their own explicit totals line. */}
            <div className="mt-1 border-t border-dashed pt-1.5 text-[10px] text-muted-foreground">
              Cumulative: {fmtNum(usageBreakdown.prompt)} input ·{" "}
              {fmtNum(usageBreakdown.completion)} output
              {[
                usageBreakdown.reasoning > 0 &&
                  `${fmtNum(usageBreakdown.reasoning)} reasoning`,
                usageBreakdown.cumCacheRead > 0 &&
                  `${fmtNum(usageBreakdown.cumCacheRead)} cache read`,
                usageBreakdown.cumCacheWrite > 0 &&
                  `${fmtNum(usageBreakdown.cumCacheWrite)} cache write`,
                cacheHitRate > 0 && `${cacheHitRate}% hit rate`,
              ]
                .filter(Boolean)
                .map((seg) =>
                  seg ? (
                    <span key={seg}> · {seg}</span>
                  ) : null,
                )}
            </div>
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
        <div className="rounded-2xl glass-panel p-4">
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
        <div className="rounded-2xl glass-panel p-4">
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
        <div className="rounded-2xl glass-panel p-4">
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
    <div className="rounded-2xl glass-panel p-4">
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