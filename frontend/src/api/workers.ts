// Worker query and mutation hooks (TanStack Query + Connect-ES).
//
// Per docs/10_Frontend_Architecture.md §6, server state lives in the
// TanStack Query cache. Mutations invalidate the relevant queries so the
// catalog/detail views refetch server-confirmed state (no optimistic
// status transitions — invariant #3).

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { workerClient } from "@/api/clients";
import type { Worker } from "@/api/gen/orchicon/api/v1/worker_pb";
import type { WorkerVersion } from "@/api/gen/orchicon/api/v1/worker_pb";
import type { WorkerStatus } from "@/api/gen/orchicon/api/v1/worker_pb";
import type { CreateWorkerRequest, UpdateWorkerVersionRequest, CreateWorkerVersionRequest, WorkerListItem } from "@/api/gen/orchicon/api/v1/worker_service_pb";

// Query keys are centralized so invalidation is type-safe and
// refactor-proof.
export const workerKeys = {
  all: ["workers"] as const,
  list: (opts?: { status?: number; search?: string; sortBy?: string; sortOrder?: string }) =>
    [...workerKeys.all, "list", opts] as const,
  detail: (id: string) => [...workerKeys.all, "detail", id] as const,
  versions: (id: string) => [...workerKeys.all, "versions", id] as const,
  editLock: (id: string) => [...workerKeys.all, "edit-lock", id] as const,
};

// useListWorkers fetches a page of workers for the resolved tenant.
// Returns WorkerListItem[] so cards get active_model_ref + version status
// in one round-trip — no per-worker ListWorkerVersions fetch.
export function useListWorkers(opts?: { status?: WorkerStatus; search?: string; sortBy?: string; sortOrder?: string }) {
  return useQuery({
    queryKey: workerKeys.list(opts ? { status: opts.status, search: opts.search, sortBy: opts.sortBy, sortOrder: opts.sortOrder } : undefined),
    queryFn: async () => {
      const res = await workerClient.listWorkers({
        pageSize: 100,
        status: opts?.status ?? undefined,
        search: opts?.search || "",
        sortBy: opts?.sortBy || "",
        sortOrder: opts?.sortOrder || "",
      });
      // Prefer items (enriched) with fallback to legacy workers during rollout.
      if ((res as any).items && (res as any).items.length > 0) {
        return (res as any).items as WorkerListItem[];
      }
      if ((res as any).workers && (res as any).workers.length > 0) {
        // Legacy fallback: synthesize items with empty model/status.
        return (res as any).workers.map((w: Worker) => ({
          worker: w,
          activeModelRef: "",
          activeVersionStatus: 0,
        })) as WorkerListItem[];
      }
      return [] as WorkerListItem[];
    },
    refetchInterval: 5_000,
  });
}

// useGetWorker fetches a single worker by id, with its latest published
// version (if any).
export function useGetWorker(id: string) {
  return useQuery({
    queryKey: workerKeys.detail(id),
    queryFn: async () => {
      const res = await workerClient.getWorker({ id });
      return {
        worker: res.worker as Worker,
        latestVersion: (res.latestVersion ?? undefined) as WorkerVersion | undefined,
      };
    },
    enabled: !!id,
  });
}

// useListWorkerVersions fetches all versions of a worker, newest first.
export function useListWorkerVersions(workerId: string) {
  return useQuery({
    queryKey: workerKeys.versions(workerId),
    queryFn: async () => {
      const res = await workerClient.listWorkerVersions({ workerId });
      return res.versions as WorkerVersion[];
    },
    enabled: !!workerId,
  });
}

// useCreateWorker creates a worker and invalidates the list.
export function useCreateWorker() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: PartialMessage<CreateWorkerRequest>) => {
      const res = await workerClient.createWorker(input);
      return { worker: res.worker as Worker, version: res.version as WorkerVersion };
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: workerKeys.list() });
    },
  });
}

// useGetWorkerVersion fetches a single version by id.
export function useGetWorkerVersion(versionId: string) {
  return useQuery({
    queryKey: [...workerKeys.all, "version", versionId] as const,
    queryFn: async () => {
      const res = await workerClient.getWorkerVersion({ id: versionId });
      return res.version as WorkerVersion;
    },
    enabled: !!versionId,
  });
}

// useSetActiveWorkerVersion sets a published version as the worker's active version.
export function useSetActiveWorkerVersion() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: { workerId: string; version: number }) => {
      await workerClient.setActiveWorkerVersion(input);
    },
    onSuccess: (_data, variables) => {
      qc.invalidateQueries({ queryKey: workerKeys.detail(variables.workerId) });
      qc.invalidateQueries({ queryKey: workerKeys.versions(variables.workerId) });
    },
  });
}

// useRevertWorkerVersionToDraft moves a published version back to draft.
export function useRevertWorkerVersionToDraft() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: { versionId: string }) => {
      await workerClient.revertWorkerVersionToDraft(input);
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: workerKeys.all });
    },
  });
}

