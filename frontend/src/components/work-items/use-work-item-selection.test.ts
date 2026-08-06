// Unit tests for the pure selection helpers (design §5.1, ADR-2 —
// cascade subtree selection + tri-state checkboxes). The React hook
// itself is a thin wrapper over these; the tri-state/cascade logic is
// what regressed in the original select-all bug.

import { describe, expect, it } from "vitest";

import type { WorkItem } from "@/api/gen/orchicon/api/v1/work_item_pb";
import {
  subtreeIds,
  subtreeSelectionState,
  visibleSelectionState,
} from "@/components/work-items/use-work-item-selection";

/** Minimal WorkItem shape — the helpers only read id/parentId. */
function item(id: string, parentId = ""): WorkItem {
  return { id, parentId } as unknown as WorkItem;
}

function childrenOf(byParent: Map<string, string[]>) {
  return (parentId: string): WorkItem[] =>
    (byParent.get(parentId) ?? []).map((id) => item(id, parentId));
}

describe("subtreeIds", () => {
  it("returns the node itself when it has no children", () => {
    expect(subtreeIds("a", childrenOf(new Map()))).toEqual(["a"]);
  });

  it("walks the whole descendant tree (BFS)", () => {
    const byParent = new Map<string, string[]>([
      ["e", ["f1", "f2"]],
      ["f1", ["t1"]],
    ]);
    expect(subtreeIds("e", childrenOf(byParent))).toEqual(["e", "f1", "f2", "t1"]);
  });
});

describe("subtreeSelectionState", () => {
  const ids = ["a", "b", "c"];

  it("unchecked when nothing in the subtree is selected", () => {
    expect(subtreeSelectionState(ids, new Set(["z"]))).toBe("unchecked");
  });

  it("checked when the whole subtree is selected", () => {
    expect(subtreeSelectionState(ids, new Set(["a", "b", "c"]))).toBe("checked");
  });

  it("indeterminate when only part of the subtree is selected", () => {
    expect(subtreeSelectionState(ids, new Set(["a"]))).toBe("indeterminate");
    expect(subtreeSelectionState(ids, new Set(["a", "b"]))).toBe("indeterminate");
  });

  it("empty subtree is unchecked", () => {
    expect(subtreeSelectionState([], new Set())).toBe("unchecked");
  });
});

describe("visibleSelectionState (header select-all tri-state)", () => {
  const visible = ["a", "b", "c"];

  it("unchecked when nothing visible is selected", () => {
    expect(visibleSelectionState(visible, new Set())).toEqual({
      allChecked: false,
      allIndeterminate: false,
    });
  });

  it("checked when every visible item is selected, even with extras outside the set", () => {
    // The header checkbox answers "are all visible items selected?" —
    // selections outside the visible set don't make it indeterminate.
    expect(visibleSelectionState(visible, new Set(["a", "b", "c", "outside"]))).toEqual({
      allChecked: true,
      allIndeterminate: false,
    });
  });

  it("indeterminate on a non-empty strict subset", () => {
    expect(visibleSelectionState(visible, new Set(["a"]))).toEqual({
      allChecked: false,
      allIndeterminate: true,
    });
  });

  it("empty visible set is unchecked", () => {
    expect(visibleSelectionState([], new Set())).toEqual({
      allChecked: false,
      allIndeterminate: false,
    });
  });
});
