// Project query and mutation hooks (TanStack Query + Connect-ES).
//
// Per docs/10_Frontend_Architecture.md §6, server state lives in the
// TanStack Query cache. Mutations invalidate the relevant queries so the
// list/detail views refetch server-confirmed state (no optimistic
// status transitions — invariant #3).

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { projectClient } from "@/api/clients";
import type { GoalField, Project } from "@/api/gen/orchicon/api/v1/project_pb";

// Query keys are centralized so invalidation is type-safe and
// refactor-proof. New project-scoped queries extend this tree.
export const projectKeys = {
  all: ["projects"] as const,
  list: () => [...projectKeys.all, "list"] as const,
  detail: (id: string) => [...projectKeys.all, "detail", id] as const,
};

// useListProjects fetches a page of projects for the resolved tenant.
// Deleted projects are excluded server-side.
export function useListProjects() {
  return useQuery({
    queryKey: projectKeys.list(),
    queryFn: async () => {
      const res = await projectClient.listProjects({ pageSize: 100 });
      return res.projects as Project[];
    },
  });
}

// useGetProject fetches a single project by id.
export function useGetProject(id: string) {
  return useQuery({
    queryKey: projectKeys.detail(id),
    queryFn: async () => {
      const res = await projectClient.getProject({ id });
      return res.project as Project;
    },
    enabled: !!id,
  });
}

// useCreateProject creates a project and invalidates the list.
export function useCreateProject() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: { name: string; slug?: string; goals?: GoalField[] }) => {
      const res = await projectClient.createProject(input);
      return res.project as Project;
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: projectKeys.list() });
    },
  });
}

// useArchiveProject archives a project and invalidates the list + detail.
export function useArchiveProject() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (id: string) => {
      const res = await projectClient.archiveProject({ id });
      return res.project as Project;
    },
    onSuccess: (project) => {
      qc.invalidateQueries({ queryKey: projectKeys.list() });
      qc.invalidateQueries({ queryKey: projectKeys.detail(project.id) });
    },
  });
}
