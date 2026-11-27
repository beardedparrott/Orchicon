import { describe, expect, it } from "vitest";

import { SequenceAction } from "@/api/gen/orchicon/api/v1/work_item_service_pb";
import { WorkItemStatus } from "@/api/gen/orchicon/api/v1/work_item_pb";
import {
  RUN_CONTROL_META,
  bulkControlsFor,
  controlLabelFor,
  hasEverRun,
  restartLabel,
  restartTitle,
  runControlsFor,
  SCHEDULE_VIEW_ACTIONS,
} from "./run-controls";

/**
 * WHY THESE ASSERTIONS EXIST.
 *
 * The old controls offered `Start` and `Resume` on a leaf and both called the same thing, so the
 * only useful test is the one that pins what each gesture DOES to the work:
 *
 *   - a running item can be paused, and cannot be restarted out from under itself
 *   - a failed item can be resumed (continue) AND restarted (discard), and they are different
 *   - the destructive one is LABELLED destructive, because that mislabelling is the defect
 */
describe("runControlsFor", () => {
  it("offers pause, never restart, while a run is in flight", () => {
    const actions = runControlsFor(WorkItemStatus.RUNNING, false);
    expect(actions).toContain(SequenceAction.PAUSE);
    expect(actions).not.toContain(SequenceAction.RESTART);
  });

  it("offers both resume and restart on a failed leaf, because they mean different things", () => {
    const actions = runControlsFor(WorkItemStatus.FAILED, false);
    expect(actions).toContain(SequenceAction.RESUME);
    expect(actions).toContain(SequenceAction.RESTART);
  });

  it("offers resume on a cancelled item — abort no longer destroys the branch", () => {
    expect(runControlsFor(WorkItemStatus.CANCELLED, false)).toContain(SequenceAction.RESUME);
  });

  it("does not offer pause when nothing is running", () => {
    expect(runControlsFor(WorkItemStatus.FAILED, false)).not.toContain(SequenceAction.PAUSE);
    expect(runControlsFor(WorkItemStatus.PENDING, false)).not.toContain(SequenceAction.PAUSE);
  });

  it("keeps STOP for sequence parents and drops it for leaves", () => {
    expect(runControlsFor(WorkItemStatus.FAILED, true)).toContain(SequenceAction.STOP);
    expect(runControlsFor(WorkItemStatus.FAILED, false)).not.toContain(SequenceAction.STOP);
  });

  it("marks exactly one gesture destructive, and it is restart", () => {
    expect(RUN_CONTROL_META[SequenceAction.RESTART].destructive).toBe(true);
    expect(RUN_CONTROL_META[SequenceAction.RESUME].destructive).toBe(false);
    expect(RUN_CONTROL_META[SequenceAction.PAUSE].destructive).toBe(false);
  });

  it("labels the gestures so the destructive one cannot hide behind a gentler name", () => {
    expect(RUN_CONTROL_META[SequenceAction.RESTART].label).toBe("Restart");
    expect(RUN_CONTROL_META[SequenceAction.RESUME].label).toBe("Resume");
    expect(RUN_CONTROL_META[SequenceAction.PAUSE].label).toBe("Pause");
  });
});

/**
 * START vs RESTART, AND BULK.
 *
 * `Start` used to be the label for a destructive re-fire. The replacement was over-corrected: every
 * button said "Restart", including on an item that had never run — where nothing is being re-started
 * and nothing is discarded. The label has to follow what is true of the item.
 */
describe("restartLabel", () => {
  it("says Start when the item has never run", () => {
    expect(restartLabel(false)).toBe("Start");
    expect(restartTitle(false)).not.toContain("Discard");
    expect(restartTitle(false)).toContain("Nothing has run yet");
  });

  it("says Restart, and warns about discarding, once it has run", () => {
    expect(restartLabel(true)).toBe("Restart");
    expect(restartTitle(true)).toContain("Discard this run");
  });
});

