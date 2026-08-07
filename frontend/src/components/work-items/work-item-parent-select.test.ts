// Unit tests for the searchable parent picker (ADR-WIT-5). The pure
// option-filtering logic (depth rule, query, self-exclusion) lives in
// filterParentOptions so it is testable without a DOM; the component
// itself is exercised in the browser E2E pass.

import { describe, expect, it, vi } from "vitest";

import type { WorkItem } from "@/api/gen/orchicon/api/v1/work_item_pb";
import {
  depthForKind,
  filterParentOptions,
} from "@/components/work-items/work-item-parent-select";

// The component transitively imports theme-store, which touches
// `document.documentElement` at module load. Vitest runs without a DOM,
// so install a minimal shim BEFORE the imports evaluate.
vi.hoisted(() => {
  const g = globalThis as Record<string, unknown>;
  if (!g.document) {
    g.document = {
      documentElement: {
        classList: { add: () => {}, remove: () => {} },
        setAttribute: () => {},
      },
    };
  }
  if (!g.localStorage) {
    g.localStorage = {
      getItem: () => null,
      setItem: () => {},
      removeItem: () => {},
      clear: () => {},
      key: () => null,
      length: 0,
    };
  }
});

function item(id: string, kind: number, title: string): WorkItem {
  return { id, kind, title } as WorkItem;
}

const epicA = item("epic-a", 1, "Alpha epic");
const epicB = item("epic-b", 1, "Beta epic");
const feature = item("feat-1", 2, "Checkout flow");
const task = item("task-1", 3, "Implement auth");
const subtask = item("sub-1", 4, "Fix button");

describe("depthForKind", () => {
  it("maps the four hierarchy kinds to their depth", () => {
    expect(depthForKind(1)).toBe(1); // epic
    expect(depthForKind(2)).toBe(2); // feature
    expect(depthForKind(3)).toBe(3); // task
    expect(depthForKind(4)).toBe(4); // subtask
  });

  it("maps unknown/recovery kinds to 0 (never a valid parent)", () => {
    expect(depthForKind(0)).toBe(0);
    expect(depthForKind(5)).toBe(0); // recovery_stop
    expect(depthForKind(99)).toBe(0);
  });
});

describe("filterParentOptions", () => {
  const all = [epicA, epicB, feature, task, subtask];

  it("offers only items strictly shallower than the child kind", () => {
    const forFeature = filterParentOptions(all, 2, undefined, "");
    expect(forFeature.map((i) => i.id)).toEqual(["epic-a", "epic-b"]);
    const forTask = filterParentOptions(all, 3, undefined, "");
    expect(forTask.map((i) => i.id).sort()).toEqual(["epic-a", "epic-b", "feat-1"]);
    const forSubtask = filterParentOptions(all, 4, undefined, "");
    expect(forSubtask.map((i) => i.id).sort()).toEqual([
      "epic-a",
      "epic-b",
      "feat-1",
      "task-1",
    ]);
  });

  it("offers nothing for an epic child (no parent can be shallower)", () => {
    expect(filterParentOptions(all, 1, undefined, "")).toEqual([]);
  });

  it("excludes the id passed as excludeId (self-parenting guard)", () => {
    const withSelf = [epicA, feature, task];
    // A task (depth 3) could parent a subtask, but the task itself must
    // be excluded when it is the item being edited.
    const opts = filterParentOptions([...withSelf, subtask], 4, "task-1", "");
    expect(opts.some((i) => i.id === "task-1")).toBe(false);
    expect(opts.some((i) => i.id === "feat-1")).toBe(true);
  });

  it("filters by case-insensitive title substring", () => {
    const opts = filterParentOptions(all, 3, undefined, "checkout");
    expect(opts.map((i) => i.id)).toEqual(["feat-1"]);
    const empty = filterParentOptions(all, 3, undefined, "nomatch");
    expect(empty).toEqual([]);
    // Query trims whitespace.
    expect(filterParentOptions(all, 3, undefined, "  ALPHA  ").map((i) => i.id)).toEqual([
      "epic-a",
    ]);
  });

  it("sorts results by title", () => {
    const opts = filterParentOptions([feature, epicB, epicA], 3, undefined, "");
    expect(opts.map((i) => i.title)).toEqual(["Alpha epic", "Beta epic", "Checkout flow"]);
  });
});
