import { describe, expect, it, beforeEach } from "vitest";

import type { CategoryState } from "@/lib/category-store";
import {
  loadCategoryState,
  saveCategoryState,
  seedAssignments,
  getItemsForCategory,
} from "@/lib/category-store";

// In-memory localStorage shim (same as work-items-preferences.test.ts)
const memoryStore = new Map<string, string>();
Object.defineProperty(globalThis, "localStorage", {
  configurable: true,
  value: {
    get length() {
      return memoryStore.size;
    },
    clear: () => memoryStore.clear(),
    getItem: (key: string) => memoryStore.get(key) ?? null,
    key: (index: number) => Array.from(memoryStore.keys())[index] ?? null,
    removeItem: (key: string) => {
      memoryStore.delete(key);
    },
    setItem: (key: string, value: string) => {
      memoryStore.set(key, value);
    },
  },
});

describe("category-store", () => {
  beforeEach(() => {
    localStorage.clear();
  });

  describe("seedAssignments", () => {
    it("assigns all items to Software Development on first load", () => {
      const result = seedAssignments("workers", ["w1", "w2", "w3"]);
      expect(result.categories).toHaveLength(1);
      expect(result.categories[0].name).toBe("Software Development");
      expect(result.assignments).toEqual({
        w1: "cat_software_dev",
        w2: "cat_software_dev",
        w3: "cat_software_dev",
      });
    });

    it("does not re-seed when assignments exist", () => {
      seedAssignments("workers", ["w1", "w2"]);
      // Manually remove an assignment
      const state = loadCategoryState("workers");
      delete state.assignments["w1"];
      saveCategoryState("workers", state);

      // seedAssignments should return existing state, not re-seed
      const result = seedAssignments("workers", ["w1", "w2", "w3"]);
      expect(result.assignments["w2"]).toBe("cat_software_dev");
      // w1 should NOT be re-seeded — it stays uncategorized
      expect(result.assignments["w1"]).toBeUndefined();
      // w3 (new item) should not appear in assignments
      expect(result.assignments["w3"]).toBeUndefined();
    });

    it("does not re-seed when all assignments are removed (full uncategorize)", () => {
      seedAssignments("workers", ["w1", "w2"]);
      // Remove ALL assignments
      const state = loadCategoryState("workers");
      state.assignments = {};
      saveCategoryState("workers", state);

      // seedAssignments should NOT re-seed — envelope exists
      const result = seedAssignments("workers", ["w1", "w2"]);
      expect(result.assignments).toEqual({});
      // Categories should still include user's original categories
      expect(result.categories).toHaveLength(1);
      expect(result.categories[0].name).toBe("Software Development");
    });

    it("does not re-seed after deleting the only category", () => {
      seedAssignments("workers", ["w1", "w2"]);
      // Delete the only category
      const state = loadCategoryState("workers");
      state.categories = [];
      state.assignments = {};
      saveCategoryState("workers", state);

      const result = seedAssignments("workers", ["w1", "w2"]);
      expect(result.assignments).toEqual({});
      expect(result.categories).toEqual([]);
    });

    it("preserves user-created categories after re-seed check", () => {
      seedAssignments("workers", ["w1", "w2"]);
      // Add a user category
      const state = loadCategoryState("workers");
      state.categories.push({
        id: "cat_infra",
        name: "Infra",
        description: "Infrastructure tasks",
        order: 1,
      });
      saveCategoryState("workers", state);

      const result = seedAssignments("workers", ["w1", "w2", "w3"]);
      expect(result.categories).toHaveLength(2);
      expect(result.categories.map((c) => c.name)).toContain("Infra");
    });
  });

  describe("getItemsForCategory", () => {
    it("groups items by category and collects uncategorized", () => {
      const state: CategoryState = {
        categories: [
          { id: "cat_a", name: "A", order: 0 },
          { id: "cat_b", name: "B", order: 1 },
        ],
        assignments: { w1: "cat_a", w2: "cat_b" },
      };
      const { categorized, uncategorized } = getItemsForCategory(state, [
        "w1",
        "w2",
        "w3",
      ]);
      expect(categorized.get("cat_a")).toEqual(["w1"]);
      expect(categorized.get("cat_b")).toEqual(["w2"]);
      expect(uncategorized).toEqual(["w3"]);
    });

    it("returns all items as uncategorized when no assignments", () => {
      const state: CategoryState = { categories: [], assignments: {} };
      const { categorized, uncategorized } = getItemsForCategory(state, [
        "w1",
        "w2",
      ]);
      expect(categorized.size).toBe(0);
      expect(uncategorized).toEqual(["w1", "w2"]);
    });
  });
});
