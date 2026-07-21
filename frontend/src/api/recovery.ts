// Recovery query and mutation hooks (TanStack Query + Connect-ES, docs/07
// §3.6, docs/06). The recovery timeline renders GetRecoveryStepRuns +
// the StreamRecoveryEvents live feed, with the full narrative
// (why/what/how/where/when — docs/06 §11).

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { recoveryClient } from "@/api/clients";
import type { RecoveryExecution } from "@/api/gen/orchicon/api/v1/recovery_pb";
import type { RecoveryStepRun } from "@/api/gen/orchicon/api/v1/recovery_pb";
import type { ContinuationPlan } from "@/api/gen/orchicon/api/v1/recovery_pb";
import type { RecoveryStatus } from "@/api/gen/orchicon/api/v1/recovery_pb";

export const recoveryKeys = {
  all: ["recoveries"] as const,
  list: (projectId?: string, taskId?: string, status?: RecoveryStatus) =>
    [...recoveryKeys.all, "list", projectId, taskId, status] as const,
  detail: (id: string) => [...recoveryKeys.all, "detail", id] as const,
  stepRuns: (id: string) => [...recoveryKeys.all, "step-runs", id] as const,
  plan: (id: string) => [...recoveryKeys.all, "plan", id] as const,
};

export function useListRecoveries(opts?: {
  projectId?: string;
  taskId?: string;
  status?: RecoveryStatus;
}) {
  return useQuery({
    queryKey: recoveryKeys.list(opts?.projectId, opts?.taskId, opts?.status),
    queryFn: async () => {
      const res = await recoveryClient.listRecoveries({
        pageSize: 100,
        projectId: opts?.projectId ?? "",
        taskId: opts?.taskId ?? "",
        status: opts?.status ?? undefined,
      });
      return res.recoveries as RecoveryExecution[];
    },
  });
}

export function useGetRecovery(id: string) {
  return useQuery({
    queryKey: recoveryKeys.detail(id),
    queryFn: async () => {
      const res = await recoveryClient.getRecovery({ id });
      return res.recovery as RecoveryExecution;
    },
    enabled: !!id,
    // Poll so the timeline advances even without the stream.
    refetchInterval: 2_000,
  });
}

export function useGetRecoveryStepRuns(recoveryId: string) {
  return useQuery({
    queryKey: recoveryKeys.stepRuns(recoveryId),
    queryFn: async () => {
      const res = await recoveryClient.getRecoveryStepRuns({ recoveryId });
      return res.stepRuns as RecoveryStepRun[];
    },
    enabled: !!recoveryId,
    refetchInterval: 2_000,
  });
}

export function useGetContinuationPlan(recoveryId: string) {
  return useQuery({
    queryKey: recoveryKeys.plan(recoveryId),
    queryFn: async () => {
      const res = await recoveryClient.getContinuationPlan({ recoveryId });
      return (res.plan ?? undefined) as ContinuationPlan | undefined;
    },
    enabled: !!recoveryId,
  });
}

export function useTriggerRecovery() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: { taskId: string; triggerReason?: string }) => {
      const res = await recoveryClient.triggerRecovery(input);
      return res.recovery as RecoveryExecution;
    },
    onSuccess: () => qc.invalidateQueries({ queryKey: recoveryKeys.list() }),
  });
}

export function useCancelRecovery() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: { recoveryId: string; reason?: string }) => {
      const res = await recoveryClient.cancelRecovery(input);
      return res.recovery as RecoveryExecution;
    },
    onSuccess: (recovery) => {
      qc.invalidateQueries({ queryKey: recoveryKeys.detail(recovery.id) });
      qc.invalidateQueries({ queryKey: recoveryKeys.list() });
    },
  });
}

export function useApproveContinuationPlan() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: { recoveryId: string; actor: string }) => {
      const res = await recoveryClient.approveContinuationPlan(input);
      return { plan: res.plan, recovery: res.recovery };
    },
    onSuccess: (_, variables) => {
      qc.invalidateQueries({ queryKey: recoveryKeys.detail(variables.recoveryId) });
      qc.invalidateQueries({ queryKey: recoveryKeys.plan(variables.recoveryId) });
      qc.invalidateQueries({ queryKey: recoveryKeys.stepRuns(variables.recoveryId) });
    },
  });
}

export function useRejectContinuationPlan() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: { recoveryId: string; actor: string; reason?: string }) => {
      const res = await recoveryClient.rejectContinuationPlan(input);
      return { plan: res.plan, recovery: res.recovery };
    },
    onSuccess: (_, variables) => {
      qc.invalidateQueries({ queryKey: recoveryKeys.detail(variables.recoveryId) });
      qc.invalidateQueries({ queryKey: recoveryKeys.plan(variables.recoveryId) });
    },
  });
}

// useBatchCancelRecoveries cancels multiple recovery executions by id.
export function useBatchCancelRecoveries() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (ids: string[]) => {
      await Promise.allSettled(ids.map((id) => recoveryClient.cancelRecovery({ recoveryId: id })));
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: recoveryKeys.all });
    },
  });
}

export function useMarkTaskSucceeded() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: {
      taskId: string;
      actorType?: string;
      actorId: string;
      reason?: string;
    }) => recoveryClient.markTaskSucceeded(input),
    onSuccess: () => qc.invalidateQueries({ queryKey: recoveryKeys.list() }),
  });
}
