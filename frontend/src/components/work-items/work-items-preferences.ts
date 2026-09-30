// Persisted view preferences for the Work Items page
// (design-notes/visual-and-functional-tweaks-to-work-items-page.md,
// ADR-WI-1/ADR-WI-3/ADR-WI-6).
//
// View state (expand sets, filters, last default view) survives
// navigation and reload. Storage is versioned localStorage envelopes
// (`{ v: 1, ... }`) written through on every change, following the
// `theme-store.ts` pattern — all access is wrapped in try/catch because
// storage can throw in private/blocked modes. Unknown/malformed JSON
// falls back to defaults; the `v` field is the migration hook.
//
// Keys (prefix `orchicon.workItems.`):
//   view                         → {v:1, view:"tree"|"board"}          (global)
//   filters.<projectId>          → {v:1, statuses:number[], kinds:number[], search, sortBy, sortOrder}
//   treeExpanded.<projectId>     → {v:1, ids:string[]}   (tree, no filter: expanded by default = collapsed)
//   treeCollapsed.<projectId>    → {v:1, ids:string[]}   (tree, filter active: collapsed by default = expanded)
//   boardCollapsed.<projectId>   → {v:1, ids:string[]}   (board: collapsed by default = expanded)
//
// Filter semantics (ADR-WI-6): a selection is OR-composed within a group
// and AND-composed across groups. The DEFAULTS are every option selected
// ("show everything"); an EMPTY selection means "show nothing" — a user
// who unchecks every type/status expects an empty page, not an unfiltered
// one (regression fixed in v0.1.205).
//
// VERSION 2: v1 envelopes stored empty `statuses`/`kinds` with the old
// "empty = all" meaning, so a v1 filter envelope would now render an
// empty page. Bumping the version makes stale v1 state fall back to the
// new defaults (everything visible) instead.

import { useCallback, useEffect, useState } from "react";

import type { WorkItem } from "@/api/gen/orchicon/api/v1/work_item_pb";
import type { WorkItemsView } from "@/components/work-items/work-items-filter-bar";
import {
  ALL_KIND_VALUES,
  ALL_STATUS_VALUES,
} from "@/components/work-items/work-item-meta";

const PREFIX = "orchicon.workItems.";
// VERSION 3: the default display sort became CHAIN ORDER (empty sort_by,
// ascending) so the tree/board default to sort_order — the sequence order.
// V2 envelopes stored `sortBy: "created_at", sortOrder: "desc"`, which
// silently disabled the server's sort_order default and made tree drags
// no-ops. Bumping the version makes stale v2 filter/view state fall back to
// the new defaults instead of pinning an explicit created_at sort.
const VERSION = 3;

export interface WorkItemFilters {
  /** OR-composed status filter; empty = nothing matches */
  statuses: number[];
  /** OR-composed kind/type filter; empty = nothing matches */
  kinds: number[];
  search: string;
  sortBy: string;
  sortOrder: string;
}

// The default sort is CHAIN ORDER (empty sort_by = the server's
// `ORDER BY sort_order NULLS LAST, created_at`), so the tree renders
// siblings in sequence order and the board orders cards within a column by
// sort_order (architecture-notes/sequential-multi-workflow-runs.md §1).
// Ascending keeps siblings 1..N; a DESC chain-order sort would reverse the
// run order. Choosing an explicit display sort (created/title/priority)
// reorders the view only — it never mutates sort_order, and the board's
// position badges (#1, #2, …) keep the true chain order unambiguous.
export const DEFAULT_FILTERS: WorkItemFilters = {
  statuses: ALL_STATUS_VALUES,
  kinds: ALL_KIND_VALUES,
  search: "",
  sortBy: "", // chain order (sort_order NULLS LAST, created_at)
  sortOrder: "asc",
};

export const DEFAULT_VIEW: WorkItemsView = "board";

// ---------------------------------------------------------------------------
// Low-level safe storage helpers
// ---------------------------------------------------------------------------

function readRaw(key: string): string | null {
  try {
    return localStorage.getItem(key);
  } catch {
    return null;
  }
}

function writeRaw(key: string, value: string): void {
  try {
    localStorage.setItem(key, value);
  } catch {
    /* storage unavailable (private/blocked) — preference just won't persist */
  }
}

function parseEnvelope<T>(key: string): T | undefined {
  const raw = readRaw(key);
  if (!raw) return undefined;
  try {
    const parsed = JSON.parse(raw) as { v?: number } & T;
    if (parsed.v !== VERSION) return undefined;
    return parsed;
  } catch {
    return undefined;
  }
}

