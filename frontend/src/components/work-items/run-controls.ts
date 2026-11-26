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

/**
 * START AND RESTART ARE THE SAME CALL AND DIFFERENT WORDS, DELIBERATELY.
 *
 * `SEQUENCE_ACTION_RESTART` fires the bound workflow. When the item has NEVER RUN there is nothing
 * to restart and nothing to discard, so calling it "Restart" is simply false — and it is the same
 * mislabelling, in reverse, that made this confusing in the first place (`Start` used to mean
 * restart). The label follows what is actually true of THIS item:
 *
 *   never dispatched  → "Start"     nothing exists to lose
 *   has a run or ran  → "Restart"   progress, history and attempt counts are discarded
 *
 * `hasRun` comes from the item itself: a bound run id, or any status that implies it was dispatched
 * (a pending item that has never fired is PENDING with no run id).
 */
export function restartLabel(hasRun: boolean): string {
  return hasRun ? "Restart" : "Start";
}

export function restartTitle(hasRun: boolean): string {
  return hasRun
    ? "Discard this run and start a new one from the first step. Progress, step history and attempt counts are lost — this cannot be undone."
    : "Fire this item's bound workflow now. Nothing has run yet, so nothing is discarded.";
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
  hasRun = true,
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

/**
 * bulkControlsFor answers the question a SELECTION asks, which is not the question a single item
 * asks. A bulk bar must never offer a gesture that only SOME of the selection can perform — the
 * operator selected them together precisely to act on them together — so each gesture is either
 * available for all of them or unavailable, with the eligible count so a partial selection can be
 * explained rather than silently ignored.
 */
export interface BulkControlAvailability {
  /** Every selected item is mid-flight, so every one can be paused. */
  canPause: boolean;
  /** Every selected item has finished-with-failure work worth continuing. */
  canResume: boolean;
  /** No selected item is mid-flight, so all can be (re)started without colliding. */
  canFire: boolean;
  /** How many items each gesture would actually affect. */
  eligible: { pause: number; resume: number; fire: number };
  total: number;
}

export function bulkControlsFor(statuses: WorkItemStatus[]): BulkControlAvailability {
  const total = statuses.length;
  let pause = 0;
  let resume = 0;
  let fire = 0;
  for (const status of statuses) {
    if (isInFlight(status)) pause++;
    if (hasResumableWork(status)) resume++;
    if (!isInFlight(status)) fire++;
  }
  return {
    canPause: total > 0 && pause === total,
    canResume: total > 0 && resume === total,
    canFire: total > 0 && fire === total,
    eligible: { pause, resume, fire },
    total,
  };
}
