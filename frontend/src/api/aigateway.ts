// AI Gateway query hooks (TanStack Query + Connect-ES, docs/07 §3.10,
// docs/01 §2: AI Gateway embedded in the control plane binary). Cost
// attribution rolls up Tenant → Project → Task → Execution (docs/10 §11
// cost explorer). Usage records are dual-written to Postgres (source
// of truth) + ClickHouse (OTel metrics) — this client reads Postgres.

import { useQuery } from "@tanstack/react-query";

import { aiGatewayClient } from "@/api/clients";
import type { AIProvider } from "@/api/gen/orchicon/api/v1/ai_gateway_pb";
import type { OpenCodeMCP } from "@/api/gen/orchicon/api/v1/ai_gateway_pb";
import type { OpenCodeModel } from "@/api/gen/orchicon/api/v1/ai_gateway_pb";
import type { UsageRecord } from "@/api/gen/orchicon/api/v1/ai_gateway_pb";
import type { CostSummary } from "@/api/gen/orchicon/api/v1/ai_gateway_pb";
import type { UsageRollup } from "@/api/gen/orchicon/api/v1/ai_gateway_pb";

export const usageKeys = {
  all: ["usage"] as const,
  providers: ["usage", "providers"] as const,
  models: ["usage", "models"] as const,
  mcps: ["usage", "mcps"] as const,
  records: (projectId?: string) => ["usage", "records", projectId] as const,
  cost: (rollup?: UsageRollup, projectId?: string, taskId?: string) =>
    ["usage", "cost", rollup, projectId, taskId] as const,
};

export function useListOpenCodeModels() {
  return useQuery({
    queryKey: usageKeys.models,
    queryFn: async () => {
      const res = await aiGatewayClient.listOpenCodeModels({});
      return (res.models ?? []) as OpenCodeModel[];
    },
    staleTime: 5 * 60 * 1000, // 5 min cache — models don't change often
  });
}

export function useListOpenCodeMCPs() {
  return useQuery({
    queryKey: usageKeys.mcps,
    queryFn: async () => {
      const res = await aiGatewayClient.listOpenCodeMCPs({});
      return (res.servers ?? []) as OpenCodeMCP[];
    },
    staleTime: 5 * 60 * 1000,
  });
}

export function useListProviders() {
  return useQuery({
    queryKey: usageKeys.providers,
    queryFn: async () => {
      const res = await aiGatewayClient.listProviders({});
      return (res.providers ?? []) as AIProvider[];
    },
  });
}

export function useGetUsage(opts?: {
  projectId?: string;
  taskId?: string;
  executionId?: string;
  provider?: string;
  model?: string;
}) {
  return useQuery({
    queryKey: usageKeys.records(opts?.projectId),
    queryFn: async () => {
      const res = await aiGatewayClient.getUsage({
        pageSize: 100,
        projectId: opts?.projectId ?? "",
        taskId: opts?.taskId ?? "",
        executionId: opts?.executionId ?? "",
        provider: opts?.provider ?? "",
        model: opts?.model ?? "",
      });
      return (res.records ?? []) as UsageRecord[];
    },
  });
}

export function useGetCost(opts: {
  rollup?: UsageRollup;
  projectId?: string;
  taskId?: string;
  executionId?: string;
}) {
  return useQuery({
    queryKey: usageKeys.cost(opts?.rollup, opts?.projectId, opts?.taskId),
    queryFn: async () => {
      const res = await aiGatewayClient.getCost({
        rollup: opts?.rollup ?? 0,
        projectId: opts?.projectId ?? "",
        taskId: opts?.taskId ?? "",
        executionId: opts?.executionId ?? "",
      });
      return {
        summaries: (res.summaries ?? []) as CostSummary[],
        total: res.total as CostSummary | undefined,
      };
    },
  });
}
