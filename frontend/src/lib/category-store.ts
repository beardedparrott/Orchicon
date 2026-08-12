// Category folder organization for Workers and Workflows pages.
//
// Categories are a frontend-only organizational layer — they do NOT
// affect the backend data model. Workers and workflows remain flat on
// the server; the UI groups them by a locally-stored category assignment.
//
// Follows the `work-items-preferences.ts` versioned envelope pattern.
// All access is wrapped in try/catch for private/blocked storage modes.
// Unknown/malformed JSON falls back to defaults; the `v` field is the
// migration hook.
//
// Storage keys (prefix `orchicon.categories.`):
//   {page}                    → {v:1, categories: Category[], assignments: Record<string,string>}
//   {page}.collapsed          → {v:1, ids: string[]}
//
// Seed data: on first load (no localStorage), all items are assigned to
// "Software Development" category. Categories are collapsed by default.

import { useCallback, useEffect, useState } from "react";

const PREFIX = "orchicon.categories.";
const VERSION = 1;

export interface Category {
  id: string;
  name: string;
  description?: string;
  order: number;
}

export interface CategoryState {
  categories: Category[];
  assignments: Record<string, string>; // entityId → categoryId
}

const SEED_CATEGORY: Category = {
  id: "cat_software_dev",
  name: "Software Development",
  description: "General-purpose workers and workflows for software development tasks",
  order: 0,
};

export type CategoryPage = "workers" | "workflows";

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

export function loadCategoryState(page: CategoryPage): CategoryState {
  const parsed = parseEnvelope<CategoryState>(`${PREFIX}${page}`);
  if (!parsed || !Array.isArray(parsed.categories)) {
    return { categories: [SEED_CATEGORY], assignments: {} };
  }
  return {
    categories: parsed.categories,
    assignments: parsed.assignments && typeof parsed.assignments === "object"
      ? parsed.assignments
      : {},
  };
}

export function saveCategoryState(page: CategoryPage, state: CategoryState): void {
  writeEnvelope(`${PREFIX}${page}`, state);
}

export function loadCollapsedState(page: CategoryPage): Set<string> {
  const parsed = parseEnvelope<{ ids: string[] }>(`${PREFIX}${page}.collapsed`);
  if (Array.isArray(parsed?.ids)) {
    return new Set(parsed!.ids.filter((id) => typeof id === "string"));
  }
  // First load (no saved collapsed state): default ALL categories to collapsed.
  const categoryState = loadCategoryState(page);
  return new Set(categoryState.categories.map((c) => c.id));
}

export function saveCollapsedState(page: CategoryPage, ids: Set<string>): void {
  writeEnvelope(`${PREFIX}${page}.collapsed`, { ids: Array.from(ids) });
}

// ---------------------------------------------------------------------------
// Seed: assigns all items to "Software Development" if no assignments exist
// ---------------------------------------------------------------------------

export function seedAssignments(
  page: CategoryPage,
  entityIds: string[],
): CategoryState {
  const existing = loadCategoryState(page);
  // Only seed when no localStorage envelope exists (first load).
  // Never re-assign items that lost their assignment (e.g. after
  // deleting a category) — they stay in Uncategorized.
  if (Object.keys(existing.assignments).length > 0) {
    return existing;
  }
  // First load: assign everything to Software Development
  const assignments: Record<string, string> = {};
  for (const id of entityIds) {
    assignments[id] = SEED_CATEGORY.id;
  }
  const state = { categories: [SEED_CATEGORY], assignments };
  saveCategoryState(page, state);
  return state;
}

// ---------------------------------------------------------------------------
// Hook — one call site per route
// ---------------------------------------------------------------------------

export interface CategoryPreferences {
  state: CategoryState;
  collapsed: Set<string>;
  toggleCollapsed: (categoryId: string) => void;
  createCategory: (name: string, description?: string) => Category;
  renameCategory: (categoryId: string, newName: string) => void;
  deleteCategory: (categoryId: string) => void;
  updateDescription: (categoryId: string, description: string) => void;
  assignItem: (entityId: string, categoryId: string) => void;
  ensureSeeded: (entityIds: string[]) => void;
}

