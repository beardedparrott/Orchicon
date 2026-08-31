// Shared drag-and-drop context for the Workers and Workflows category
// folders. Makes item rows freely draggable into their category folders
// (and into "Uncategorized" to remove an assignment), replacing the old
// per-row category dropdown.
//
// Reuses the verified work-items click-suppression standard so a drag-and-
// release never triggers the accidental item open/select bug:
//   - PointerSensor with a 5px distance gate (a plain click never starts a
//     drag);
//   - a DOCUMENT-level native capture-phase click listener gated by a
//     suppressClickRef flag: after a dnd-kit drag the browser dispatches a
//     synthetic click whose propagation path is only [target -> document],
//     so a container/row listener would never fire — only a document capture
//     listener does (verified empirically in work-items-board/tree);
//   - a ~300ms setTimeout backstop in onDragEnd that clears the flag when the
//     pointer is released outside any clickable target (no click to consume).
//
// Categories are a frontend-only organizational layer (see
// lib/category-store.ts): dropping calls `onAssign(entityId, categoryId)`,
// the same local mutation the old dropdown used. No backend surface.

import { useEffect, useMemo, useRef } from "react";
import {
  DndContext,
  KeyboardSensor,
  PointerSensor,
  pointerWithin,
  useSensor,
  useSensors,
  type DragEndEvent,
} from "@dnd-kit/core";
import { sortableKeyboardCoordinates } from "@dnd-kit/sortable";
import type { Category } from "@/lib/category-store";

const UNCAT_ID = "uncategorized";

interface CategoryDndContextProps {
  /** Valid droppable targets (each category id); "uncategorized" is implicit. */
  categories: Category[];
  /** Called on drop: assign the dragged entity to a category ("" removes). */
  onAssign: (entityId: string, categoryId: string) => void;
  children: React.ReactNode;
}

export function CategoryDndContext({
  categories,
  onAssign,
  children,
}: CategoryDndContextProps) {
  const suppressClickRef = useRef(false);

  // Document-level capture-phase click suppression (see file header). The
  // flag is set in onDragStart and consumed here when it actually suppresses
  // the post-drag click; the onDragEnd setTimeout is the backstop.
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
    useSensor(PointerSensor, { activationConstraint: { distance: 5 } }),
    useSensor(KeyboardSensor, { coordinateGetter: sortableKeyboardCoordinates }),
  );

  const validTargets = useMemo(
    () => new Set([...categories.map((c) => c.id), UNCAT_ID]),
    [categories],
  );

  const handleDragStart = () => {
    suppressClickRef.current = true;
  };

  const handleDragEnd = (event: DragEndEvent) => {
    // Backstop: keep the flag set long enough that the drag's post-mouseup
    // click (dispatched immediately after pointerup) lands inside it. A
    // setTimeout(0) could clear the flag BEFORE the click and let the <Link>
    // navigate after a real drag.
    window.setTimeout(() => {
      suppressClickRef.current = false;
    }, 300);

    const { active, over } = event;
    if (!over) return;
    const entityId = String(active.id);
    const targetId = String(over.id);
    // No-op when the collision resolves back to the dragged row itself.
    if (entityId === targetId) return;
    // Only accept known folder targets (category ids or "uncategorized").
    if (!validTargets.has(targetId)) return;
    if (targetId === UNCAT_ID) {
      onAssign(entityId, "");
    } else {
      onAssign(entityId, targetId);
    }
  };

  const handleDragCancel = () => {
    // A cancelled drag (e.g. Escape) still ends with a possible click under
    // the pointer — arm the same backstop so a stray click is suppressed and
    // the flag clears shortly after.
    window.setTimeout(() => {
      suppressClickRef.current = false;
    }, 300);
  };

  return (
    <DndContext
      sensors={sensors}
      collisionDetection={pointerWithin}
      onDragStart={handleDragStart}
      onDragEnd={handleDragEnd}
      onDragCancel={handleDragCancel}
    >
      {children}
    </DndContext>
  );
}
