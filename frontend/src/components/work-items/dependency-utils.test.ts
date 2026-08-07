// Unit tests for the pure work-item presentation helpers
// (design-notes/complete-ui-and-functionality-overhaul-of-work-item-page.md
// §5.2/§5.4 — "Put this in dependency-utils.ts as a pure function
// computeBlockState(nodes, edges) with a unit test").
//
// These cover the two regression classes from the work-item review:
//   1. search results must keep their tree ancestors (no orphaned rows),
//   2. select-all/filter helpers must never include dimmed ancestors.

import { describe, expect, it } from "vitest";

import {
  DependencyType,
  WorkItemKind,
  WorkItemStatus,
  type WorkItem,
  type WorkItemDependency,
} from "@/api/gen/orchicon/api/v1/work_item_pb";
import {
  buildTreeData,
  computeBlockState,
  filterItemsByKindStatus,
  matchesSearch,
} from "@/components/work-items/dependency-utils";

/** Minimal WorkItem shape — the helpers only read presentation fields. */
function item(partial: Partial<WorkItem> & { id: string; title: string }): WorkItem {
  return {
    parentId: "",
    kind: WorkItemKind.TASK,
    status: WorkItemStatus.PENDING,
    description: "",
    ...partial,
  } as unknown as WorkItem;
}

function edge(partial: { fromId: string; toId: string; type?: number }) {
  return { type: DependencyType.BLOCKS, ...partial } as unknown as WorkItemDependency;
}

describe("computeBlockState", () => {
  it("marks the dependent blocked by a non-terminal BLOCKS edge", () => {
    const a = item({ id: "a", title: "A" });
    const b = item({ id: "b", title: "B", status: WorkItemStatus.READY });
    const { blockedBy } = computeBlockState([a, b], [edge({ fromId: "a", toId: "b" })]);
    expect(blockedBy.get("b")?.map((i) => i.id)).toEqual(["a"]);
  });

  it("marks the dependent blocked by a DEPENDS_ON edge", () => {
    const a = item({ id: "a", title: "A" });
    const b = item({ id: "b", title: "B" });
    const { blockedBy, blocks } = computeBlockState(
      [a, b],
      [edge({ fromId: "a", toId: "b", type: DependencyType.DEPENDS_ON })],
    );
    expect(blockedBy.get("b")?.map((i) => i.id)).toEqual(["a"]);
    expect(blocks.get("a")?.map((i) => i.id)).toEqual(["b"]);
  });

  it("ignores RELATES_TO edges", () => {
    const a = item({ id: "a", title: "A" });
    const b = item({ id: "b", title: "B" });
    const { blockedBy } = computeBlockState(
      [a, b],
      [edge({ fromId: "a", toId: "b", type: DependencyType.RELATES_TO })],
    );
    expect(blockedBy.get("b")).toBeUndefined();
  });

  it("a terminal blocker no longer blocks", () => {
    const a = item({ id: "a", title: "A", status: WorkItemStatus.SUCCEEDED });
    const b = item({ id: "b", title: "B" });
    const { blockedBy } = computeBlockState([a, b], [edge({ fromId: "a", toId: "b" })]);
    expect(blockedBy.get("b")).toBeUndefined();
  });

  it("skips edges whose nodes are not in the graph", () => {
    const b = item({ id: "b", title: "B" });
    const { blockedBy } = computeBlockState([b], [edge({ fromId: "ghost", toId: "b" })]);
    expect(blockedBy.get("b")).toBeUndefined();
  });
});

describe("matchesSearch", () => {
  const task = item({ id: "t", title: "Design the migration runner", description: "Schema DSL details" });

  it("matches title case-insensitively", () => {
    expect(matchesSearch(task, "migration")).toBe(true);
    expect(matchesSearch(task, "MIGRATION")).toBe(true);
  });

  it("matches description", () => {
    expect(matchesSearch(task, "schema")).toBe(true);
  });

  it("does not match unrelated text", () => {
    expect(matchesSearch(task, "nonexistent")).toBe(false);
  });

  it("empty/whitespace search matches everything", () => {
    expect(matchesSearch(task, "")).toBe(true);
    expect(matchesSearch(task, "   ")).toBe(true);
  });
});