function writeEnvelope(key: string, value: object): void {
  writeRaw(key, JSON.stringify({ v: VERSION, ...value }));
}

// ---------------------------------------------------------------------------
// Pure serialize/parse — exported for unit tests
// ---------------------------------------------------------------------------

export function loadViewPreference(): WorkItemsView {
  const parsed = parseEnvelope<{ view: string }>(`${PREFIX}view`);
  return parsed?.view === "tree" || parsed?.view === "board" || parsed?.view === "archive" ? parsed.view : DEFAULT_VIEW;
}

export function saveViewPreference(view: WorkItemsView): void {
  writeEnvelope(`${PREFIX}view`, { view });
}

function normalizeFilters(raw: Partial<WorkItemFilters> | undefined): WorkItemFilters {
  // No stored envelope → the default "show everything" selection.
  if (!raw) return DEFAULT_FILTERS;
  // Malformed/corrupt envelopes fall back to the defaults too (the v
  // field is the migration hook, so future versions land here).
  if (!Array.isArray(raw.statuses) || !Array.isArray(raw.kinds)) return DEFAULT_FILTERS;
  const statuses = raw.statuses.filter((s) => Number.isInteger(s));
  const kinds = raw.kinds.filter((k) => Number.isInteger(k));
  return {
    statuses,
    kinds,
    search: typeof raw?.search === "string" ? raw.search : "",
    sortBy: typeof raw?.sortBy === "string" && raw.sortBy !== "" ? raw.sortBy : DEFAULT_FILTERS.sortBy,
    sortOrder: raw?.sortOrder === "asc" || raw?.sortOrder === "desc" ? raw.sortOrder : DEFAULT_FILTERS.sortOrder,
  };
}

export function loadFiltersPreference(projectId: string): WorkItemFilters {
  const parsed = parseEnvelope<WorkItemFilters>(`${PREFIX}filters.${projectId}`);
  return normalizeFilters(parsed);
}

export function saveFiltersPreference(projectId: string, filters: WorkItemFilters): void {
  writeEnvelope(`${PREFIX}filters.${projectId}`, filters);
}

export function loadExpandedPreference(projectId: string, kind: CollapseViewKind): Set<string> {
  const parsed = parseEnvelope<{ ids: string[] }>(`${PREFIX}${kind}Expanded.${projectId}`);
  return new Set(Array.isArray(parsed?.ids) ? parsed!.ids.filter((id) => typeof id === "string") : []);
}

export function saveExpandedPreference(
  projectId: string,
  kind: CollapseViewKind,
  ids: Set<string>,
): void {
  writeEnvelope(`${PREFIX}${kind}Expanded.${projectId}`, { ids: Array.from(ids) });
}

// Collapsed sets are the inverse of the expanded sets: an EMPTY set means
// "nothing is collapsed" (everything expanded — the board's default and
// the tree's default while a filter is active). The board stores
// collapsed ids because its default state is expanded; the tree stores
// collapsed ids for filter-mode so auto-expanded ancestors can still be
// collapsed and that choice survives navigation (ADR-WI-3).
//
// `CollapseViewKind` is the set of views that OWN a collapse slice. The Archive
// view joins Tree and Board here rather than keeping a local `useState`: the
// toolbar's Expand all / Collapse all buttons drive these persisted sets, so a
// view whose collapse state lives elsewhere is a view those buttons cannot reach
// — which is exactly why they did nothing in the archive view.
export type CollapseViewKind = "tree" | "board" | "archive";

export function loadCollapsedPreference(projectId: string, kind: CollapseViewKind): Set<string> {
  const parsed = parseEnvelope<{ ids: string[] }>(`${PREFIX}${kind}Collapsed.${projectId}`);
  return new Set(Array.isArray(parsed?.ids) ? parsed!.ids.filter((id) => typeof id === "string") : []);
}

export function saveCollapsedPreference(
  projectId: string,
  kind: CollapseViewKind,
  ids: Set<string>,
): void {
  writeEnvelope(`${PREFIX}${kind}Collapsed.${projectId}`, { ids: Array.from(ids) });
}

// parentIds returns the ids of every item that is the parent of at least
// one other item — the set of rows that can be expanded/collapsed. Used
// by the Expand all / Collapse all controls (ADR-WIT-4). Computed from
// the FULL items list (not the filtered set) so collapsing a filtered-out
// ancestor is harmless and matches the persisted-set semantics.
export function parentIds(items: WorkItem[]): string[] {
  const ids = new Set<string>();
  for (const item of items) {
    if (item.parentId) ids.add(item.parentId);
  }
  return Array.from(ids);
}

// ---------------------------------------------------------------------------
// Bulk expand/collapse — the pure decision (ADR-WIT-4)
// ---------------------------------------------------------------------------

