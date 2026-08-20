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
import type { Timestamp } from "@bufbuild/protobuf";
import {
  WorkItemStatus,
  type WorkItem,
} from "@/api/gen/orchicon/api/v1/work_item_pb";
import type { WorkflowRun } from "@/api/gen/orchicon/api/v1/workflow_pb";
import {
  sequenceParentIds,
  sortByChainOrder,
} from "@/components/work-items/sequence-utils";

function tsToMs(ts?: Timestamp): number {
  if (!ts) return 0;
  return Number(ts.seconds) * 1000;
}

/**
 * Effective fire time (ms) for an Upcoming-view item: recurring items use
 * next_run_at (the computed next occurrence), everything else uses
 * scheduled_start_at. Schedules.tsx sorts and groups upcoming items by
 * this value, so a recurring item's next occurrence time drives its
 * position in the agenda.
 */
export function upcomingSortTime(item: WorkItem): number {
  if (item.status === WorkItemStatus.RECURRING) {
    return tsToMs(item.nextRunAt);
  }
  return tsToMs(item.scheduledStartAt);
}

// Active statuses that count as "currently running" for the Running view:
// work items whose bound workflow run is in flight (RUNNING /
// CHECKPOINTING / RECOVERING). These are only ever set for tickets bound
// to an in-flight run. Disjoint from terminal statuses and from
// SCHEDULED. A sequence parent (children + no bound workflow) has no
// workflow_run_id, so the Running predicate is extended separately
// (isSequenceRunningParent).
export const ACTIVE_RUNNING_STATUSES = new Set([4, 5, 9]);

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
 * History membership for a workflow run: true iff the run actually
 * executed, i.e. it carries a real start time. This is the fix for missing
 * runs — every real execution records started_at (recurring fires whose
 * work item re-armed to SCHEDULED/RECURRING, prior runs of a work item
 * that ran more than once, and in-flight runs all carry it). A run that
 * was created but never started (started_at NULL — tests only in practice)
 * is excluded; it has no real run time to show or sort by.
 */
export function isHistoryRun(run: WorkflowRun): boolean {
  return run.startedAt !== undefined;
}

/**
 * Effective run time (ms) for a History-view run: its real started_at.
 * Falls back to created_at only for robustness; a run with neither
 * (started_at and created_at both absent) returns 0 so it sorts last.
 * This is the fix for ordering — a recurring/re-scheduled item's next
 * (future) scheduled firing time never displaces a run's actual start.
 */
export function historyRunRanAt(run: WorkflowRun): number {
  return tsToMs(run.startedAt) || tsToMs(run.createdAt) || 0;
}
