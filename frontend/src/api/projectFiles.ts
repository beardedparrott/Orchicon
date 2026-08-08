import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { projectClient } from "@/api/clients";
import { projectKeys } from "@/api/projects";
import type { FileTreeEntry, Project } from "@/api/gen/orchicon/api/v1/project_pb";

// useListProjectDir fetches the immediate children of a directory.
// If dirPath is provided, lists that path directly (filesystem browse).
// Otherwise uses projectId + subpath.
export function useListProjectDir(
  projectId: string,
  subpath: string,
  dirPath?: string,
) {
  return useQuery({
    queryKey: [...projectKeys.all, "files", projectId, subpath, dirPath] as const,
    queryFn: async () => {
      const res = await projectClient.listProjectFiles({
        id: projectId,
        subpath,
        dirPath,
      });
      return {
        parentPath: res.parentPath,
        dirName: res.dirName,
        entries: (res.entries || []) as FileTreeEntry[],
      };
    },
    enabled: !!projectId || !!dirPath,
    staleTime: 30_000,
    // Poll so the browse tree reflects the latest files/directories on the
    // machine while the page is open — the control plane lists the host
    // filesystem live (bind-mounted into the container), but nothing
    // re-fetched without a poll. The project/work-item context section
    // "should automatically have the most newest files".
    refetchInterval: 5_000,
  });
}

// useListDirPath fetches children of an arbitrary directory path
// (filesystem browse mode — no project needed).
export function useListDirPath(dirPath: string) {
  return useQuery({
    queryKey: ["filesystem", dirPath] as const,
    queryFn: async () => {
      const res = await projectClient.listProjectFiles({
        id: "",
        subpath: "",
        dirPath,
      });
      return {
        parentPath: res.parentPath,
        dirName: res.dirName,
        entries: (res.entries || []) as FileTreeEntry[],
      };
    },
    enabled: !!dirPath,
    staleTime: 30_000,
    refetchInterval: 5_000,
  });
}

// useUpdateProjectDir updates the project_dir on a project.
export function useUpdateProjectDir() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: { id: string; projectDir: string }) => {
      const res = await projectClient.updateProject({
        id: input.id,
        projectDir: input.projectDir,
      });
      return res.project as Project;
    },
    onSuccess: (project) => {
      qc.invalidateQueries({ queryKey: projectKeys.list() });
      qc.invalidateQueries({ queryKey: projectKeys.detail(project.id) });
      qc.invalidateQueries({
        queryKey: [...projectKeys.all, "files", project.id],
      });
    },
  });
}

// useUpdateContextFiles saves the selected context file paths.
export function useUpdateContextFiles() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: { id: string; contextFiles: string[] }) => {
      const res = await projectClient.updateProject({
        id: input.id,
        contextFiles: { files: input.contextFiles },
      });
      return res.project as Project;
    },
    onSuccess: (project) => {
      qc.invalidateQueries({ queryKey: projectKeys.list() });
      qc.invalidateQueries({ queryKey: projectKeys.detail(project.id) });
      // A context-files save can change what the browse tree should show
      // (the stored selection) — clear the file-tree cache too.
      qc.invalidateQueries({
        queryKey: [...projectKeys.all, "files", project.id],
      });
    },
  });
}
