// Unit tests for the bulk "Run" classifier (ADR-WI-9).
//
// `partitionRunable` / `classifyRun` decide which selected items can be
// kicked off at once and which must be skipped (with a reason for the
// toast). These cover the full kind × state × workflow matrix from
// `architecture-notes/bulk-run-button.md` §3 so the partial-success UX
// is exercised deterministically.

import { describe, expect, it } from "vitest";

import type { WorkItem } from "@/api/gen/orchicon/api/v1/work_item_pb";
import { WorkItemKind, WorkItemStatus } from "@/api/gen/orchicon/api/v1/work_item_pb";
import { classifyRun, partitionRunable } from "@/components/work-items/batch-run";

/** Minimal WorkItem — classifyRun only reads id/parentId/kind/status/workflowId. */
function item(
  id: string,
  opts: Partial<WorkItem> = {},
): WorkItem {
  return {
    id,
    parentId: opts.parentId ?? "",
    kind: opts.kind ?? WorkItemKind.TASK,
    status: opts.status ?? WorkItemStatus.READY,
    workflowId: opts.workflowId ?? "",
    title: opts.title ?? id,
  } as unknown as WorkItem;
}

describe("classifyRun", () => {
  const emptyParentSet = new Set<string>();

  it("skips terminal items (already finished)", () => {
    for (const status of [
      WorkItemStatus.SUCCEEDED,
      WorkItemStatus.FAILED,
      WorkItemStatus.CANCELLED,
      WorkItemStatus.SKIPPED,
    ]) {
      expect(classifyRun(item("t", { status }), emptyParentSet)).toEqual({
        ok: false,
        code: "terminal",
        reason: "already finished",
      });
    }
  });

  it("skips in-flight items (running / checkpointing / recovering / scheduled / blocked)", () => {
    expect(
      classifyRun(item("r", { status: WorkItemStatus.RUNNING }), emptyParentSet),
    ).toEqual({ ok: false, code: "in-flight", reason: "already running or scheduled" });
    expect(
      classifyRun(item("c", { status: WorkItemStatus.CHECKPOINTING }), emptyParentSet),
    ).toEqual({ ok: false, code: "in-flight", reason: "already running or scheduled" });
    expect(
      classifyRun(item("k", { status: WorkItemStatus.RECOVERING }), emptyParentSet),
    ).toEqual({ ok: false, code: "in-flight", reason: "already running or scheduled" });
    expect(
      classifyRun(item("s", { status: WorkItemStatus.SCHEDULED }), emptyParentSet),
    ).toEqual({ ok: false, code: "in-flight", reason: "already running or scheduled" });
    expect(
      classifyRun(item("b", { status: WorkItemStatus.BLOCKED }), emptyParentSet),
    ).toEqual({
      ok: false,
      code: "in-flight",
      reason: "is blocked by unfinished dependencies",
    });
  });

  it("skips parent kinds (epic / feature) — nothing to run", () => {
    expect(
      classifyRun(item("e", { kind: WorkItemKind.EPIC }), emptyParentSet),
    ).toEqual({ ok: false, code: "parent", reason: "is a parent kind — nothing to run" });
    expect(
      classifyRun(item("f", { kind: WorkItemKind.FEATURE }), emptyParentSet),
    ).toEqual({ ok: false, code: "parent", reason: "is a parent kind — nothing to run" });
  });

  it("skips workflow-less task/subtask/recovery leaves", () => {
    for (const kind of [
      WorkItemKind.TASK,
      WorkItemKind.SUBTASK,
      WorkItemKind.RECOVERY_STOP,
      WorkItemKind.RECOVERY_RETRY_N,
    ]) {
      expect(classifyRun(item("n", { kind }), emptyParentSet)).toEqual({
        ok: false,
        code: "no-workflow",
        reason: "has no workflow bound — open it to bind one",
      });
    }
  });

  it("runs workflow-bound task/subtask leaves", () => {
    for (const kind of [WorkItemKind.TASK, WorkItemKind.SUBTASK]) {
      expect(
        classifyRun(item("w", { kind, workflowId: "wf-1" }), emptyParentSet),
      ).toEqual({ ok: true });
    }
  });

  it("runs a task/subtask sequence parent (has children) even without a workflow", () => {
    const parentSet = new Set<string>(["p"]);
    expect(classifyRun(item("p", { kind: WorkItemKind.TASK }), parentSet)).toEqual({
      ok: true,
    });
    expect(classifyRun(item("p", { kind: WorkItemKind.SUBTASK }), parentSet)).toEqual({
      ok: true,
    });
  });

  it("does not treat a parent kind as a runnable sequence parent", () => {
    // An epic can be a parent, but it is not schedulable — the parent
    // verdict must win over the sequence-parent verdict.
    const parentSet = new Set<string>(["e"]);
    expect(classifyRun(item("e", { kind: WorkItemKind.EPIC }), parentSet)).toEqual({
      ok: false,
      code: "parent",
      reason: "is a parent kind — nothing to run",
    });
  });

  it("prefers the terminal verdict over the parent verdict", () => {
    const parentSet = new Set<string>(["e"]);
    expect(
      classifyRun(item("e", { kind: WorkItemKind.EPIC, status: WorkItemStatus.SUCCEEDED }), parentSet),
    ).toEqual({ ok: false, code: "terminal", reason: "already finished" });
  });
});

describe("partitionRunable", () => {
  it("splits the set into runable and skipped with reasons", () => {
    const items = [
      item("a", { kind: WorkItemKind.TASK, workflowId: "wf" }), // run
      item("b", { kind: WorkItemKind.EPIC }), // parent
      item("c", { status: WorkItemStatus.SUCCEEDED }), // terminal
      item("d", { status: WorkItemStatus.RUNNING }), // in-flight
      item("e"), // no-workflow leaf
    ];
    const { runable, skipped } = partitionRunable(items, new Set<string>());
    expect(runable.map((i) => i.id)).toEqual(["a"]);
    expect(skipped.map((s) => s.id)).toEqual(["b", "c", "d", "e"]);
    expect(skipped).toContainEqual({ id: "b", title: "b", reason: "is a parent kind — nothing to run" });
    expect(skipped).toContainEqual({ id: "c", title: "c", reason: "already finished" });
    expect(skipped).toContainEqual({ id: "d", title: "d", reason: "already running or scheduled" });
    expect(skipped).toContainEqual({ id: "e", title: "e", reason: "has no workflow bound — open it to bind one" });
  });

  it("runs a task sequence parent alongside a workflow-bound leaf", () => {
    const items = [
      item("parent", { kind: WorkItemKind.TASK }), // has children -> run
      item("leaf", { kind: WorkItemKind.SUBTASK, workflowId: "wf" }), // workflow-bound -> run
      item("orphan", { kind: WorkItemKind.SUBTASK }), // no workflow -> skip
    ];
    const { runable, skipped } = partitionRunable(items, new Set(["parent"]));
    expect(runable.map((i) => i.id)).toEqual(["parent", "leaf"]);
    expect(skipped.map((s) => s.id)).toEqual(["orphan"]);
  });

  it("returns empty runable when everything is skipped (nothing to start)", () => {
    const items = [item("x", { status: WorkItemStatus.SUCCEEDED })];
    const { runable, skipped } = partitionRunable(items, new Set<string>());
    expect(runable).toEqual([]);
    expect(skipped).toHaveLength(1);
  });
});