describe("filterItemsByKindStatus", () => {
  const epic = item({ id: "e", title: "Platform Modernization", kind: WorkItemKind.EPIC });
  const task = item({ id: "t", title: "Design the migration runner", kind: WorkItemKind.TASK, status: WorkItemStatus.READY });
  const subtask = item({ id: "s", title: "Write DSL spec", kind: WorkItemKind.SUBTASK, status: WorkItemStatus.PENDING });

  it("filters by kind", () => {
    const ids = filterItemsByKindStatus([epic, task, subtask], [WorkItemKind.TASK], []).map((i) => i.id);
    expect(ids).toEqual(["t"]);
  });

  it("filters by status", () => {
    const ids = filterItemsByKindStatus([epic, task, subtask], [], [WorkItemStatus.READY]).map((i) => i.id);
    expect(ids).toEqual(["t"]);
  });

  it("filters by search", () => {
    const ids = filterItemsByKindStatus([epic, task, subtask], [], [], "dsl").map((i) => i.id);
    expect(ids).toEqual(["s"]);
  });

  it("composes kind + status + search", () => {
    const ids = filterItemsByKindStatus(
      [epic, task, subtask],
      [WorkItemKind.TASK],
      [WorkItemStatus.READY],
      "migration",
    ).map((i) => i.id);
    expect(ids).toEqual(["t"]);
  });

  it("OR-composes multiple kinds", () => {
    const ids = filterItemsByKindStatus(
      [epic, task, subtask],
      [WorkItemKind.EPIC, WorkItemKind.TASK],
      [],
    ).map((i) => i.id);
    expect(ids).toEqual(["e", "t"]);
  });

  it("OR-composes multiple statuses", () => {
    const ids = filterItemsByKindStatus(
      [epic, task, subtask],
      [],
      [WorkItemStatus.PENDING, WorkItemStatus.READY],
    ).map((i) => i.id);
    expect(ids).toEqual(["e", "t", "s"]);
  });

  it("empty selections filter nothing", () => {
    const ids = filterItemsByKindStatus([epic, task, subtask], [], []).map((i) => i.id);
    expect(ids).toEqual(["e", "t", "s"]);
  });
});

describe("buildTreeData (regression: search must not orphan matches)", () => {
  const epic = item({ id: "e", title: "Platform Modernization", kind: WorkItemKind.EPIC });
  const feature = item({ id: "f", title: "Migration Runner", kind: WorkItemKind.FEATURE, parentId: "e" });
  const task = item({ id: "t", title: "Design the migration runner", kind: WorkItemKind.TASK, parentId: "f" });
  const subtask = item({ id: "s", title: "Write DSL spec", kind: WorkItemKind.SUBTASK, parentId: "t" });
  const otherEpic = item({ id: "e2", title: "Developer Experience", kind: WorkItemKind.EPIC });
  const all = [epic, feature, task, subtask, otherEpic];

  it("deep search results keep their whole ancestor chain", () => {
    const data = buildTreeData(all, [], [], "dsl");
    expect(data.matches.map((i) => i.id)).toEqual(["s"]);
    // The searched subtask must be reachable under its epic — the
    // regression that previously rendered an empty tree.
    expect(data.treeItems.map((i) => i.id).sort()).toEqual(["e", "f", "s", "t"].sort());
    expect(data.ancestorIds.has("e")).toBe(true);
    expect(data.ancestorIds.has("f")).toBe(true);
    expect(data.ancestorIds.has("t")).toBe(true);
    // The unrelated epic is not dragged into the filtered view.
    expect(data.treeItems.some((i) => i.id === "e2")).toBe(false);
  });

  it("kind filter keeps ancestors as dimmed containers only", () => {
    const data = buildTreeData(all, [WorkItemKind.TASK], []);
    expect(data.matches.map((i) => i.id)).toEqual(["t"]);
    expect(data.treeItems.map((i) => i.id).sort()).toEqual(["e", "f", "t"].sort());
    expect(data.ancestorIds.has("e")).toBe(true);
    expect(data.ancestorIds.has("t")).toBe(false); // t is a match, not just an ancestor
  });

  it("no filters: every item is a match and no ancestor-only rows exist", () => {
    const data = buildTreeData(all, [], []);
    expect(data.matches.map((i) => i.id).sort()).toEqual(all.map((i) => i.id).sort());
    expect(data.treeItems.map((i) => i.id).sort()).toEqual(all.map((i) => i.id).sort());
  });
});
