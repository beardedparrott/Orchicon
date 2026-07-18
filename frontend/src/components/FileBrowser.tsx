import { useState, useCallback, useRef } from "react";
import {
  File,
  Folder,
  ChevronRight,
  ChevronDown,
  CheckSquare,
  Square,
  Loader2,
} from "lucide-react";

import {
  useListProjectDir,
  useUpdateProjectDir,
  useUpdateContextFiles,
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
  const [selectedFiles, setSelectedFiles] = useState<string[]>(initialSelectedFiles);
  const [expandedPaths, setExpandedPaths] = useState<Set<string>>(new Set());
  const selectedSet = useRef(new Set(selectedFiles));

  // Keep the ref in sync
  selectedSet.current = new Set(selectedFiles);

  const updateDir = useUpdateProjectDir();
  const updateFiles = useUpdateContextFiles();

  const persistSelection = useCallback((next: string[]) => {
    setSelectedFiles(next);
    updateFiles.mutate({ id: projectId, contextFiles: next });
  }, [projectId, updateFiles]);

  // Directory input
  const handleSaveDir = () => {
    updateDir.mutate(
      { id: projectId, projectDir: dirInput },
      { onSuccess: () => setEditDir(false) },
    );
  };

  // Expand/collapse a directory
  const toggleExpanded = (path: string) => {
    setExpandedPaths((prev) => {
      const next = new Set(prev);
      if (next.has(path)) {
        next.delete(path);
      } else {
        next.add(path);
      }
      return next;
    });
  };

  // Toggle selection of a single entry
  const toggleEntry = (path: string) => {
    const set = selectedSet.current;
    const next = set.has(path)
      ? selectedFiles.filter((f) => f !== path)
      : [...selectedFiles, path];
    persistSelection(next);
  };

  // Select all / deselect all
  const selectAll = (entries: FileTreeEntry[], loadedDirs: Map<string, FileTreeEntry[]>) => {
    const all: string[] = [];
    const walk = (e: FileTreeEntry) => {
      if (!e.isDir) {
        if (e.path) all.push(e.path);
        return;
      }
      const children = loadedDirs.get(e.path);
      if (children) {
        for (const c of children) walk(c);
      }
    };
    for (const e of entries) walk(e);
    const next = [...new Set([...selectedFiles, ...all])];
    persistSelection(next);
  };

  const deselectAll = (entries: FileTreeEntry[], loadedDirs: Map<string, FileTreeEntry[]>) => {
    const allSet = new Set<string>();
    const walk = (e: FileTreeEntry) => {
      if (!e.isDir) {
        if (e.path) allSet.add(e.path);
        return;
      }
      const children = loadedDirs.get(e.path);
      if (children) {
        for (const c of children) walk(c);
      }
    };
    for (const e of entries) walk(e);
    const next = selectedFiles.filter((f) => !allSet.has(f));
    persistSelection(next);
  };

  return (
    <Card>
      <CardHeader>
        <CardTitle>Project Context Files</CardTitle>
        <CardDescription>
          Select files and folders from the project directory to include as
          context for AI workers. Click a folder to expand it. Checked file
          paths are listed in the worker's prompt.
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
                  className="flex-1 font-mono"
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
                <div className="flex-1 rounded-md border bg-muted/30 px-3 py-2 text-sm font-mono text-muted-foreground truncate">
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
          <DirectoryTree
            projectId={projectId}
            subpath=""
            depth={0}
            selectedSet={selectedSet.current}
            expandedPaths={expandedPaths}
            onToggleExpanded={toggleExpanded}
            onToggleEntry={toggleEntry}
            onSelectAll={selectAll}
            onDeselectAll={deselectAll}
          />
        )}

        {/* Selected files summary */}
        {selectedFiles.length > 0 && (
          <div>
            <p className="text-xs text-muted-foreground mb-1">
              {selectedFiles.length} path{selectedFiles.length !== 1 ? "s" : ""} selected:
            </p>
            <div className="flex flex-wrap gap-1 max-h-24 overflow-y-auto">
              {selectedFiles.slice(0, 30).map((f) => (
                <span
                  key={f}
                  className="inline-flex items-center gap-1 rounded-md bg-primary/10 px-2 py-0.5 text-xs font-mono"
                >
                  <File className="h-3 w-3 shrink-0" />
                  <span className="truncate max-w-[200px]">{f}</span>
                  <button
                    type="button"
                    className="hover:text-destructive shrink-0"
                    onClick={() => toggleEntry(f)}
                  >
                    ×
                  </button>
                </span>
              ))}
              {selectedFiles.length > 30 && (
                <span className="text-xs text-muted-foreground">
                  …and {selectedFiles.length - 30} more
                </span>
              )}
            </div>
          </div>
        )}
      </CardContent>
    </Card>
  );
}

// ─── Directory tree node ────────────────────────────────────────────

interface DirectoryTreeProps {
  projectId: string;
  subpath: string;
  depth: number;
  selectedSet: Set<string>;
  expandedPaths: Set<string>;
  onToggleExpanded: (path: string) => void;
  onToggleEntry: (path: string) => void;
  onSelectAll: (entries: FileTreeEntry[], loadedDirs: Map<string, FileTreeEntry[]>) => void;
  onDeselectAll: (entries: FileTreeEntry[], loadedDirs: Map<string, FileTreeEntry[]>) => void;
}

