// Workflow query and mutation hooks (TanStack Query + Connect-ES).
//
// Per docs/10_Frontend_Architecture.md §6, server state lives in the
// TanStack Query cache. Mutations invalidate the relevant queries so the
// catalog/editor/run views refetch server-confirmed state (no optimistic
// status transitions — invariant #3).
//
// The workflow editor's draft canvas state (unsaved steps) lives in a
// Zustand store (docs/10 §6); save = explicit commit via the publish
// mutation. The steps JSON is the source of truth for the DAG and is
// stored in workflow_versions.steps.

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { workflowClient } from "@/api/clients";
import type { Workflow } from "@/api/gen/orchicon/api/v1/workflow_pb";
import type { WorkflowVersion } from "@/api/gen/orchicon/api/v1/workflow_pb";
import type { WorkflowRun } from "@/api/gen/orchicon/api/v1/workflow_pb";
import type { WorkflowStepRun } from "@/api/gen/orchicon/api/v1/workflow_pb";
import type { WorkflowStatus } from "@/api/gen/orchicon/api/v1/workflow_pb";
import type { WorkflowRunStatus } from "@/api/gen/orchicon/api/v1/workflow_pb";
import type { CreateWorkflowRequest, CreateWorkflowVersionRequest } from "@/api/gen/orchicon/api/v1/workflow_service_pb";
import type { UpdateWorkflowVersionRequest } from "@/api/gen/orchicon/api/v1/workflow_service_pb";
import type { PartialMessage } from "@bufbuild/protobuf";

// Query keys are centralized so invalidation is type-safe.
export const workflowKeys = {
  all: ["workflows"] as const,
  list: (projectId?: string, status?: WorkflowStatus) =>
    [...workflowKeys.all, "list", projectId, status] as const,
  detail: (id: string) => [...workflowKeys.all, "detail", id] as const,
  versions: (id: string) => [...workflowKeys.all, "versions", id] as const,
  editLock: (id: string) => [...workflowKeys.all, "edit-lock", id] as const,
  runs: (workflowId: string, status?: WorkflowRunStatus) =>
    [...workflowKeys.all, "runs", workflowId, status] as const,
  run: (id: string) => [...workflowKeys.all, "run", id] as const,
  stepRuns: (runId: string) => [...workflowKeys.all, "step-runs", runId] as const,
};

// useListWorkflows fetches a page of workflows, optionally scoped to a
// project (empty = all tenant workflows including templates).
export function useListWorkflows(opts?: { projectId?: string; status?: WorkflowStatus; search?: string; sortBy?: string; sortOrder?: string }) {
  return useQuery({
    queryKey: workflowKeys.list(opts?.projectId, opts?.status),
    queryFn: async () => {
      const res = await workflowClient.listWorkflows({
        pageSize: 100,
        projectId: opts?.projectId ?? "",
        status: opts?.status ?? undefined,
        search: opts?.search || "",
        sortBy: opts?.sortBy || "",
        sortOrder: opts?.sortOrder || "",
      });
      return res.workflows as Workflow[];
    },
  });
}

// useGetWorkflow fetches a single workflow by id, with its latest
// published version (if any).
export function useGetWorkflow(id: string) {
  return useQuery({
    queryKey: workflowKeys.detail(id),
    queryFn: async () => {
      const res = await workflowClient.getWorkflow({ id });
      return {
        workflow: res.workflow as Workflow,
        latestVersion: (res.latestVersion ?? undefined) as WorkflowVersion | undefined,
      };
    },
    enabled: !!id,
  });
}

// useListWorkflowVersions fetches all versions of a workflow, newest first.
export function useListWorkflowVersions(workflowId: string) {
  return useQuery({
    queryKey: workflowKeys.versions(workflowId),
    queryFn: async () => {
      const res = await workflowClient.listWorkflowVersions({ workflowId });
      return res.versions as WorkflowVersion[];
    },
    enabled: !!workflowId,
  });
}

// useCreateWorkflow creates a workflow (draft + first draft version) and
// invalidates the list.
export function useCreateWorkflow() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: PartialMessage<CreateWorkflowRequest>) => {
      const res = await workflowClient.createWorkflow(input);
      return {
        workflow: res.workflow as Workflow,
        version: res.version as WorkflowVersion,
      };
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: workflowKeys.list() });
    },
  });
}

// useCreateWorkflowVersion creates a new draft version from the latest
// published version of a published or deprecated workflow.
export function useCreateWorkflowVersion() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: PartialMessage<CreateWorkflowVersionRequest>) => {
      const res = await workflowClient.createWorkflowVersion(input);
      return {
        workflow: res.workflow as Workflow,
        version: res.version as WorkflowVersion,
      };
    },
    onSuccess: (data) => {
      qc.invalidateQueries({ queryKey: workflowKeys.list() });
      qc.invalidateQueries({ queryKey: workflowKeys.detail(data.workflow.id) });
      qc.invalidateQueries({ queryKey: workflowKeys.versions(data.workflow.id) });
    },
  });
}

// usePublishWorkflow publishes the draft version of a workflow.
export function usePublishWorkflow() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (workflowId: string) => {
      const res = await workflowClient.publishWorkflow({ workflowId });
      return {
        workflow: res.workflow as Workflow,
        version: res.version as WorkflowVersion,
      };
    },
    onSuccess: (data) => {
      qc.invalidateQueries({ queryKey: workflowKeys.list() });
      qc.invalidateQueries({ queryKey: workflowKeys.detail(data.workflow.id) });
      qc.invalidateQueries({ queryKey: workflowKeys.versions(data.workflow.id) });
    },
  });
}

