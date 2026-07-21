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
import type { CreateWorkerRequest, UpdateWorkerVersionRequest, CreateWorkerVersionRequest } from "@/api/gen/orchicon/api/v1/worker_service_pb";

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
      return res.workers as Worker[];
    },
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
      qc.invalidateQueries({ queryKey: workerKeys.list() });
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
      qc.invalidateQueries({ queryKey: workerKeys.versions(version.workerId) });
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
      qc.invalidateQueries({ queryKey: workerKeys.versions(version.workerId) });
    },
  });
}

// Type helper for partial messages.
import type { PartialMessage } from "@bufbuild/protobuf";
