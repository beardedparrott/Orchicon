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

import type { WorkItemsView } from "@/components/work-items/work-items-filter-bar";
import {
  ALL_KIND_VALUES,
  ALL_STATUS_VALUES,
} from "@/components/work-items/work-item-meta";

const PREFIX = "orchicon.workItems.";
const VERSION = 2;

export interface WorkItemFilters {
  /** OR-composed status filter; empty = nothing matches */
  statuses: number[];
  /** OR-composed kind/type filter; empty = nothing matches */
  kinds: number[];
  search: string;
  sortBy: string;
  sortOrder: string;
}

export const DEFAULT_FILTERS: WorkItemFilters = {
  statuses: ALL_STATUS_VALUES,
  kinds: ALL_KIND_VALUES,
  search: "",
  sortBy: "created_at",
  sortOrder: "desc",
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
  return parsed?.view === "tree" || parsed?.view === "board" ? parsed.view : DEFAULT_VIEW;
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

export function loadExpandedPreference(projectId: string, kind: "tree" | "board"): Set<string> {
  const parsed = parseEnvelope<{ ids: string[] }>(`${PREFIX}${kind}Expanded.${projectId}`);
  return new Set(Array.isArray(parsed?.ids) ? parsed!.ids.filter((id) => typeof id === "string") : []);
}

export function saveExpandedPreference(
  projectId: string,
  kind: "tree" | "board",
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
export function loadCollapsedPreference(projectId: string, kind: "tree" | "board"): Set<string> {
  const parsed = parseEnvelope<{ ids: string[] }>(`${PREFIX}${kind}Collapsed.${projectId}`);
  return new Set(Array.isArray(parsed?.ids) ? parsed!.ids.filter((id) => typeof id === "string") : []);
}

export function saveCollapsedPreference(
  projectId: string,
  kind: "tree" | "board",
  ids: Set<string>,
): void {
  writeEnvelope(`${PREFIX}${kind}Collapsed.${projectId}`, { ids: Array.from(ids) });
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

  // Per-project slices: re-read when the project selector changes.
  useEffect(() => {
    setFiltersState(loadFiltersPreference(projectId));
    setTreeExpandedState(loadExpandedPreference(projectId, "tree"));
    setTreeCollapsedState(loadCollapsedPreference(projectId, "tree"));
    setBoardCollapsedState(loadCollapsedPreference(projectId, "board"));
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
  };
}