// useDeprecateWorkflow deprecates a published workflow.
export function useDeprecateWorkflow() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (workflowId: string) => {
      const res = await workflowClient.deprecateWorkflow({ workflowId });
      return res.workflow as Workflow;
    },
    onSuccess: (workflow) => {
      qc.invalidateQueries({ queryKey: workflowKeys.list() });
      qc.invalidateQueries({ queryKey: workflowKeys.detail(workflow.id) });
    },
  });
}

// useUpdateWorkflowVersion saves edits to a draft version's steps. This
// is the "save" action in the visual editor (docs/02 §2.4). Only draft
// versions are mutable; published versions are immutable.
export function useUpdateWorkflowVersion() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: PartialMessage<UpdateWorkflowVersionRequest>) => {
      const res = await workflowClient.updateWorkflowVersion(input);
      return res.version as WorkflowVersion;
    },
    onSuccess: (version) => {
      qc.invalidateQueries({ queryKey: workflowKeys.versions(version.workflowId) });
      qc.invalidateQueries({ queryKey: workflowKeys.detail(version.workflowId) });
    },
  });
}

// useStartWorkflow creates a WorkflowRun from a published version.
export function useStartWorkflow() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: {
      workflowId: string;
      projectId: string;
      runContext?: string;
    }) => {
      const res = await workflowClient.startWorkflow(input);
      return res.run as WorkflowRun;
    },
    onSuccess: (run) => {
      qc.invalidateQueries({ queryKey: workflowKeys.runs(run.workflowId) });
    },
  });
}

// useDeleteWorkflow hard-deletes a workflow and invalidates the list.
export function useDeleteWorkflow() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (id: string) => {
      await workflowClient.deleteWorkflow({ id });
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: workflowKeys.list() });
    },
  });
}

// useBatchDeleteWorkflows hard-deletes multiple workflows by id.
export function useBatchDeleteWorkflows() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (ids: string[]) => {
      await Promise.allSettled(ids.map((id) => workflowClient.deleteWorkflow({ id })));
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: workflowKeys.list() });
    },
  });
}

// useDeleteWorkflowVersion deletes a single draft version and
// invalidates the versions list.
export function useDeleteWorkflowVersion() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: { workflowId: string; versionId: string }) => {
      await workflowClient.deleteWorkflowVersion(input);
    },
    onSuccess: (_data, input) => {
      qc.invalidateQueries({ queryKey: workflowKeys.versions(input.workflowId) });
      qc.invalidateQueries({ queryKey: workflowKeys.detail(input.workflowId) });
    },
  });
}

// useAbortWorkflow aborts a running workflow run.
export function useAbortWorkflow() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: { runId: string; reason?: string }) => {
      const res = await workflowClient.abortWorkflow(input);
      return res.run as WorkflowRun;
    },
    onSuccess: (run) => {
      qc.invalidateQueries({ queryKey: workflowKeys.run(run.id) });
      qc.invalidateQueries({ queryKey: workflowKeys.runs(run.workflowId) });
      qc.invalidateQueries({ queryKey: workflowKeys.stepRuns(run.id) });
    },
  });
}

// useGetWorkflowRun fetches a single workflow run by id.
export function useGetWorkflowRun(id: string) {
  return useQuery({
    queryKey: workflowKeys.run(id),
    queryFn: async () => {
      const res = await workflowClient.getWorkflowRun({ id });
      return res.run as WorkflowRun;
    },
    enabled: !!id,
  });
}

// useListWorkflowRuns fetches a page of runs for a workflow.
export function useListWorkflowRuns(workflowId: string, status?: WorkflowRunStatus) {
  return useQuery({
    queryKey: workflowKeys.runs(workflowId, status),
    queryFn: async () => {
      const res = await workflowClient.listWorkflowRuns({
        workflowId,
        pageSize: 100,
        status: status ?? undefined,
      });
      return res.runs as WorkflowRun[];
    },
    enabled: !!workflowId,
  });
}

// useGetWorkflowStepRuns fetches all step runs for a workflow run. Used
// by the run view to overlay live step transitions on the canvas.
export function useGetWorkflowStepRuns(runId: string) {
  return useQuery({
    queryKey: workflowKeys.stepRuns(runId),
    queryFn: async () => {
      const res = await workflowClient.getWorkflowStepRuns({ runId });
      return res.stepRuns as WorkflowStepRun[];
    },
    enabled: !!runId,
    // Poll every 2s so step transitions refresh even without the stream.
    refetchInterval: 2_000,
  });
}

// --- edit locks (docs/07 §3.3) ---

export function useAcquireWorkflowEditLock() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: { workflowId: string; actor: string }) => {
      const res = await workflowClient.acquireEditLock(input);
      return { lock: res.lock, acquired: res.acquired };
    },
    onSuccess: (_, variables) => {
      qc.invalidateQueries({ queryKey: workflowKeys.editLock(variables.workflowId) });
    },
  });
}

export function useReleaseWorkflowEditLock() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: { workflowId: string; actor: string }) => {
      await workflowClient.releaseEditLock(input);
    },
    onSuccess: (_, variables) => {
      qc.invalidateQueries({ queryKey: workflowKeys.editLock(variables.workflowId) });
    },
  });
}

export function useGetWorkflowEditLock(workflowId: string) {
  return useQuery({
    queryKey: workflowKeys.editLock(workflowId),
    queryFn: async () => {
      const res = await workflowClient.getEditLock({ workflowId });
      return res.lock ?? null;
    },
    enabled: !!workflowId,
    // Poll every 10s so other users' lock releases are detected.
    refetchInterval: 10_000,
  });
}
