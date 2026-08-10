// Tree view for the Work Items page (design §5.1/§5.2/§5.4, ADR-2/ADR-3).
//
// Presentational: the page shell owns data fetching + filtering and
// passes the built tree (matches + ancestors), the dependency block
// state, and the shared selection. This view renders the rows with
// cascade-aware tri-state checkboxes, indent guides, blocked chips, and
// file-explorer auto-expand when a filter is active.
//
// Drag-to-reorder (architecture-notes/sequential-multi-workflow-runs.md
// §1): siblings within a parent can be dragged into a new chain order.
// The drop calls `onReorder(parentId, childIds)` → ReorderWorkItems RPC →
// sort_order is renumbered in ONE server-side transaction. The new order
// is derived from the true sort_order rank (see handleDragEnd), never the
// rendered display order, so a drag persists a coherent chain under any
// active display sort. The user's sort/filter dropdowns never call the RPC
// — they only change display order (AGENTS.md invariant #1). A mid-sequence
// drag is safe: the sequence cursor is derived from sort_order at reconcile
// time, so the drag shifts only future arming.
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
import {
  DndContext,
  KeyboardSensor,
  PointerSensor,
  useSensor,
  useSensors,
  type DragEndEvent,
  type DragStartEvent,
} from "@dnd-kit/core";
import {
  SortableContext,
  arrayMove,
  sortableKeyboardCoordinates,
  useSortable,
  verticalListSortingStrategy,
} from "@dnd-kit/sortable";
import { CSS } from "@dnd-kit/utilities";
import { ChevronRight, GripVertical, SearchX } from "lucide-react";
import { useEffect, useMemo, useRef, useState } from "react";

