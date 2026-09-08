// DiffTree — tree of changed files with per-file change badges.
//
// Builds a nested tree from the flat path list (split on "/"), renders
// collapsible folders, and leaf file rows with +adds/−dels badges and
// click-through to the diff tab (parent onSelect).

import { useState } from "react";
import { Folder, FolderOpen, FileCode2, ChevronRight, ListTree } from "lucide-react";

import { cn } from "@/lib/utils";
import type { FileGroup } from "@/lib/diff/sideBySide";

interface DiffTreeProps {
  files: FileGroup[];
  onSelect: (path: string) => void;
  selectedPath?: string | null;
}

interface TreeNode {
  name: string;
  path: string;
  children: TreeNode[];
  file?: FileGroup;
}

function buildTree(files: FileGroup[]): TreeNode[] {
  const root: TreeNode[] = [];
  const nodeMap = new Map<string, TreeNode>();

  for (const f of files) {
    const parts = f.path.split("/");
    let level = root;
    let curPath = "";
    parts.forEach((part, i) => {
      curPath = curPath ? `${curPath}/${part}` : part;
      let node = nodeMap.get(curPath);
      if (!node) {
        node = {
          name: part,
          path: curPath,
          children: [],
          file: i === parts.length - 1 ? f : undefined,
        };
        nodeMap.set(curPath, node);
        level.push(node);
      } else if (i === parts.length - 1) {
        node.file = f;
      }
      level = node.children;
    });
  }
  return root;
}

function TreeItem({
  node,
  depth,
  onSelect,
  selectedPath,
  defaultOpen,
}: {
  node: TreeNode;
  depth: number;
  onSelect: (path: string) => void;
  selectedPath?: string | null;
  defaultOpen?: boolean;
}) {
  const [open, setOpen] = useState(defaultOpen ?? depth < 1);
  const isSelected = node.path === selectedPath;

  if (node.file) {
    const g = node.file;
    return (
      <button
        type="button"
        onClick={() => onSelect(g.path)}
        className={cn(
          "flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-left text-xs hover:bg-accent/50",
          isSelected ? "bg-accent/70 text-accent-foreground" : "",
        )}
        style={{ paddingLeft: `${8 + depth * 14}px` }}
      >
        <FileCode2 aria-hidden="true" className="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
        <span className="flex-1 truncate font-mono">{node.name}</span>
        <span className="shrink-0 text-[10px] font-semibold text-emerald-600 dark:text-emerald-400">
          +{g.adds}
        </span>
        <span className="shrink-0 text-[10px] font-semibold text-red-600 dark:text-red-400">
          −{g.dels}
        </span>
      </button>
    );
  }

  if (node.children.length === 0) return null;

  return (
    <div>
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="flex w-full items-center gap-1.5 rounded-md px-2 py-1 text-left text-xs font-medium text-muted-foreground hover:bg-accent/50 hover:text-foreground"
        style={{ paddingLeft: `${8 + depth * 14}px` }}
        aria-expanded={open}
      >
        <ChevronRight aria-hidden="true" className={cn("h-3 w-3 transition-transform", open ? "rotate-90" : "")} />
        {open ? (
          <FolderOpen aria-hidden="true" className="h-3.5 w-3.5 shrink-0 text-cyan-600 dark:text-cyan-400" />
        ) : (
          <Folder aria-hidden="true" className="h-3.5 w-3.5 shrink-0 text-cyan-600 dark:text-cyan-400" />
        )}
        <span className="truncate">{node.name}</span>
      </button>
      {open && (
        <div>
          {node.children.map((c) => (
            <TreeItem
              key={c.path}
              node={c}
              depth={depth + 1}
              onSelect={onSelect}
              selectedPath={selectedPath}
            />
          ))}
        </div>
      )}
    </div>
  );
}

export function DiffTree({ files, onSelect, selectedPath }: DiffTreeProps) {
  if (files.length === 0) {
    return (
      <div className="flex flex-1 flex-col items-center justify-center p-8 text-center text-sm text-muted-foreground">
        <ListTree aria-hidden="true" className="mb-3 h-8 w-8 opacity-40" />
        No changed files.
      </div>
    );
  }
  const tree = buildTree(files);
  return (
    <div className="flex-1 overflow-y-auto p-1.5">
      {tree.map((n) => (
        <TreeItem key={n.path} node={n} depth={0} onSelect={onSelect} selectedPath={selectedPath} />
      ))}
    </div>
  );
}
