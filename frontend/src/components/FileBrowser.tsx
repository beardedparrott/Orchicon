import { useState } from "react";
import {
  File,
  Folder,
  ChevronRight,
  ChevronDown,
  CheckSquare,
  Square,
  Loader2,
  ArrowUp,
  Search,
  X,
} from "lucide-react";

import {
  useListProjectDir,
  useListDirPath,
  useUpdateProjectDir,
  useUpdateContextFiles,
} from "@/api/projectFiles";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
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
  const [browsing, setBrowsing] = useState(!projectDir);
  const [browsePath, setBrowsePath] = useState("");
  const [selectedFiles, setSelectedFiles] = useState<string[]>(initialSelectedFiles);
  const [expandedPaths, setExpandedPaths] = useState<Set<string>>(new Set());
  const [searchQuery, setSearchQuery] = useState("");

  const updateDir = useUpdateProjectDir();
  const updateFiles = useUpdateContextFiles();

  const persistSelection = (next: string[]) => {
    setSelectedFiles(next);
    updateFiles.mutate({ id: projectId, contextFiles: next });
  };

  const toggleExpanded = (path: string) => {
    setExpandedPaths((prev) => {
      const next = new Set(prev);
      if (next.has(path)) next.delete(path);
      else next.add(path);
      return next;
    });
  };

  const toggleEntry = (path: string) => {
    const set = new Set(selectedFiles);
    const next = set.has(path)
      ? selectedFiles.filter((f) => f !== path)
      : [...selectedFiles, path];
    persistSelection(next);
  };

  const selectAll = (entries: FileTreeEntry[]) => {
    const all: string[] = [];
    const walk = (e: FileTreeEntry) => {
      if (!e.isDir && e.path) all.push(e.path);
      if (e.children) for (const c of e.children) walk(c);
    };
    for (const e of entries) walk(e);
    persistSelection([...new Set([...selectedFiles, ...all])]);
  };

  const deselectAll = (entries: FileTreeEntry[]) => {
    const allSet = new Set<string>();
    const walk = (e: FileTreeEntry) => {
      if (!e.isDir && e.path) allSet.add(e.path);
      if (e.children) for (const c of e.children) walk(c);
    };
    for (const e of entries) walk(e);
    persistSelection(selectedFiles.filter((f) => !allSet.has(f)));
  };

  // Browse mode: pick a project directory by navigating the filesystem
  const handleBrowseSelect = (path: string) => {
    updateDir.mutate(
      { id: projectId, projectDir: path },
      { onSuccess: () => setBrowsing(false) },
    );
  };

  // Browse mode: select a file — sets project_dir to the current browse
  // directory and adds the file to context_files.
  const handleSelectFile = (fileRelPath: string) => {
    const browseDir = browsePath || "~";
    const nextFiles = selectedFiles.includes(fileRelPath)
      ? selectedFiles
      : [...selectedFiles, fileRelPath];
    setSelectedFiles(nextFiles);
    updateDir.mutate(
      { id: projectId, projectDir: browseDir },
      {
        onSuccess: () => {
          setBrowsing(false);
          updateFiles.mutate({ id: projectId, contextFiles: nextFiles });
        },
      },
    );
  };

  // Initial browse path: home directory
  const initialBrowsePath = browsePath || "~";

  return (
    <Card>
      <CardHeader>
        <CardTitle>Project Context Files</CardTitle>
        <CardDescription>
          {browsing
            ? "Navigate to your project directory and click \"Select this folder\"."
            : "Select files and folders to include as context for AI workers. Click a folder to expand it."}
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        {/* Search bar */}
        <div className="relative">
          <Search className="absolute left-2.5 top-1/2 -translate-y-1/2 h-4 w-4 text-muted-foreground" />
          <input
            type="text"
            placeholder="Search files and folders…"
            value={searchQuery}
            onChange={(e) => setSearchQuery(e.target.value)}
            className="w-full rounded-md border bg-background pl-8 pr-8 py-1.5 text-sm outline-none focus:ring-2 focus:ring-ring"
          />
          {searchQuery && (
            <button
              type="button"
              className="absolute right-2 top-1/2 -translate-y-1/2 text-muted-foreground hover:text-foreground"
              onClick={() => setSearchQuery("")}
            >
              <X className="h-4 w-4" />
            </button>
          )}
        </div>

        {browsing ? (
          <>
            {/* Breadcrumb */}
            <div className="flex items-center gap-2 text-sm">
              <span className="text-muted-foreground">Browse:</span>
              <span className="font-mono text-xs truncate">{initialBrowsePath}</span>
              <Button
                variant="ghost"
                size="sm"
                className="text-xs h-6"
                onClick={() => setBrowsePath("")}
              >
                Home
              </Button>
            </div>
            <BrowseTree
              path={initialBrowsePath}
              searchQuery={searchQuery}
              onSelect={handleBrowseSelect}
              onSelectFile={handleSelectFile}
              onNavigate={setBrowsePath}
            />
            {updateDir.isPending && (
              <p className="text-xs text-muted-foreground">Saving directory…</p>
            )}
          </>
        ) : (
          <>
            {/* Set directory */}
            <div className="flex items-center gap-2">
              <Label className="shrink-0 text-sm">Project directory</Label>
              <span className="flex-1 truncate rounded-md border bg-muted/30 px-3 py-1.5 text-xs font-mono text-muted-foreground">
                {projectDir}
              </span>
              <Button
                variant="outline"
                size="sm"
                onClick={() => setBrowsing(true)}
              >
                Change
              </Button>
            </div>

            {/* File tree with checkboxes */}
            {projectDir && (
              <FileTreeContainer
                projectId={projectId}
                subpath=""
                searchQuery={searchQuery}
                selectedSet={new Set(selectedFiles)}
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
          </>
        )}
      </CardContent>
    </Card>
  );
}

// ─── Browse filesystem tree (pick a directory) ────────────────────

interface BrowseTreeProps {
  path: string;
  searchQuery: string;
  onSelect: (path: string) => void;
  onSelectFile: (path: string) => void;
  onNavigate: (path: string) => void;
}

function BrowseTree({ path, searchQuery, onSelect, onSelectFile, onNavigate }: BrowseTreeProps) {
  const { data, isLoading, error } = useListDirPath(path);

  const q = searchQuery.toLowerCase().trim();
  const allDirs = (data?.entries ?? []).filter((e) => e.isDir);
  const allFiles = (data?.entries ?? []).filter((e) => !e.isDir);
  const dirs = q ? allDirs.filter((e) => e.name.toLowerCase().includes(q)) : allDirs;
  const files = q ? allFiles.filter((e) => e.name.toLowerCase().includes(q)) : allFiles;

  // Parent directory for "go up"
  const goUp = () => {
    const parts = path.replace(/^~/, "").split("/").filter(Boolean);
    if (parts.length === 0) return;
    const parent = parts.slice(0, -1).join("/");
    onNavigate(parent ? `~/${parent}` : "~");
  };

  if (isLoading) {
    return (
      <div className="flex items-center gap-2 py-4 text-sm text-muted-foreground">
        <Loader2 className="h-4 w-4 animate-spin" />
        Loading…
      </div>
    );
  }

  if (error) {
    return <p className="text-sm text-destructive">Error: {String(error)}</p>;
  }

  return (
    <div className="rounded-md border max-h-[400px] overflow-y-auto">
      {/* Go up */}
      <div
        className="flex items-center gap-2 px-3 py-2 text-sm hover:bg-muted/40 cursor-pointer border-b"
        onClick={goUp}
      >
        <ArrowUp className="h-4 w-4 text-muted-foreground" />
        <span className="text-muted-foreground">..</span>
      </div>

      {/* Directories first */}
      {dirs.length === 0 && files.length === 0 && (
        <p className="px-3 py-4 text-sm text-muted-foreground">Empty directory</p>
      )}

      {dirs.map((entry) => (
        <div
          key={entry.path}
          className="flex items-center gap-2 px-3 py-2 text-sm hover:bg-muted/40 cursor-pointer border-b last:border-0"
        >
          <Folder className="h-4 w-4 text-amber-500 shrink-0" />
          <span
            className="flex-1 truncate"
            onClick={() => onNavigate(entry.path.startsWith("~") ? entry.path : `~/${entry.path}`)}
          >
            {entry.name}/
          </span>
          <Button
            variant="outline"
            size="sm"
            className="text-xs h-7 shrink-0"
            onClick={() => onSelect(entry.path.startsWith("~") ? entry.path : `~/${entry.path}`)}
          >
            Select this folder
          </Button>
        </div>
      ))}

      {files.slice(0, 20).map((entry) => (
        <div
          key={entry.path}
          className="flex items-center gap-2 px-3 py-2 text-sm hover:bg-muted/40 cursor-pointer border-b last:border-0"
        >
          <File className="h-4 w-4 shrink-0" />
          <span
            className="flex-1 truncate"
            onClick={() => onSelectFile(entry.path)}
          >
            {entry.name}
          </span>
          <Button
            variant="outline"
            size="sm"
            className="text-xs h-7 shrink-0"
            onClick={() => onSelectFile(entry.path)}
          >
            Select
          </Button>
        </div>
      ))}
      {files.length > 20 && (
        <p className="px-3 py-1 text-xs text-muted-foreground">
          …{files.length - 20} more files
        </p>
      )}
    </div>
  );
}

// ─── File tree with checkboxes (after project_dir is set) ──────────

interface FileTreeContainerProps {
  projectId: string;
  subpath: string;
  depth?: number;
  searchQuery: string;
  selectedSet: Set<string>;
  expandedPaths: Set<string>;
  onToggleExpanded: (path: string) => void;
  onToggleEntry: (path: string) => void;
  onSelectAll: (entries: FileTreeEntry[]) => void;
  onDeselectAll: (entries: FileTreeEntry[]) => void;
}

function FileTreeContainer({
  projectId,
  subpath,
  depth = 0,
  searchQuery,
  selectedSet,
  expandedPaths,
  onToggleExpanded,
  onToggleEntry,
  onSelectAll,
  onDeselectAll,
}: FileTreeContainerProps) {
  const { data, isLoading, error } = useListProjectDir(projectId, subpath);
  const q = searchQuery.toLowerCase().trim();
  const allEntries = data?.entries ?? [];
  const entries = q ? allEntries.filter((e) => e.name.toLowerCase().includes(q)) : allEntries;

  if (isLoading) {
    return (
      <div className="flex items-center gap-2 py-2 text-sm text-muted-foreground">
        <Loader2 className="h-4 w-4 animate-spin" />
        Loading…
      </div>
    );
  }

  if (error) {
    return <p className="text-sm text-destructive">Error: {String(error)}</p>;
  }

  if (entries.length === 0 && depth === 0) {
    return <p className="text-sm text-muted-foreground">The project directory is empty.</p>;
  }

  return (
    <div className="rounded-md border divide-y max-h-[400px] overflow-y-auto">
      {depth === 0 && entries.length > 0 && (
        <div className="flex items-center gap-2 px-3 py-1.5 bg-muted/20 sticky top-0 border-b">
          <span className="text-xs text-muted-foreground mr-auto">
            {data?.dirName || ""}
          </span>
          <button
            type="button"
            className="text-xs text-muted-foreground hover:text-foreground"
            onClick={() => onSelectAll(entries)}
          >
            Select all
          </button>
          <span className="text-xs text-muted-foreground">·</span>
          <button
            type="button"
            className="text-xs text-muted-foreground hover:text-foreground"
            onClick={() => onDeselectAll(entries)}
          >
            Deselect all
          </button>
        </div>
      )}
      {entries.map((entry) => (
        <FileRow
          key={entry.path}
          entry={entry}
          depth={depth}
          projectId={projectId}
          searchQuery={searchQuery}
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

// ─── Single row in the file tree ──────────────────────────────────

interface FileRowProps {
  entry: FileTreeEntry;
  depth: number;
  projectId: string;
  searchQuery: string;
  selectedSet: Set<string>;
  expandedPaths: Set<string>;
  onToggleExpanded: (path: string) => void;
  onToggleEntry: (path: string) => void;
  onSelectAll: (entries: FileTreeEntry[]) => void;
  onDeselectAll: (entries: FileTreeEntry[]) => void;
}

function FileRow({
  entry,
  depth,
  projectId,
  searchQuery,
  selectedSet,
  expandedPaths,
  onToggleExpanded,
  onToggleEntry,
  onSelectAll,
  onDeselectAll,
}: FileRowProps) {
  const isExpanded = expandedPaths.has(entry.path);
  const isSelected = selectedSet.has(entry.path);

  if (entry.isDir) {
    return (
      <div>
        <div
          className="flex items-center gap-1 px-2 py-1.5 text-sm hover:bg-muted/40 cursor-pointer"
          style={{ paddingLeft: depth * 20 + 8 }}
        >
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
          <Folder className="h-4 w-4 text-amber-500 shrink-0" />
          <span className="truncate" onClick={() => onToggleExpanded(entry.path)}>
            {entry.name}
          </span>
        </div>
        {isExpanded && (
          <FileTreeContainer
            projectId={projectId}
            subpath={entry.path}
            depth={depth + 1}
            searchQuery={searchQuery}
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

  return (
    <div
      className="flex items-center gap-1 px-2 py-1.5 text-sm hover:bg-muted/40 cursor-pointer"
      style={{ paddingLeft: depth * 20 + 8 }}
    >
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
      <File className="h-4 w-4 text-sky-500 shrink-0" />
      <span className="truncate">{entry.name}</span>
    </div>
  );
}