import type { WorkItem } from "@/api/gen/orchicon/api/v1/work_item_pb";
import { KindBadge, PositionBadge, StatusPill } from "@/components/work-items/work-item-badges";
import type { BlockState } from "@/components/work-items/dependency-utils";
import { computeSequencePositions, sortByChainOrder } from "@/components/work-items/sequence-utils";
import { BlockedChip } from "@/components/work-items/work-item-card";
import { subtreeSelectionState } from "@/components/work-items/use-work-item-selection";
import { Card, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { cn } from "@/lib/utils";

export interface WorkItemsTreeProps {
  /** matches + ancestors (the rendered tree rows) */
  treeItems: WorkItem[];
  /** the FULL unfiltered items list (for chain-position badges) */
  allItems?: WorkItem[];
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
  /** reorders siblings under parentId ("" = top level) via the RPC */
  onReorder: (parentId: string, childIds: string[]) => void;
  isLoading: boolean;
  error: unknown;
  hasQuery: boolean;
}

export function WorkItemsTree({
  treeItems,
  allItems,
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
  onReorder,
  isLoading,
  error,
  hasQuery,
}: WorkItemsTreeProps) {
  const [activeId, setActiveId] = useState<string | null>(null);
  // A drag ends with the browser dispatching a click on the element under
  // the pointer — the row's title is a <Link>, so an unguarded click after
  // a drop navigates to the item's detail page, undoing the reorder UX.
  // Set on drag start (only fires once the pointer sensor activates, so a
  // plain click never sets it); the click handler consumes + clears it,
  // and the timeout clears it when the pointer was released outside.
  const suppressClickRef = useRef(false);
  // A drag ends with the browser dispatching a click on the element under
  // the pointer — the row titles are <Link>s, so without this the post-drag
  // click navigates to the item's detail page, undoing the reorder UX. The
  // suppression MUST be a DOCUMENT-level native capture listener: after a
  // dnd-kit drag the click is dispatched against the dragged row's node and
  // its propagation path is just [target → document] — listeners on the
  // container div or the row itself are NOT reached (verified empirically:
  // a document capture listener fires, container/row listeners never do).
  useEffect(() => {
    const onCaptureClick = (e: MouseEvent) => {
      if (suppressClickRef.current) {
        e.preventDefault();
        e.stopPropagation();
        suppressClickRef.current = false;
      }
    };
    document.addEventListener("click", onCaptureClick, true);
    return () => document.removeEventListener("click", onCaptureClick, true);
  }, []);

  const sensors = useSensors(
    useSensor(PointerSensor, { activationConstraint: { distance: 3 } }),
    useSensor(KeyboardSensor, { coordinateGetter: sortableKeyboardCoordinates }),
  );

  const itemById = new Map(treeItems.map((i) => [i.id, i]));
  // Chain positions from the FULL list so a filter never distorts badges.
  const positions = useMemo(
    () => computeSequencePositions(allItems ?? treeItems),
    [allItems, treeItems],
  );

  const handleDragStart = (event: DragStartEvent) => {
    if (hasQuery) return; // reorder only without an active filter (see below)
    setActiveId(String(event.active.id));
    suppressClickRef.current = true;
  };

  const handleDragEnd = (event: DragEndEvent) => {
    setActiveId(null);
    // Keep the click-suppression flag SET long enough that the drag's
    // post-mouseup click is guaranteed to land inside it (the click is
    // dispatched immediately after pointerup, so a setTimeout(0) could
    // clear the flag BEFORE the click — that race let the title <Link>
    // navigate after a real drag). The onClickCapture handler consumes the
    // flag when it suppresses the click; this timer is only the backstop
    // for a release outside the list where no click ever fires.
    window.setTimeout(() => {
      suppressClickRef.current = false;
    }, 300);
    // Drag-to-reorder is disabled under an active filter: treeItems then
    // holds only the matching rows + ancestors, so the sibling set is a
    // PARTIAL view of the true chain — dropping would reorder just the
    // visible subset and the RPC would silently append the hidden
    // siblings after it, corrupting the chain. Reorder requires the full
    // sibling set, which is only visible with no filter.
    if (hasQuery) return;
    const { active, over } = event;
    if (!over || active.id === over.id) return;
    const activeItem = itemById.get(String(active.id));
    const overItem = itemById.get(String(over.id));
    if (!activeItem || !overItem) return;
    const parentId = activeItem.parentId ?? "";
    // The drop must stay within the same sibling group.
    if ((overItem.parentId ?? "") !== parentId) return;
    // Derive the new order from the TRUE chain order (sort_order rank, then
    // created_at — the backend's `ORDER BY sort_order NULLS LAST, created_at`),
    // never from the rendered display order. The page's list query honors the
    // user's sort dropdown (chain order / created / title / priority), so the
    // rendered sibling order only matches the chain when "Sort: chain order"
    // is active — deriving from the display order would make drags no-ops in
    // any other view and silently corrupt the chain (the sequence cursor is
    // derived from sort_order at reconcile time, so a corrupted rank would
    // reorder future arming). The RPC renumbers sort_order 1..N in one
    // transaction, so the result is always a coherent chain.
    const siblings = sortByChainOrder(
      treeItems.filter((i) => (i.parentId ?? "") === parentId),
    );
    const oldIndex = siblings.findIndex((i) => i.id === activeItem.id);
    const newIndex = siblings.findIndex((i) => i.id === overItem.id);
    if (oldIndex < 0 || newIndex < 0) return;
    const next = arrayMove(siblings, oldIndex, newIndex);
    onReorder(parentId, next.map((i) => i.id));
  };

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
    <DndContext
      sensors={sensors}
      onDragStart={handleDragStart}
      onDragEnd={handleDragEnd}
      onDragCancel={() => {
        setActiveId(null);
        window.setTimeout(() => {
          suppressClickRef.current = false;
        }, 300);
      }}
    >
      <div className="overflow-x-auto">
        <div className="min-w-[640px] space-y-0.5">
          <SortableContext
            items={roots.map((i) => i.id)}
            strategy={verticalListSortingStrategy}
          >
            {roots.map((item) => (
              <TreeNode
                key={item.id}
                item={item}
                childrenOf={childrenOf}
                depth={0}
                positions={positions}
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
                activeId={activeId}
                dragDisabled={filterActive}
              />
            ))}
          </SortableContext>
        </div>
      </div>
    </DndContext>
  );
}

function TreeNode({
  item,
  childrenOf,
  depth,
  positions,
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
  activeId,
  dragDisabled,
}: {
  item: WorkItem;
  childrenOf: (parentId: string) => WorkItem[];
  depth: number;
  positions: Map<string, number>;
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
  activeId: string | null;
  dragDisabled: boolean;
}) {
  const children = childrenOf(item.id);
  const hasChildren = children.length > 0;
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
  const selectable = !(filterActive && ancestorIds.has(item.id) && !matchIds.has(item.id));

  const subtreeState = subtreeSelectionState(
    [item.id, ...collectSubtreeIds(item.id, childrenOf)],
    selected,
  );
  const triState = hasChildren && subtreeState === "indeterminate";
  const checked = subtreeState === "checked";

  return (
    <div>
      <SortableTreeRow
        item={item}
        depth={depth}
        position={positions.get(item.id)}
        selected={selected.has(item.id)}
        onToggleSelect={onToggleSelect}
        selectable={selectable}
        checked={checked}
        triState={triState}
        blockState={blockState}
        matchIds={matchIds}
        ancestorIds={ancestorIds}
        filterActive={filterActive}
        hasChildren={hasChildren}
        expanded={expanded}
        onToggleExpand={() =>
          filterActive ? onToggleCollapse(item.id) : onToggleExpand(item.id)
        }
        isActive={activeId === item.id}
        dragDisabled={filterActive}
      />
      {expanded && (
        <SortableContext
          items={children.map((c) => c.id)}
          strategy={verticalListSortingStrategy}
        >
          {children.map((child) => (
            <TreeNode
              key={child.id}
              item={child}
              childrenOf={childrenOf}
              depth={depth + 1}
              positions={positions}
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
              activeId={activeId}
              dragDisabled={dragDisabled}
            />
          ))}
        </SortableContext>
      )}
    </div>
  );
}

