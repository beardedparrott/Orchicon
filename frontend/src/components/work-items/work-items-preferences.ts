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
//   treeExpanded.<projectId>     → {v:1, ids:string[]}                 (collapsed by default)
//   boardExpanded.<projectId>    → {v:1, ids:string[]}                 (collapsed by default)

import { useCallback, useEffect, useState } from "react";

import type { WorkItemsView } from "@/components/work-items/work-items-filter-bar";

const PREFIX = "orchicon.workItems.";
const VERSION = 1;

export interface WorkItemFilters {
  /** OR-composed status filter; empty = all statuses */
  statuses: number[];
  /** OR-composed kind/type filter; empty = all types */
  kinds: number[];
  search: string;
  sortBy: string;
  sortOrder: string;
}

export const DEFAULT_FILTERS: WorkItemFilters = {
  statuses: [],
  kinds: [],
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
  const statuses = Array.isArray(raw?.statuses)
    ? raw!.statuses.filter((s) => Number.isInteger(s))
    : [];
  const kinds = Array.isArray(raw?.kinds)
    ? raw!.kinds.filter((k) => Number.isInteger(k))
    : [];
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

// ---------------------------------------------------------------------------
// Hook — one call site in the route shell
// ---------------------------------------------------------------------------

export interface WorkItemsPreferences {
  view: WorkItemsView;
  setView: (view: WorkItemsView) => void;
  filters: WorkItemFilters;
  setFilters: (patch: Partial<WorkItemFilters>) => void;
  treeExpanded: Set<string>;
  toggleTreeExpanded: (id: string) => void;
  boardExpanded: Set<string>;
  toggleBoardExpanded: (id: string) => void;
}

export function useWorkItemsPreferences(projectId: string): WorkItemsPreferences {
  const [view, setViewState] = useState<WorkItemsView>(loadViewPreference);
  const [filters, setFiltersState] = useState<WorkItemFilters>(() =>
    loadFiltersPreference(projectId),
  );
  const [treeExpanded, setTreeExpandedState] = useState<Set<string>>(() =>
    loadExpandedPreference(projectId, "tree"),
  );
  const [boardExpanded, setBoardExpandedState] = useState<Set<string>>(() =>
    loadExpandedPreference(projectId, "board"),
  );

  // Per-project slices: re-read when the project selector changes.
  useEffect(() => {
    setFiltersState(loadFiltersPreference(projectId));
    setTreeExpandedState(loadExpandedPreference(projectId, "tree"));
    setBoardExpandedState(loadExpandedPreference(projectId, "board"));
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

  const toggleBoardExpanded = useCallback(
    (id: string) => {
      setBoardExpandedState((prev) => {
        const next = new Set(prev);
        if (next.has(id)) next.delete(id);
        else next.add(id);
        saveExpandedPreference(projectId, "board", next);
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
    boardExpanded,
    toggleBoardExpanded,
  };
}
