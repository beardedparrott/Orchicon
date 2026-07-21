// Project query and mutation hooks (TanStack Query + Connect-ES).
//
// Per docs/10_Frontend_Architecture.md §6, server state lives in the
// TanStack Query cache. Mutations invalidate the relevant queries so the
// list/detail views refetch server-confirmed state (no optimistic
// status transitions — invariant #3).

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { projectClient } from "@/api/clients";
import type { GoalField, Project, ProjectStatus } from "@/api/gen/orchicon/api/v1/project_pb";

// Query keys are centralized so invalidation is type-safe and
// refactor-proof. New project-scoped queries extend this tree.
export const projectKeys = {
  all: ["projects"] as const,
  list: () => [...projectKeys.all, "list"] as const,
  detail: (id: string) => [...projectKeys.all, "detail", id] as const,
};

// useListProjects fetches a page of projects for the resolved tenant.
// Deleted projects are excluded server-side unless status is explicitly set.
export function useListProjects(opts?: { search?: string; status?: ProjectStatus; sortBy?: string; sortOrder?: string }) {
  return useQuery({
    queryKey: [...projectKeys.list(), opts],
    queryFn: async () => {
      const res = await projectClient.listProjects({
        pageSize: 100,
        search: opts?.search || "",
        status: opts?.status,
        sortBy: opts?.sortBy || "",
        sortOrder: opts?.sortOrder || "",
      });
      return res.projects as Project[];
    },
  });
}

// useUpdateProject updates the mutable fields of a project (name, slug,
// goals). Partial update — only non-nil fields are written.
export function useUpdateProject() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: {
      id: string;
      name?: string;
      slug?: string;
      goals?: { fields: GoalField[] };
    }) => {
      const res = await projectClient.updateProject(input);
      return res.project as Project;
    },
    onSuccess: (project) => {
      qc.invalidateQueries({ queryKey: projectKeys.list() });
      qc.invalidateQueries({ queryKey: projectKeys.detail(project.id) });
    },
  });
}

// useActivateProject transitions a drafting project to active status.
export function useActivateProject() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (id: string) => {
      const res = await projectClient.activateProject({ id });
      return res.project as Project;
    },
    onSuccess: (project) => {
      qc.invalidateQueries({ queryKey: projectKeys.list() });
      qc.invalidateQueries({ queryKey: projectKeys.detail(project.id) });
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

// useDeleteProject hard-deletes a project and invalidates the list.
export function useDeleteProject() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (id: string) => {
      await projectClient.deleteProject({ id });
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: projectKeys.list() });
    },
  });
}

// useBatchDeleteProjects hard-deletes multiple projects by id.
export function useBatchDeleteProjects() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (ids: string[]) => {
      await Promise.allSettled(ids.map((id) => projectClient.deleteProject({ id })));
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: projectKeys.list() });
    },
  });
}
