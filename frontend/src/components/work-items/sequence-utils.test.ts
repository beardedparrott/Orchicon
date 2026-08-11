import { describe, expect, it } from "vitest";

import type { WorkItem } from "@/api/gen/orchicon/api/v1/work_item_pb";
import {
  byChainOrder,
  computeSequencePositions,
  sequenceParentIds,
  sortByChainOrder,
} from "@/components/work-items/sequence-utils";

function item(id: string, parentId: string, sortOrder: number, createdAtSecs = 0, workflowId = ""): WorkItem {
  return {
    id,
    parentId,
    sortOrder,
    workflowId,
    createdAt: { seconds: BigInt(createdAtSecs), nanos: 0 },
  } as unknown as WorkItem;
}

describe("byChainOrder / sortByChainOrder", () => {
  it("orders by sort_order rank regardless of input order", () => {
    const items = [item("b", "P", 2), item("a", "P", 1), item("c", "P", 3)];
    expect(sortByChainOrder(items).map((i) => i.id)).toEqual(["a", "b", "c"]);
    // The input array is never mutated.
    expect(items.map((i) => i.id)).toEqual(["b", "a", "c"]);
  });

  it("null sort_order (0) sorts LAST and falls back to created_at", () => {
    const items = [
      item("new", "P", 0, 200),
      item("first", "P", 1, 100),
      item("second", "P", 2, 100),
    ];
    expect(sortByChainOrder(items).map((i) => i.id)).toEqual(["first", "second", "new"]);
  });

  it("is order-independent and transitive (stable chain derivation)", () => {
    const shuffled = [item("a", "P", 2), item("b", "P", 1), item("c", "P", 3), item("d", "P", 4)];
    const sorted = [...shuffled].sort(byChainOrder);
    expect(sorted.map((i) => i.id)).toEqual(["b", "a", "c", "d"]);
  });
});

describe("computeSequencePositions", () => {
  it("ranks siblings by sort_order within each sequence parent", () => {
    const items = [
      item("P", "", 0), // sequence parent (has children, no workflow)
      item("a1", "P", 2),
      item("a2", "P", 1),
      item("a3", "P", 3),
      item("Q", "", 0), // sequence parent
      item("b1", "Q", 1),
    ];
    const pos = computeSequencePositions(items);
    expect(pos.get("a1")).toBe(2);
    expect(pos.get("a2")).toBe(1);
    expect(pos.get("a3")).toBe(3);
    expect(pos.get("b1")).toBe(1);
    // Top-level items have no position.
    expect(pos.get("P")).toBeUndefined();
  });

  it("gives positions to children even when the parent carries a stale workflow binding", () => {
    // "BP" has children, so it IS a sequence parent regardless of its own
    // workflow binding — its children get chain positions.
    const items = [
      item("BP", "", 0, 0, "wf-1"),
      item("c1", "BP", 1),
      item("c2", "BP", 2),
    ];
    const pos = computeSequencePositions(items);
    expect(pos.get("c1")).toBe(1);
    expect(pos.get("c2")).toBe(2);
  });

  it("null sort_order (0) sorts LAST and falls back to created_at", () => {
    const items = [
      item("P", "", 0),
      item("new", "P", 0, 200),
      item("first", "P", 1, 100),
      item("second", "P", 2, 100),
    ];
    const pos = computeSequencePositions(items);
    expect(pos.get("first")).toBe(1);
    expect(pos.get("second")).toBe(2);
    expect(pos.get("new")).toBe(3);
  });
});

describe("sequenceParentIds", () => {
  it("collects every id that is someone's parent", () => {
    const items = [item("P", "", 1), item("c", "P", 1), item("solo", "", 1)];
    const parents = sequenceParentIds(items);
    expect(parents.has("P")).toBe(true);
    expect(parents.has("solo")).toBe(false);
    expect(parents.has("c")).toBe(false);
  });

  it("treats a parent with children as a sequence even with its own workflow binding", () => {
    const items = [
      item("BP", "", 1, 0, "wf-1"), // parent carries a stale workflow, still a sequence
      item("c", "BP", 1),
    ];
    const parents = sequenceParentIds(items);
    expect(parents.has("BP")).toBe(true);
  });
});
