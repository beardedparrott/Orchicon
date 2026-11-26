import { describe, expect, it } from "vitest";

import { SequenceAction } from "@/api/gen/orchicon/api/v1/work_item_service_pb";
import { WorkItemStatus } from "@/api/gen/orchicon/api/v1/work_item_pb";
import { RUN_CONTROL_META, runControlsFor } from "./run-controls";

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