function DirectoryTree({
  projectId,
  subpath,
  depth,
  selectedSet,
  expandedPaths,
  onToggleExpanded,
  onToggleEntry,
  onSelectAll,
  onDeselectAll,
}: DirectoryTreeProps) {
  const { data, isLoading, error } = useListProjectDir(projectId, subpath);
  const entries = data?.entries ?? [];

  const loadedDirs = new Map<string, FileTreeEntry[]>();

  if (isLoading) {
    return (
      <div className="flex items-center gap-2 py-2 text-sm text-muted-foreground" style={{ paddingLeft: depth * 20 }}>
        <Loader2 className="h-4 w-4 animate-spin" />
        Loading…
      </div>
    );
  }

  if (error) {
    return (
      <p className="text-sm text-destructive" style={{ paddingLeft: depth * 20 }}>
        Error: {String(error)}
      </p>
    );
  }

  if (entries.length === 0 && depth === 0) {
    return <p className="text-sm text-muted-foreground">The project directory is empty.</p>;
  }

  return (
    <div className="rounded-md border divide-y max-h-[400px] overflow-y-auto">
      {depth === 0 && entries.length > 1 && (
        <div className="flex items-center gap-2 px-3 py-1.5 bg-muted/20 sticky top-0 border-b">
          <span className="text-xs text-muted-foreground mr-auto">
            {data?.dirName || ""}
          </span>
          <button
            type="button"
            className="text-xs text-muted-foreground hover:text-foreground"
            onClick={() => onSelectAll(entries, loadedDirs)}
          >
            Select all
          </button>
          <span className="text-xs text-muted-foreground">·</span>
          <button
            type="button"
            className="text-xs text-muted-foreground hover:text-foreground"
            onClick={() => onDeselectAll(entries, loadedDirs)}
          >
            Deselect all
          </button>
        </div>
      )}
      {entries.map((entry) => (
        <DirEntryRow
          key={entry.path}
          entry={entry}
          depth={depth}
          projectId={projectId}
          selectedSet={selectedSet}
          expandedPaths={expandedPaths}
          onToggleExpanded={onToggleExpanded}
          onToggleEntry={onToggleEntry}
          onSelectAll={onSelectAll}
          onDeselectAll={onDeselectAll}
        />
      ))}
    </div>
  );
}

// ─── Single directory entry row ─────────────────────────────────────

interface DirEntryRowProps {
  entry: FileTreeEntry;
  depth: number;
  projectId: string;
  selectedSet: Set<string>;
  expandedPaths: Set<string>;
  onToggleExpanded: (path: string) => void;
  onToggleEntry: (path: string) => void;
  onSelectAll: (entries: FileTreeEntry[], loadedDirs: Map<string, FileTreeEntry[]>) => void;
  onDeselectAll: (entries: FileTreeEntry[], loadedDirs: Map<string, FileTreeEntry[]>) => void;
}

function DirEntryRow({
  entry,
  depth,
  projectId,
  selectedSet,
  expandedPaths,
  onToggleExpanded,
  onToggleEntry,
  onSelectAll,
  onDeselectAll,
}: DirEntryRowProps) {
  const isExpanded = expandedPaths.has(entry.path);
  const isSelected = selectedSet.has(entry.path);

  if (entry.isDir) {
    return (
      <div>
        <div
          className="flex items-center gap-1 px-2 py-1.5 text-sm hover:bg-muted/40 cursor-pointer"
          style={{ paddingLeft: depth * 20 + 8 }}
        >
          {/* Expand/collapse arrow */}
          <button
            type="button"
            className="text-muted-foreground hover:text-foreground shrink-0"
            onClick={(e) => { e.stopPropagation(); onToggleExpanded(entry.path); }}
          >
            {isExpanded ? (
              <ChevronDown className="h-4 w-4" />
            ) : (
              <ChevronRight className="h-4 w-4" />
            )}
          </button>

          {/* Checkbox */}
          <button
            type="button"
            className="text-muted-foreground hover:text-foreground shrink-0"
            onClick={() => onToggleEntry(entry.path)}
          >
            {isSelected ? (
              <CheckSquare className="h-4 w-4" />
            ) : (
              <Square className="h-4 w-4" />
            )}
          </button>

          {/* Folder icon */}
          <Folder className="h-4 w-4 text-amber-500 shrink-0" />

          {/* Name */}
          <span
            className="truncate"
            onClick={() => onToggleExpanded(entry.path)}
          >
            {entry.name}
          </span>
        </div>

        {/* Children (lazy loaded when expanded) */}
        {isExpanded && (
          <DirectoryTree
            projectId={projectId}
            subpath={entry.path}
            depth={depth + 1}
            selectedSet={selectedSet}
            expandedPaths={expandedPaths}
            onToggleExpanded={onToggleExpanded}
            onToggleEntry={onToggleEntry}
            onSelectAll={onSelectAll}
            onDeselectAll={onDeselectAll}
          />
        )}
      </div>
    );
  }

  // File entry
  return (
    <div
      className="flex items-center gap-1 px-2 py-1.5 text-sm hover:bg-muted/40 cursor-pointer"
      style={{ paddingLeft: depth * 20 + 8 }}
    >
      {/* Checkbox */}
      <button
        type="button"
        className="text-muted-foreground hover:text-foreground shrink-0"
        onClick={() => onToggleEntry(entry.path)}
      >
        {isSelected ? (
          <CheckSquare className="h-4 w-4" />
        ) : (
          <Square className="h-4 w-4" />
        )}
      </button>

      {/* File icon */}
      <File className="h-4 w-4 text-sky-500 shrink-0" />

      {/* Name */}
      <span className="truncate">{entry.name}</span>
    </div>
  );
}
