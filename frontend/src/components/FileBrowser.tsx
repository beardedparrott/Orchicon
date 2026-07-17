import { useState, useCallback } from "react";
import {
  File,
  Folder,
  FolderOpen,
  CheckSquare,
  Square,
  Loader2,
} from "lucide-react";

import {
  useListProjectFiles,
  useUpdateProjectDir,
  useUpdateContextFiles,
  collectAllFilePaths,
} from "@/api/projectFiles";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import type { FileTreeEntry } from "@/api/gen/orchicon/api/v1/project_pb";

interface FileBrowserProps {
  projectId: string;
  projectDir: string;
  initialSelectedFiles: string[];
}

export function FileBrowser({
  projectId,
  projectDir,
  initialSelectedFiles,
}: FileBrowserProps) {
  const [editDir, setEditDir] = useState(false);
  const [dirInput, setDirInput] = useState(projectDir || "");

  const { data: fileTree, isLoading, error } = useListProjectFiles(projectId, 10);
  const updateDir = useUpdateProjectDir();
  const updateFiles = useUpdateContextFiles();

  const [selectedFiles, setSelectedFiles] = useState(initialSelectedFiles);

  const selectedSet = new Set(selectedFiles);

  const allFilePaths = fileTree ? collectAllFilePaths(fileTree) : [];

  const handleSaveDir = () => {
    updateDir.mutate(
      { id: projectId, projectDir: dirInput },
      {
        onSuccess: () => setEditDir(false),
      },
    );
  };

  const toggleFile = useCallback((path: string) => {
    setSelectedFiles((prev) => {
      const set = new Set(prev);
      if (set.has(path)) {
        set.delete(path);
      } else {
        set.add(path);
      }
      const next = Array.from(set);
      updateFiles.mutate({ id: projectId, contextFiles: next });
      return next;
    });
  }, [projectId, updateFiles]);

  const selectAll = () => {
    const next = [...new Set([...selectedFiles, ...allFilePaths])];
    setSelectedFiles(next);
    updateFiles.mutate({ id: projectId, contextFiles: next });
  };

  const deselectAll = () => {
    const allSet = new Set(allFilePaths);
    const next = selectedFiles.filter((f) => !allSet.has(f));
    setSelectedFiles(next);
    updateFiles.mutate({ id: projectId, contextFiles: next });
  };

  const renderTree = (node?: FileTreeEntry, depth = 0) => {
    if (!node) return null;
    return (
      <div key={node.path || node.name}>
        {node.children && node.children.length > 0 ? (
          <div>
            {depth === 0 ? null : (
              <div
                className="flex items-center gap-2 px-2 py-1 text-sm"
                style={{ paddingLeft: `${depth * 20 + 8}px` }}
              >
                <button
                  type="button"
                  className="text-muted-foreground hover:text-foreground"
                  onClick={() => toggleFile(node.path)}
                >
                  {selectedSet.has(node.path) ? (
                    <CheckSquare className="h-4 w-4" />
                  ) : (
                    <Square className="h-4 w-4" />
                  )}
                </button>
                <FolderOpen className="h-4 w-4 text-amber-500 shrink-0" />
                <span className="text-muted-foreground">{node.name}/</span>
              </div>
            )}
            {node.children.map((child) => renderTree(child, depth + 1))}
          </div>
        ) : (
          <div
            className="flex items-center gap-2 px-2 py-1 text-sm hover:bg-muted/50 rounded-sm"
            style={{ paddingLeft: `${depth * 20 + 8}px` }}
          >
            <button
              type="button"
              className="text-muted-foreground hover:text-foreground"
              onClick={() => toggleFile(node.path)}
            >
              {selectedSet.has(node.path) ? (
                <CheckSquare className="h-4 w-4" />
              ) : (
                <Square className="h-4 w-4" />
              )}
            </button>
            {node.isDir ? (
              <Folder className="h-4 w-4 text-amber-500 shrink-0" />
            ) : (
              <File className="h-4 w-4 text-sky-500 shrink-0" />
            )}
            <span className="truncate">{node.name}</span>
          </div>
        )}
      </div>
    );
  };

  return (
    <Card>
      <CardHeader>
        <CardTitle>Project Context Files</CardTitle>
        <CardDescription>
          Select files and folders from the project directory to include as
          context for AI workers. The contents of selected files will appear
          in the worker's prompt when this project is used in a workflow.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        {/* Project directory input */}
        <div className="space-y-2">
          <Label htmlFor="project-dir">Project directory</Label>
          <div className="flex gap-2">
            {editDir ? (
              <>
                <Input
                  id="project-dir"
                  value={dirInput}
                  onChange={(e) => setDirInput(e.target.value)}
                  placeholder="/path/to/your/project"
                  className="flex-1"
                />
                <Button onClick={handleSaveDir} disabled={updateDir.isPending}>
                  {updateDir.isPending ? "Saving…" : "Save"}
                </Button>
                <Button variant="outline" onClick={() => { setEditDir(false); setDirInput(projectDir); }}>
                  Cancel
                </Button>
              </>
            ) : (
              <>
                <div className="flex-1 rounded-md border bg-muted/30 px-3 py-2 text-sm font-mono text-muted-foreground">
                  {projectDir || "Not set"}
                </div>
                <Button variant="outline" onClick={() => setEditDir(true)}>
                  {projectDir ? "Change" : "Set directory"}
                </Button>
              </>
            )}
          </div>
        </div>

        {/* File tree */}
        {projectDir && (
          <div className="space-y-3">
            <div className="flex items-center justify-between">
              <span className="text-sm font-medium">Select context files</span>
              {allFilePaths.length > 0 && (
                <div className="flex gap-2">
                  <Button
                    variant="outline"
                    size="sm"
                    onClick={selectAll}
                    className="text-xs"
                  >
                    Select all
                  </Button>
                  <Button
                    variant="outline"
                    size="sm"
                    onClick={deselectAll}
                    className="text-xs"
                  >
                    Deselect all
                  </Button>
                </div>
              )}
            </div>

            {isLoading ? (
              <div className="flex items-center gap-2 py-4 text-sm text-muted-foreground">
                <Loader2 className="h-4 w-4 animate-spin" />
                Loading file tree…
              </div>
            ) : error ? (
              <p className="text-sm text-destructive">
                Failed to list files: {String(error)}
              </p>
            ) : !fileTree || !fileTree.children || fileTree.children.length === 0 ? (
              <p className="text-sm text-muted-foreground">
                No files found in the project directory.
              </p>
            ) : (
              <div className="rounded-md border divide-y max-h-80 overflow-y-auto">
                {fileTree.children.map((child: FileTreeEntry) => renderTree(child))}
              </div>
            )}

            {/* Selected files summary */}
            {selectedFiles.length > 0 && (
              <div>
                <p className="text-xs text-muted-foreground mb-1">
                  {selectedFiles.length} file{selectedFiles.length !== 1 ? "s" : ""} selected:
                </p>
                <div className="flex flex-wrap gap-1">
                  {selectedFiles.slice(0, 20).map((f) => (
                    <span
                      key={f}
                      className="inline-flex items-center gap-1 rounded-md bg-primary/10 px-2 py-0.5 text-xs font-mono"
                    >
                      <File className="h-3 w-3" />
                      {f}
                      <button
                        type="button"
                        className="hover:text-destructive"
                        onClick={() => toggleFile(f)}
                      >
                        ×
                      </button>
                    </span>
                  ))}
                  {selectedFiles.length > 20 && (
                    <span className="text-xs text-muted-foreground">
                      …and {selectedFiles.length - 20} more
                    </span>
                  )}
                </div>
              </div>
            )}
          </div>
        )}
      </CardContent>
    </Card>
  );
}
