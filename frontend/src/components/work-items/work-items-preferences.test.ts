// Unit tests for the persisted work-items view preferences
// (design-notes/visual-and-functional-tweaks-to-work-items-page.md §3).

import { describe, expect, it, beforeEach } from "vitest";

import {
  DEFAULT_FILTERS,
  loadExpandedPreference,
  loadFiltersPreference,
  loadViewPreference,
  saveExpandedPreference,
  saveFiltersPreference,
  saveViewPreference,
} from "@/components/work-items/work-items-preferences";

// Vitest runs in a node environment (no jsdom). The preferences module
// wraps its storage access in try/catch, but we still install a minimal
// in-memory localStorage shim so the round-trip tests exercise the real
// serialize/parse path.
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

describe("work-items preferences (localStorage envelopes)", () => {
  beforeEach(() => {
    localStorage.clear();
  });

  it("view defaults to board and round-trips tree/board", () => {
    expect(loadViewPreference()).toBe("board");
    saveViewPreference("tree");
    expect(loadViewPreference()).toBe("tree");
    saveViewPreference("board");
    expect(loadViewPreference()).toBe("board");
  });

  it("filters default to empty statuses/kinds with created_at desc sort", () => {
    const f = loadFiltersPreference("proj-1");
    expect(f).toEqual(DEFAULT_FILTERS);
  });

  it("filters round-trip per project without cross-talk", () => {
    saveFiltersPreference("proj-1", {
      statuses: [2, 5],
      kinds: [3],
      search: "migration",
      sortBy: "title",
      sortOrder: "asc",
    });
    saveFiltersPreference("proj-2", {
      statuses: [],
      kinds: [],
      search: "other",
      sortBy: "created_at",
      sortOrder: "desc",
    });
    expect(loadFiltersPreference("proj-1")).toEqual({
      statuses: [2, 5],
      kinds: [3],
      search: "migration",
      sortBy: "title",
      sortOrder: "asc",
    });
    expect(loadFiltersPreference("proj-2").search).toBe("other");
    expect(loadFiltersPreference("proj-1").search).toBe("migration");
  });

  it("expanded sets default to empty (collapsed) and round-trip", () => {
    expect(loadExpandedPreference("proj-1", "board").size).toBe(0);
    expect(loadExpandedPreference("proj-1", "tree").size).toBe(0);
    saveExpandedPreference("proj-1", "board", new Set(["a", "b"]));
    saveExpandedPreference("proj-2", "board", new Set(["c"]));
    expect(Array.from(loadExpandedPreference("proj-1", "board")).sort()).toEqual(["a", "b"]);
    expect(Array.from(loadExpandedPreference("proj-2", "board"))).toEqual(["c"]);
    // tree slice stays independent from board
    expect(loadExpandedPreference("proj-1", "tree").size).toBe(0);
  });

  it("malformed JSON falls back to defaults instead of crashing", () => {
    localStorage.setItem("orchicon.workItems.view", "{not json");
    localStorage.setItem("orchicon.workItems.filters.proj-1", "garbage");
    localStorage.setItem("orchicon.workItems.boardExpanded.proj-1", "42");
    expect(loadViewPreference()).toBe("board");
    expect(loadFiltersPreference("proj-1")).toEqual(DEFAULT_FILTERS);
    expect(loadExpandedPreference("proj-1", "board").size).toBe(0);
  });

  it("old-version envelopes are ignored (forward compatible)", () => {
    localStorage.setItem(
      "orchicon.workItems.view",
      JSON.stringify({ v: 0, view: "tree" }),
    );
    expect(loadViewPreference()).toBe("board");
  });

  it("invalid sort values normalize to defaults", () => {
    localStorage.setItem(
      "orchicon.workItems.filters.proj-1",
      JSON.stringify({
        v: 1,
        statuses: "nope",
        kinds: ["x", "y"],
        search: 42,
        sortBy: "",
        sortOrder: "sideways",
      }),
    );
    expect(loadFiltersPreference("proj-1")).toEqual(DEFAULT_FILTERS);
  });
});
