// Bulk "Run" (kick off) for the Work Items page (ADR-WI-9).
//
// One code path for starting a selected set of work items at once. The
// transport reuses the existing per-item `updateWorkItem` mutation with
// `auto_start_workflow: true` — the exact same path the single-item
// "Scheduled start" card uses. There is no batch-start RPC, so this is a
// `Promise.allSettled` of per-item updates, mirroring `useBatchMoveWorkItems`.
//
// The classification (which selected items can actually be started) lives
// in `partitionRunable`; the hook runs it, dispatches the runnable subset,
// and reports one summary toast ("Started X · Skipped Y: …"). Items that
// cannot start are skipped with a per-item reason — one blocked item never
// dead-ends the whole batch.
//
// No optimistic status write: the READY→RUNNING / →SCHEDULED transition is
// async (post-commit arming), so flipping the cache client-side would flash.
// `useUpdateWorkItem`'s invalidation + the page's 5s poll reflect the truth
// (ADR-WI-9 point 7).

import { useCallback, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";

import { useUpdateWorkItem, workItemKeys } from "@/api/workItems";
import { useToast } from "@/components/ui/toast";
import { isTerminal, type WorkItem } from "@/components/work-items/work-item-meta";
import { WorkItemKind, WorkItemStatus } from "@/api/gen/orchicon/api/v1/work_item_pb";

/** Per-item verdict returned by `partitionRunable`. */
export type RunSkip =
  | { ok: true }
  | {
      ok: false;
      code: "terminal" | "in-flight" | "parent" | "no-workflow";
      reason: string;
    };

/**
 * Statuses where an item cannot be started: already executing, scheduled,
 * or blocked by unfinished dependencies. SCHEDULED is added to the
 * system-managed (manually-unmovable) set so a re-run never double-dispatches
 * an on-deck item. SKIPPED is also terminal, so the terminal check runs first.
 */
const IN_FLIGHT_STATUSES = new Set<number>([
  WorkItemStatus.RUNNING,
  WorkItemStatus.CHECKPOINTING,
  WorkItemStatus.RECOVERING,
  WorkItemStatus.SCHEDULED,
  WorkItemStatus.BLOCKED,
]);

/**
 * Parent kinds that are not schedulable (ADR-WIT-2): an epic/feature is a
 * container, not something a run can kick off.
 */
const PARENT_KINDS = new Set<number>([
  WorkItemKind.EPIC,
  WorkItemKind.FEATURE,
]);

/** Regular schedulable kinds per `isSchedulableKind`: task + subtask. */
function isRegularSchedulableKind(kind: number): boolean {
  return kind === WorkItemKind.TASK || kind === WorkItemKind.SUBTASK;
}

/**
 * Classify a single candidate for the run set. See the matrix in
 * `architecture-notes/bulk-run-button.md` §3:
 *
 * | status | terminal | in-flight | verdict |
 * | kind | parent kind | task/subtask w/ children | task/subtask w/o workflow |
 * | otherwise (workflow-bound leaf) | run |
 */
export function classifyRun(
  item: WorkItem,
  parentIdSet: Set<string>,
): RunSkip {
  if (isTerminal(item.status)) {
    return { ok: false, code: "terminal", reason: "already finished" };
  }
  if (IN_FLIGHT_STATUSES.has(item.status)) {
    return {
      ok: false,
      code: "in-flight",
      reason:
        item.status === WorkItemStatus.BLOCKED
          ? "is blocked by unfinished dependencies"
          : "already running or scheduled",
    };
  }
  if (PARENT_KINDS.has(item.kind)) {
    return { ok: false, code: "parent", reason: "is a parent kind — nothing to run" };
  }
  // task / subtask / recovery kinds only reach here.
  const isParent = parentIdSet.has(item.id);
  // A task/subtask sequence parent (has children) runs its children in
  // chain order; a recovery kind is leaf-only for the MVP, so it still
  // needs a bound workflow.
  if (isParent && isRegularSchedulableKind(item.kind)) {
    return { ok: true };
  }
  if (item.workflowId) {
    return { ok: true };
  }
  return {
    ok: false,
    code: "no-workflow",
    reason: "has no workflow bound — open it to bind one",
  };
}

/**
 * Split a run set into the items that can be kicked off and the items that
 * must be skipped (with a reason for the toast). `parentIdSet` is the set of
 * ids that have at least one direct child in the project — used to detect
 * sequence parents without a workflow binding.
 */
export function partitionRunable(
  items: WorkItem[],
  parentIdSet: Set<string>,
): { runable: WorkItem[]; skipped: { id: string; title: string; reason: string }[] } {
  const runable: WorkItem[] = [];
  const skipped: { id: string; title: string; reason: string }[] = [];
  for (const item of items) {
    const verdict = classifyRun(item, parentIdSet);
    if (verdict.ok) {
      runable.push(item);
    } else {
      skipped.push({ id: item.id, title: item.title, reason: verdict.reason });
    }
  }
  return { runable, skipped };
}

/** Context a caller must supply alongside the ids. */
export interface RunContext {
  /** Full item map (page already derives this). */
  itemsById: Map<string, WorkItem>;
  /** Ids that have at least one direct child in the project. */
  parentIdSet: Set<string>;
}

/**
 * Kicks off a set of work items with the partial-success semantics of
 * ADR-WI-9. Partitions the (already visible-intersected) ids, dispatches the
 * runnable subset via `Promise.allSettled`, and reports one summary toast
 * ("Started X · Skipped Y: …"). Server-rejected items trigger a refetch so
 * the cache shows truth.
 *
 * No optimistic status write — the transition is async post-commit (see file
 * header); the page's invalidation + 5s poll reflect the change.
 */
export function useBatchRunWorkItems(projectId: string) {
  const qc = useQueryClient();
  const toast = useToast();
  const updateWorkItem = useUpdateWorkItem(projectId);
  const [pendingCount, setPendingCount] = useState(0);

  const runSelected = useCallback(
    async (ids: string[], ctx: RunContext) => {
      const items = ids
        .map((id) => ctx.itemsById.get(id))
        .filter((item): item is WorkItem => !!item);
      if (items.length === 0) return;

      const { runable, skipped } = partitionRunable(items, ctx.parentIdSet);

      if (runable.length === 0) {
        toast.error(
          skipped.length > 0
            ? `Nothing started: ${skipped
                .map((s) => `"${s.title}" ${s.reason}`)
                .join(", ")}`
            : "Nothing to start.",
          { title: "Nothing to start" },
        );
        return;
      }

      setPendingCount((c) => c + 1);
      try {
        const results = await Promise.allSettled(
          runable.map((item) =>
            updateWorkItem.mutateAsync({ id: item.id, autoStartWorkflow: true }),
          ),
        );
        const failed = results.filter((r) => r.status === "rejected").length;
        if (failed > 0) {
          qc.invalidateQueries({ queryKey: workItemKeys.list(projectId) });
        }
        const startedCount = runable.length - failed;
        const parts = [`Started ${startedCount}`];
        if (skipped.length > 0) {
          parts.push(
            `Skipped ${skipped.length}: ${skipped
              .map((s) => `"${s.title}" ${s.reason}`)
              .join(", ")}`,
          );
        }
        if (failed > 0) parts.push(`${failed} failed`);
        toast.success(parts.join(" · "));
      } finally {
        setPendingCount((c) => Math.max(0, c - 1));
      }
    },
    [projectId, qc, toast, updateWorkItem],
  );

  return { runSelected, isPending: pendingCount > 0 };
}