// The sortable row: wraps the row chrome with dnd-kit so siblings can be
// dragged into a new chain order (the RPC call lives in the page shell).
function SortableTreeRow({
  item,
  depth,
  position,
  selected,
  onToggleSelect,
  selectable,
  checked,
  triState,
  blockState,
  matchIds,
  ancestorIds,
  filterActive,
  hasChildren,
  expanded,
  onToggleExpand,
  isActive,
  dragDisabled,
}: {
  item: WorkItem;
  depth: number;
  /** chain-order position (#N) — set only for sequence children */
  position?: number;
  selected: boolean;
  onToggleSelect: (id: string) => void;
  selectable: boolean;
  checked: boolean;
  triState: boolean;
  blockState: BlockState;
  matchIds: Set<string>;
  ancestorIds: Set<string>;
  filterActive: boolean;
  hasChildren: boolean;
  expanded: boolean;
  onToggleExpand: () => void;
  isActive: boolean;
  dragDisabled: boolean;
}) {
  const { attributes, listeners, setNodeRef, transform, transition, isDragging } =
    useSortable({
      id: item.id,
      data: { parentId: item.parentId ?? "" },
      // Reorder only without an active filter: under a filter treeItems is
      // a partial sibling view, so a drag would reorder just the visible
      // subset (see handleDragEnd). Disabling the sortable also hides the
      // drag affordance entirely.
      disabled: dragDisabled,
    });

  const isMatch = matchIds.has(item.id);
  const isAncestor = ancestorIds.has(item.id);

  return (
    <div
      ref={setNodeRef}
      style={{ transform: CSS.Transform.toString(transform), transition }}
      {...attributes}
      {...(dragDisabled ? {} : listeners)}
      className={cn(
        "flex cursor-grab items-center gap-1.5 rounded-md border border-transparent px-1.5 py-1.5 transition-colors hover:border-border hover:bg-accent/50 active:cursor-grabbing",
        selected && "bg-accent/60",
        filterActive && isAncestor && !isMatch && "opacity-70",
        filterActive && isMatch && "border-border/60 bg-primary/5",
        isDragging && "z-10 opacity-50",
        isActive && "ring-2 ring-ring",
        dragDisabled && "cursor-default",
      )}
      aria-roledescription="draggable"
      aria-label={
        dragDisabled
          ? `${item.title}${hasChildren ? " (has children)" : ""}`
          : hasChildren
            ? `${item.title} (draggable, has children)`
            : `${item.title} (draggable)`
      }
    >
      {/* indent guides */}
      {Array.from({ length: depth }, (_, i) => (
        <span
          key={i}
          aria-hidden
          className="h-6 w-[18px] shrink-0 border-l border-dashed border-border/60"
        />
      ))}
      <GripVertical
        aria-hidden
        className={cn(
          "h-3.5 w-3.5 shrink-0 text-muted-foreground/60",
          dragDisabled && "opacity-0",
        )}
      />
      <input
        type="checkbox"
        checked={checked}
        disabled={!selectable}
        ref={(el) => {
          if (el) el.indeterminate = triState;
        }}
        onChange={() => onToggleSelect(item.id)}
        // Stop the sortable drag listeners from hijacking the checkbox.
        onPointerDown={(e) => e.stopPropagation()}
        onKeyDown={(e) => e.stopPropagation()}
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
          onClick={onToggleExpand}
          aria-expanded={expanded}
          aria-label={expanded ? `Collapse ${item.title}` : `Expand ${item.title}`}
          onPointerDown={(e) => e.stopPropagation()}
          onKeyDown={(e) => e.stopPropagation()}
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
        {position ? (
          <PositionBadge
            position={position}
            label={`Sequential Order #${position}`}
            className="hidden sm:inline-flex"
          />
        ) : null}
        <BlockedChip
          blockedBy={blockState.blockedBy}
          id={item.id}
          depsCount={depsCountOf(item.id, blockState)}
        />
        <StatusPill status={item.status} className="hidden sm:inline-flex" />
      </span>
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
