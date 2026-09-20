// Project query and mutation hooks (TanStack Query + Connect-ES).
//
// Per docs/10_Frontend_Architecture.md §6, server state lives in the
// TanStack Query cache. Mutations invalidate the relevant queries so the
// list/detail views refetch server-confirmed state (no optimistic
// status transitions — invariant #3).

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { projectClient } from "@/api/clients";
import { CreateProjectRequest, ExecutionMode, UpdateProjectRequest } from "@/api/gen/orchicon/api/v1/project_pb";
import type { GoalField, Project, ProjectStatus } from "@/api/gen/orchicon/api/v1/project_pb";
import { gitStrategyToProto } from "@/components/GitStrategySelect";
import type { GitStrategy as GitStrategyName, GitStrategyNullable } from "@/components/GitStrategySelect";

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
    refetchInterval: 5_000,
  });
}

// useUpdateProject updates the mutable fields of a project (name, slug,
// goals, max_concurrent_runs, default_runtime_image, execution_mode).
// Partial update — only defined fields are sent (undefined = unchanged,
// empty default_runtime_image = clear back to inherit).
export function useUpdateProject() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: {
      id: string;
      name?: string;
      slug?: string;
      goals?: { fields: GoalField[] };
      maxConcurrentRuns?: number;
      gitStrategy?: string;
      git_strategy?: string;
      defaultRuntimeImage?: string;
      default_runtime_image?: string;
      executionMode?: string | number;
      execution_mode?: string | number;
      projectDir?: string;
      project_dir?: string;
    }) => {
      const raw = input as {
        gitStrategy?: string;
        git_strategy?: string;
        defaultRuntimeImage?: string;
        default_runtime_image?: string;
        executionMode?: string | number;
        execution_mode?: string | number;
        projectDir?: string;
        project_dir?: string;
      };
      const protoVal = raw.gitStrategy ?? raw.git_strategy;
      const enumVal = protoVal ? gitStrategyToProto(protoVal as GitStrategyName | GitStrategyNullable) : undefined;
      const imgVal = raw.defaultRuntimeImage ?? raw.default_runtime_image;
      const execRaw = raw.executionMode ?? raw.execution_mode;
      const execVal = executionModeToProto(execRaw);
      const dirVal = raw.projectDir ?? raw.project_dir;
      const payload = new UpdateProjectRequest({ id: input.id });
      if (input.name !== undefined) { payload.name = input.name; }
      if (input.slug !== undefined) { payload.slug = input.slug; }
      if (input.goals !== undefined) { payload.goals = input.goals as unknown as UpdateProjectRequest["goals"]; }
      if (input.maxConcurrentRuns !== undefined) { payload.maxConcurrentRuns = input.maxConcurrentRuns; }
      if (enumVal !== undefined) { payload.gitStrategy = enumVal; }
      if (imgVal !== undefined) { payload.defaultRuntimeImage = imgVal; }
      if (execVal !== undefined) { payload.executionMode = execVal; }
      if (dirVal !== undefined) { payload.projectDir = dirVal; }
      const res = await projectClient.updateProject(payload);
      return res.project as Project;
    },
    onSuccess: (project) => {
      qc.invalidateQueries({ queryKey: projectKeys.list() });
      qc.invalidateQueries({ queryKey: projectKeys.detail(project.id) });
    },
  });
}

// executionModeToProto maps UI strings ("runtime" | "local") or proto
// enum numbers to the ExecutionMode enum. UNSPECIFIED/unknown → undefined
// (field absent = unchanged).
export function executionModeToProto(v: string | number | undefined): ExecutionMode | undefined {
  if (v === ExecutionMode.RUNTIME || v === ExecutionMode.LOCAL) return v;
  if (v === 1) return ExecutionMode.RUNTIME;
  if (v === 2) return ExecutionMode.LOCAL;
  if (typeof v === "string") {
    const s = v.toLowerCase();
    if (s === "runtime") return ExecutionMode.RUNTIME;
    if (s === "local") return ExecutionMode.LOCAL;
  }
  return undefined;
}

export function protoToExecutionMode(v: number | string | undefined): "runtime" | "local" {
  if (v === 2 || v === "local" || v === "LOCAL") return "local";
  return "runtime";
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
    mutationFn: async (input: { name: string; slug?: string; goals?: GoalField[]; gitStrategy?: string; git_strategy?: string; defaultRuntimeImage?: string; executionMode?: string | number }) => {
      const raw = input as { gitStrategy?: string; git_strategy?: string };
      const protoVal = raw.gitStrategy ?? raw.git_strategy;
      const enumVal = protoVal ? gitStrategyToProto(protoVal as GitStrategyName | GitStrategyNullable) : undefined;
      const execVal = executionModeToProto(input.executionMode);
      const payload = new CreateProjectRequest({ tenantId: "", name: input.name, slug: input.slug ?? "", goals: input.goals ?? [] });
      if (enumVal !== undefined) { payload.gitStrategy = enumVal; }
      if (input.defaultRuntimeImage !== undefined) { payload.defaultRuntimeImage = input.defaultRuntimeImage; }
      if (execVal !== undefined) { payload.executionMode = execVal; }
      const res = await projectClient.createProject(payload);
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
      await Promise.all(ids.map((id) => projectClient.deleteProject({ id })));
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: projectKeys.all });
    },
  });
}
