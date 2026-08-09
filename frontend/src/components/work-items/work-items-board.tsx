// Kanban board for the Work Items page (design §5.3/§5.4, ADR-4/ADR-5/ADR-8).
//
// Presentational: the page shell owns data fetching + filtering and
// passes the visible items + block state. This view renders the columns
// and handles drag & drop:
//
// - One column per server status; the drop target maps 1:1 to a
//   WorkItemStatus value (ADR-8). checkpointing/recovering render in
//   Running, scheduled in Pending — the card keeps its REAL status pill.
// - Columns are responsive: flex-1 min-w-[280px] so they share the
//   viewport width. Horizontal scroll is a fallback on very small screens.
// - Drag & drop via @dnd-kit (Pointer + Keyboard sensors). Collision
//   detection uses pointerWithin (not closestCenter) to prevent cross-
//   column snapping — the pointer must be literally inside a droppable.
//   Running is fully read-only: useDroppable disabled + pointerWithin
//   exclusion + explicit guard in handleDragEnd.
// - Drops are server-confirmed (ADR-5): the card shows a transient
//   "moving…" state and stays in its origin column until the refetch
//   lands. keepPreviousData on the list query prevents empty flashes.
// - Advisory gates before the mutation: a blocked card cannot be dropped
//   on Ready, and Epics/Features accept only pending|succeeded|cancelled.
// - "Move to…" select per card = the assistive/keyboard path (design §5.5).
// - Hierarchy: parent/child relationships shown with expand/collapse arrows
//   (Epic → Feature → Task → Subtask). Children are indented under their
//   parent card within the same column.

