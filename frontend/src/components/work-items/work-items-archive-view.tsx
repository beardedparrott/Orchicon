// Dedicated Archive view for the Work Items page (option A — archived-only
// tree with active ghost anchors, design-notes 2026-09-01). Renders archived
// work items as a hierarchy using the same tree semantics as the active tree
// view (buildArchiveTreeData over the archived set + expand/collapse + kind/
// status pills), NOT a flat row list — archived parents still have archived
// children (full subtrees survive archiving; only the block-don't-cascade
// rule prevents a parent with ACTIVE children from being archived).
//
// Ghost anchors: when an archived item's ancestor chain crosses into an
// ACTIVE item, that ancestor renders as a muted, non-restorable "active" row
// so the cross-boundary connection stays visible without turning the
// archive view into the board. Ghost anchors are never selectable for bulk
// restore and never count toward the archived total.
//
// Restore stays selection-driven, never auto-cascading: per-row Restore
// restores exactly that item; "Restore selected" restores exactly the
// checked set (checking a parent auto-selects its archived children via the
// existing cascade selection — that IS the consent mechanism, mirrors Bulk
// Archive).

import { ChevronRight, Archive as ArchiveIcon, RotateCcw } from "lucide-react";
import { useState } from "react";

import type { WorkItem } from "@/api/gen/orchicon/api/v1/work_item_pb";
import { Button } from "@/components/ui/button";
import { KindBadge } from "@/components/work-items/work-item-badges";
import { statusMeta, statusMetaFromString } from "@/components/work-items/work-item-meta";
import { buildArchiveTreeData } from "@/components/work-items/dependency-utils";
import { subtreeSelectionState } from "@/components/work-items/use-work-item-selection";
import { cn } from "@/lib/utils";
import { Link } from "@tanstack/react-router";

export interface WorkItemsArchiveViewProps {
  /** archived work items (ListWorkItems include_archived=true) */
  items?: WorkItem[];
  /** active work items for the same project(s) — used ONLY to resolve ghost
   *  anchor rows for archived items whose parent is still active */
  activeItems?: WorkItem[];
  isLoading: boolean;
  error?: unknown;
  /** @param id the archived work item id to restore */
  onRestore: (id: string) => void;
  restorePending: boolean;
  selected: Set<string>;
  onToggleSelect: (id: string) => void;
  onRestoreSelected: () => void;
  restoreSelectedPending: boolean;
}

