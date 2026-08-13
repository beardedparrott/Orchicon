import { describe, expect, it } from "vitest";

import type { WorkItem } from "@/api/gen/orchicon/api/v1/work_item_pb";
import {
  activeSequenceParentIds,
  isHistoryItem,
  queuedSequenceChildren,
  upcomingSortTime,
} from "@/lib/schedules-model";

// Helper to build a work item with only the fields the predicates read.
// status: 1 pending, 4 running, 6 succeeded, 10 scheduled, 11 recurring.
function wi(
  id: string,
  status: number,
  opts: {
    parentId?: string;
    workflowRunId?: string;
    hasScheduledStart?: boolean;
    hasNextRunAt?: boolean;
    nextRunAtSeconds?: number;
    hasChildren?: boolean;
  } = {},
): WorkItem {
  return {
    id,
    status,
    parentId: opts.parentId ?? "",
    workflowRunId: opts.workflowRunId ?? "",
    scheduledStartAt: opts.hasScheduledStart
      ? { seconds: BigInt(1000), nanos: 0 }
      : undefined,
    nextRunAt: opts.hasNextRunAt
      ? { seconds: BigInt(opts.nextRunAtSeconds ?? 3000), nanos: 0 }
      : undefined,
    updatedAt: { seconds: BigInt(2000), nanos: 0 },
    createdAt: { seconds: BigInt(1000), nanos: 0 },
  } as unknown as WorkItem;
}

describe("activeSequenceParentIds", () => {
  it("identifies a running parent with children and no bound workflow", () => {
    const items = [
      wi("parent", 4, { hasChildren: true }),
      wi("child1", 4, { parentId: "parent", workflowRunId: "run1" }),
      wi("child2", 1, { parentId: "parent" }),
    ];
    expect([...activeSequenceParentIds(items)]).toEqual(["parent"]);
  });

  it("does not treat a running leaf with a bound run as a sequence parent", () => {
    const items = [wi("leaf", 4, { workflowRunId: "run1" })];
    expect(activeSequenceParentIds(items).size).toBe(0);
  });
});

describe("queuedSequenceChildren", () => {
  // Mirrors the verified sequential run state after the parent fires and
  // child 1 is running: parent running, child1 running with a bound run,
  // children 2/3 pending — the queued remainder. None of the children
  // carry a scheduled start (only the parent is scheduled).
  const sequenceState = () => [
    wi("parent", 4, { hasChildren: true }),
    wi("child1", 4, { parentId: "parent", workflowRunId: "run1" }),
    wi("child2", 1, { parentId: "parent" }),
    wi("child3", 1, { parentId: "parent" }),
    wi("unrelated", 1),
  ];

  it("returns the pending children of the active sequence parent, in chain order", () => {
    const queued = queuedSequenceChildren(sequenceState());
    expect(queued.map((i) => i.id)).toEqual(["child2", "child3"]);
  });

  it("excludes the armed (running) child and pending items with no active parent", () => {
    const queued = queuedSequenceChildren(sequenceState());
    expect(queued.map((i) => i.id)).not.toContain("child1");
    expect(queued.map((i) => i.id)).not.toContain("unrelated");
  });

  it("returns nothing once the sequence is complete (parent succeeded)", () => {
    const done = [
      wi("parent", 6, { hasChildren: true }),
      wi("child1", 6, { parentId: "parent", workflowRunId: "run1" }),
      wi("child2", 6, { parentId: "parent", workflowRunId: "run2" }),
      wi("child3", 6, { parentId: "parent", workflowRunId: "run3" }),
    ];
    expect(queuedSequenceChildren(done)).toEqual([]);
  });
});

describe("isHistoryItem", () => {
  it("includes a scheduled item that reached a terminal status", () => {
    const item = wi("single", 6, { workflowRunId: "run1", hasScheduledStart: true });
    expect(isHistoryItem(item, [item])).toBe(true);
  });

  it("includes a completed sequence child with no scheduled start (the bug)", () => {
    // A sequence child succeeds with workflow_run_id set but no
    // scheduled_start_at — only the parent is scheduled.
    const items = [
      wi("parent", 6, { hasChildren: true }),
      wi("child1", 6, { parentId: "parent", workflowRunId: "run1" }),
      wi("child2", 6, { parentId: "parent", workflowRunId: "run2" }),
    ];
    expect(isHistoryItem(items[1], items)).toBe(true);
    expect(isHistoryItem(items[2], items)).toBe(true);
  });

  it("includes a completed sequence parent", () => {
    const items = [
      wi("parent", 6, { hasChildren: true }),
      wi("child1", 6, { parentId: "parent", workflowRunId: "run1" }),
    ];
    expect(isHistoryItem(items[0], items)).toBe(true);
  });

  it("includes a single workflow run started without a schedule (the bug)", () => {
    // Manual / start-immediately run: workflow_run_id set, no
    // scheduled_start_at, terminal status.
    const item = wi("single", 6, { workflowRunId: "run1" });
    expect(isHistoryItem(item, [item])).toBe(true);
  });

  it("excludes terminal items that never ran a workflow and are not sequence parents", () => {
    const item = wi("leaf", 6);
    expect(isHistoryItem(item, [item])).toBe(false);
  });

  it("excludes non-terminal items", () => {
    const running = wi("single", 4, { workflowRunId: "run1" });
    expect(isHistoryItem(running, [running])).toBe(false);
  });
});

describe("upcomingSortTime", () => {
  it("uses scheduled_start_at for a scheduled item", () => {
    const item = wi("sched", 10, { hasScheduledStart: true });
    expect(upcomingSortTime(item)).toBe(1_000_000);
  });

  it("uses next_run_at for a recurring item, not scheduled_start_at", () => {
    const item = wi("recur", 11, { hasScheduledStart: true, hasNextRunAt: true });
    expect(upcomingSortTime(item)).toBe(3_000_000);
  });

  it("uses next_run_at even when the recurring item has no scheduled start", () => {
    const item = wi("recur", 11, { hasNextRunAt: true });
    expect(upcomingSortTime(item)).toBe(3_000_000);
  });

  it("returns 0 when neither time is set", () => {
    const item = wi("bare", 1);
    expect(upcomingSortTime(item)).toBe(0);
  });

  it("orders recurring and scheduled items chronologically by fire time", () => {
    const recurringLater = wi("recur-later", 11, { hasNextRunAt: true, nextRunAtSeconds: 5000 });
    const scheduledSoon = wi("sched-soon", 10, { hasScheduledStart: true }); // 1_000_000
    const recurringSoon = wi("recur-soon", 11, { hasScheduledStart: true, hasNextRunAt: true, nextRunAtSeconds: 3000 });
    const sorted = [recurringLater, scheduledSoon, recurringSoon].sort(
      (a, b) => upcomingSortTime(a) - upcomingSortTime(b),
    );
    expect(sorted.map((i) => i.id)).toEqual(["sched-soon", "recur-soon", "recur-later"]);
  });
});