/** Which persisted set a bulk collapse action writes, and what it holds. */
export interface CollapseEffect {
  /** the slice to write */
  slice: "board" | "archive" | "treeCollapsed" | "treeExpanded";
  /** the ids the slice should hold (empty = cleared) */
  ids: Set<string>;
}

/**
 * What "expand all" / "collapse all" MEAN for a view — as a pure function, so
 * each view's rule is a value a test can assert rather than a branch buried in a
 * hook.
 *
 * THE THREE RULES, and why they differ:
 *
 *   board / archive — rows default EXPANDED, so the persisted set is the
 *     EXPLICITLY COLLAPSED one. "Expand all" therefore CLEARS it and "collapse
 *     all" fills it with every parent id. A filter does not change this (neither
 *     view re-derives its rows from a filtered ancestor walk).
 *   tree, filter active — rows default EXPANDED too (file-explorer auto-expand so
 *     matches stay reachable), so the same rule applies to the collapsed set.
 *   tree, no filter — rows default COLLAPSED, so the persisted set is the
 *     EXPLICITLY EXPANDED one: "expand all" fills it, "collapse all" clears it.
 */
export function bulkCollapseEffect(
  view: WorkItemsView,
  filterActive: boolean,
  parentIDs: string[],
  expand: boolean,
): CollapseEffect {
  if (view === "board" || view === "archive") {
    return { slice: view, ids: expand ? new Set<string>() : new Set(parentIDs) };
  }
  if (filterActive) {
    return {
      slice: "treeCollapsed",
      ids: expand ? new Set<string>() : new Set(parentIDs),
    };
  }
  return {
    slice: "treeExpanded",
    ids: expand ? new Set(parentIDs) : new Set<string>(),
  };
}

/**
 * Apply a CollapseEffect: set the state, then persist it under the view's own
 * key.
 *
 * It takes the setters from the caller rather than reaching for module state,
 * because the hook owns them — and it persists through the SAME
 * saveCollapsedPreference/saveExpandedPreference pair `toggleTreeCollapsed` uses,
 * so a bulk action and a per-row click write the identical envelope shape.
 */
function applyCollapseEffect(
  eff: CollapseEffect,
  projectId: string,
  set: {
    board: (ids: Set<string>) => void;
    archive: (ids: Set<string>) => void;
    treeCollapsed: (ids: Set<string>) => void;
    treeExpanded: (ids: Set<string>) => void;
  },
): void {
  switch (eff.slice) {
    case "board":
      set.board(eff.ids);
      saveCollapsedPreference(projectId, "board", eff.ids);
      return;
    case "archive":
      set.archive(eff.ids);
      saveCollapsedPreference(projectId, "archive", eff.ids);
      return;
    case "treeCollapsed":
      set.treeCollapsed(eff.ids);
      saveCollapsedPreference(projectId, "tree", eff.ids);
      return;
    case "treeExpanded":
      set.treeExpanded(eff.ids);
      saveExpandedPreference(projectId, "tree", eff.ids);
      return;
  }
}

// ---------------------------------------------------------------------------
// Hook — one call site in the route shell
// ---------------------------------------------------------------------------

export interface WorkItemsPreferences {
  view: WorkItemsView;
  setView: (view: WorkItemsView) => void;
  filters: WorkItemFilters;
  setFilters: (patch: Partial<WorkItemFilters>) => void;
  /** Tree rows explicitly expanded (normal mode; default collapsed). */
  treeExpanded: Set<string>;
  toggleTreeExpanded: (id: string) => void;
  /** Tree rows explicitly collapsed while a filter is active (default expanded). */
  treeCollapsed: Set<string>;
  toggleTreeCollapsed: (id: string) => void;
  /** Board rows explicitly collapsed (default expanded). */
  boardCollapsed: Set<string>;
  toggleBoardCollapsed: (id: string) => void;
  /** Archive rows explicitly collapsed (default expanded). */
  archiveCollapsed: Set<string>;
  toggleArchiveCollapsed: (id: string) => void;
  /**
   * Expand all rows that have children in the given view. "Expand all"
   * always means *show the full tree*: board/tree-filter-active clear
   * the collapsed set; tree-without-filter adds every parent id to the
   * expanded set.
   */
  expandAll: (view: WorkItemsView, filterActive: boolean, parentIds: string[]) => void;
  /**
   * Collapse every row that has children in the given view: the inverse
   * of expandAll (sets the collapsed/expanded sets to the parent ids /
   * empty respectively).
   */
  collapseAll: (view: WorkItemsView, filterActive: boolean, parentIds: string[]) => void;
}