/** Format an archived-at timestamp to a compact local date+time. */
function formatArchivedAt(archivedAt?: WorkItem["archivedAt"]): string {
  if (!archivedAt) return "";
  const d = new Date(Number(archivedAt.seconds) * 1000);
  return d.toLocaleString([], {
    month: "short",
    day: "numeric",
    year: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
}

export function WorkItemsArchiveView({
  items,
  activeItems,
  isLoading,
  error,
  onRestore,
  restorePending,
  selected,
  onToggleSelect,
  onRestoreSelected,
  restoreSelectedPending,
}: WorkItemsArchiveViewProps) {
  const [collapsed, setCollapsed] = useState<Set<string>>(new Set());
  const toggleCollapse = (id: string) =>
    setCollapsed((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });

  if (isLoading) {
    return <p className="text-sm text-muted-foreground">Loading archived work items…</p>;
  }
  if (error) {
    return <p className="text-sm text-destructive">Failed to load archived work items: {String(error)}</p>;
  }
  const archived = items ?? [];
  if (archived.length === 0) {
    return (
      <div className="flex flex-col items-center gap-2 py-16 text-center text-muted-foreground">
        <ArchiveIcon aria-hidden="true" className="h-8 w-8" />
        <p className="text-sm">No archived work items.</p>
      </div>
    );
  }

  const { treeItems, activeAnchorIds } = buildArchiveTreeData(archived, activeItems);
  // Cascade selection walks ONLY archived children — ghost anchors are
  // never selectable, so they must never appear in the subtree a parent
  // toggle selects (see use-work-item-selection's subtreeIds walk).
  const archivedChildrenOf = (parentId: string) =>
    archived.filter((i) => i.parentId === parentId);
  const roots = treeItems.filter((i) => !i.parentId || !treeItems.some((p) => p.id === i.parentId));

  return (
    <div className="space-y-3">
      {selected.size > 0 && (
        <div className="flex items-center justify-between gap-3 rounded-2xl glass-panel px-4 py-2.5">
          <p className="text-sm text-muted-foreground">{selected.size} selected</p>
          <Button
            variant="outline"
            size="sm"
            onClick={onRestoreSelected}
            disabled={restoreSelectedPending}
          >
            <RotateCcw aria-hidden="true" className="mr-1 h-3.5 w-3.5" />
            Restore selected
          </Button>
        </div>
      )}
      <div className="min-w-[640px] space-y-0.5">
        {roots.map((item) => (
          <ArchiveTreeNode
            key={item.id}
            item={item}
            depth={0}
            treeItems={treeItems}
            activeAnchorIds={activeAnchorIds}
            archivedChildrenOf={archivedChildrenOf}
            collapsed={collapsed}
            onToggleCollapse={toggleCollapse}
            selected={selected}
            onToggleSelect={onToggleSelect}
            onRestore={onRestore}
            restorePending={restorePending}
          />
        ))}
      </div>
    </div>
  );
}

function ArchiveTreeNode({
  item,
  depth,
  treeItems,
  activeAnchorIds,
  archivedChildrenOf,
  collapsed,
  onToggleCollapse,
  selected,
  onToggleSelect,
  onRestore,
  restorePending,
}: {
  item: WorkItem;
  depth: number;
  treeItems: WorkItem[];
  activeAnchorIds: Set<string>;
  archivedChildrenOf: (parentId: string) => WorkItem[];
  collapsed: Set<string>;
  onToggleCollapse: (id: string) => void;
  selected: Set<string>;
  onToggleSelect: (id: string) => void;
  onRestore: (id: string) => void;
  restorePending: boolean;
}) {
  const isGhost = activeAnchorIds.has(item.id);
  const children = treeItems.filter((i) => i.parentId === item.id);
  const hasChildren = children.length > 0;
  const expanded = !collapsed.has(item.id);

  // Archived-only subtree ids drive the tri-state checkbox: a ghost anchor
  // is never part of the selectable subtree (it is never in
  // archivedChildrenOf's output), so checking an archived parent can never
  // reach across the archive boundary into active items.
  const archivedSubtreeIds = isGhost ? [] : collectArchivedSubtreeIds(item.id, archivedChildrenOf);
  const subtreeState = subtreeSelectionState(archivedSubtreeIds, selected);

  const original = item.archivedFromStatus
    ? statusMetaFromString(item.archivedFromStatus)
    : statusMeta(0);
  const activeMeta = statusMeta(item.status);

  return (
    <div>
      <div
        className={cn(
          "flex items-center gap-1.5 rounded-md border border-transparent px-1.5 py-1.5",
          isGhost ? "opacity-60" : "hover:border-border hover:bg-accent/50",
          selected.has(item.id) && "bg-accent/60",
        )}
      >
        {Array.from({ length: depth }, (_, i) => (
          <span
            key={i}
            aria-hidden="true"
            className="h-6 w-[18px] shrink-0 border-l border-dashed border-border/60"
          />
        ))}
        {hasChildren ? (
          <button
            type="button"
            onClick={() => onToggleCollapse(item.id)}
            aria-expanded={expanded}
            aria-label={expanded ? `Collapse ${item.title}` : `Expand ${item.title}`}
            className="flex h-5 w-5 shrink-0 items-center justify-center rounded text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
          >
            <ChevronRight
              aria-hidden="true"
              className={cn("h-3.5 w-3.5 transition-transform", expanded && "rotate-90")}
            />
          </button>
        ) : (
          <span className="w-5 shrink-0" />
        )}
        <input
          type="checkbox"
          checked={subtreeState === "checked"}
          ref={(el) => {
            if (el) el.indeterminate = subtreeState === "indeterminate";
          }}
          disabled={isGhost}
          onChange={() => onToggleSelect(item.id)}
          className={cn("h-4 w-4 shrink-0 rounded border-input", isGhost ? "cursor-not-allowed opacity-30" : "cursor-pointer")}
          aria-label={
            isGhost
              ? `${item.title} is active — not selectable from the archive view`
              : `Select ${item.title}${hasChildren ? " and its archived descendants" : ""}`
          }
        />
        <KindBadge kind={item.kind} className="hidden sm:inline-flex" />
        <Link
          to="/work-items/$id"
          params={{ id: item.id }}
          className="min-w-0 flex-1 truncate text-sm font-medium text-foreground hover:underline"
        >
          {item.title}
        </Link>
        {isGhost ? (
          <span
            className={cn("inline-flex items-center gap-1 rounded-full px-1.5 py-0.5 text-[10px] font-medium", activeMeta.pill)}
            title="This ancestor is active, not archived — shown to keep the hierarchy connected."
          >
            <span className={cn("h-1 w-1 rounded-full", activeMeta.dot)} />
            active
          </span>
        ) : (
          <span className={cn("inline-flex items-center gap-1 rounded-full px-1.5 py-0.5 text-[10px] font-medium", original.pill)}>
            <span className={cn("h-1 w-1 rounded-full", original.dot)} />
            Archived from: {original.label}
          </span>
        )}
        {!isGhost && item.archivedAt && (
          <span className="hidden text-xs text-muted-foreground md:inline">{formatArchivedAt(item.archivedAt)}</span>
        )}
        {!isGhost && (
          <Button
            variant="outline"
            size="sm"
            onClick={() => onRestore(item.id)}
            disabled={restorePending}
            title="Restore this work item to the active views"
          >
            <RotateCcw aria-hidden="true" className="mr-1 h-3.5 w-3.5" />
            Restore
          </Button>
        )}
      </div>
      {expanded &&
        children.map((child) => (
          <ArchiveTreeNode
            key={child.id}
            item={child}
            depth={depth + 1}
            treeItems={treeItems}
            activeAnchorIds={activeAnchorIds}
            archivedChildrenOf={archivedChildrenOf}
            collapsed={collapsed}
            onToggleCollapse={onToggleCollapse}
            selected={selected}
            onToggleSelect={onToggleSelect}
            onRestore={onRestore}
            restorePending={restorePending}
          />
        ))}
    </div>
  );
}

function collectArchivedSubtreeIds(
  id: string,
  archivedChildrenOf: (parentId: string) => WorkItem[],
): string[] {
  const out = [id];
  const queue = [id];
  while (queue.length > 0) {
    const current = queue.shift()!;
    for (const child of archivedChildrenOf(current)) {
      out.push(child.id);
      queue.push(child.id);
    }
  }
  return out;
}