export function useCategoryPreferences(page: CategoryPage): CategoryPreferences {
  const [state, setState] = useState<CategoryState>(() => loadCategoryState(page));
  const [collapsed, setCollapsedState] = useState<Set<string>>(() => loadCollapsedState(page));

  // Reload when page changes
  useEffect(() => {
    setState(loadCategoryState(page));
    setCollapsedState(loadCollapsedState(page));
  }, [page]);

  const toggleCollapsed = useCallback(
    (categoryId: string) => {
      setCollapsedState((prev) => {
        const next = new Set(prev);
        if (next.has(categoryId)) next.delete(categoryId);
        else next.add(categoryId);
        saveCollapsedState(page, next);
        return next;
      });
    },
    [page],
  );

  const createCategory = useCallback(
    (name: string, description?: string): Category => {
      const id = `cat_${name.toLowerCase().replace(/[^a-z0-9]+/g, "_").replace(/^_|_$/g, "")}_${Date.now()}`;
      const newCat: Category = {
        id,
        name,
        description,
        order: state.categories.length,
      };
      const next = { ...state, categories: [...state.categories, newCat] };
      setState(next);
      saveCategoryState(page, next);
      // New folders are collapsed by default
      setCollapsedState((prev) => {
        const next = new Set(prev);
        next.add(id);
        saveCollapsedState(page, next);
        return next;
      });
      return newCat;
    },
    [page, state],
  );

  const renameCategory = useCallback(
    (categoryId: string, newName: string) => {
      const next = {
        ...state,
        categories: state.categories.map((c) =>
          c.id === categoryId ? { ...c, name: newName } : c,
        ),
      };
      setState(next);
      saveCategoryState(page, next);
    },
    [page, state],
  );

  const deleteCategory = useCallback(
    (categoryId: string) => {
      // Move items to uncategorized (delete their assignment)
      const assignments = { ...state.assignments };
      for (const [entityId, catId] of Object.entries(assignments)) {
        if (catId === categoryId) {
          delete assignments[entityId];
        }
      }
      const next = {
        categories: state.categories.filter((c) => c.id !== categoryId),
        assignments,
      };
      setState(next);
      saveCategoryState(page, next);
    },
    [page, state],
  );

  const updateDescription = useCallback(
    (categoryId: string, description: string) => {
      const next = {
        ...state,
        categories: state.categories.map((c) =>
          c.id === categoryId ? { ...c, description } : c,
        ),
      };
      setState(next);
      saveCategoryState(page, next);
    },
    [page, state],
  );

  const assignItem = useCallback(
    (entityId: string, categoryId: string) => {
      const assignments = { ...state.assignments };
      if (!categoryId) {
        // Move to uncategorized: remove the assignment
        delete assignments[entityId];
      } else {
        assignments[entityId] = categoryId;
      }
      const next = { ...state, assignments };
      setState(next);
      saveCategoryState(page, next);
    },
    [page, state],
  );

  const ensureSeeded = useCallback(
    (entityIds: string[]) => {
      const seeded = seedAssignments(page, entityIds);
      setState(seeded);
    },
    [page],
  );

  return {
    state,
    collapsed,
    toggleCollapsed,
    createCategory,
    renameCategory,
    deleteCategory,
    updateDescription,
    assignItem,
    ensureSeeded,
  };
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

export function getItemsForCategory(
  state: CategoryState,
  allItemIds: string[],
): { categorized: Map<string, string[]>; uncategorized: string[] } {
  const categorized = new Map<string, string[]>();
  const uncategorized: string[] = [];

  for (const id of allItemIds) {
    const catId = state.assignments[id];
    if (catId) {
      const list = categorized.get(catId);
      if (list) list.push(id);
      else categorized.set(catId, [id]);
    } else {
      uncategorized.push(id);
    }
  }

  return { categorized, uncategorized };
}
