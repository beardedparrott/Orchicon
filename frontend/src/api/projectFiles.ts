import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { projectClient } from "@/api/clients";
import { projectKeys } from "@/api/projects";
import type {
  FileTreeEntry,
  Project,
} from "@/api/gen/orchicon/api/v1/project_pb";

// useListProjectFiles fetches the file tree for a project's directory.
export function useListProjectFiles(
  projectId: string,
  maxDepth = 5,
) {
  return useQuery({
    queryKey: [...projectKeys.all, "files", projectId] as const,
    queryFn: async () => {
      const res = await projectClient.listProjectFiles({
        id: projectId,
        maxDepth,
      });
      return res.root as FileTreeEntry | undefined;
    },
    enabled: !!projectId,
  });
}

// useUpdateProjectDir updates the project_dir on a project.
export function useUpdateProjectDir() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: {
      id: string;
      projectDir: string;
    }) => {
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
    mutationFn: async (input: {
      id: string;
      contextFiles: string[];
    }) => {
      const res = await projectClient.updateProject({
        id: input.id,
        contextFiles: { files: input.contextFiles },
      });
      return res.project as Project;
    },
    onSuccess: (project) => {
      qc.invalidateQueries({ queryKey: projectKeys.list() });
      qc.invalidateQueries({ queryKey: projectKeys.detail(project.id) });
    },
  });
}

// collectAllFilePaths recursively collects all file paths from a
// FileTreeEntry tree, returning the relative paths.
export function collectAllFilePaths(root?: FileTreeEntry): string[] {
  if (!root) return [];
  const paths: string[] = [];
  function walk(node: FileTreeEntry) {
    if (!node.isDir && node.path) {
      paths.push(node.path);
    }
    if (node.children) {
      for (const child of node.children) {
        walk(child);
      }
    }
  }
  walk(root);
  return paths;
}