// usePublishWorkerVersion publishes the draft version of a worker.
export function usePublishWorkerVersion() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (workerId: string) => {
      const res = await workerClient.publishWorkerVersion({ workerId });
      return { worker: res.worker as Worker, version: res.version as WorkerVersion };
    },
    onSuccess: (data) => {
      qc.invalidateQueries({ queryKey: workerKeys.list() });
      qc.invalidateQueries({ queryKey: workerKeys.detail(data.worker.id) });
      qc.invalidateQueries({ queryKey: workerKeys.versions(data.worker.id) });
      qc.invalidateQueries({ queryKey: [...workerKeys.all, "version", data.version.id] as const });
    },
  });
}

// useDeprecateWorker deprecates a published worker.
export function useDeprecateWorker() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (workerId: string) => {
      const res = await workerClient.deprecateWorker({ workerId });
      return res.worker as Worker;
    },
    onSuccess: (worker) => {
      qc.invalidateQueries({ queryKey: workerKeys.list() });
      qc.invalidateQueries({ queryKey: workerKeys.detail(worker.id) });
    },
  });
}

// useDeleteWorkerVersion deletes a single worker version (any status).
export function useDeleteWorkerVersion() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: { workerId: string; versionId: string }) => {
      await workerClient.deleteWorkerVersion(input);
    },
    onSuccess: (_data, variables) => {
      qc.invalidateQueries({ queryKey: workerKeys.versions(variables.workerId) });
    },
  });
}

// useDeleteWorker hard-deletes a worker and invalidates the list.
export function useDeleteWorker() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (id: string) => {
      await workerClient.deleteWorker({ id });
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: workerKeys.list() });
    },
  });
}

// useBatchDeleteWorkers hard-deletes multiple workers by id.
export function useBatchDeleteWorkers() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (ids: string[]) => {
      await Promise.all(ids.map((id) => workerClient.deleteWorker({ id })));
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: workerKeys.all });
    },
  });
}

// useBulkUpdateWorkerModel sets model_ref on every requested worker and
// publishes the affected version in a single round trip. Per-worker
// outcomes (updated / skipped / error) are returned in the response so
// the caller can render a per-worker result summary.
export function useBulkUpdateWorkerModel() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: { workerIds: string[]; modelRef: string }) => {
      const res = await workerClient.bulkUpdateWorkerModel(input);
      return res;
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: workerKeys.all });
    },
  });
}

// useUpdateWorker updates the mutable header fields of a draft worker.
export function useUpdateWorker() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (req: { id: string; name?: string; description?: string; purpose?: string }) => {
      const res = await workerClient.updateWorker(req);
      return res.worker;
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: workerKeys.all });
    },
  });
}

// useRetireWorker retires a deprecated worker.
export function useRetireWorker() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (workerId: string) => {
      const res = await workerClient.retireWorker({ workerId });
      return res.worker as Worker;
    },
    onSuccess: (worker) => {
      qc.invalidateQueries({ queryKey: workerKeys.list() });
      qc.invalidateQueries({ queryKey: workerKeys.detail(worker.id) });
    },
  });
}

// useAcquireEditLock acquires an edit lock on a worker for the visual
// editor (docs/07 §3.3).
export function useAcquireEditLock() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: { workerId: string; actor: string }) => {
      const res = await workerClient.acquireEditLock(input);
      return { lock: res.lock, acquired: res.acquired };
    },
    onSuccess: (_, variables) => {
      qc.invalidateQueries({ queryKey: workerKeys.editLock(variables.workerId) });
    },
  });
}

// useReleaseEditLock releases a held edit lock.
export function useReleaseEditLock() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: { workerId: string; actor: string }) => {
      await workerClient.releaseEditLock(input);
    },
    onSuccess: (_, variables) => {
      qc.invalidateQueries({ queryKey: workerKeys.editLock(variables.workerId) });
    },
  });
}

// useGetEditLock returns the current edit lock state for a worker.
export function useGetEditLock(workerId: string) {
  return useQuery({
    queryKey: workerKeys.editLock(workerId),
    queryFn: async () => {
      const res = await workerClient.getEditLock({ workerId });
      return res.lock ?? null;
    },
    enabled: !!workerId,
    // Poll every 10s so other users' lock releases are detected.
    refetchInterval: 10_000,
  });
}

// useUpdateWorkerVersion updates the mutable fields of a draft WorkerVersion.
export function useUpdateWorkerVersion() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: PartialMessage<UpdateWorkerVersionRequest>) => {
      const res = await workerClient.updateWorkerVersion(input);
      return res.version as WorkerVersion;
    },
    onSuccess: (version) => {
      qc.invalidateQueries({ queryKey: workerKeys.detail(version.workerId) });
      qc.invalidateQueries({ queryKey: workerKeys.versions(version.workerId) });
      qc.invalidateQueries({ queryKey: [...workerKeys.all, "version", version.id] as const });
    },
  });
}

// useCreateWorkerVersion creates a new draft version from the latest
// published version of a Worker.
export function useCreateWorkerVersion() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: PartialMessage<CreateWorkerVersionRequest>) => {
      const res = await workerClient.createWorkerVersion(input);
      return res.version as WorkerVersion;
    },
    onSuccess: (version) => {
      qc.invalidateQueries({ queryKey: workerKeys.detail(version.workerId) });
      qc.invalidateQueries({ queryKey: workerKeys.versions(version.workerId) });
      qc.invalidateQueries({ queryKey: [...workerKeys.all, "version", version.id] as const });
    },
  });
}

// Type helper for partial messages.
import type { PartialMessage } from "@bufbuild/protobuf";
