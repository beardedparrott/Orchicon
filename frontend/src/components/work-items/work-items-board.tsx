// Kanban board for the Work Items page (design §5.3/§5.4, ADR-4/ADR-5/ADR-8).
//
// Presentational: the page shell owns data fetching + filtering and
// passes the visible items + block state. This view renders the columns
// and handles drag & drop:
//
// - One column per server status; the drop target maps 1:1 to a
//   WorkItemStatus value (ADR-8). checkpointing/recovering render in
//   Running, scheduled in Pending — the card keeps its REAL status pill.
// - Columns are responsive: flex-1 min-w-[200px] so they share the
//   viewport width. Horizontal scroll is a fallback on very small screens.
// - Drag & drop via @dnd-kit (Pointer + Keyboard sensors). Drops are
//   server-confirmed (ADR-5): the card shows a transient "moving…" state
//   and stays in its origin column until the refetch lands.
// - Advisory gates before the mutation: a blocked card cannot be dropped
//   on Ready, and Epics/Features accept only pending|succeeded|cancelled.
// - "Move to…" select per card = the assistive/keyboard path (design §5.5).
// - Hierarchy: parent/child relationships shown with expand/collapse arrows
//   (Epic → Feature → Task → Subtask). Children are indented under their
//   parent card within the same column.

import { useMemo, useState } from "react";
import {
  DndContext,
  KeyboardSensor,
  PointerSensor,
  closestCenter,
  useDroppable,
  useSensor,
  useSensors,
  type DragEndEvent,
} from "@dnd-kit/core";
import { SortableContext, sortableKeyboardCoordinates, useSortable } from "@dnd-kit/sortable";
import { CSS } from "@dnd-kit/utilities";
import { ChevronRight, Lock, SearchX } from "lucide-react";