export function useWorkItemsPreferences(projectId: string): WorkItemsPreferences {
  const [view, setViewState] = useState<WorkItemsView>(loadViewPreference);
  const [filters, setFiltersState] = useState<WorkItemFilters>(() =>
    loadFiltersPreference(projectId),
  );
  const [treeExpanded, setTreeExpandedState] = useState<Set<string>>(() =>
    loadExpandedPreference(projectId, "tree"),
  );
  const [treeCollapsed, setTreeCollapsedState] = useState<Set<string>>(() =>
    loadCollapsedPreference(projectId, "tree"),
  );
  const [boardCollapsed, setBoardCollapsedState] = useState<Set<string>>(() =>
    loadCollapsedPreference(projectId, "board"),
  );
  const [archiveCollapsed, setArchiveCollapsedState] = useState<Set<string>>(() =>
    loadCollapsedPreference(projectId, "archive"),
  );

  // Per-project slices: re-read when the project selector changes.
  useEffect(() => {
    setFiltersState(loadFiltersPreference(projectId));
    setTreeExpandedState(loadExpandedPreference(projectId, "tree"));
    setTreeCollapsedState(loadCollapsedPreference(projectId, "tree"));
    setBoardCollapsedState(loadCollapsedPreference(projectId, "board"));
    setArchiveCollapsedState(loadCollapsedPreference(projectId, "archive"));
  }, [projectId]);

  const setView = useCallback((next: WorkItemsView) => {
    setViewState(next);
    saveViewPreference(next);
  }, []);

  const setFilters = useCallback(
    (patch: Partial<WorkItemFilters>) => {
      setFiltersState((prev) => {
        const next = { ...prev, ...patch };
        saveFiltersPreference(projectId, next);
        return next;
      });
    },
    [projectId],
  );

  const toggleTreeExpanded = useCallback(
    (id: string) => {
      setTreeExpandedState((prev) => {
        const next = new Set(prev);
        if (next.has(id)) next.delete(id);
        else next.add(id);
        saveExpandedPreference(projectId, "tree", next);
        return next;
      });
    },
    [projectId],
  );

  const toggleTreeCollapsed = useCallback(
    (id: string) => {
      setTreeCollapsedState((prev) => {
        const next = new Set(prev);
        if (next.has(id)) next.delete(id);
        else next.add(id);
        saveCollapsedPreference(projectId, "tree", next);
        return next;
      });
    },
    [projectId],
  );

  const toggleBoardCollapsed = useCallback(
    (id: string) => {
      setBoardCollapsedState((prev) => {
        const next = new Set(prev);
        if (next.has(id)) next.delete(id);
        else next.add(id);
        saveCollapsedPreference(projectId, "board", next);
        return next;
      });
    },
    [projectId],
  );

  const toggleArchiveCollapsed = useCallback(
    (id: string) => {
      setArchiveCollapsedState((prev) => {
        const next = new Set(prev);
        if (next.has(id)) next.delete(id);
        else next.add(id);
        saveCollapsedPreference(projectId, "archive", next);
        return next;
      });
    },
    [projectId],
  );

  // Bulk expand/collapse (ADR-WIT-4): the DECISION lives in one pure function
  // (`bulkCollapseEffect`), so what "expand all" means for each view is asserted
  // in a test rather than re-derived here — and so the archive view cannot be
  // silently left out of the switch a third time.
  const expandAll = useCallback(
    (view: WorkItemsView, filterActive: boolean, parentIDs: string[]) => {
      applyCollapseEffect(bulkCollapseEffect(view, filterActive, parentIDs, true), projectId, {
        board: setBoardCollapsedState,
        archive: setArchiveCollapsedState,
        treeCollapsed: setTreeCollapsedState,
        treeExpanded: setTreeExpandedState,
      });
    },
    [projectId],
  );

  const collapseAll = useCallback(
    (view: WorkItemsView, filterActive: boolean, parentIDs: string[]) => {
      applyCollapseEffect(bulkCollapseEffect(view, filterActive, parentIDs, false), projectId, {
        board: setBoardCollapsedState,
        archive: setArchiveCollapsedState,
        treeCollapsed: setTreeCollapsedState,
        treeExpanded: setTreeExpandedState,
      });
    },
    [projectId],
  );

  return {
    view,
    setView,
    filters,
    setFilters,
    treeExpanded,
    toggleTreeExpanded,
    treeCollapsed,
    toggleTreeCollapsed,
    boardCollapsed,
    toggleBoardCollapsed,
    archiveCollapsed,
    toggleArchiveCollapsed,
    expandAll,
    collapseAll,
  };
}
