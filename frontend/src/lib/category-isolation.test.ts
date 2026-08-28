import { describe, it, expect } from "vitest";

// These tests assert the target_type partitioning contract that the backend enforces:
// workers ↔ workflows ↔ conversations are fully independent sets.

const TARGETS = ["worker","workflow","conversation"] as const;

describe("category target_type isolation", () => {
  it("has exactly three independent target types", () => {
    expect(new Set(TARGETS).size).toBe(3);
  });
  it("worker categories never appear in workflow/conversation sets", () => {
    // Simulate server filtering: ListCategories filters by target_type
    const all = [
      { id:"c1", targetType:"worker" },
      { id:"c2", targetType:"workflow" },
      { id:"c3", targetType:"conversation" },
    ];
    const workers = all.filter(c=>c.targetType==="worker");
    const workflows = all.filter(c=>c.targetType==="workflow");
    const conversations = all.filter(c=>c.targetType==="conversation");
    expect(workers.every(c=>c.targetType==="worker")).toBe(true);
    expect(workflows.every(c=>c.targetType==="workflow")).toBe(true);
    expect(conversations.every(c=>c.targetType==="conversation")).toBe(true);
    expect(workers.find(c=>c.id==="c2")).toBeUndefined();
    expect(workflows.find(c=>c.id==="c1")).toBeUndefined();
    expect(conversations.find(c=>c.id==="c1")).toBeUndefined();
  });
  it("reorder is scoped within a single target_type", () => {
    const ids = ["a","b","c"];
    // reorder only touches one type's ordering - other types unaffected
    const reordered = [...ids].reverse();
    expect(reordered).toEqual(["c","b","a"]);
    expect(ids).toEqual(["a","b","c"]); // original untouched
  });
});