import { useCallback, useMemo, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import {
  DndContext,
  DragOverlay,
  KeyboardSensor,
  PointerSensor,
  pointerWithin,
  useDroppable,
  useSensor,
  useSensors,
  type DragEndEvent,
  type DragStartEvent,
} from "@dnd-kit/core";
import { SortableContext, sortableKeyboardCoordinates, useSortable } from "@dnd-kit/sortable";
import { CSS } from "@dnd-kit/utilities";
import { ChevronRight, Lock, SearchX } from "lucide-react";

import { useUpdateWorkItem, workItemKeys } from "@/api/workItems";
import { WorkItemStatus, type WorkItem } from "@/api/gen/orchicon/api/v1/work_item_pb";
import { useBatchMoveWorkItems, validateMove } from "@/components/work-items/batch-move";
import {
  BOARD_COLUMNS,
  MANUALLY_UNMOVABLE_STATUSES,
  allowedStatusesForKind,
  columnForStatus,
  kindMeta,
  statusMeta,
} from "@/components/work-items/work-item-meta";
import type { BlockState } from "@/components/work-items/dependency-utils";
import { blockingTitles } from "@/components/work-items/dependency-utils";
import { computeSequencePositions } from "@/components/work-items/sequence-utils";
import { PositionBadge } from "@/components/work-items/work-item-badges";
import { WorkItemCard } from "@/components/work-items/work-item-card";
import { Card, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { useToast } from "@/components/ui/toast";
import { cn } from "@/lib/utils";

export interface WorkItemsBoardProps {
  projectId: string;
  /** items that pass the active kind/status filters (all columns) */
  items: WorkItem[];
  blockState: BlockState;
  selected: Set<string>;
  onToggleSelect: (id: string) => void;
  /** persisted per-project collapse state; default EXPANDED (ADR-WI-3) —
   *  an empty set means nothing is collapsed so children stay visible */
  collapsedIds: Set<string>;
  onToggleCollapse: (id: string) => void;
  isLoading: boolean;
  error: unknown;
  hasQuery: boolean;
}

// ---------------------------------------------------------------------------
// Board hierarchy (expand/collapse for parent/child relationships)
// ---------------------------------------------------------------------------

interface HierarchyNode {
  item: WorkItem;
  children: HierarchyNode[];
  depth: number;
}

/** Build a hierarchical tree from flat items, grouped by parentId.
 *  Items whose parent is not in this column's item set are treated as
 *  root-level nodes (orphaned children should still render). */
function buildHierarchy(items: WorkItem[]): HierarchyNode[] {
  const itemIds = new Set(items.map((i) => i.id));
  const byParent = new Map<string, WorkItem[]>();
  for (const item of items) {
    // If the parent is not in this column, treat the item as root-level
    const key = item.parentId && itemIds.has(item.parentId) ? item.parentId : "";
    const list = byParent.get(key);
    if (list) list.push(item);
    else byParent.set(key, [item]);
  }

  function buildLevel(parentId: string, depth: number): HierarchyNode[] {
    const children = byParent.get(parentId) || [];
    return children.map((item) => ({
      item,
      children: buildLevel(item.id, depth + 1),
      depth,
    }));
  }

  return buildLevel("", 0);
}

export function WorkItemsBoard({
  projectId,
  items,
  blockState,
  selected,
  onToggleSelect,
  collapsedIds,
  onToggleCollapse,
  isLoading,
  error,
  hasQuery,
}: WorkItemsBoardProps) {
  const updateStatus = useUpdateWorkItem(projectId);
  const { moveItems: batchMoveItems } = useBatchMoveWorkItems(projectId);
  const qc = useQueryClient();
  const toast = useToast();
  const [movingId, setMovingId] = useState<string | null>(null);
  const [activeItem, setActiveItem] = useState<WorkItem | null>(null);
  /** size of the selection being dragged together; 0/1 = plain single drag */
  const [dragCount, setDragCount] = useState(0);

  const sensors = useSensors(
    useSensor(PointerSensor, {
      activationConstraint: { distance: 3 },
    }),
    useSensor(KeyboardSensor, { coordinateGetter: sortableKeyboardCoordinates }),
  );

  const itemsById = useMemo(() => new Map(items.map((i) => [i.id, i])), [items]);
  // Chain positions (sort_order rank within parent) so every card with a
  // parent shows its true sequence position regardless of display sort.
  const positions = useMemo(() => computeSequencePositions(items), [items]);

  const handleDragStart = useCallback(
    (event: DragStartEvent) => {
      const { active } = event;
      const item = itemsById.get(String(active.id));
      if (item) {
        setActiveItem(item);
        setDragCount(selected.has(item.id) && selected.size > 1 ? selected.size : 0);
      }
    },
    [itemsById, selected],
  );

  const handleMove = (item: WorkItem, targetStatus: number) => {
    if (targetStatus === item.status) return;

    // Advisory per-item gate shared with the batch path (ADR-WI-4).
    const validation = validateMove(item, targetStatus, blockState);
    if (!validation.ok) {
      switch (validation.code) {
        case "system-managed":
          toast.info(
            `"${item.title}" cannot be moved to ${statusMeta(targetStatus).titleLabel} via drag. Open the work item to start a workflow.`,
            { title: "System-managed status" },
          );
          return;
        case "blocked":
          toast.error(
            `Cannot move to ${statusMeta(targetStatus).titleLabel}: blocked by ${blockingTitles(blockState.blockedBy, item.id)}`,
            { title: "Blocked" },
          );
          return;
        case "kind":
          toast.error(
            `"${item.title}" cannot move to ${statusMeta(targetStatus).titleLabel}: ${kindMeta(item.kind).label}s only accept ${allowedStatusesForKind(item.kind).map((s) => statusMeta(s).titleLabel).join(", ")}.`,
            { title: "Transition not allowed" },
          );
          return;
      }
    }

    // Optimistic cache update: immediately move the card to the target
    // column so it doesn't disappear during the mutation. The card will
    // snap back if the server rejects the move.
    //
    // `setQueriesData` (not `setQueryData`) with the trimmed prefix key —
    // the page's live list query is the 4-element key ending in the
    // `{search,sortBy,sortOrder}` opts object; a bare 3-element
    // `setQueryData` wrote to a phantom cache entry and the card stayed
    // in its origin column until the 5s poll refetched.
    const listKey = workItemKeys.list(projectId);
    qc.setQueriesData({ queryKey: listKey }, (old: WorkItem[] | undefined) => {
      if (!old) return old;
      return old.map((i) =>
        i.id === item.id ? { ...i, status: targetStatus } : i,
      );
    });

    setMovingId(item.id);
    updateStatus.mutate(
      { id: item.id, status: targetStatus as WorkItemStatus },
      {
        onSuccess: (updated) => {
          // Server confirms — update with the real server data
          qc.setQueriesData({ queryKey: listKey }, (old: WorkItem[] | undefined) => {
            if (!old) return old;
            return old.map((i) => (i.id === updated.id ? updated : i));
          });
          toast.success(
            `Moved "${updated.title}" to ${statusMeta(updated.status).titleLabel}`,
          );
        },
        onError: () => {
          // Revert optimistic update on error
          qc.invalidateQueries({ queryKey: listKey });
        },
        onSettled: () => setMovingId(null),
      },
    );
  };

  const handleDragEnd = (event: DragEndEvent) => {
    setActiveItem(null);
    setDragCount(0);
    const { active, over } = event;
    if (!over) return;
    const item = itemsById.get(String(active.id));
    if (!item) return;
    const overData = over.data.current as { type?: string; status?: number } | undefined;
    let targetStatus: number | undefined;
    if (overData?.type === "column") {
      targetStatus = overData.status;
    } else if (overData?.type === "card" && overData.status != null) {
      // Resolve the COLUMN the card is rendered in, not the card's actual
      // status. A SCHEDULED card renders in the Pending column, so dropping
      // on it should move the dragged item to Pending, not Scheduled.
      // Without this, dropping on a card in the wrong status snaps the
      // dragged item to the wrong column (ADR-9 §3).
      targetStatus = columnForStatus(overData.status);
    }
    if (targetStatus == null) return;
    // Guard: system-managed statuses (Running, Checkpointing, Recovering)
    // are read-only — only the server sets these via workflow execution.
    if (MANUALLY_UNMOVABLE_STATUSES.has(targetStatus)) return;
    // Multi-drag (ADR-WI-4): dragging a selected card moves the whole
    // selection; a non-selected card still moves alone.
    if (selected.has(item.id) && selected.size > 1) {
      void batchMoveItems(Array.from(selected), targetStatus, { itemsById, blockState });
      return;
    }
    handleMove(item, targetStatus);
  };

  if (isLoading) {
    return (
      <div
        className="flex flex-1 gap-3 overflow-hidden rounded-lg border"
        style={{ minHeight: "calc(100vh - 280px)" }}
        aria-busy="true"
      >
        {BOARD_COLUMNS.map((col) => (
          <div
            key={col.status}
            className="h-full flex-1 animate-pulse rounded-lg border bg-card/50"
          />
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
  if (items.length === 0) {
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
              : "Create work items to populate the board."}
          </CardDescription>
        </CardHeader>
      </Card>
    );
  }

  return (
    <DndContext
      sensors={sensors}
      collisionDetection={pointerWithin}
      onDragStart={handleDragStart}
      onDragEnd={handleDragEnd}
      // Disable dnd-kit auto-scroll: the board overflows horizontally
      // (flex-1 min-w-[280px] columns), so near the viewport edge the
      // auto-scroller shifts the board mid-drag and pointerWithin then
      // resolves the drop against a DIFFERENT column than the one the
      // pointer was over — silently moving cards to the wrong status
      // (e.g. "Failed" → "Cancelled" at 1280px). The board is scrollable
      // manually; the drop must always resolve to the visible column
      // under the pointer (AC3: drag to other columns).
      autoScroll={false}
    >
      <div
        className="flex flex-1 gap-3 overflow-x-auto rounded-lg border bg-muted/20 p-3"
        style={{ minHeight: "calc(100vh - 280px)" }}
      >
        {BOARD_COLUMNS.map((col) => {
          const colItems = items.filter((i) => columnForStatus(i.status) === col.status);
          return (
            <BoardColumn
              key={col.status}
              column={col}
              items={colItems}
              allItems={items}
              selected={selected}
              onToggleSelect={onToggleSelect}
              blockState={blockState}
              movingId={movingId}
              onMove={handleMove}
              collapsedIds={collapsedIds}
              onToggleCollapse={onToggleCollapse}
              dragCount={dragCount}
              positions={positions}
            />
          );
        })}
      </div>
      <DragOverlay dropAnimation={null}>
        {activeItem ? (
          <div className="relative w-[280px] opacity-90 shadow-xl ring-2 ring-ring/40">
            <WorkItemCard
              item={activeItem}
              selected={selected.has(activeItem.id)}
              onToggleSelect={() => {}}
              blockedBy={blockState.blockedBy}
              depsCount={
                (blockState.blocks.get(activeItem.id)?.length ?? 0) +
                (blockState.blockedBy.get(activeItem.id)?.length ?? 0)
              }
            />
            {dragCount > 1 && (
              <span
                aria-hidden
                className="absolute -right-2 -top-2 flex h-6 min-w-6 items-center justify-center rounded-full bg-primary px-1.5 text-xs font-semibold text-primary-foreground shadow-md"
              >
                +{dragCount - 1}
              </span>
            )}
          </div>
        ) : null}
      </DragOverlay>
    </DndContext>
  );
}

// ---------------------------------------------------------------------------
// Board column — responsive width, hierarchy-aware
// ---------------------------------------------------------------------------

function BoardColumn({
  column,
  items,
  allItems,
  selected,
  onToggleSelect,
  blockState,
  movingId,
  onMove,
  collapsedIds,
  onToggleCollapse,
  dragCount,
  positions,
}: {
  column: { status: number; label: string };
  items: WorkItem[];
  allItems: WorkItem[];
  selected: Set<string>;
  onToggleSelect: (id: string) => void;
  blockState: BlockState;
  movingId: string | null;
  onMove: (item: WorkItem, targetStatus: number) => void;
  collapsedIds: Set<string>;
  onToggleCollapse: (id: string) => void;
  dragCount: number;
  positions: Map<string, number>;
}) {
  const isReadOnly = MANUALLY_UNMOVABLE_STATUSES.has(column.status);

  // Read-only columns (Running, Checkpointing, Recovering) are not
  // droppable — only the server transitions items into these statuses
  // via workflow execution.
  const { setNodeRef, isOver } = useDroppable({
    id: `col-${column.status}`,
    data: { type: "column", status: column.status },
    disabled: isReadOnly,
  });

  // Build hierarchy for this column's items
  const hierarchy = useMemo(() => {
    const nodes = buildHierarchy(items);
    return nodes;
  }, [items]);

  return (
    <section
      ref={setNodeRef}
      aria-label={`${column.label} column${isReadOnly ? " (system-managed)" : ""}`}
      className={cn(
        "flex flex-1 snap-start flex-col rounded-lg border transition-colors",
        isOver && !isReadOnly && "bg-accent/40 ring-2 ring-ring",
        isReadOnly
          ? "border-dashed bg-muted/30 opacity-75"
          : "bg-card/40",
      )}
    >
      <div className="sticky top-0 z-10 flex items-center gap-2 rounded-t-lg border-b bg-card/90 px-3 py-2.5 backdrop-blur">
        <span
          aria-hidden
          className={cn("h-2.5 w-2.5 rounded-full", statusMeta(column.status).dot)}
        />
        <div className="flex flex-col">
          <h3 className="text-sm font-semibold tracking-tight">{column.label}</h3>
          {isReadOnly && (
            <span className="text-[10px] leading-tight text-muted-foreground">
              System-managed
            </span>
          )}
        </div>
        {isReadOnly && (
          <Tooltip>
            <TooltipTrigger asChild>
              <Lock aria-hidden className="h-3.5 w-3.5 text-muted-foreground" />
            </TooltipTrigger>
            <TooltipContent>System-managed — only set by workflows</TooltipContent>
          </Tooltip>
        )}
        <span className="ml-auto rounded-full bg-muted px-2 py-0.5 text-xs font-medium tabular-nums text-muted-foreground">
          {items.length}
        </span>
      </div>
      <div className="flex flex-1 flex-col gap-1.5 overflow-y-auto p-2">
        <SortableContext items={items.map((i) => i.id)}>
          {hierarchy.map((node) => (
            <HierarchyNodeComponent
              key={node.item.id}
              node={node}
              allItems={allItems}
              selected={selected}
              onToggleSelect={onToggleSelect}
              blockState={blockState}
              movingId={movingId}
              onMove={onMove}
              collapsedIds={collapsedIds}
              onToggleCollapse={onToggleCollapse}
              dragCount={dragCount}
              positions={positions}
            />
          ))}
        </SortableContext>
        {items.length === 0 && (
          <div className="flex flex-1 items-center justify-center px-2 py-8">
            <p className="text-center text-xs text-muted-foreground">
              {isReadOnly ? "No items" : "Drop items here"}
            </p>
          </div>
        )}
      </div>
    </section>
  );
}

// ---------------------------------------------------------------------------
// Hierarchy node — renders a card with expand/collapse for children
// ---------------------------------------------------------------------------

function HierarchyNodeComponent({
  node,
  allItems,
  selected,
  onToggleSelect,
  blockState,
  movingId,
  onMove,
  collapsedIds,
  onToggleCollapse,
  dragCount,
  positions,
  depth = 0,
}: {
  node: HierarchyNode;
  allItems: WorkItem[];
  selected: Set<string>;
  onToggleSelect: (id: string) => void;
  blockState: BlockState;
  movingId: string | null;
  onMove: (item: WorkItem, targetStatus: number) => void;
  collapsedIds: Set<string>;
  onToggleCollapse: (id: string) => void;
  dragCount: number;
  positions: Map<string, number>;
  depth?: number;
}) {
  // Persisted per-project collapse state; default EXPANDED (ADR-WI-3) so
  // children stay visible until the user collapses a parent.
  const expanded = !collapsedIds.has(node.item.id);
  const hasChildren = node.children.length > 0;

  return (
    <div>
      <div style={{ paddingLeft: depth * 16 }}>
        <SortableCard
          item={node.item}
          selected={selected}
          onToggleSelect={onToggleSelect}
          blockState={blockState}
          moving={movingId === node.item.id}
          onMove={onMove}
          hasChildren={hasChildren}
          expanded={expanded}
          onToggleExpand={() => onToggleCollapse(node.item.id)}
          multiDragCount={dragCount}
          position={positions.get(node.item.id)}
        />
      </div>
      {expanded &&
        hasChildren &&
        node.children.map((child) => (
          <HierarchyNodeComponent
            key={child.item.id}
            node={child}
            allItems={allItems}
            selected={selected}
            onToggleSelect={onToggleSelect}
            blockState={blockState}
            movingId={movingId}
            onMove={onMove}
            collapsedIds={collapsedIds}
            onToggleCollapse={onToggleCollapse}
            dragCount={dragCount}
            positions={positions}
            depth={depth + 1}
          />
        ))}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Sortable card — wraps WorkItemCard with dnd-kit sortable + hierarchy controls
// ---------------------------------------------------------------------------

function SortableCard({
  item,
  selected,
  onToggleSelect,
  blockState,
  moving,
  onMove,
  hasChildren = false,
  expanded = true,
  onToggleExpand,
  multiDragCount = 0,
  position,
}: {
  item: WorkItem;
  selected: Set<string>;
  onToggleSelect: (id: string) => void;
  blockState: BlockState;
  moving: boolean;
  onMove: (item: WorkItem, targetStatus: number) => void;
  hasChildren?: boolean;
  expanded?: boolean;
  onToggleExpand?: () => void;
  /** >1 when the drag carries the whole selection (ADR-WI-4) */
  multiDragCount?: number;
  /** chain position badge (#1, #2, …) — true sequence order */
  position?: number;
}) {
  const {
    attributes,
    listeners,
    setNodeRef,
    transform,
    transition,
    isDragging,
  } = useSortable({
    id: item.id,
    data: { type: "card", status: item.status },
  });

  const style = {
    transform: CSS.Transform.toString(transform),
    transition,
  };

  return (
    <div
      ref={setNodeRef}
      style={style}
      {...attributes}
      {...listeners}
      className={cn(
        "cursor-grab rounded-md outline-none focus-visible:ring-2 focus-visible:ring-ring active:cursor-grabbing",
        isDragging && "z-10 opacity-60",
      )}
      aria-roledescription="draggable"
      aria-label={multiDragCount > 1 ? `Dragging ${multiDragCount} items` : undefined}
    >
      <WorkItemCard
        item={item}
        selected={selected.has(item.id)}
        onToggleSelect={onToggleSelect}
        blockedBy={blockState.blockedBy}
        badge={position ? <PositionBadge position={position} /> : undefined}
        depsCount={
          (blockState.blocks.get(item.id)?.length ?? 0) +
          (blockState.blockedBy.get(item.id)?.length ?? 0)
        }
        moving={moving}
        actions={
          <div className="flex items-center gap-1">
            {hasChildren && (
              <button
                type="button"
                onClick={(e) => {
                  e.stopPropagation();
                  onToggleExpand?.();
                }}
                onPointerDown={(e) => e.stopPropagation()}
                onKeyDown={(e) => e.stopPropagation()}
                aria-label={expanded ? "Collapse children" : "Expand children"}
                aria-expanded={expanded}
                className="flex h-5 w-5 items-center justify-center rounded text-muted-foreground transition-colors hover:bg-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring"
              >
                <ChevronRight
                  className={cn(
                    "h-3 w-3 transition-transform",
                    expanded && "rotate-90",
                  )}
                />
              </button>
            )}
            <MoveToMenu item={item} disabled={moving} onMove={onMove} />
          </div>
        }
      />
    </div>
  );
}

/** Assistive/keyboard path: a small select listing the allowed target
 *  statuses, performing the identical server-confirmed mutation.
 *  System-managed statuses (Running, Checkpointing, Recovering) are
 *  excluded — only the server sets these via workflow execution. */
function MoveToMenu({
  item,
  disabled,
  onMove,
}: {
  item: WorkItem;
  disabled: boolean;
  onMove: (item: WorkItem, targetStatus: number) => void;
}) {
  // Exclude current status and all system-managed statuses
  const allowed = allowedStatusesForKind(item.kind).filter(
    (s) => s !== item.status && !MANUALLY_UNMOVABLE_STATUSES.has(s),
  );
  return (
    <select
      value=""
      disabled={disabled || allowed.length === 0}
      aria-label={`Move ${item.title} to…`}
      onPointerDown={(e) => e.stopPropagation()}
      onKeyDown={(e) => e.stopPropagation()}
      onChange={(e) => {
        const status = Number(e.target.value);
        if (status) onMove(item, status);
        e.target.value = "";
      }}
      className="h-6 max-w-[7.5rem] cursor-pointer rounded border border-input bg-background px-1 text-[11px] text-muted-foreground focus-visible:ring-2 focus-visible:ring-ring"
    >
      <option value="" disabled>
        Move to…
      </option>
      {allowed.map((s) => (
        <option key={s} value={s}>
          {statusMeta(s).titleLabel}
        </option>
      ))}
    </select>
  );
}
