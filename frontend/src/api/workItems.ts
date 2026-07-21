// Work item query and mutation hooks (TanStack Query + Connect-ES).
//
// Per docs/10_Frontend_Architecture.md §6, server state lives in the
// TanStack Query cache. Mutations invalidate the relevant queries so the
// tree/board/graph views refetch server-confirmed state (no optimistic
// status transitions — invariant #3).

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { workItemClient } from "@/api/clients";
import type { WorkItem } from "@/api/gen/orchicon/api/v1/work_item_pb";
import type { DependencyGraph } from "@/api/gen/orchicon/api/v1/work_item_pb";
import type { WorkItemStatus } from "@/api/gen/orchicon/api/v1/work_item_pb";
import type { CreateWorkItemRequest } from "@/api/gen/orchicon/api/v1/work_item_service_pb";
import type { UpdateWorkItemRequest } from "@/api/gen/orchicon/api/v1/work_item_service_pb";
import type { PartialMessage } from "@bufbuild/protobuf";

// Query keys are centralized so invalidation is type-safe.
export const workItemKeys = {
  all: ["work-items"] as const,
  list: (projectId: string, parentId?: string, status?: number, opts?: { search?: string; sortBy?: string; sortOrder?: string }) =>
    [...workItemKeys.all, "list", projectId, parentId, status, opts] as const,
  detail: (id: string) => [...workItemKeys.all, "detail", id] as const,
  graph: (projectId: string) =>
    [...workItemKeys.all, "graph", projectId] as const,
};

// useListWorkItems fetches a page of work items for a project, optionally
// filtered by parent (tree) or status (Kanban), with free-text search and
// sort_by/sort_order.
export function useListWorkItems(
  projectId: string,
  opts?: { parentId?: string; status?: WorkItemStatus; search?: string; sortBy?: string; sortOrder?: string },
) {
  const parentId = opts?.parentId;
  const status = opts?.status;
  const listOpts = { search: opts?.search, sortBy: opts?.sortBy, sortOrder: opts?.sortOrder };
  return useQuery({
    queryKey: workItemKeys.list(projectId, parentId, status, listOpts),
    queryFn: async () => {
      const res = await workItemClient.listWorkItems({
        projectId,
        parentId: parentId ?? undefined,
        status: status ?? undefined,
        search: opts?.search || "",
        sortBy: opts?.sortBy || "",
        sortOrder: opts?.sortOrder || "",
        pageSize: 1000,
      });
      return res.workItems as WorkItem[];
    },
    enabled: !!projectId,
  });
}

// useGetWorkItem fetches a single work item by id.
export function useGetWorkItem(id: string) {
  return useQuery({
    queryKey: workItemKeys.detail(id),
    queryFn: async () => {
      const res = await workItemClient.getWorkItem({ id });
      return res.workItem as WorkItem;
    },
    enabled: !!id,
  });
}

// useGetDependencyGraph fetches the full DAG (nodes + edges) for a
// project. Used by the read-only React Flow dependency graph (docs/10).
export function useGetDependencyGraph(projectId: string) {
  return useQuery({
    queryKey: workItemKeys.graph(projectId),
    queryFn: async () => {
      const res = await workItemClient.getDependencyGraph({ projectId });
      return res.graph as DependencyGraph;
    },
    enabled: !!projectId,
  });
}

// useCreateWorkItem creates a work item and invalidates the list + graph.
export function useCreateWorkItem() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: PartialMessage<CreateWorkItemRequest>) => {
      const res = await workItemClient.createWorkItem(input);
      return res.workItem as WorkItem;
    },
    onSuccess: (item) => {
      qc.invalidateQueries({ queryKey: workItemKeys.list(item.projectId) });
      qc.invalidateQueries({ queryKey: workItemKeys.graph(item.projectId) });
    },
  });
}

// useUpdateWorkItem updates a work item (partial, optimistic concurrency
// handled server-side via version CAS).
export function useUpdateWorkItem(projectId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: PartialMessage<UpdateWorkItemRequest>) => {
      const res = await workItemClient.updateWorkItem(input);
      return res.workItem as WorkItem;
    },
    onSuccess: (item) => {
      qc.invalidateQueries({ queryKey: workItemKeys.list(projectId) });
      qc.invalidateQueries({ queryKey: workItemKeys.detail(item.id) });
      qc.invalidateQueries({ queryKey: workItemKeys.graph(projectId) });
    },
  });
}

// useDeleteWorkItem soft-deletes (cancels) a work item.
export function useDeleteWorkItem(projectId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (id: string) => {
      const res = await workItemClient.deleteWorkItem({ id });
      return res.workItem as WorkItem;
    },
    onSuccess: (item) => {
      qc.invalidateQueries({ queryKey: workItemKeys.list(projectId) });
      qc.invalidateQueries({ queryKey: workItemKeys.detail(item.id) });
      qc.invalidateQueries({ queryKey: workItemKeys.graph(projectId) });
    },
  });
}

// useHardDeleteWorkItem permanently removes a work item and its
// dependencies. After success, the caller is responsible for navigating
// away from the detail page.
export function useHardDeleteWorkItem(projectId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (id: string) => {
      await workItemClient.hardDeleteWorkItem({ id });
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: workItemKeys.list(projectId) });
      qc.invalidateQueries({ queryKey: workItemKeys.graph(projectId) });
    },
  });
}

// useBatchDeleteWorkItems hard-deletes multiple work items by id.
export function useBatchDeleteWorkItems(projectId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (ids: string[]) => {
      await Promise.all(ids.map((id) => workItemClient.hardDeleteWorkItem({ id })));
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: workItemKeys.list(projectId) });
      qc.invalidateQueries({ queryKey: workItemKeys.graph(projectId) });
    },
  });
}

// useAddDependency adds an edge to the work DAG.
export function useAddDependency(projectId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: {
      projectId: string;
      fromId: string;
      toId: string;
      type: number;
    }) => {
      const res = await workItemClient.addDependency(input);
      return res.dependency;
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: workItemKeys.graph(projectId) });
    },
  });
}

// useRemoveDependency removes an edge from the work DAG.
export function useRemoveDependency(projectId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (id: string) => {
      await workItemClient.removeDependency({ id });
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: workItemKeys.graph(projectId) });
    },
  });
}

// useAssignWorker binds a worker to a task/subtask.
export function useAssignWorker(projectId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: { id: string; workerRef: string }) => {
      const res = await workItemClient.assignWorker(input);
      return res.workItem as WorkItem;
    },
    onSuccess: (item) => {
      qc.invalidateQueries({ queryKey: workItemKeys.list(projectId) });
      qc.invalidateQueries({ queryKey: workItemKeys.detail(item.id) });
    },
  });
}

// useUnassignWorker removes the worker binding from a task/subtask.
export function useUnassignWorker(projectId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (id: string) => {
      const res = await workItemClient.unassignWorker({ id });
      return res.workItem as WorkItem;
    },
    onSuccess: (item) => {
      qc.invalidateQueries({ queryKey: workItemKeys.list(projectId) });
      qc.invalidateQueries({ queryKey: workItemKeys.detail(item.id) });
    },
  });
}
