// Unit tests for the persisted work-items view preferences
// (design-notes/visual-and-functional-tweaks-to-work-items-page.md §3).

import { describe, expect, it, beforeEach } from "vitest";

import { WorkItemKind, WorkItemStatus } from "@/api/gen/orchicon/api/v1/work_item_pb";
import {
  DEFAULT_FILTERS,
  bulkCollapseEffect,
  loadCollapsedPreference,
  loadExpandedPreference,
  loadFiltersPreference,
  loadViewPreference,
  parentIds,
  saveCollapsedPreference,
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

  it("filters default to every status/kind selected with chain-order sort", () => {
    const f = loadFiltersPreference("proj-1");
    expect(f).toEqual(DEFAULT_FILTERS);
    // Default = "show everything": every filterable kind/status selected.
    expect(f.kinds).toContain(WorkItemKind.EPIC);
    expect(f.kinds).toContain(WorkItemKind.RECOVERY_STOP);
    expect(f.statuses).toContain(WorkItemStatus.PENDING);
    expect(f.statuses).toContain(WorkItemStatus.CHECKPOINTING);
    // Default display sort is CHAIN ORDER (empty sort_by + ascending) so
    // the tree/board default to sort_order, never an explicit created_at
    // sort that would disable the server's chain-order default.
    expect(f.sortBy).toBe("");
    expect(f.sortOrder).toBe("asc");
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

  it("an explicitly cleared selection round-trips as empty (show nothing)", () => {
    saveFiltersPreference("proj-1", {
      statuses: [],
      kinds: [],
      search: "",
      sortBy: "created_at",
      sortOrder: "desc",
    });
    expect(loadFiltersPreference("proj-1").statuses).toEqual([]);
    expect(loadFiltersPreference("proj-1").kinds).toEqual([]);
  });

  it("expanded sets default to empty (collapsed) and round-trip", () => {
    expect(loadExpandedPreference("proj-1", "tree").size).toBe(0);
    saveExpandedPreference("proj-1", "tree", new Set(["a", "b"]));
    saveExpandedPreference("proj-2", "tree", new Set(["c"]));
    expect(Array.from(loadExpandedPreference("proj-1", "tree")).sort()).toEqual(["a", "b"]);
    expect(Array.from(loadExpandedPreference("proj-2", "tree"))).toEqual(["c"]);
  });

  it("collapsed sets default to empty (expanded) and round-trip per kind", () => {
    expect(loadCollapsedPreference("proj-1", "board").size).toBe(0);
    expect(loadCollapsedPreference("proj-1", "tree").size).toBe(0);
    saveCollapsedPreference("proj-1", "board", new Set(["a", "b"]));
    saveCollapsedPreference("proj-1", "tree", new Set(["z"]));
    expect(Array.from(loadCollapsedPreference("proj-1", "board")).sort()).toEqual(["a", "b"]);
    expect(Array.from(loadCollapsedPreference("proj-1", "tree"))).toEqual(["z"]);
    // tree slice stays independent from board
    expect(loadCollapsedPreference("proj-2", "board").size).toBe(0);
  });

  it("the archive view OWNS a collapsed slice, independent of tree and board", () => {
    // The archive view's collapse state used to be component-local `useState`,
    // which is why the toolbar's Expand all / Collapse all buttons did nothing
    // there: those buttons drive these persisted slices, and a set living inside
    // the component is not one they can reach.
    expect(loadCollapsedPreference("proj-1", "archive").size).toBe(0);
    saveCollapsedPreference("proj-1", "archive", new Set(["arc-1", "arc-2"]));
    expect(Array.from(loadCollapsedPreference("proj-1", "archive")).sort()).toEqual([
      "arc-1",
      "arc-2",
    ]);
    // Writing the archive slice must not disturb the other two.
    saveCollapsedPreference("proj-1", "tree", new Set(["t1"]));
    saveCollapsedPreference("proj-1", "board", new Set(["b1"]));
    expect(Array.from(loadCollapsedPreference("proj-1", "archive")).sort()).toEqual([
      "arc-1",
      "arc-2",
    ]);
    expect(Array.from(loadCollapsedPreference("proj-1", "tree"))).toEqual(["t1"]);
    expect(Array.from(loadCollapsedPreference("proj-1", "board"))).toEqual(["b1"]);
  });

  it("malformed JSON falls back to defaults instead of crashing", () => {
    localStorage.setItem("orchicon.workItems.view", "{not json");
    localStorage.setItem("orchicon.workItems.filters.proj-1", "garbage");
    localStorage.setItem("orchicon.workItems.boardCollapsed.proj-1", "42");
    expect(loadViewPreference()).toBe("board");
    expect(loadFiltersPreference("proj-1")).toEqual(DEFAULT_FILTERS);
    expect(loadCollapsedPreference("proj-1", "board").size).toBe(0);
  });

  it("old-version envelopes are ignored (forward compatible)", () => {
    localStorage.setItem(
      "orchicon.workItems.view",
      JSON.stringify({ v: 0, view: "tree" }),
    );
    expect(loadViewPreference()).toBe("board");
  });

  it("v2 envelopes (created_at desc) migrate to the chain-order default", () => {
    // VERSION 3: a stored v2 filter with an explicit created_at desc sort
    // (which silently disabled the server's sort_order default) must fall
    // back to the new chain-order default rather than pin the stale sort.
    localStorage.setItem(
      "orchicon.workItems.filters.proj-1",
      JSON.stringify({
        v: 2,
        statuses: [2],
        kinds: [3],
        search: "stale",
        sortBy: "created_at",
        sortOrder: "desc",
      }),
    );
    expect(loadFiltersPreference("proj-1")).toEqual(DEFAULT_FILTERS);
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

describe("parentIds (expand/collapse all — ADR-WIT-4)", () => {
  const item = (id: string, parentId?: string) => ({
    id,
    parentId: parentId ?? "",
  }) as never;

  it("returns every id that is someone's parent, deduplicated", () => {
    const ids = parentIds([item("a"), item("b", "a"), item("c", "a"), item("d", "b")]);
    expect(ids.sort()).toEqual(["a", "b"]);
  });

  it("returns an empty array when nothing has children", () => {
    expect(parentIds([item("a"), item("b")])).toEqual([]);
    expect(parentIds([])).toEqual([]);
  });

  it("ignores items without a parent id", () => {
    const ids = parentIds([item("a"), item("b", "a")]);
    expect(ids).toEqual(["a"]);
  });
});

// ---------------------------------------------------------------------------
// Bulk expand/collapse — what the toolbar's two buttons MEAN per view
// (ADR-WIT-4). These are the assertions that would have caught the reported bug:
// in the Archive view both buttons were dead, because the archive view's collapse
// state was component-local and the switch did not know the view existed.
// ---------------------------------------------------------------------------

describe("bulkCollapseEffect", () => {
  const parents = ["p1", "p2"];

  it("ARCHIVE: expand-all CLEARS its collapsed slice, collapse-all fills it", () => {
    // The archive view is a tree whose rows default EXPANDED (like the board),
    // so the persisted set is the explicitly-collapsed one.
    const expand = bulkCollapseEffect("archive", false, parents, true);
    expect(expand.slice).toBe("archive");
    expect(expand.ids.size).toBe(0);

    const collapse = bulkCollapseEffect("archive", false, parents, false);
    expect(collapse.slice).toBe("archive");
    expect(Array.from(collapse.ids).sort()).toEqual(["p1", "p2"]);
  });

  it("ARCHIVE: a filter does not change the rule (it has no ancestor walk)", () => {
    expect(bulkCollapseEffect("archive", true, parents, true).slice).toBe("archive");
    expect(bulkCollapseEffect("archive", true, parents, false).slice).toBe("archive");
  });

  it("BOARD keeps its own slice and rule", () => {
    expect(bulkCollapseEffect("board", false, parents, true).slice).toBe("board");
    expect(bulkCollapseEffect("board", false, parents, true).ids.size).toBe(0);
    expect(Array.from(bulkCollapseEffect("board", false, parents, false).ids)).toEqual(parents);
  });

  it("TREE with no filter writes the EXPANDED set (rows default collapsed)", () => {
    const expand = bulkCollapseEffect("tree", false, parents, true);
    expect(expand.slice).toBe("treeExpanded");
    expect(Array.from(expand.ids).sort()).toEqual(["p1", "p2"]);

    const collapse = bulkCollapseEffect("tree", false, parents, false);
    expect(collapse.slice).toBe("treeExpanded");
    expect(collapse.ids.size).toBe(0);
  });

  it("TREE with a filter writes the COLLAPSED set (rows auto-expand)", () => {
    const expand = bulkCollapseEffect("tree", true, parents, true);
    expect(expand.slice).toBe("treeCollapsed");
    expect(expand.ids.size).toBe(0);

    const collapse = bulkCollapseEffect("tree", true, parents, false);
    expect(collapse.slice).toBe("treeCollapsed");
    expect(Array.from(collapse.ids).sort()).toEqual(["p1", "p2"]);
  });

  it("every view has a rule — no view falls through to the tree's", () => {
    // A regression guard for the actual defect: a view with NO branch in the
    // switch silently inherited the tree's slice, so its own control did nothing.
    const views = ["board", "archive", "tree"] as const;
    for (const v of views) {
      for (const filtered of [false, true]) {
        for (const expand of [false, true]) {
          const eff = bulkCollapseEffect(v, filtered, parents, expand);
          expect(eff.slice).toBeTruthy();
          expect(eff.ids).toBeInstanceOf(Set);
        }
      }
    }
    // Archive and tree never write each other's slices.
    expect(bulkCollapseEffect("archive", false, parents, false).slice).not.toBe("treeExpanded");
    expect(bulkCollapseEffect("tree", false, parents, false).slice).not.toBe("archive");
  });
});
