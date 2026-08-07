// Tree view for the Work Items page (design §5.1/§5.2/§5.4, ADR-2/ADR-3).
//
// Presentational: the page shell owns data fetching + filtering and
// passes the built tree (matches + ancestors), the dependency block
// state, and the shared selection. This view renders the rows with
// cascade-aware tri-state checkboxes, indent guides, blocked chips, and
// file-explorer auto-expand when a filter is active.
//
// Expand/collapse is persisted per project (ADR-WI-3):
//   - No active filter: `expandedIds` (explicitly expanded; default
//     collapsed) drives the rows.
//   - Active filter: rows default EXPANDED so filtered matches stay
//     reachable under their ancestors, but the user can still collapse a
//     parent — `collapsedIds` records the explicit collapse and survives
//     navigation (regression fix: collapse was previously impossible
//     while a filter was active).

import { Link } from "@tanstack/react-router";
import { ChevronRight, SearchX } from "lucide-react";

import type { WorkItem } from "@/api/gen/orchicon/api/v1/work_item_pb";
import { KindBadge, StatusPill } from "@/components/work-items/work-item-badges";
import type { BlockState } from "@/components/work-items/dependency-utils";
import { BlockedChip } from "@/components/work-items/work-item-card";
import { subtreeSelectionState } from "@/components/work-items/use-work-item-selection";
import { Card, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { cn } from "@/lib/utils";

export interface WorkItemsTreeProps {
  /** matches + ancestors (the rendered tree rows) */
  treeItems: WorkItem[];
  /** ids that pass the active filters (highlighted rows) */
  matchIds: Set<string>;
  /** ids of ancestor-only container rows (dimmed when filtering) */
  ancestorIds: Set<string>;
  filterActive: boolean;
  /** persisted per-project expand state (normal mode; ADR-WI-3) */
  expandedIds: Set<string>;
  onToggleExpand: (id: string) => void;
  /** persisted per-project collapse state while a filter is active */
  collapsedIds: Set<string>;
  onToggleCollapse: (id: string) => void;
  blockState: BlockState;
  selected: Set<string>;
  onToggleSelect: (id: string) => void;
  isLoading: boolean;
  error: unknown;
  hasQuery: boolean;
}

export function WorkItemsTree({
  treeItems,
  matchIds,
  ancestorIds,
  filterActive,
  expandedIds,
  onToggleExpand,
  collapsedIds,
  onToggleCollapse,
  blockState,
  selected,
  onToggleSelect,
  isLoading,
  error,
  hasQuery,
}: WorkItemsTreeProps) {
  if (isLoading) {
    return (
      <div className="space-y-2" aria-busy="true">
        {[0, 1, 2, 3].map((i) => (
          <div key={i} className="h-10 animate-pulse rounded-md border bg-card/50" />
        ))}
      </div>
    );
  }
  if (error) {
    return (
      <p className="text-sm text-destructive">
        Failed to load work items: {String(error)}
      </p>
    );
  }
  if (treeItems.length === 0) {
    return (
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <SearchX className="h-5 w-5 text-muted-foreground" />
            {hasQuery ? "No matching work items" : "No work items yet"}
          </CardTitle>
          <CardDescription>
            {hasQuery
              ? "Try widening the filters or clearing the search."
              : "Create an epic to start building the work hierarchy."}
          </CardDescription>
        </CardHeader>
      </Card>
    );
  }

  const childrenOf = (parentId: string) =>
    treeItems.filter((i) => i.parentId === parentId);
  const roots = treeItems.filter((i) => !i.parentId);

  return (
    <div className="overflow-x-auto">
      <div className="min-w-[640px] space-y-0.5">
        {roots.map((item) => (
          <TreeNode
            key={item.id}
            item={item}
            childrenOf={childrenOf}
            depth={0}
            selected={selected}
            onToggleSelect={onToggleSelect}
            blockState={blockState}
            matchIds={matchIds}
            ancestorIds={ancestorIds}
            filterActive={filterActive}
            expandedIds={expandedIds}
            onToggleExpand={onToggleExpand}
            collapsedIds={collapsedIds}
            onToggleCollapse={onToggleCollapse}
          />
        ))}
      </div>
    </div>
  );
}

function TreeNode({
  item,
  childrenOf,
  depth,
  selected,
  onToggleSelect,
  blockState,
  matchIds,
  ancestorIds,
  filterActive,
  expandedIds,
  onToggleExpand,
  collapsedIds,
  onToggleCollapse,
}: {
  item: WorkItem;
  childrenOf: (parentId: string) => WorkItem[];
  depth: number;
  selected: Set<string>;
  onToggleSelect: (id: string) => void;
  blockState: BlockState;
  matchIds: Set<string>;
  ancestorIds: Set<string>;
  filterActive: boolean;
  expandedIds: Set<string>;
  onToggleExpand: (id: string) => void;
  collapsedIds: Set<string>;
  onToggleCollapse: (id: string) => void;
}) {
  const children = childrenOf(item.id);
  const hasChildren = children.length > 0;
  const isMatch = matchIds.has(item.id);
  const isAncestor = ancestorIds.has(item.id);
  // No active filter: the persisted expanded set drives rows (default
  // collapsed). Active filter: rows default expanded (file-explorer
  // auto-expand so matches stay reachable), but the user can collapse a
  // parent explicitly — the persisted collapsed set records that choice.
  const expanded = filterActive ? !collapsedIds.has(item.id) : expandedIds.has(item.id);
  // Dimmed ancestor container rows (kept so filtered results stay
  // reachable under their parents) are NOT selectable: checking one would
  // cascade-select the very epics/features the user filtered out, and
  // "Delete N selected" would hard-delete them. The header select-all
  // also only covers matches (the page shell computes it over
  // `treeData.matches`).
  const selectable = !(filterActive && isAncestor && !isMatch);

  const subtreeState = subtreeSelectionState(
    [item.id, ...collectSubtreeIds(item.id, childrenOf)],
    selected,
  );
  const triState = hasChildren && subtreeState === "indeterminate";
  const checked = subtreeState === "checked";

  return (
    <div>
      <div
        className={cn(
          "flex items-center gap-1.5 rounded-md border border-transparent px-1.5 py-1.5 transition-colors hover:border-border hover:bg-accent/50",
          selected.has(item.id) && "bg-accent/60",
          filterActive && isAncestor && !isMatch && "opacity-70",
          filterActive && isMatch && "border-border/60 bg-primary/5",
        )}
      >
        {/* indent guides */}
        {Array.from({ length: depth }, (_, i) => (
          <span
            key={i}
            aria-hidden
            className="h-6 w-[18px] shrink-0 border-l border-dashed border-border/60"
          />
        ))}
        <input
          type="checkbox"
          checked={checked}
          disabled={!selectable}
          ref={(el) => {
            if (el) el.indeterminate = triState;
          }}
          onChange={() => onToggleSelect(item.id)}
          className={cn(
            "h-4 w-4 shrink-0 rounded border-input",
            selectable ? "cursor-pointer" : "cursor-not-allowed opacity-50",
          )}
          aria-label={
            selectable
              ? `Select ${item.title}${hasChildren ? " and its descendants" : ""}`
              : `Select ${item.title} (filtered out — not selectable)`
          }
        />
        {hasChildren ? (
          <button
            type="button"
            onClick={() =>
              filterActive ? onToggleCollapse(item.id) : onToggleExpand(item.id)
            }
            aria-expanded={expanded}
            aria-label={expanded ? `Collapse ${item.title}` : `Expand ${item.title}`}
            className="flex h-5 w-5 shrink-0 items-center justify-center rounded text-muted-foreground transition-colors hover:bg-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring"
          >
            <ChevronRight
              className={cn("h-3.5 w-3.5 transition-transform motion-reduce:transition-none", expanded && "rotate-90")}
            />
          </button>
        ) : (
          <span className="w-5 shrink-0" />
        )}
        <KindBadge kind={item.kind} className="hidden sm:inline-flex" />
        <Link
          to="/work-items/$id"
          params={{ id: item.id }}
          className="min-w-0 flex-1 truncate text-sm font-medium text-foreground hover:underline focus-visible:ring-2 focus-visible:ring-ring"
        >
          {item.title}
        </Link>
        <span className="flex shrink-0 items-center gap-1.5">
          <BlockedChip
            blockedBy={blockState.blockedBy}
            id={item.id}
            depsCount={depsCountOf(item.id, blockState)}
          />
          <StatusPill status={item.status} className="hidden sm:inline-flex" />
        </span>
      </div>
      {expanded &&
        children.map((child) => (
          <TreeNode
            key={child.id}
            item={child}
            childrenOf={childrenOf}
            depth={depth + 1}
            selected={selected}
            onToggleSelect={onToggleSelect}
            blockState={blockState}
            matchIds={matchIds}
            ancestorIds={ancestorIds}
            filterActive={filterActive}
            expandedIds={expandedIds}
            onToggleExpand={onToggleExpand}
            collapsedIds={collapsedIds}
            onToggleCollapse={onToggleCollapse}
          />
        ))}
    </div>
  );
}

function depsCountOf(id: string, blockState: BlockState): number {
  return (
    (blockState.blocks.get(id)?.length ?? 0) +
    (blockState.blockedBy.get(id)?.length ?? 0)
  );
}

function collectSubtreeIds(id: string, childrenOf: (parentId: string) => WorkItem[]): string[] {
  const out: string[] = [];
  const queue = [id];
  while (queue.length > 0) {
    const current = queue.shift()!;
    for (const child of childrenOf(current)) {
      out.push(child.id);
      queue.push(child.id);
    }
  }
  return out;
}
