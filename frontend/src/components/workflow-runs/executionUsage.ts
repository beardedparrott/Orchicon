// Compact cost + context aggregation for the run Heads-Up view, shared
// by HeadsUpTile and HeadsUpExpandedModal.
//
// The PURE math mirrors ExecutionContextSidebar's usageBreakdown exactly
// so a tile/modal reads numerically identically to the same execution's
// sidebar on /executions/$id: cost = summed costUsd; workingSet = the PEAK
// single-step FRESH token count (prompt + completion + reasoning, cache
// EXCLUDED). The context window resolves through the same chain as the
// sidebar (native provider models, then opencode discovery limits.context)
// and stays 0/unknown rather than fabricated when unresolvable.
import { useMemo } from "react";

import { useGetUsage, useListOpenCodeModels } from "@/api/aigateway";
import type { UsageRecord } from "@/api/gen/orchicon/api/v1/ai_gateway_pb";
import { useProviderModels } from "@/api/providers";

export interface ExecutionUsageSummary {
  /** Peak single-step fresh tokens (prompt+completion+reasoning). */
  workingSet: number;
  /** Summed costUsd across all usage records. */
  cost: number;
  /** Resolved model context window, 0 when unknown. */
  contextWindow: number;
  /** workingSet as % of contextWindow, 0 when unknown. */
  contextPct: number;
  /** False when no usage records exist (nothing to show). */
  hasRecords: boolean;
}

/** Pure aggregation — unit-testable without React Query. */
export function summarizeUsage(
  records: readonly UsageRecord[] | undefined,
): Pick<ExecutionUsageSummary, "workingSet" | "cost" | "hasRecords"> {
  let workingSet = 0;
  let cost = 0;
  let count = 0;
  for (const r of records ?? []) {
    count++;
    const fresh =
      (Number(r.promptTokens) || 0) +
      (Number(r.completionTokens) || 0) +
      (Number(r.reasoningTokens) || 0);
    if (fresh > workingSet) workingSet = fresh;
    cost += Number(r.costUsd) || 0;
  }
  return { workingSet, cost, hasRecords: count > 0 };
}

/** Compact token count for tile-sized surfaces (1.2k, 45, 3.1M). */
export function formatCompactTokens(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 10_000) return `${(n / 1000).toFixed(0)}k`;
  if (n >= 1_000) return `${(n / 1000).toFixed(1)}k`;
  return String(n);
}

/**
 * Live cost + context summary for one execution. All queries are guarded
 * by `enabled && Boolean(executionId)` so upcoming (execution-less) tiles
 * issue no usage/model queries.
 */
export function useExecutionUsageSummary(
  executionId: string,
  enabled = true,
): ExecutionUsageSummary {
  const on = enabled && Boolean(executionId);
  const { data: usage } = useGetUsage({ executionId }, on);
  const { data: models } = useListOpenCodeModels(undefined, undefined, on);
  const latest = usage?.length ? usage[usage.length - 1] : undefined;
  const { data: nativeState } = useProviderModels(
    latest?.provider ?? "",
    on && Boolean(latest?.provider),
  );
  const nativeModels = nativeState?.models;
  const summary = useMemo(() => summarizeUsage(usage), [usage]);
  const contextWindow = useMemo(() => {
    if (nativeModels && nativeModels.length > 0 && latest?.model) {
      const found = nativeModels.find((m) => m.id === latest.model);
      const ctx = Number(found?.context) || 0;
      if (ctx > 0) return ctx;
    }
    if (!models || models.length === 0) return 0;
    if (!latest?.provider || !latest?.model) return 0;
    const ref = `${latest.provider}/${latest.model}`;
    const found =
      models.find((m) => m.modelRef === ref) ??
      models.find((m) => m.id === latest.model);
    const ctx = found?.limits?.context ? Number(found.limits.context) : 0;
    return ctx > 0 ? ctx : 0;
  }, [nativeModels, models, latest]);
  const contextPct =
    contextWindow > 0
      ? Math.min(100, Math.round((summary.workingSet / contextWindow) * 100))
      : 0;
  return { ...summary, contextWindow, contextPct };
}
