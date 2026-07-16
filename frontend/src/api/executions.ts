// Execution + adapter query/mutation hooks (TanStack Query + Connect-ES).
// docs/07 §3.7 (RuntimeAdapterService), §3.8 (ExecutionService).

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { adapterClient, executionClient } from "@/api/clients";
import { useStream } from "@/api/useStream";
import type { RuntimeAdapter } from "@/api/gen/orchicon/api/v1/adapter_pb";
import type { WorkerExecution } from "@/api/gen/orchicon/api/v1/execution_pb";
import type { ExecutionEvent } from "@/api/gen/orchicon/api/v1/execution_pb";
import type { ApprovalRequest } from "@/api/gen/orchicon/api/v1/execution_pb";
import type { StreamExecutionEventsRequest } from "@/api/gen/orchicon/api/v1/execution_pb";
import type { StreamExecutionEventsResponse } from "@/api/gen/orchicon/api/v1/execution_pb";
import type { PartialMessage } from "@bufbuild/protobuf";

// --- adapter keys ---

export const adapterKeys = {
  all: ["adapters"] as const,
  list: (kind?: string) => [...adapterKeys.all, "list", kind] as const,
  detail: (id: string) => [...adapterKeys.all, "detail", id] as const,
};

export function useListAdapters(kind?: string) {
  return useQuery({
    queryKey: adapterKeys.list(kind),
    queryFn: async () => {
      const res = await adapterClient.listAdapters({
        pageSize: 100,
        kind: kind ?? undefined,
      });
      return res.adapters as RuntimeAdapter[];
    },
  });
}

export function useGetAdapterCapabilities(id: string) {
  return useQuery({
    queryKey: adapterKeys.detail(id),
    queryFn: async () => {
      const res = await adapterClient.getAdapterCapabilities({ id });
      return res.capabilities;
    },
    enabled: !!id,
  });
}

// --- execution keys ---

export const executionKeys = {
  all: ["executions"] as const,
  list: (projectId?: string, status?: number) =>
    [...executionKeys.all, "list", projectId, status] as const,
  detail: (id: string) => [...executionKeys.all, "detail", id] as const,
  pendingApprovals: (executionId?: string) =>
    [...executionKeys.all, "approvals", executionId] as const,
};

export function useListExecutions(opts?: {
  projectId?: string;
  taskId?: string;
  status?: number;
  workflowRunId?: string;
}) {
  return useQuery({
    queryKey: executionKeys.list(opts?.projectId, opts?.status),
    queryFn: async () => {
      const res = await executionClient.listExecutions({
        pageSize: 100,
        projectId: opts?.projectId ?? undefined,
        taskId: opts?.taskId ?? undefined,
        status: opts?.status ?? undefined,
        workflowRunId: opts?.workflowRunId ?? undefined,
      });
      return res.executions as WorkerExecution[];
    },
    refetchInterval: 3_000,
  });
}

export function useGetExecution(id: string) {
  return useQuery({
    queryKey: executionKeys.detail(id),
    queryFn: async () => {
      const res = await executionClient.getExecution({ id });
      return res.execution as WorkerExecution;
    },
    enabled: !!id,
    // The execution detail is also kept fresh by the event stream's
    // onEvent invalidation, but a 3s polling fallback is important:
    // status transitions (running → succeeded) may arrive after the
    // last event has been emitted (the final "result" event sometimes
    // lands before the DB row's status is updated, or the stream
    // closes cleanly with no closing event). Without polling, the page
    // would sit on stale data until the user manually refreshed.
    refetchInterval: 3_000,
  });
}

// useStreamExecutionEvents wraps the generic useStream hook to subscribe
// to the StreamExecutionEvents server-stream RPC (docs/10 §4).
export function useStreamExecutionEvents(opts: {
  executionId: string;
  enabled?: boolean;
  onEvent?: (event: ExecutionEvent) => void;
}) {
  const { executionId, enabled = true, onEvent } = opts;
  const request: PartialMessage<StreamExecutionEventsRequest> = {
    executionId,
  };
  return useStream({
    name: "execution-events",
    stream: (req) => executionClient.streamExecutionEvents(req),
    request,
    getEventId: (resp: StreamExecutionEventsResponse) => {
      const evt = resp.event;
      if (!evt) return "";
      return `${evt.eventType}-${resp.sequence}`;
    },
    getSequence: (resp: StreamExecutionEventsResponse) => resp.sequence,
    filter: (resp: StreamExecutionEventsResponse) => {
      if (!resp.event) return false;
      return resp.event.executionId === executionId;
    },
    onEvent: (resp: StreamExecutionEventsResponse) => {
      if (resp.event) onEvent?.(resp.event);
    },
    enabled,
  });
}

// --- execution mutations ---

export function usePauseExecution() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (id: string) => {
      const res = await executionClient.pauseExecution({ id });
      return res.execution as WorkerExecution;
    },
    onSuccess: (exec) => {
      qc.invalidateQueries({ queryKey: executionKeys.detail(exec.id) });
    },
  });
}

export function useResumeExecution() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (id: string) => {
      const res = await executionClient.resumeExecution({ id });
      return res.execution as WorkerExecution;
    },
    onSuccess: (exec) => {
      qc.invalidateQueries({ queryKey: executionKeys.detail(exec.id) });
    },
  });
}

export function useCancelExecution() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: { id: string; reason?: string }) => {
      const res = await executionClient.cancelExecution(input);
      return res.execution as WorkerExecution;
    },
    onSuccess: (exec) => {
      qc.invalidateQueries({ queryKey: executionKeys.detail(exec.id) });
      qc.invalidateQueries({ queryKey: executionKeys.list() });
    },
  });
}

export function useCheckpointNow() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (id: string) => {
      const res = await executionClient.checkpointNow({ id });
      return res;
    },
    onSuccess: (_, id) => {
      qc.invalidateQueries({ queryKey: executionKeys.detail(id) });
    },
  });
}

export function useDeleteExecution() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (id: string) => {
      await executionClient.deleteExecution({ id });
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: executionKeys.all });
    },
  });
}

// --- Tier 2 approval ---

export function useListPendingApprovals(executionId?: string) {
  return useQuery({
    queryKey: executionKeys.pendingApprovals(executionId),
    queryFn: async () => {
      const res = await executionClient.listPendingApprovals({
        executionId: executionId ?? undefined,
      });
      return res.approvals as ApprovalRequest[];
    },
    refetchInterval: 5_000, // poll every 5s for pending approvals
  });
}

export function useApproveToolCall() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: {
      requestId: string;
      approved: boolean;
      reason?: string;
    }) => {
      const res = await executionClient.approveToolCall(input);
      return res.approval as ApprovalRequest;
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: executionKeys.pendingApprovals() });
    },
  });
}
