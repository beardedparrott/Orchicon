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
 * Compare two work items by TRUE chain order: `sort_order` rank within
 * (parent_id), then created_at. Items with a null sort_order (0 — not yet
 * reordered) sort LAST, mirroring the backend's
 * `ORDER BY sort_order NULLS LAST, created_at`. Never derived from display
 * order, so it is safe to use regardless of the active display sort.
 */
export function byChainOrder(a: WorkItem, b: WorkItem): number {
  const ao = a.sortOrder !== 0 ? a.sortOrder : Number.MAX_VALUE;
  const bo = b.sortOrder !== 0 ? b.sortOrder : Number.MAX_VALUE;
  if (ao !== bo) return ao - bo;
  return tsToMs(a.createdAt) - tsToMs(b.createdAt);
}

/**
 * Stable chain-order sort of an array of work items (see {@link byChainOrder}).
 * Returns a new array — never mutates the input.
 */
export function sortByChainOrder(items: WorkItem[]): WorkItem[] {
  return [...items].sort(byChainOrder);
}

/**
 * 1-based chain position of every SEQUENCE child, keyed by item id.
 * Derived from `sort_order` rank within (parent_id) — see
 * {@link byChainOrder}. The position is never derived from display order.
 * Every child of a sequence parent (any item with children — see
 * {@link sequenceParentIds}) gets a position; a parented card whose parent
 * is a leaf is not a sequence child and gets no badge.
 */
export function computeSequencePositions(items: WorkItem[]): Map<string, number> {
  const seqParents = sequenceParentIds(items);
  const byParent = new Map<string, WorkItem[]>();
  for (const item of items) {
    if (!item.parentId) continue;
    if (!seqParents.has(item.parentId)) continue; // not a sequence child
    const list = byParent.get(item.parentId);
    if (list) list.push(item);
    else byParent.set(item.parentId, [item]);
  }
  const positions = new Map<string, number>();
  for (const siblings of byParent.values()) {
    sortByChainOrder(siblings).forEach((item, idx) =>
      positions.set(item.id, idx + 1),
    );
  }
  return positions;
}

/**
 * Sequence-parent ids: every item that has children IS a sequence parent —
 * its children run one-after-another, each in its own bound workflow. The
 * parent's own `workflowId` is irrelevant: a parent-with-children is a
 * sequence container (the "has children" model), so even a parent that
 * carries a stale workflow binding gets a sequence badge/chip for its
 * children and is routed to the sequence engine at fire time. A leaf with
 * no children is never a sequence parent.
 */
export function sequenceParentIds(items: WorkItem[]): Set<string> {
  const hasChildren = new Set<string>();
  for (const item of items) {
    if (item.parentId) hasChildren.add(item.parentId);
  }
  return hasChildren;
}

function tsToMs(ts?: Timestamp): number {
  if (!ts) return 0;
  return Number(ts.seconds) * 1000;
}