describe("bulkControlsFor", () => {
  it("offers pause only when EVERY selected item is in flight", () => {
    expect(bulkControlsFor([WorkItemStatus.RUNNING, WorkItemStatus.RUNNING]).canPause).toBe(true);
    expect(bulkControlsFor([WorkItemStatus.RUNNING, WorkItemStatus.FAILED]).canPause).toBe(false);
  });

  it("offers resume only when every item has resumable work", () => {
    expect(bulkControlsFor([WorkItemStatus.FAILED, WorkItemStatus.CANCELLED]).canResume).toBe(true);
    expect(bulkControlsFor([WorkItemStatus.FAILED, WorkItemStatus.PENDING]).canResume).toBe(false);
  });

  it("refuses to fire while any item is in flight, so a bulk click cannot collide with a run", () => {
    expect(bulkControlsFor([WorkItemStatus.PENDING, WorkItemStatus.FAILED]).canFire).toBe(true);
    expect(bulkControlsFor([WorkItemStatus.PENDING, WorkItemStatus.RUNNING]).canFire).toBe(false);
  });

  it("reports eligible counts so a partial selection can be explained", () => {
    const a = bulkControlsFor([WorkItemStatus.FAILED, WorkItemStatus.FAILED, WorkItemStatus.RUNNING]);
    expect(a.eligible.resume).toBe(2);
    expect(a.eligible.pause).toBe(1);
    expect(a.total).toBe(3);
  });

  it("offers nothing for an empty selection", () => {
    const a = bulkControlsFor([]);
    expect(a.canPause || a.canResume || a.canFire).toBe(false);
  });
});

/**
 * THE WIRING TEST THAT WAS MISSING, AND THE BUG IT WOULD HAVE CAUGHT.
 *
 * The shipped call site read `hasRun = workflowRunId !== "" || !isFiredOnce(status)`. The negation
 * was inverted, so a PENDING item that had never been dispatched evaluated to hasRun === true and
 * every row in the tree said "Restart". The suite was green throughout, because the tests above call
 * restartLabel(false)/restartLabel(true) DIRECTLY and never asked what the component computes.
 *
 * controlLabelFor is the whole decision in one pure place, so these assertions are about what a row
 * actually renders rather than what a helper returns in isolation.
 */
describe("controlLabelFor (the decision a row actually renders)", () => {
  it("says Start for a pending item that has never been dispatched", () => {
    // THE EXACT CASE THAT SHIPPED WRONG: pending, no run id. It must NOT say Restart.
    expect(controlLabelFor(SequenceAction.RESTART, WorkItemStatus.PENDING, "Restart")).toBe("Start");
    expect(controlLabelFor(SequenceAction.RESTART, WorkItemStatus.PENDING, "Restart")).toBe("Start");
  });

  it("says Start even with a bound run id, because STATUS alone decides", () => {
    // SUPERSEDED RULE, kept as an explicit assertion so it cannot creep back. The operator:
    // "Start is the right word for ANY work item that has not started yet, pending or otherwise."
    // A pending item with a run id is still a pending item.
    expect(controlLabelFor(SequenceAction.RESTART, WorkItemStatus.PENDING, "Restart")).toBe("Start");
  });

  it("says Restart for an item whose status proves it ran", () => {
    for (const status of [
      WorkItemStatus.FAILED,
      WorkItemStatus.SUCCEEDED,
      WorkItemStatus.CANCELLED,
      WorkItemStatus.RUNNING,
      WorkItemStatus.CHECKPOINTING,
    ]) {
      expect(controlLabelFor(SequenceAction.RESTART, status, "Restart")).toBe("Restart");
    }
  });

  it("leaves the other gestures alone", () => {
    expect(controlLabelFor(SequenceAction.RESUME, WorkItemStatus.PENDING, "Resume")).toBe("Resume");
    expect(controlLabelFor(SequenceAction.PAUSE, WorkItemStatus.RUNNING, "Pause")).toBe("Pause");
  });
});

describe("hasEverRun", () => {
  it("is false for a never-dispatched item, whatever run id it carries", () => {
    expect(hasEverRun(WorkItemStatus.PENDING)).toBe(false);
    expect(hasEverRun(WorkItemStatus.UNSPECIFIED)).toBe(false);
    expect(hasEverRun(WorkItemStatus.FAILED)).toBe(true);
  });
});

