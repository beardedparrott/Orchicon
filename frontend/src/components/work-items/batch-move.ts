// Bulk status moves for the Work Items page (design-notes/visual-and-
// functional-tweaks-to-work-items-page.md, ADR-WI-4/ADR-WI-5).
//
// One code path for every multi-item move — board multi-drag and the
// filter bar's bulk "Move to…" menu. The server has no batch-status RPC,
// so the transport is a `Promise.all` of the existing per-item
// `updateWorkItem` mutation (the design's accepted fallback); every write
// still goes through the transactional outbox and partial failure is
// tolerated by the partial-success UX.
//
// Semantics: the move set is pre-validated with the same advisory gates
// the single-card path uses (system-managed statuses, blocked → Ready,
// kind restrictions). Valid items move; invalid items are skipped and
// reported in the result toast — one blocked item never dead-ends a
// 10-item drag.

import { useCallback, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";

import { useUpdateWorkItem, workItemKeys } from "@/api/workItems";
import {
  WorkItemStatus,
  type WorkItem,
} from "@/api/gen/orchicon/api/v1/work_item_pb";
import {
  MANUALLY_UNMOVABLE_STATUSES,
  allowedStatusesForKind,
  kindMeta,
  statusMeta,
} from "@/components/work-items/work-item-meta";
import type { BlockState } from "@/components/work-items/dependency-utils";
import { blockingTitles } from "@/components/work-items/dependency-utils";
import { useToast } from "@/components/ui/toast";

export type MoveValidation =
  | { ok: true }
  | { ok: false; code: "system-managed" | "blocked" | "kind"; reason: string };

/**
 * Advisory per-item gate for a status transition — the single source of
 * truth reused by the single-card path and the batch path. The server
 * stays authoritative; this only prevents obviously-wrong moves in the UI.
 */
export function validateMove(
  item: WorkItem,
  targetStatus: number,
  blockState: BlockState,
): MoveValidation {
  if (MANUALLY_UNMOVABLE_STATUSES.has(targetStatus)) {
    return {
      ok: false,
      code: "system-managed",
      reason: "is system-managed (only workflows set this status)",
    };
  }
  const blockers = blockState.blockedBy.get(item.id) ?? [];
  if (targetStatus === WorkItemStatus.READY && blockers.length > 0) {
    return {
      ok: false,
      code: "blocked",
      reason: `is blocked by ${blockingTitles(blockState.blockedBy, item.id)}`,
    };
  }
  if (!allowedStatusesForKind(item.kind).includes(targetStatus)) {
    return {
      ok: false,
      code: "kind",
      reason: `as a ${kindMeta(item.kind).label.toLowerCase()} only accepts ${allowedStatusesForKind(
        item.kind,
      )
        .map((s) => statusMeta(s).titleLabel)
        .join(", ")}`,
    };
  }
  return { ok: true };
}

export interface MoveSkip {
  id: string;
  title: string;
  reason: string;
}

/**
 * Split a move set into the items that can move and the items that must
 * be skipped (with the reason for the toast). Items already in the target
 * status are silently ignored — not an error, just a no-op.
 */
export function partitionMoveable(
  items: WorkItem[],
  targetStatus: number,
  blockState: BlockState,
): { moveable: WorkItem[]; skipped: MoveSkip[] } {
  const moveable: WorkItem[] = [];
  const skipped: MoveSkip[] = [];
  for (const item of items) {
    if (item.status === targetStatus) continue;
    const validation = validateMove(item, targetStatus, blockState);
    if (validation.ok) moveable.push(item);
    else skipped.push({ id: item.id, title: item.title, reason: validation.reason });
  }
  return { moveable, skipped };
}

/** Context a caller must supply alongside the ids: the full item map and
 *  the derived block state (both already computed by the board/page). */
export interface MoveContext {
  itemsById: Map<string, WorkItem>;
  blockState: BlockState;
}

/**
 * Moves a set of work items to a target status with the partial-success
 * semantics of ADR-WI-4. Optimistically updates the list cache for the
 * valid items, runs the per-item mutations, and reports one summary toast
 * ("Moved 3 to Ready · Skipped 2: …").
 */
export function useBatchMoveWorkItems(projectId: string) {
  const qc = useQueryClient();
  const toast = useToast();
  const updateStatus = useUpdateWorkItem(projectId);
  const [pendingCount, setPendingCount] = useState(0);

  const moveItems = useCallback(
    async (ids: string[], targetStatus: number, ctx: MoveContext) => {
      const items = ids
        .map((id) => ctx.itemsById.get(id))
        .filter((item): item is WorkItem => !!item);
      if (items.length === 0) return;

      const { moveable, skipped } = partitionMoveable(items, targetStatus, ctx.blockState);

      if (moveable.length === 0) {
        toast.error(
          skipped.length > 0
            ? `Nothing moved to ${statusMeta(targetStatus).titleLabel}: ${skipped
                .map((s) => `"${s.title}" ${s.reason}`)
                .join(", ")}`
            : `Already in ${statusMeta(targetStatus).titleLabel}.`,
          { title: "Nothing to move" },
        );
        return;
      }

      // Optimistic cache update: move the valid cards immediately so the
      // board doesn't flash them back to the origin column.
      //
      // `setQueriesData` (not `setQueryData`) with the trimmed prefix key:
      // the page's live list query uses the 4-element key ending in the
      // `{search,sortBy,sortOrder}` opts object, so a bare 3-element
      // `setQueryData` would create a phantom cache entry the board never
      // reads — cards stayed in the origin column until the 5s poll. The
      // prefix filter matches the live query (and any other list query
      // for the project), so the optimistic move actually renders.
      const listKey = workItemKeys.list(projectId);
      const movedIds = new Set(moveable.map((i) => i.id));
      qc.setQueriesData({ queryKey: listKey }, (old: WorkItem[] | undefined) => {
        if (!old) return old;
        return old.map((i) =>
          movedIds.has(i.id) ? { ...i, status: targetStatus } : i,
        );
      });

      setPendingCount((c) => c + 1);
      try {
        const results = await Promise.allSettled(
          moveable.map((item) =>
            updateStatus.mutateAsync({ id: item.id, status: targetStatus as WorkItemStatus }),
          ),
        );
        const failed = results.filter((r) => r.status === "rejected").length;
        const movedCount = moveable.length - failed;
        if (failed > 0) {
          // Server rejected some moves — refetch so the cache shows truth.
          qc.invalidateQueries({ queryKey: listKey });
        }
        const parts = [`Moved ${movedCount} to ${statusMeta(targetStatus).titleLabel}`];
        if (skipped.length > 0) {
          parts.push(
            `Skipped ${skipped.length}: ${skipped.map((s) => `"${s.title}" ${s.reason}`).join(", ")}`,
          );
        }
        if (failed > 0) parts.push(`${failed} failed`);
        toast.success(parts.join(" · "));
      } finally {
        setPendingCount((c) => Math.max(0, c - 1));
      }
    },
    [projectId, qc, toast, updateStatus],
  );

  return { moveItems, isPending: pendingCount > 0 };
}
