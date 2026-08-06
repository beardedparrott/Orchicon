// Kanban board for the Work Items page (design §5.3/§5.4, ADR-4/ADR-5/ADR-8).
//
// Presentational: the page shell owns data fetching + filtering and
// passes the visible items + block state. This view renders the columns
// and handles drag & drop:
//
// - One column per server status; the drop target maps 1:1 to a
//   WorkItemStatus value (ADR-8). checkpointing/recovering render in
//   Running, scheduled in Pending — the card keeps its REAL status pill.
// - Drag & drop via @dnd-kit (Pointer + Keyboard sensors). Drops are
//   server-confirmed (ADR-5): the card shows a transient "moving…" state
//   and stays in its origin column until the refetch lands.
// - Advisory gates before the mutation: a blocked card cannot be dropped
//   on Ready, and Epics/Features accept only pending|succeeded|cancelled.
// - "Move to…" select per card = the assistive/keyboard path (design §5.5).

import { useState } from "react";
import {
  DndContext,
  KeyboardSensor,
  PointerSensor,
  closestCorners,
  useDroppable,
  useSensor,
  useSensors,
  type DragEndEvent,
} from "@dnd-kit/core";
import { SortableContext, sortableKeyboardCoordinates, useSortable } from "@dnd-kit/sortable";
import { CSS } from "@dnd-kit/utilities";
import { SearchX } from "lucide-react";

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
    useSensor(PointerSensor, { activationConstraint: { distance: 6 } }),
    useSensor(KeyboardSensor, { coordinateGetter: sortableKeyboardCoordinates }),
  );

  const itemsById = new Map(items.map((i) => [i.id, i]));

  const handleMove = (item: WorkItem, targetStatus: number) => {
    if (targetStatus === item.status) return;

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
    handleMove(item, targetStatus);
  };

  if (isLoading) {
    return (
      <div className="flex gap-4 overflow-x-auto" aria-busy="true">
        {BOARD_COLUMNS.map((col) => (
          <div
            key={col.status}
            className="h-64 w-[280px] shrink-0 animate-pulse rounded-lg border bg-card/50"
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
      collisionDetection={closestCorners}
      onDragEnd={handleDragEnd}
    >
      <div className="flex snap-x gap-4 overflow-x-auto pb-4">
        {BOARD_COLUMNS.map((col) => {
          const colItems = items.filter((i) => columnForStatus(i.status) === col.status);
          return (
            <BoardColumn
              key={col.status}
              column={col}
              items={colItems}
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

function BoardColumn({
  column,
  items,
  selected,
  onToggleSelect,
  blockState,
  movingId,
  onMove,
}: {
  column: { status: number; label: string };
  items: WorkItem[];
  selected: Set<string>;
  onToggleSelect: (id: string) => void;
  blockState: BlockState;
  movingId: string | null;
  onMove: (item: WorkItem, targetStatus: number) => void;
}) {
  const { setNodeRef, isOver } = useDroppable({
    id: `col-${column.status}`,
    data: { type: "column", status: column.status },
  });

  return (
    <section
      ref={setNodeRef}
      aria-label={`${column.label} column`}
      className={cn(
        "flex w-[280px] shrink-0 snap-start flex-col rounded-lg border bg-card/40 transition-colors",
        isOver && "bg-accent/40 ring-2 ring-ring",
      )}
    >
      <div className="sticky top-0 z-10 flex items-center gap-2 rounded-t-lg border-b bg-card/90 px-3 py-2 backdrop-blur">
        <span
          aria-hidden
          className={cn("h-2 w-2 rounded-full", statusMeta(column.status).dot)}
        />
        <h3 className="text-sm font-semibold">{column.label}</h3>
        <span className="ml-auto rounded-full bg-muted px-2 py-0.5 text-xs font-medium tabular-nums text-muted-foreground">
          {items.length}
        </span>
      </div>
      <div className="flex flex-1 flex-col gap-2 p-2">
        <SortableContext items={items.map((i) => i.id)}>
          {items.map((item) => (
            <SortableCard
              key={item.id}
              item={item}
              selected={selected}
              onToggleSelect={onToggleSelect}
              blockState={blockState}
              moving={movingId === item.id}
              onMove={onMove}
            />
          ))}
        </SortableContext>
        {items.length === 0 && (
          <p className="px-2 py-6 text-center text-xs text-muted-foreground">
            Drop items here
          </p>
        )}
      </div>
    </section>
  );
}

function SortableCard({
  item,
  selected,
  onToggleSelect,
  blockState,
  moving,
  onMove,
}: {
  item: WorkItem;
  selected: Set<string>;
  onToggleSelect: (id: string) => void;
  blockState: BlockState;
  moving: boolean;
  onMove: (item: WorkItem, targetStatus: number) => void;
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
        actions={<MoveToMenu item={item} disabled={moving} onMove={onMove} />}
      />
    </div>
  );
}

/** Assistive/keyboard path: a small select listing the allowed target
 *  statuses, performing the identical server-confirmed mutation. */
function MoveToMenu({
  item,
  disabled,
  onMove,
}: {
  item: WorkItem;
  disabled: boolean;
  onMove: (item: WorkItem, targetStatus: number) => void;
}) {
  const allowed = allowedStatusesForKind(item.kind).filter((s) => s !== item.status);
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