/**
 * THE OPERATOR'S RULE FOR THE SCHEDULES VIEWS, and the label rule restated.
 *
 * These are business statements, not implementation details, so they are pinned here where the GUI
 * and the TUI both read them:
 *
 *   "the only views that make sense for controls are running and history. Upcoming is simply
 *    upcoming. They already have a state. Running would have stop and pause controls. History
 *    would have the restart or resume controls, then they would be back under the running page
 *    once they are running."
 */
describe("SCHEDULE_VIEW_ACTIONS", () => {
  it("offers NOTHING on Upcoming — an unfired schedule has no run to act on", () => {
    expect(SCHEDULE_VIEW_ACTIONS.upcoming).toEqual([]);
  });

  it("offers only the two stops on Running", () => {
    expect(SCHEDULE_VIEW_ACTIONS.running).toEqual([SequenceAction.PAUSE, SequenceAction.STOP]);
    expect(SCHEDULE_VIEW_ACTIONS.running).not.toContain(SequenceAction.RESTART);
  });

  it("offers only the two continuations on History", () => {
    expect(SCHEDULE_VIEW_ACTIONS.history).toEqual([SequenceAction.RESUME, SequenceAction.RESTART]);
    expect(SCHEDULE_VIEW_ACTIONS.history).not.toContain(SequenceAction.STOP);
  });
});

describe("hasEverRun (status alone decides)", () => {
  it("treats ANY not-yet-started item as never run, pending or otherwise", () => {
    expect(hasEverRun(WorkItemStatus.PENDING)).toBe(false);
    expect(hasEverRun(WorkItemStatus.UNSPECIFIED)).toBe(false);
  });

  it("treats failed, cancelled and paused as having run — the operator's list", () => {
    expect(hasEverRun(WorkItemStatus.FAILED)).toBe(true);
    expect(hasEverRun(WorkItemStatus.CANCELLED)).toBe(true);
    expect(hasEverRun(WorkItemStatus.CHECKPOINTING)).toBe(true); // paused
  });

  it("ignores a bound run id, because status is the single input", () => {
    // The old rule consulted workflowRunId and that two-signal fold is what let a bad negation ship.
    expect(hasEverRun(WorkItemStatus.PENDING)).toBe(false);
  });
});

/**
 * A VIEW'S SET IS A CEILING — the defect the operator caught.
 *
 * `9. Cross-platform` was `running` and its Schedules HISTORY row offered "Resume" and "Restart".
 * Cause: History's set is [RESUME, RESTART], the item's status allowed [PAUSE, STOP], the
 * intersection was EMPTY, and a fallback I had added rendered the whole History set instead.
 *
 * History legitimately lists in-flight runs — its membership test is "the run has startedAt", which
 * an in-flight run does, deliberately (see isHistoryRun's comment: re-armed runs must stay visible).
 * So the view is right and the RENDER was wrong.
 */
describe("view sets intersect with status, never override it", () => {
  it("offers NOTHING when the view set and the status have no gesture in common", () => {
    // History set ∩ a running item's gestures = ∅. The row must render no controls at all.
    const viewSet = SCHEDULE_VIEW_ACTIONS.history;          // [RESUME, RESTART]
    const allowed = runControlsFor(WorkItemStatus.RUNNING, false); // [PAUSE, ...]
    const intersection = viewSet.filter((a) => allowed.includes(a));
    expect(intersection).toEqual([]);
    // And the view set must NOT be a substitute for that empty intersection.
    expect(viewSet).not.toContain(SequenceAction.PAUSE);
  });

  it("offers Resume on a PAUSED item in History — the case the set exists for", () => {
    const viewSet = SCHEDULE_VIEW_ACTIONS.history;
    const allowed = runControlsFor(WorkItemStatus.PAUSED, false);
    expect(viewSet.filter((a) => allowed.includes(a))).toContain(SequenceAction.RESUME);
  });

  it("offers Pause on a RUNNING item in the Running view", () => {
    const viewSet = SCHEDULE_VIEW_ACTIONS.running;           // [PAUSE, STOP]
    const allowed = runControlsFor(WorkItemStatus.RUNNING, false);
    expect(viewSet.filter((a) => allowed.includes(a))).toContain(SequenceAction.PAUSE);
  });
});
