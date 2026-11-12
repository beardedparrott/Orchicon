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

/**
 * itemHasSchedule reports whether a work item still carries a schedule to remove.
 *
 * A Schedules history row is a PAST RUN, and its item has usually already consumed its schedule — so a
 * "remove schedule" control there cleared nothing and the row stayed put, which the operator reported as
 * "the delete button does nothing". Offering an action that cannot change anything is the defect; hiding it
 * makes the absence visible.
 */
export function itemHasSchedule(item?: {
  recurringSchedule?: unknown;
  nextRunAt?: unknown;
  scheduledStartAt?: unknown;
  status?: number;
}): boolean {
  if (!item) return false;
  if (item.recurringSchedule) return true; // present (even empty) means the recurrence field is set
  if (item.nextRunAt) return true;
  if (item.scheduledStartAt) return true;
  return (
    item.status === WorkItemStatus.SCHEDULED || item.status === WorkItemStatus.RECURRING
  );
}

/**
 * runToItemIdMap maps each RUN id to the work item it belongs to.
 *
 * HISTORY ROWS ARE PER RUN and their selection keys are RUN ids, so a bulk action can arrive carrying run
 * ids rather than work item ids. This is the translation, kept pure so the rule can be tested without
 * rendering the route.
 */
export function runToItemIdMap(
  runs: ReadonlyArray<{ id?: string; workItemId?: string }>,
): Map<string, string> {
  const m = new Map<string, string>();
  for (const r of runs) {
    if (r.id && r.workItemId) m.set(r.id, r.workItemId);
  }
  return m;
}

/**
 * classifyScheduleRemoval splits the Schedules page's selection into what each row ACTUALLY is.
 *
 * THE DEFECT THIS ENCODES. The page had one action, "remove schedule", applied to every row — but an
 * upcoming/running row is a WORK ITEM and a history row is a WORKFLOW RUN. Clearing a schedule on a past run
 * is a no-op (it was consumed when the run fired), so the click changed nothing, the row stayed, and it read
 * as a broken button. A run row can only disappear when the RUN is deleted.
 *
 * `stillScheduledItemIds` is the second half of that: deleting a run whose item is STILL recurring would let
 * it fire again and recreate the row, which is indistinguishable from "delete didn't work". Those have their
 * schedule cleared in the same action.
 */
export function classifyScheduleRemoval(
  keys: readonly string[],
  runToItem: ReadonlyMap<string, string>,
  itemsById: ReadonlyMap<string, ScheduleBearingItem>,
): { runIds: string[]; itemIds: string[]; stillScheduledItemIds: string[] } {
  const runIds: string[] = [];
  const itemIds: string[] = [];
  const stillScheduledItemIds: string[] = [];
  const seenScheduled = new Set<string>();

  for (const key of keys) {
    const itemId = runToItem.get(key);
    if (itemId) {
      runIds.push(key);
      if (itemHasSchedule(itemsById.get(itemId)) && !seenScheduled.has(itemId)) {
        seenScheduled.add(itemId);
        stillScheduledItemIds.push(itemId);
      }
      continue;
    }
    itemIds.push(key);
  }
  return { runIds, itemIds, stillScheduledItemIds };
}

/** The slice of a work item the removal decision needs. */
export interface ScheduleBearingItem {
  recurringSchedule?: unknown;
  nextRunAt?: unknown;
  scheduledStartAt?: unknown;
  status?: number;
}
