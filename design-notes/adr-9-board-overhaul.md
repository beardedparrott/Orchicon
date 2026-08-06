# ADR-9: Board Overhaul — Running Status Restriction, Layout, and Drop Resolution

## Status

Proposed

## Context

The Work Items Kanban board has four critical bugs from four failed iterations:

1. **Drag-to-running triggers workflows**: Users can drag items to the RUNNING column, which should only be set by the TaskReconciler. This bypasses the workflow system.

2. **Board too small**: Columns use `flex-1 min-w-[200px]` which sizes to content, not viewport. With 7 columns on a typical screen, each gets ~140px — barely enough for a card.

3. **Drop-to-pending goes to running**: When dropping on a card in the Pending column, `handleDragEnd` reads the card's actual status (`overData.status`). If a SCHEDULED item sits in the Pending column (via `columnForStatus(SCHEDULED) === PENDING`), the drop resolves to SCHEDULED, not PENDING.

4. **Work items disappear on workflow move**: When the TaskReconciler changes status (e.g., PENDING → RUNNING), the 5s poll refetches. If filtered by status, the item vanishes. Also, during mutation pending state, cards may be removed before refetch lands.

## Decision

### 1. Restrict Drag to Running

Add `MANUALLY_UNMOVABLE_STATUSES` to `work-item-meta.ts`:

```typescript
export const MANUALLY_UNMOVABLE_STATUSES = new Set<number>([
  WorkItemStatus.RUNNING,
  WorkItemStatus.CHECKPOINTING,
  WorkItemStatus.RECOVERING,
]);
```

In `handleMove()` (work-items-board.tsx), add a gate:

```typescript
if (MANUALLY_UNMOVABLE_STATUSES.has(targetStatus)) {
  toast.error(
    `Cannot manually move to ${statusMeta(targetStatus).label}: this status is managed by the workflow system.`,
    { title: "System-managed status" },
  );
  return;
}
```

The Running column gets visual treatment:
- Muted background (`bg-muted/30`)
- "System-managed" subtitle text
- No drop highlight on drag-over
- Cursor shows "not-allowed" when dragging over it

The `MoveToMenu` select also filters out these statuses:

```typescript
const allowed = allowedStatusesForKind(item.kind)
  .filter((s) => s !== item.status && !MANUALLY_UNMOVABLE_STATUSES.has(s));
```

### 2. Fix Board Sizing

**Before:**
```tsx
<div className="flex gap-3 overflow-x-auto pb-2">
  {BOARD_COLUMNS.map((col) => (
    <div className="flex flex-1 min-w-[200px] ...">
```

**After:**
```tsx
<div className="flex min-h-[calc(100vh-280px)] gap-3 overflow-x-auto pb-2">
  {BOARD_COLUMNS.map((col) => (
    <div className="flex flex-1 min-w-[240px] ...">
```

Key changes:
- Board container: `min-h-[calc(100vh-280px)]` — fills viewport below filter bar
- Column minimum: `min-w-[240px]` — slightly wider for better card fit
- Column body: `flex-1 overflow-y-auto` — scrollable when many items
- Board width: `w-full` — explicit full width

### 3. Fix Drop Target Resolution

**Before (buggy):**
```typescript
const overData = over.data.current as { type?: string; status?: number } | undefined;
let targetStatus: number | undefined;
if (overData?.type === "column") targetStatus = overData.status;
else if (overData?.type === "card") targetStatus = overData.status; // BUG: card's actual status
```

**After (fixed):**
```typescript
const overData = over.data.current as { type?: string; status?: number } | undefined;
let targetStatus: number | undefined;
if (overData?.type === "column") {
  targetStatus = overData.status;
} else if (overData?.type === "card") {
  // Resolve the COLUMN the card is rendered in, not the card's actual status.
  // A SCHEDULED card renders in the Pending column, so dropping on it
  // should move the dragged item to Pending, not Scheduled.
  targetStatus = columnForStatus(overData.status);
}
```

This ensures dropping on any card within a column targets that column's status.

### 4. Prevent Item Disappearance

**Optimistic cache update on drag-end:**

In `handleMove()`, after the advisory gates pass:

```typescript
setMovingId(item.id);

// Optimistic: immediately move card to target column in cache
const qc = useQueryClient();
const listKey = workItemKeys.list(projectId);
qc.setQueryData(listKey, (old: WorkItem[] | undefined) => {
  if (!old) return old;
  return old.map((i) =>
    i.id === item.id ? { ...i, status: targetStatus } : i
  );
});

updateStatus.mutate(
  { id: item.id, status: targetStatus as WorkItemStatus },
  {
    onSuccess: (updated) => {
      // Server confirms — update with real data
      qc.setQueryData(listKey, (old: WorkItem[] | undefined) => {
        if (!old) return old;
        return old.map((i) => (i.id === updated.id ? updated : i));
      });
      toast.success(`Moved "${updated.title}" to ${statusMeta(updated.status).label}`);
    },
    onError: () => {
      // Revert optimistic update on error
      qc.invalidateQueries({ queryKey: listKey });
    },
    onSettled: () => setMovingId(null),
  },
);
```

This ensures the card appears in the new column immediately, even before the server confirms.

## Consequences

### Positive
- Users cannot accidentally trigger workflows by dragging to Running
- Board fills the viewport, making drag-and-drop practical
- Drop targets resolve correctly regardless of card status mapping
- Cards don't disappear during status transitions
- Visual feedback clearly communicates system-managed statuses

### Negative
- Running column is visually muted (acceptable — it's read-only)
- Optimistic updates add complexity (but prevent the worse UX of disappearing cards)
- The `columnForStatus` resolution adds a lookup per drop (negligible cost)

### Risks
- Optimistic updates may show stale data if the server rejects the mutation (mitigated by onError revert)
- The `MANUALLY_UNMOVABLE_STATUSES` set must stay in sync with backend status management (documented in ADR)
