import { useCallback, useEffect, useState } from "react";
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
import type { FileTreeEntry } from "@/api/gen/orchicon/api/v1/project_pb";

interface FileBrowserProps {
  projectId: string;
  projectDir: string;
  initialSelectedFiles: string[];
  readOnly?: boolean;
  /** When provided, selection is reported back instead of persisting to
   *  the project (used by the work item forms, which carry their own
   *  context_files field). When absent, selection saves to the project
   *  via useUpdateContextFiles (project page). */
  onChange?: (next: string[]) => void;
  /** Card title; defaults to "Project Context Files". */
  title?: string;
  /** Card description when editing. */
  description?: string;
  /** Read-only empty-state hint. */
  emptyHint?: string;
}

export function FileBrowser({
  projectId,
  projectDir,
  initialSelectedFiles,
  readOnly = false,
  onChange,
  title = "Project Context Files",
  description,
  emptyHint,
}: FileBrowserProps) {
  const [showDirPicker, setShowDirPicker] = useState(false);
  const [browsePath, setBrowsePath] = useState("");
  const [selectedFiles, setSelectedFiles] = useState<string[]>(initialSelectedFiles);
  const [expandedPaths, setExpandedPaths] = useState<Set<string>>(new Set());
  const [searchQuery, setSearchQuery] = useState("");
  // dirPaths tracks which selected paths are directories, matched against
  // the loaded tree (a persisted API string doesn't carry is_dir — the
  // tree-match heuristic from the architecture note). Seeded from the
  // project root listing so read-only views can still show a folder icon.
  const [dirPaths, setDirPaths] = useState<Set<string>>(new Set());
  const rootListing = useListProjectDir(projectId, "");
  useEffect(() => {
    if (readOnly && rootListing.data?.entries) {
      setDirPaths((prev) => {
        if (prev.size > 0) return prev;
        const next = new Set(prev);
        for (const e of rootListing.data?.entries ?? []) {
          if (e.isDir && e.path) next.add(e.path);
        }
        return next;
      });
    }
  }, [readOnly, rootListing.data]);

  const updateDir = useUpdateProjectDir();
  const updateFiles = useUpdateContextFiles();

  const persistSelection = (next: string[]) => {
    setSelectedFiles(next);
    if (onChange) {
      onChange(next);
      return;
    }
    updateFiles.mutate({ id: projectId, contextFiles: next });
  };

  const registerDir = useCallback((path: string) => {
    setDirPaths((prev) => {
      if (prev.has(path)) return prev;
      const next = new Set(prev);
      next.add(path);
      return next;
    });
  }, []);

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
      if (e.path) all.push(e.path);
      if (e.children) for (const c of e.children) walk(c);
    };
    for (const e of entries) walk(e);
    persistSelection([...new Set([...selectedFiles, ...all])]);
  };

  const deselectAll = (entries: FileTreeEntry[]) => {
    const allSet = new Set<string>();
    const walk = (e: FileTreeEntry) => {
      if (e.path) allSet.add(e.path);
      if (e.children) for (const c of e.children) walk(c);
    };
    for (const e of entries) walk(e);
    persistSelection(selectedFiles.filter((f) => !allSet.has(f)));
  };

  const handleBrowseSelect = (path: string) => {
    if (onChange) {
      // Work-item mode: there's no project_dir to set — "Select this
      // folder" just navigates into it so the checkbox tree can pick
      // it (or files within it).
      setBrowsePath(path);
      setShowDirPicker(false);
      return;
    }
    updateDir.mutate(
      { id: projectId, projectDir: path },
      { onSuccess: () => setShowDirPicker(false) },
    );
  };

  const handleSelectFile = (fileRelPath: string) => {
    const browseDir = browsePath || "~";
    const nextFiles = selectedFiles.includes(fileRelPath)
      ? selectedFiles
      : [...selectedFiles, fileRelPath];
    setSelectedFiles(nextFiles);
    if (onChange) {
      onChange(nextFiles);
      setShowDirPicker(false);
      return;
    }
    updateDir.mutate(
      { id: projectId, projectDir: browseDir },
      {
        onSuccess: () => {
          setShowDirPicker(false);
          updateFiles.mutate({ id: projectId, contextFiles: nextFiles });
        },
      },
    );
  };

  const rootDir = browsePath || projectDir || "~";
  const hasDir = !!projectDir;

  return (
    <Card>
      <CardHeader>
        <CardTitle>{title}</CardTitle>
        <CardDescription>
          {readOnly
            ? emptyHint ??
              "Context files selected for this project. Click Edit to modify."
            : description ??
              (showDirPicker
                ? "Navigate to a directory and click \"Select this folder\" to set it as the project root."
                : "Expand folders and check files or directories to include as context for AI workers.")}
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        {readOnly ? (
          <>
            {/* Read-only: just list the selected files */}
            <div className="flex items-center gap-2 text-sm">
              <span className="text-muted-foreground shrink-0">Root:</span>
              <span className="flex-1 truncate rounded-xl glass-panel px-2 py-1 font-mono text-xs text-muted-foreground border border-white/10">
                {hasDir ? projectDir : "~ (not set)"}
              </span>
            </div>
            {selectedFiles.length === 0 ? (
              <p className="text-sm text-muted-foreground">
                No context paths selected. Click Edit to configure.
              </p>
            ) : (
              <div className="space-y-1">
                <p className="text-xs text-muted-foreground">
                  {selectedFiles.length} path{selectedFiles.length !== 1 ? "s" : ""} selected as context:
                </p>
                <div className="rounded-md border divide-y max-h-[300px] overflow-y-auto">
                  {selectedFiles.map((f) => (
                    <div
                      key={f}
                      className="flex items-center gap-2 px-3 py-2 text-xs font-mono"
                    >
                      {dirPaths.has(f) ? (
                        <Folder aria-hidden="true" className="h-3 w-3 shrink-0 text-amber-500" />
                      ) : (
                        <File aria-hidden="true" className="h-3 w-3 shrink-0 text-sky-500" />
                      )}
                      <span className="truncate">{f}</span>
                    </div>
                  ))}
                </div>
              </div>
            )}
          </>
        ) : (
          <>
            {/* Search bar */}
            <div className="relative">
              <Search aria-hidden="true" className="absolute left-2.5 top-1/2 -translate-y-1/2 h-4 w-4 text-muted-foreground" />
              <input
                type="text"
                placeholder="Search files and folders…"
                value={searchQuery}
                onChange={(e) => setSearchQuery(e.target.value)}
                className="w-full rounded-xl glass-input pl-8 pr-8 py-1.5 text-sm outline-none focus:ring-2 focus:ring-ring"
              />
              {searchQuery && (
                <button
                  type="button"
                  className="absolute right-2 top-1/2 -translate-y-1/2 text-muted-foreground hover:text-foreground"
                  onClick={() => setSearchQuery("")}
                >
                  <X aria-hidden="true" className="h-4 w-4" />
                </button>
              )}
            </div>
            {!showDirPicker && searchQuery && (
              <p className="text-xs text-muted-foreground -mt-2">
                Search filters the current directory — expand subdirectories to search within them.
              </p>
            )}

            {/* Directory bar */}
            <div className="flex items-center gap-2 text-sm">
              <span className="text-muted-foreground shrink-0">Root:</span>
              <span className="flex-1 truncate rounded-xl glass-panel px-2 py-1 font-mono text-xs text-muted-foreground border border-white/10">
                {hasDir ? projectDir : "~ (not set)"}
              </span>
              {/* Only a project may change its root directory (and thereby
                  the mount scope). Work-item context selection is confined
                  to the project directory, so the arbitrary-filesystem
                  browse is hidden there — the checkbox tree below is
                  already rooted at projectDir. */}
              {!onChange && (
                <Button
                  variant="outline"
                  size="sm"
                  className="text-xs h-7 shrink-0"
                  onClick={() => { setShowDirPicker(!showDirPicker); setBrowsePath(projectDir || "~"); }}
                >
                  {showDirPicker ? "Cancel" : hasDir ? "Change" : "Set directory"}
                </Button>
              )}
            </div>

            {showDirPicker ? (
              <>
                <BrowseTree
                  path={browsePath || "~"}
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
                {/* File tree with checkboxes */}
                <FileTreeContainer
                  projectId={projectId}
                  dirPath={hasDir ? undefined : rootDir}
                  subpath=""
                  searchQuery={searchQuery}
                  selectedSet={new Set(selectedFiles)}
                  expandedPaths={expandedPaths}
                  onToggleExpanded={toggleExpanded}
                  onToggleEntry={toggleEntry}
                  onSelectAll={selectAll}
                  onDeselectAll={deselectAll}
                  onRegisterDir={registerDir}
                />

                {/* Selected files summary with remove buttons */}
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
                          {dirPaths.has(f) ? (
                            <Folder aria-hidden="true" className="h-3 w-3 shrink-0 text-amber-500" />
                          ) : (
                            <File aria-hidden="true" className="h-3 w-3 shrink-0" />
                          )}
                          <span className="truncate max-w-[300px]">{f}</span>
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

  const parentOf = (p: string) => {
    const parts = p.split("/").filter(Boolean);
    if (parts.length === 0) return "~";
    if (p.startsWith("~")) {
      const rel = parts.slice(1, -1).join("/");
      return rel ? `~/${rel}` : "~";
    }
    // Absolute path: /a/b/c → /a/b
    if (p.startsWith("/")) {
      const parent = "/" + parts.slice(0, -1).join("/");
      return parent === "" ? "/" : parent;
    }
    return parts.slice(0, -1).join("/") || "~";
  };

  const goUp = () => {
    const parent = parentOf(path);
    onNavigate(parent);
  };

  if (isLoading) {
    return (
      <div className="flex items-center gap-2 py-4 text-sm text-muted-foreground">
        <Loader2 aria-hidden="true" className="h-4 w-4 animate-spin" />
        Loading…
      </div>
    );
  }

  if (error) {
    return <p className="text-sm text-destructive">Error: {String(error)}</p>;
  }

  return (
    <div className="rounded-md border max-h-[400px] overflow-y-auto">
      <div
        className="flex items-center gap-2 px-3 py-2 text-sm hover:bg-muted/40 cursor-pointer border-b"
        onClick={goUp}
      >
        <ArrowUp aria-hidden="true" className="h-4 w-4 text-muted-foreground" />
        <span className="text-muted-foreground">..</span>
      </div>

      {dirs.length === 0 && files.length === 0 && (
        <p className="px-3 py-4 text-sm text-muted-foreground">Empty directory</p>
      )}

          {dirs.map((entry) => (
            <div
              key={entry.path}
              className="flex items-center gap-2 px-3 py-2 text-sm hover:bg-muted/40 cursor-pointer border-b last:border-0"
            >
              <Folder aria-hidden="true" className="h-4 w-4 text-amber-500 shrink-0" />
              <span
                className="flex-1 truncate"
                onClick={() => onNavigate(entry.path)}
              >
                {entry.name}/
              </span>
              <Button
                variant="outline"
                size="sm"
                className="text-xs h-7 shrink-0"
                onClick={() => onSelect(entry.path)}
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
              <File aria-hidden="true" className="h-4 w-4 shrink-0" />
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

// ─── File tree with checkboxes (default view) ─────────────────────

interface FileTreeContainerProps {
  projectId: string;
  dirPath?: string;
  subpath: string;
  depth?: number;
  searchQuery: string;
  selectedSet: Set<string>;
  expandedPaths: Set<string>;
  onToggleExpanded: (path: string) => void;
  onToggleEntry: (path: string) => void;
  onSelectAll: (entries: FileTreeEntry[]) => void;
  onDeselectAll: (entries: FileTreeEntry[]) => void;
  onRegisterDir?: (path: string) => void;
  readOnly?: boolean;
}

function FileTreeContainer({
  projectId,
  dirPath,
  subpath,
  depth = 0,
  searchQuery,
  selectedSet,
  expandedPaths,
  onToggleExpanded,
  onToggleEntry,
  onSelectAll,
  onDeselectAll,
  onRegisterDir,
  readOnly = false,
}: FileTreeContainerProps) {
  const projectResult = useListProjectDir(projectId, subpath);
  const dirResult = useListDirPath(dirPath ? (subpath || dirPath) : "");
  const { data, isLoading, error } = dirPath ? dirResult : projectResult;

  const q = searchQuery.toLowerCase().trim();
  const allEntries = data?.entries ?? [];
  const entries = q ? allEntries.filter((e) => e.name.toLowerCase().includes(q)) : allEntries;

  if (isLoading) {
    return (
      <div className="flex items-center gap-2 py-2 text-sm text-muted-foreground">
        <Loader2 aria-hidden="true" className="h-4 w-4 animate-spin" />
        Loading…
      </div>
    );
  }

  if (error) {
    return <p className="text-sm text-destructive">Error: {String(error)}</p>;
  }

  if (entries.length === 0 && depth === 0) {
    return <p className="text-sm text-muted-foreground">The directory is empty.</p>;
  }

  return (
    <div className="rounded-md border divide-y max-h-[400px] overflow-y-auto">
      {depth === 0 && entries.length > 0 && (
        <div className="flex items-center gap-2 px-3 py-1.5 bg-muted/20 sticky top-0 border-b">
          <span className="text-xs text-muted-foreground mr-auto">
            {data?.dirName || ""}
          </span>
          {!readOnly && (
            <>
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
            </>
          )}
        </div>
      )}
      {entries.map((entry) => (
        <FileRow
          key={entry.path}
          entry={entry}
          depth={depth}
          projectId={projectId}
          dirPath={dirPath}
          searchQuery={searchQuery}
          selectedSet={selectedSet}
          expandedPaths={expandedPaths}
          onToggleExpanded={onToggleExpanded}
          onToggleEntry={onToggleEntry}
          onSelectAll={onSelectAll}
          onDeselectAll={onDeselectAll}
          onRegisterDir={onRegisterDir}
          readOnly={readOnly}
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
  dirPath?: string;
  searchQuery: string;
  selectedSet: Set<string>;
  expandedPaths: Set<string>;
  onToggleExpanded: (path: string) => void;
  onToggleEntry: (path: string) => void;
  onSelectAll: (entries: FileTreeEntry[]) => void;
  onDeselectAll: (entries: FileTreeEntry[]) => void;
  onRegisterDir?: (path: string) => void;
  readOnly?: boolean;
}

function FileRow({
  entry,
  depth,
  projectId,
  dirPath,
  searchQuery,
  selectedSet,
  expandedPaths,
  onToggleExpanded,
  onToggleEntry,
  onSelectAll,
  onDeselectAll,
  onRegisterDir,
  readOnly = false,
}: FileRowProps) {
  const isExpanded = expandedPaths.has(entry.path);
  const isSelected = selectedSet.has(entry.path);
  useEffect(() => {
    if (entry.isDir && onRegisterDir) onRegisterDir(entry.path);
  }, [entry.isDir, entry.path, onRegisterDir]);

  if (entry.isDir) {
    return (
      <div>
        <div
          className="flex items-center gap-1 px-2 py-1.5 text-sm hover:bg-muted/40 cursor-pointer"
          style={{ paddingLeft: depth * 20 + 8 }}
        >
          {readOnly ? (
            <span className="w-8 shrink-0" />
          ) : (
            <button
              type="button"
              className="text-muted-foreground hover:text-foreground shrink-0"
              onClick={(e) => { e.stopPropagation(); onToggleExpanded(entry.path); }}
            >
              {isExpanded ? (
                <ChevronDown aria-hidden="true" className="h-4 w-4" />
              ) : (
                <ChevronRight aria-hidden="true" className="h-4 w-4" />
              )}
            </button>
          )}
          {readOnly ? (
            <span className="w-4 shrink-0" />
          ) : (
            <button
              type="button"
              className="text-muted-foreground hover:text-foreground shrink-0"
              onClick={() => onToggleEntry(entry.path)}
            >
              {isSelected ? (
                <CheckSquare aria-hidden="true" className="h-4 w-4" />
              ) : (
                <Square aria-hidden="true" className="h-4 w-4" />
              )}
            </button>
          )}
          <Folder aria-hidden="true" className="h-4 w-4 text-amber-500 shrink-0" />
          <span className="truncate" onClick={() => onToggleExpanded(entry.path)}>
            {entry.name}
          </span>
        </div>
        {isExpanded && (
          <FileTreeContainer
            projectId={projectId}
            dirPath={dirPath}
            subpath={entry.path}
            depth={depth + 1}
            searchQuery={searchQuery}
            selectedSet={selectedSet}
            expandedPaths={expandedPaths}
            onToggleExpanded={onToggleExpanded}
            onToggleEntry={onToggleEntry}
            onSelectAll={onSelectAll}
            onDeselectAll={onDeselectAll}
            readOnly={readOnly}
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
      {readOnly ? (
        <span className="w-4 shrink-0" />
      ) : (
        <button
          type="button"
          className="text-muted-foreground hover:text-foreground shrink-0"
          onClick={() => onToggleEntry(entry.path)}
        >
          {isSelected ? (
            <CheckSquare aria-hidden="true" className="h-4 w-4" />
          ) : (
            <Square aria-hidden="true" className="h-4 w-4" />
          )}
        </button>
      )}
      <File aria-hidden="true" className="h-4 w-4 text-sky-500 shrink-0" />
      <span className="truncate">{entry.name}</span>
    </div>
  );
}
