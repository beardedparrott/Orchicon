// ask-metrics.ts — the Ask chat box's session stat strip (the data half).
//
// The PURE shape + formatters live in ask-metrics-format.ts (no API imports, so
// they are unit-testable and stay in step with the TUI's equivalent,
// internal/tui/models.go). This module owns the reads and the LIVE refresh.
//
// The numbers come from the usage records the Ask turn path ALREADY writes,
// scoped by session_id (the conversation id) — the read-back path GetUsage
// gained for exactly this. Nothing here recomputes pricing or token counts from
// messages: the recorded row is the authority.
import { useEffect, useMemo, useRef } from "react";

import { useQuery } from "@tanstack/react-query";

import { aiGatewayClient } from "@/api/clients";
import { useListOpenCodeModels } from "@/api/aigateway";
import { useProviderModels } from "@/api/providers";
import type { UsageRecord } from "@/api/gen/orchicon/api/v1/ai_gateway_pb";
import { formatAskMetricsLine, summarizeAskMetrics } from "@/lib/ask-metrics-format";
import { ORCHICON_ADAPTER_KIND, parseModelRef } from "@/lib/model-ref";

// One place to import the shape and the formatters from.
export * from "@/lib/ask-metrics-format";

export const askMetricsKeys = {
  session: (convId: string) => ["ask-metrics", "session", convId] as const,
};

/** useAskSessionMetrics reads one conversation's usage records (newest first). */
export function useAskSessionMetrics(convId: string | null | undefined) {
  const id = convId ?? "";
  return useQuery({
    queryKey: askMetricsKeys.session(id),
    queryFn: async () => {
      const res = await aiGatewayClient.getUsage({ sessionId: id, pageSize: 200 });
      return (res.records ?? []) as UsageRecord[];
    },
    enabled: id !== "",
    staleTime: 0,
  });
}

/**
 * useModelContextWindow resolves a model ref's context window through the SAME
 * per-adapter source the picker uses: the providers sourcing view under the
 * native adapter, opencode-CLI discovery otherwise. Returns 0 when unknown, so
 * the strip shows the bare occupancy rather than a fabricated denominator.
 */
export function useModelContextWindow(modelRef: string): number {
  const parsed = useMemo(() => parseModelRef(modelRef), [modelRef]);
  const native = parsed?.adapter === ORCHICON_ADAPTER_KIND;
  const hasProvider = !!parsed?.provider;

  const providerQ = useProviderModels(native && hasProvider ? parsed!.provider : "", native && hasProvider);
  const cliQ = useListOpenCodeModels(parsed?.adapter, parsed?.provider, !native && hasProvider);

  return useMemo(() => {
    if (!parsed || !parsed.provider) return 0;
    if (native) {
      const m = (providerQ.data?.models ?? []).find((x) => x.id === parsed.model);
      return Number(m?.context ?? 0) || 0;
    }
    const m = (cliQ.data ?? []).find((x) => x.id === parsed.model);
    return Number(m?.limits?.context ?? 0) || 0;
  }, [native, parsed, providerQ.data, cliQ.data]);
}

/**
 * useAskMetrics is the composer's strip: the session numbers plus a refetch
 * hook the caller drives on turn completion.
 */
export function useAskMetrics(convId: string | null | undefined, modelRef: string) {
  const q = useAskSessionMetrics(convId);
  const ctxWindow = useModelContextWindow(modelRef);
  const metrics = useMemo(
    () => summarizeAskMetrics(q.data ?? [], modelRef, ctxWindow),
    [q.data, modelRef, ctxWindow],
  );
  return { metrics, line: formatAskMetricsLine(metrics), refetch: q.refetch };
}

/**
 * useAskMetricsLive is useAskMetrics plus the completion-driven refresh: it
 * refetches whenever a streaming turn ENDS (true -> false), so the strip
 * updates as the conversation progresses without polling. A conversation switch
 * also re-reads immediately — a stale chat's numbers must never linger under a
 * new one.
 */
export function useAskMetricsLive(
  convId: string | null | undefined,
  modelRef: string,
  isStreaming: boolean,
) {
  const { metrics, line, refetch } = useAskMetrics(convId, modelRef);
  const wasStreaming = useRef(isStreaming);
  useEffect(() => {
    if (wasStreaming.current && !isStreaming) void refetch();
    wasStreaming.current = isStreaming;
  }, [isStreaming, refetch]);
  const prevConv = useRef(convId);
  useEffect(() => {
    if (prevConv.current !== convId) void refetch();
    prevConv.current = convId;
  }, [convId, refetch]);
  return { metrics, line };
}
