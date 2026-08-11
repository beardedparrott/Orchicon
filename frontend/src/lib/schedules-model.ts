// Pure schedule-view membership predicates (schedules.tsx). Extracted so
// the fix for queued sequence children / completed-workflow history is
// unit-testable without rendering the page. No business logic — every
// decision is derived from server state (AGENTS.md invariant #1).
//
// Sequence runs (a parent with children and no bound workflow) fan out to
// per-child workflows: the engine resets every descendant to pending on
// fire and arms one child at a time. Only the parent carries
// scheduled_start_at, so the children never match a SCHEDULED query; the
// predicates here recover them from the full project list.
import type { WorkItem } from "@/api/gen/orchicon/api/v1/work_item_pb";
import {
  sequenceParentIds,
  sortByChainOrder,
} from "@/components/work-items/sequence-utils";

// Active statuses that count as "currently running" for the Running view:
// work items whose bound workflow run is in flight (RUNNING /
// CHECKPOINTING / RECOVERING). These are only ever set for tickets bound
// to an in-flight run. Disjoint from terminal statuses and from
// SCHEDULED. A sequence parent (children + no bound workflow) has no
// workflow_run_id, so the Running predicate is extended separately
// (isSequenceRunningParent).
export const ACTIVE_RUNNING_STATUSES = new Set([4, 5, 9]);

// Terminal statuses that count as "previously ran" for History.
export const TERMINAL_STATUSES = new Set([6, 7, 8]);

// Statuses used by the queued-derivation helpers.
const PENDING = 1;

/**
 * Ids of the items that are currently acting as sequence containers:
 * running (no bound workflow run) parents with children. Their pending
 * children are the queued remainder of the chain.
 */
export function activeSequenceParentIds(items: WorkItem[]): Set<string> {
  const parents = sequenceParentIds(items);
  const active = new Set<string>();
  for (const item of items) {
    if (
      !item.workflowRunId &&
      ACTIVE_RUNNING_STATUSES.has(item.status) &&
      parents.has(item.id)
    ) {
      active.add(item.id);
    }
  }
  return active;
}

/**
 * Queued sequence children: pending children of an active sequence
 * parent, in chain order. These are the "remaining queued children" of a
 * sequential run — they have no scheduled_start_at of their own (only the
 * parent is scheduled), so they never match the SCHEDULED query and must
 * be derived from the full project list for the Upcoming view.
 */
export function queuedSequenceChildren(
  items: WorkItem[],
  kindFilter?: string,
): WorkItem[] {
  const activeParents = activeSequenceParentIds(items);
  const base = items.filter(
    (i) =>
      i.parentId &&
      activeParents.has(i.parentId) &&
      i.status === PENDING &&
      (!kindFilter || i.kind === Number(kindFilter)),
  );
  return sortByChainOrder(base);
}

/**
 * History membership: any item that previously ran a workflow. That means
 * a terminal status AND (had a scheduled start OR carried a workflow run
 * OR is a completed sequence parent). The workflowRunId clause is what
 * makes completed sequence children — and single workflow runs started
 * without a schedule — appear: they reach a terminal status with
 * workflow_run_id set but no scheduled_start_at, which the old
 * scheduledStartAt-only predicate dropped.
 */
export function isHistoryItem(
  item: WorkItem,
  allItems: WorkItem[],
): boolean {
  if (!TERMINAL_STATUSES.has(item.status)) return false;
  if (item.scheduledStartAt) return true;
  if (item.workflowRunId) return true;
  // A terminal parent with children and no bound run completed its whole
  // sequence chain (or was cancelled) — it belongs in history too.
  return sequenceParentIds(allItems).has(item.id);
}
