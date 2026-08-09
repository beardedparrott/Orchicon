import { describe, expect, it } from "vitest";

import type { WorkItem } from "@/api/gen/orchicon/api/v1/work_item_pb";
import { computeSequencePositions, sequenceParentIds } from "@/components/work-items/sequence-utils";

function item(id: string, parentId: string, sortOrder: number, createdAtSecs = 0): WorkItem {
  return {
    id,
    parentId,
    sortOrder,
    createdAt: { seconds: BigInt(createdAtSecs), nanos: 0 },
  } as unknown as WorkItem;
}

describe("computeSequencePositions", () => {
  it("ranks siblings by sort_order within each parent", () => {
    const items = [
      item("a1", "P", 2),
      item("a2", "P", 1),
      item("a3", "P", 3),
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

  it("null sort_order (0) sorts LAST and falls back to created_at", () => {
    const items = [
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
});
