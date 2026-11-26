/**
 * Run-control vocabulary for the work item surfaces.
 *
 * THREE GESTURES, AND THE DIFFERENCE IS WHAT THEY DO TO YOUR WORK:
 *
 *   Pause    — stop the run at a resumable point. Keeps the worktree, the branch and every
 *              succeeded step. Nothing is lost, nothing is replayed.
 *   Resume   — continue the run that already exists. Succeeded steps stay succeeded; only the
 *              failed ones are re-armed, and the previous attempt's summary travels with them.
 *   Restart  — throw the run away and fire a new one from the first step. DESTRUCTIVE, and it
 *              says so, because the old Start button did exactly this while being labelled
 *              something gentler.
 *
 * The labels are load-bearing. `Start` used to mean restart, and `Resume` used to mean Start, so
 * the destructive gesture was the one wearing the reassuring name — and the non-destructive one
 * did nothing at all.
 */
import { SequenceAction } from "@/api/gen/orchicon/api/v1/work_item_service_pb";
import { WorkItemStatus } from "@/api/gen/orchicon/api/v1/work_item_pb";

const ACTIVE = new Set([
  WorkItemStatus.RUNNING,
  WorkItemStatus.CHECKPOINTING,
  WorkItemStatus.RECOVERING,
]);

const QUEUED = new Set([WorkItemStatus.READY, WorkItemStatus.ASSIGNED]);

/** A run is in flight: pausing is meaningful, and restarting would collide with it. */
export const isInFlight = (status: WorkItemStatus) =>
  ACTIVE.has(status) || QUEUED.has(status);

/** There is finished-with-a-failure work worth continuing. */
export const hasResumableWork = (status: WorkItemStatus) =>
  status === WorkItemStatus.FAILED || status === WorkItemStatus.CANCELLED;

export interface RunControlMeta {
  label: string;
  /** Shown on hover. Says what happens to the WORK, not to a database row. */
  title: string;
  /** True when the gesture discards progress — the UI confirms these. */
  destructive: boolean;
}

export const RUN_CONTROL_META: Record<
  SequenceAction.PAUSE | SequenceAction.RESUME | SequenceAction.RESTART,
  RunControlMeta
> = {
  [SequenceAction.PAUSE]: {
    label: "Pause",
    title:
      "Stop this run at a resumable point. The worktree, branch and every succeeded step are kept, so Resume continues where it stopped.",
    destructive: false,
  },
  [SequenceAction.RESUME]: {
    label: "Resume",
    title:
      "Continue the run that already exists: succeeded steps stay succeeded and only the failed ones re-run, carrying the previous attempt's notes so nothing is re-derived.",
    destructive: false,
  },
  [SequenceAction.RESTART]: {
    label: "Restart",
    title:
      "Discard this run and start a new one from the first step. Progress, step history and attempt counts are lost — this cannot be undone.",
    destructive: true,
  },
};

/**
 * runControlsFor decides which of the three a surface should offer, from the item's status and
 * whether it is a sequence parent.
 *
 * Kept in one place so the work item page, the run page and the schedules page cannot drift into
 * offering a gesture that means something different depending on where you clicked it.
 */
export function runControlsFor(
  status: WorkItemStatus,
  hasChildren: boolean,
): SequenceAction[] {
  const out: SequenceAction[] = [];

  // Pause only makes sense while something is actually running.
  if (isInFlight(status)) {
    out.push(SequenceAction.PAUSE);
  }

  // Resume needs finished-with-failure work to continue. CANCELLED counts: the work item is
  // cancelled by abort, and abort no longer means "the branch is gone".
  if (hasResumableWork(status)) {
    out.push(SequenceAction.RESUME);
  }

  // Restart is available whenever nothing is in flight — it is how you start fresh, and also how
  // you recover when Resume has nothing to resume.
  if (!isInFlight(status)) {
    out.push(SequenceAction.RESTART);
  }

  // A parent keeps the chain-parking gesture; a leaf gets Pause/Resume/Restart instead. STOP on a
  // leaf aborts its run, which is exactly the destructive behaviour Pause exists to replace.
  if (hasChildren && status !== WorkItemStatus.SUCCEEDED) {
    out.push(SequenceAction.STOP);
  }

  return out;
}