import { useUpdateWorkItem } from "@/api/workItems";
import { WorkItemStatus, type WorkItem } from "@/api/gen/orchicon/api/v1/work_item_pb";
import {
  BOARD_COLUMNS,
  allowedStatusesForKind,
  columnForStatus,
  kindMeta,
  statusMeta,
} from "@/components/work-items/work-item-meta";
import type { BlockState } from "@/components/work-items/dependency-utils";
import { blockingTitles } from "@/components/work-items/dependency-utils";
import { WorkItemCard } from "@/components/work-items/work-item-card";
import { Card, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { useToast } from "@/components/ui/toast";
import { cn } from "@/lib/utils";

/** Statuses that are read-only on the board — only the server sets these. */
const READ_ONLY_STATUSES = new Set<number>([WorkItemStatus.RUNNING]);

export interface WorkItemsBoardProps {
  projectId: string;
  /** items that pass the active kind/status filters (all columns) */
  items: WorkItem[];
  blockState: BlockState;
  selected: Set<string>;
  onToggleSelect: (id: string) => void;
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

/** Build a hierarchical tree from flat items, grouped by parentId. */
function buildHierarchy(items: WorkItem[]): HierarchyNode[] {
  const byParent = new Map<string, WorkItem[]>();
  for (const item of items) {
    const key = item.parentId || "";
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
  isLoading,
  error,
  hasQuery,
}: WorkItemsBoardProps) {
  const updateStatus = useUpdateWorkItem(projectId);
  const toast = useToast();
  const [movingId, setMovingId] = useState<string | null>(null);

  const sensors = useSensors(
    useSensor(PointerSensor, {
      activationConstraint: { distance: 3 },
    }),
    useSensor(KeyboardSensor, { coordinateGetter: sortableKeyboardCoordinates }),
  );

  const itemsById = useMemo(() => new Map(items.map((i) => [i.id, i])), [items]);

  const handleMove = (item: WorkItem, targetStatus: number) => {
    if (targetStatus === item.status) return;

    // Running is read-only: only the server sets this status when a
    // workflow is executing. Users must go INTO the work item to start
    // a workflow — drag-and-drop and the Move-to menu cannot set it.
    if (READ_ONLY_STATUSES.has(targetStatus)) {
      toast.info(
        `"${item.title}" cannot be moved to ${statusMeta(targetStatus).label} via drag. Open the work item to start a workflow.`,
        { title: "Read-only status" },
      );
      return;
    }

    // Advisory dependency gate: blocked items cannot be dropped on Ready.
    const blockers = blockState.blockedBy.get(item.id) ?? [];
    if (targetStatus === WorkItemStatus.READY && blockers.length > 0) {
      toast.error(
        `Cannot move to Ready: blocked by ${blockingTitles(blockState.blockedBy, item.id)}`,
        { title: "Blocked" },
      );
      return;
    }

    // Advisory kind gate: Epics/Features are not schedulable.
    if (!allowedStatusesForKind(item.kind).includes(targetStatus)) {
      toast.error(
        `"${item.title}" cannot move to ${statusMeta(targetStatus).label}: ${kindMeta(item.kind).label}s only accept ${allowedStatusesForKind(item.kind).map((s) => statusMeta(s).label).join(", ")}.`,
        { title: "Transition not allowed" },
      );
      return;
    }

    setMovingId(item.id);
    updateStatus.mutate(
      { id: item.id, status: targetStatus as WorkItemStatus },
      {
        onSuccess: (updated) => {
          toast.success(
            `Moved "${updated.title}" to ${statusMeta(updated.status).label}`,
          );
        },
        // Errors surface via the global mutation onError toast (main.tsx).
        onSettled: () => setMovingId(null),
      },
    );
  };

  const handleDragEnd = (event: DragEndEvent) => {
    const { active, over } = event;
    if (!over) return;
    const item = itemsById.get(String(active.id));
    if (!item) return;
    const overData = over.data.current as { type?: string; status?: number } | undefined;
    let targetStatus: number | undefined;
    if (overData?.type === "column") targetStatus = overData.status;
    else if (overData?.type === "card") targetStatus = overData.status;
    if (targetStatus == null) return;
    // Guard: Running is a read-only column. closestCenter can
    // incorrectly snap to the nearest card/column boundary, so this
    // explicit check prevents accidental workflow kicks.
    if (READ_ONLY_STATUSES.has(targetStatus)) return;
    handleMove(item, targetStatus);
  };

  if (isLoading) {
    return (
      <div
        className="flex gap-3 overflow-hidden rounded-lg border"
        style={{ height: "calc(100vh - 280px)", minHeight: "400px" }}
        aria-busy="true"
      >
        {BOARD_COLUMNS.map((col) => (
          <div
            key={col.status}
            className="h-full flex-1 min-w-[200px] animate-pulse rounded-lg border bg-card/50"
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
      collisionDetection={closestCenter}
      onDragEnd={handleDragEnd}
    >
      <div
        className="flex gap-3 overflow-x-auto rounded-lg border bg-card/20 p-2"
        style={{ height: "calc(100vh - 280px)", minHeight: "400px" }}
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
            />
          );
        })}
      </div>
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
}: {
  column: { status: number; label: string };
  items: WorkItem[];
  allItems: WorkItem[];
  selected: Set<string>;
  onToggleSelect: (id: string) => void;
  blockState: BlockState;
  movingId: string | null;
  onMove: (item: WorkItem, targetStatus: number) => void;
}) {
  const isReadOnly = READ_ONLY_STATUSES.has(column.status);

  // Read-only columns (e.g. Running) are not droppable — only the
  // server transitions items into these statuses via workflow execution.
  const { setNodeRef, isOver } = useDroppable({
    id: `col-${column.status}`,
    data: { type: "column", status: column.status },
    disabled: isReadOnly,
  });

  // Build hierarchy for this column's items
  const hierarchy = useMemo(() => {
    const nodes = buildHierarchy(items);
    // Only show root-level items in the column; children are nested
    return nodes;
  }, [items]);

  return (
    <section
      ref={setNodeRef}
      aria-label={`${column.label} column${isReadOnly ? " (read-only)" : ""}`}
      className={cn(
        "flex flex-1 min-w-[200px] snap-start flex-col rounded-lg border bg-card/40 transition-colors",
        isOver && !isReadOnly && "bg-accent/40 ring-2 ring-ring",
        isReadOnly && "opacity-80",
      )}
    >
      <div className="sticky top-0 z-10 flex items-center gap-2 rounded-t-lg border-b bg-card/90 px-3 py-2 backdrop-blur">
        <span
          aria-hidden
          className={cn("h-2 w-2 rounded-full", statusMeta(column.status).dot)}
        />
        <h3 className="text-sm font-semibold">{column.label}</h3>
        {isReadOnly && (
          <Lock aria-hidden className="h-3 w-3 text-muted-foreground" />
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
            />
          ))}
        </SortableContext>
        {items.length === 0 && (
          <p className="px-2 py-6 text-center text-xs text-muted-foreground">
            {isReadOnly ? "No items" : "Drop items here"}
          </p>
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
  depth = 0,
}: {
  node: HierarchyNode;
  allItems: WorkItem[];
  selected: Set<string>;
  onToggleSelect: (id: string) => void;
  blockState: BlockState;
  movingId: string | null;
  onMove: (item: WorkItem, targetStatus: number) => void;
  depth?: number;
}) {
  const [expanded, setExpanded] = useState(true);
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
          onToggleExpand={() => setExpanded((v) => !v)}
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
    >
      <WorkItemCard
        item={item}
        selected={selected.has(item.id)}
        onToggleSelect={onToggleSelect}
        blockedBy={blockState.blockedBy}
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
 *  Running is excluded — it is a system-managed status. */
function MoveToMenu({
  item,
  disabled,
  onMove,
}: {
  item: WorkItem;
  disabled: boolean;
  onMove: (item: WorkItem, targetStatus: number) => void;
}) {
  // Exclude current status and all read-only statuses (Running, etc.)
  const allowed = allowedStatusesForKind(item.kind).filter(
    (s) => s !== item.status && !READ_ONLY_STATUSES.has(s),
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
          {statusMeta(s).label}
        </option>
      ))}
    </select>
  );
}
