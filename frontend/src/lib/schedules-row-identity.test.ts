// schedules-row-identity.test.ts — the reported Schedules defects.
//
// REPORTED: "I still can not delete schedules out of the schedule section. It does nothing when you hit the
// delete button. It also seems to map multiple of the same item together (i.e. if I check a box on one that
// had more than one run, both are checked automatically instead of being recognized as two separate
// records)."
//
// Both symptoms had ONE cause: a history row is a RUN, but its checkbox was keyed by the WORK ITEM. Two runs
// of one item therefore shared a selection key (both checked), and "remove schedule" fired against an item
// whose schedule a past run had already consumed (nothing to clear — the button appeared broken).

import { describe, expect, it } from "vitest";
import { WorkItemStatus } from "@/api/gen/orchicon/api/v1/work_item_pb";
import { itemHasSchedule, runToItemIdMap } from "./schedules-model";

describe("runToItemIdMap", () => {
  it("maps each run to its work item, so two runs of one item stay distinct keys", () => {
    const m = runToItemIdMap([
      { id: "run-1", workItemId: "item-1" },
      { id: "run-2", workItemId: "item-1" },
    ]);
    expect(m.size).toBe(2);
    expect(m.get("run-1")).toBe("item-1");
    expect(m.get("run-2")).toBe("item-1");
    // The IDs DIFFER, which is the whole point: keying selection by item-1 is what made one checkbox
    // check every run of that item.
    expect([...m.keys()]).toEqual(["run-1", "run-2"]);
  });

  it("skips runs with no bound item rather than inventing a key", () => {
    const m = runToItemIdMap([{ id: "run-1" }, { id: "run-2", workItemId: "item-1" }]);
    expect(m.has("run-1")).toBe(false);
    expect(m.get("run-2")).toBe("item-1");
  });
});

describe("itemHasSchedule", () => {
  it("is false when a past run's item has no schedule left", () => {
    // The reported "delete does nothing": nothing to remove, so the control must not be offered.
    expect(itemHasSchedule({ status: WorkItemStatus.SUCCEEDED })).toBe(false);
    expect(itemHasSchedule(undefined)).toBe(false);
  });

  it("is true for a recurring item", () => {
    expect(itemHasSchedule({ recurringSchedule: {}, status: WorkItemStatus.RECURRING })).toBe(true);
  });

  it("is true for a one-shot scheduled item, by either clock", () => {
    expect(itemHasSchedule({ scheduledStartAt: { seconds: 1n }, status: WorkItemStatus.PENDING })).toBe(true);
    expect(itemHasSchedule({ nextRunAt: { seconds: 1n }, status: WorkItemStatus.PENDING })).toBe(true);
  });

  it("is true for a scheduled or recurrent status even with no schedule object", () => {
    expect(itemHasSchedule({ status: WorkItemStatus.SCHEDULED })).toBe(true);
    expect(itemHasSchedule({ status: WorkItemStatus.RECURRING })).toBe(true);
  });
});
