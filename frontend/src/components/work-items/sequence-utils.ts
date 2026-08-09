// Pure sequence-chain presentation helpers (architecture-notes/
// sequential-multi-workflow-runs.md §1, §4).
//
// A parent work item with children and no bound workflow IS a sequence
// run; its children run one-after-another in sort_order. This module
// derives the chain order (position badges) from sort_order so the real
// chain order is never ambiguous — even when the board/tree display sort
// reorders the cards. No business logic, just derived server state
// (AGENTS.md invariant #1).

import type { WorkItem } from "@/api/gen/orchicon/api/v1/work_item_pb";
import type { Timestamp } from "@bufbuild/protobuf";

/**
 * 1-based chain position of every item that has a parent, keyed by item id.
 * Derived from `sort_order` rank within (parent_id); items with a null
 * sort_order (not yet reordered) fall back to created_at and sort LAST,
 * mirroring the backend's `ORDER BY sort_order NULLS LAST, created_at`.
 * The position is never derived from display order.
 */
export function computeSequencePositions(items: WorkItem[]): Map<string, number> {
  const byParent = new Map<string, WorkItem[]>();
  for (const item of items) {
    if (!item.parentId) continue;
    const list = byParent.get(item.parentId);
    if (list) list.push(item);
    else byParent.set(item.parentId, [item]);
  }
  const positions = new Map<string, number>();
  for (const siblings of byParent.values()) {
    const ordered = [...siblings].sort((a, b) => {
      const ao = a.sortOrder !== 0 ? a.sortOrder : Number.MAX_VALUE;
      const bo = b.sortOrder !== 0 ? b.sortOrder : Number.MAX_VALUE;
      if (ao !== bo) return ao - bo;
      return tsToMs(a.createdAt) - tsToMs(b.createdAt);
    });
    ordered.forEach((item, idx) => positions.set(item.id, idx + 1));
  }
  return positions;
}

/**
 * Sequence-parent ids: every item that is someone's parent. An item is a
 * sequence parent when it has children and no bound workflow; the Schedules
 * Running membership predicate keys off this derived set.
 */
export function sequenceParentIds(items: WorkItem[]): Set<string> {
  const ids = new Set<string>();
  for (const item of items) {
    if (item.parentId) ids.add(item.parentId);
  }
  return ids;
}

function tsToMs(ts?: Timestamp): number {
  if (!ts) return 0;
  return Number(ts.seconds) * 1000;
}
