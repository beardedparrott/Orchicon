// schedules-removal.test.ts — WHAT "DELETE" DOES, per row kind.
//
// REPORTED three times: "I still can not delete schedules out of the schedule section. It does nothing when you
// hit the delete button."
//
// The page had ONE action for every row, but the rows are different things: upcoming/running rows are WORK
// ITEMS (clearing the schedule removes them), and history rows are WORKFLOW RUNS — where clearing a schedule
// is a no-op, because the run already consumed it. The run row can only disappear when the RUN is deleted.

import { describe, expect, it } from "vitest";
import { WorkItemStatus } from "@/api/gen/orchicon/api/v1/work_item_pb";
import { classifyScheduleRemoval, type ScheduleBearingItem } from "./schedules-model";

const runs = new Map<string, string>([
  ["run-1", "item-1"], // a past run of a still-recurring item
  ["run-2", "item-2"], // a past run of a finished item
]);
const items = new Map<string, ScheduleBearingItem>([
  ["item-1", { recurringSchedule: {}, status: WorkItemStatus.RECURRING }],
  ["item-2", { status: WorkItemStatus.SUCCEEDED }],
]);

describe("classifyScheduleRemoval", () => {
  it("treats a history row as a RUN, so it is deleted rather than schedule-cleared", () => {
    const got = classifyScheduleRemoval(["run-2"], runs, items);
    expect(got.runIds).toEqual(["run-2"]);
    expect(got.itemIds).toEqual([]);
    // item-2 has no schedule left — and that is exactly why the old code's click changed nothing.
    expect(got.stillScheduledItemIds).toEqual([]);
  });

  it("also clears the recurrence when the run's item is STILL scheduled, so the row cannot come back", () => {
    const got = classifyScheduleRemoval(["run-1"], runs, items);
    expect(got.runIds).toEqual(["run-1"]);
    expect(got.stillScheduledItemIds).toEqual(["item-1"]);
  });

  it("treats an upcoming/running row as an ITEM, for schedule clearing", () => {
    const got = classifyScheduleRemoval(["item-1"], runs, items);
    expect(got.itemIds).toEqual(["item-1"]);
    expect(got.runIds).toEqual([]);
  });

  it("keeps two runs of one item as two distinct deletions", () => {
    const two = new Map(runs);
    two.set("run-3", "item-1");
    const got = classifyScheduleRemoval(["run-1", "run-3"], two, items);
    expect(got.runIds).toEqual(["run-1", "run-3"]);
    // ...while the recurrence is cleared ONCE, because it belongs to the item.
    expect(got.stillScheduledItemIds).toEqual(["item-1"]);
  });

  it("handles a mixed selection", () => {
    const got = classifyScheduleRemoval(["run-2", "item-1"], runs, items);
    expect(got.runIds).toEqual(["run-2"]);
    expect(got.itemIds).toEqual(["item-1"]);
  });

  it("is total over an unknown key — it is treated as an item rather than dropped", () => {
    const got = classifyScheduleRemoval(["no-such-key"], runs, items);
    expect(got.itemIds).toEqual(["no-such-key"]);
  });
});
