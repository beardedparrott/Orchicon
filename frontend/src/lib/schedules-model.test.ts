import { describe, expect, it } from "vitest";

import type { WorkItem } from "@/api/gen/orchicon/api/v1/work_item_pb";
import type { WorkflowRun } from "@/api/gen/orchicon/api/v1/workflow_pb";
import {
  activeSequenceParentIds,
  historyRunRanAt,
  isHistoryRun,
  queuedSequenceChildren,
  upcomingSortTime,
} from "@/lib/schedules-model";

// Helper to build a workflow run with only the fields the predicates read.
// A run is "executed" when it carries a real started_at.
function run(
  id: string,
  opts: {
    hasStartedAt?: boolean;
    startedAtSeconds?: number;
    hasCreatedAt?: boolean;
    createdAtSeconds?: number;
  } = {},
): WorkflowRun {
  return {
    id,
    startedAt: opts.hasStartedAt
      ? { seconds: BigInt(opts.startedAtSeconds ?? 4000), nanos: 0 }
      : undefined,
    createdAt: opts.hasCreatedAt
      ? { seconds: BigInt(opts.createdAtSeconds ?? 1000), nanos: 0 }
      : undefined,
  } as unknown as WorkflowRun;
}
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

describe("isHistoryRun", () => {
  it("includes a run that actually executed (has a real started_at)", () => {
    expect(isHistoryRun(run("r1", { hasStartedAt: true }))).toBe(true);
  });

  it("includes a recurring-fire run whose item re-armed (the missing-runs bug)", () => {
    // A recurring fire creates a NEW run with a started_at even though the
    // work item itself re-armed back to SCHEDULED/RECURRING — the run is
    // the history unit, not the item status.
    expect(isHistoryRun(run("r2", { hasStartedAt: true }))).toBe(true);
  });

  it("includes a prior (older) run of a work item that ran more than once", () => {
    expect(isHistoryRun(run("r-old", { hasStartedAt: true }))).toBe(true);
    expect(isHistoryRun(run("r-new", { hasStartedAt: true }))).toBe(true);
  });

  it("includes an in-flight run (still running, not terminal)", () => {
    expect(isHistoryRun(run("r-run", { hasStartedAt: true }))).toBe(true);
  });

  it("excludes a run that was created but never started (no started_at)", () => {
    expect(isHistoryRun(run("r-armed", { hasCreatedAt: true }))).toBe(false);
  });
});

describe("historyRunRanAt", () => {
  it("returns the run's real started_at in ms", () => {
    expect(historyRunRanAt(run("r1", { hasStartedAt: true, startedAtSeconds: 5000 }))).toBe(5_000_000);
  });

  it("falls back to created_at when started_at is absent", () => {
    expect(historyRunRanAt(run("r2", { hasCreatedAt: true, createdAtSeconds: 2000 }))).toBe(2_000_000);
  });

  it("returns 0 when neither started_at nor created_at is set", () => {
    expect(historyRunRanAt(run("r3"))).toBe(0);
  });

  it("orders runs by real start time descending (the 2am ordering bug)", () => {
    // A run scheduled at 2am but started later must sort below later runs;
    // historyRunRanAt uses the run's actual started_at, not the item's
    // (future) scheduled firing time.
    const twoAm = run("r-2am", { hasStartedAt: true, startedAtSeconds: 2 * 3600 });
    const afternoon = run("r-pm", { hasStartedAt: true, startedAtSeconds: 14 * 3600 });
    const sorted = [twoAm, afternoon].sort(
      (a, b) => historyRunRanAt(b) - historyRunRanAt(a),
    );
    expect(sorted.map((r) => r.id)).toEqual(["r-pm", "r-2am"]);
  });

  it("reverses order when sorted ascending", () => {
    const early = run("r-a", { hasStartedAt: true, startedAtSeconds: 1000 });
    const late = run("r-b", { hasStartedAt: true, startedAtSeconds: 9000 });
    const sorted = [late, early].sort(
      (a, b) => historyRunRanAt(a) - historyRunRanAt(b),
    );
    expect(sorted.map((r) => r.id)).toEqual(["r-a", "r-b"]);
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
