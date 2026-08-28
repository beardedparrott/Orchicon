// Server-backed category store with local collapsed cache + one-time localStorage seeder.
//
// Categories and assignments are now tenant-scoped server state via CategoryService
// (target_type partitioned: worker | workflow | conversation). Collapse state
// stays local (not server-meaningful). The old localStorage keys
// `orchicon.categories.{workers,workflows,conversations}` become a cache +
// one-time seeder on first load after deploy. This fixes mobile persistence:
// sheet close / viewport change / reload no longer loses state because
// categories are fetched from the server on every mount.

import { useCallback, useEffect, useMemo, useState, useRef } from "react";
import {
  useListCategories,
  useCreateCategory,
  useUpdateCategory,
  useDeleteCategory,
  useAssignToCategory,
  useUnassignFromCategory,
  type CategoryTargetType,
  type CategoryDTO,
} from "@/api/categories";
import { categoryClient } from "@/api/clients";
import { CategoryTargetType as ProtoTargetType } from "@/api/gen/orchicon/api/v1/category_pb";

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
  assignments: Record<string, string>;
}

export type CategoryPage = "workers" | "workflows" | "conversations";

function pageToTarget(page: CategoryPage): CategoryTargetType {
  if (page === "workers") return "worker";
  if (page === "workflows") return "workflow";
  return "conversation";
}

function protoTarget(page: CategoryPage): ProtoTargetType {
  const t = pageToTarget(page);
  if (t === "worker") return ProtoTargetType.WORKER;
  if (t === "workflow") return ProtoTargetType.WORKFLOW;
  return ProtoTargetType.CONVERSATION;
}

function dtoToCategory(dto: CategoryDTO): Category {
  return { id: dto.id, name: dto.name, description: dto.description || undefined, order: dto.sortOrder ?? 0 };
}

// --- local collapsed helpers (stays local) ---
function readRaw(key: string): string | null {
  try { return localStorage.getItem(key); } catch { return null; }
}
function writeRaw(key: string, value: string): void {
  try { localStorage.setItem(key, value); } catch { /* ignore */ }
}
function parseEnvelope<T>(key: string): T | undefined {
  const raw = readRaw(key);
  if (!raw) return undefined;
  try {
    const parsed = JSON.parse(raw) as { v?: number } & T;
    if (parsed.v !== VERSION) return undefined;
    return parsed;
  } catch { return undefined; }
}
function writeEnvelope(key: string, value: object): void {
  writeRaw(key, JSON.stringify({ v: VERSION, ...value }));
}
function hasEnvelope(key: string): boolean {
  return readRaw(key) !== null;
}

export function loadCollapsedState(page: CategoryPage): Set<string> {
  const parsed = parseEnvelope<{ ids: string[] }>(`${PREFIX}${page}.collapsed`);
  if (Array.isArray(parsed?.ids)) return new Set(parsed!.ids.filter((id) => typeof id === "string"));
  return new Set<string>();
}
export function saveCollapsedState(page: CategoryPage, ids: Set<string>): void {
  writeEnvelope(`${PREFIX}${page}.collapsed`, { ids: Array.from(ids) });
}

// Kept for tests / fallback: read local cache if server is unreachable.
export function loadCategoryState(page: CategoryPage, noSeed?: boolean): CategoryState {
  const parsed = parseEnvelope<CategoryState>(`${PREFIX}${page}`);
  if (!parsed || !Array.isArray(parsed.categories)) {
    if (noSeed) return { categories: [], assignments: {} };
    return { categories: [], assignments: {} };
  }
  return {
    categories: parsed.categories,
    assignments: parsed.assignments && typeof parsed.assignments === "object" ? parsed.assignments : {},
  };
}
export function saveCategoryState(page: CategoryPage, state: CategoryState): void {
  writeEnvelope(`${PREFIX}${page}`, state);
}

const SEED_DONE_KEY = "orchicon.categories.seeded.";
function hasSeeded(page: CategoryPage): boolean {
  try { return localStorage.getItem(SEED_DONE_KEY + page) === "1"; } catch { return true; }
}
function markSeeded(page: CategoryPage) {
  try { localStorage.setItem(SEED_DONE_KEY + page, "1"); } catch { /* ignore */ }
}

export function seedAssignments(page: CategoryPage, entityIds: string[]): CategoryState {
  if (hasEnvelope(`${PREFIX}${page}`)) {
    return loadCategoryState(page);
  }
  const assignments: Record<string, string> = {};
  for (const id of entityIds) assignments[id] = "cat_software_dev";
  const state: CategoryState = { categories: [{ id: "cat_software_dev", name: "Software Development", description: "General-purpose workers and workflows for software development tasks", order: 0 }], assignments };
  saveCategoryState(page, state);
  return state;
}

// --- Hook ---
export interface CategoryPreferences {
  state: CategoryState;
  collapsed: Set<string>;
  toggleCollapsed: (categoryId: string) => void;
  createCategory: (name: string, description?: string) => Category | void;
  renameCategory: (categoryId: string, newName: string) => void;
  deleteCategory: (categoryId: string) => void;
  updateDescription: (categoryId: string, description: string) => void;
  assignItem: (entityId: string, categoryId: string) => void;
  ensureSeeded: (entityIds: string[]) => void;
  isLoading: boolean;
}

export function useCategoryPreferences(page: CategoryPage, options?: { noSeed?: boolean }): CategoryPreferences {
  const targetType = pageToTarget(page);
  const { data, isLoading } = useListCategories(targetType);

  const createMut = useCreateCategory(targetType);
  const updateMut = useUpdateCategory(targetType);
  const deleteMut = useDeleteCategory(targetType);
  const assignMut = useAssignToCategory(targetType);
  const unassignMut = useUnassignFromCategory(targetType);

  const [collapsed, setCollapsedState] = useState<Set<string>>(() => loadCollapsedState(page));
  useEffect(() => { setCollapsedState(loadCollapsedState(page)); }, [page]);

  const seedingRef = useRef(false);
  useEffect(() => {
    if (options?.noSeed) return;
    if (seedingRef.current) return;
    if (isLoading) return;
    if (hasSeeded(page)) return;
    const local = loadCategoryState(page);
    const serverEmpty = !data || data.categories.length === 0;
    if (!serverEmpty) { markSeeded(page); return; }
    if (!local.categories.length && Object.keys(local.assignments).length === 0) { markSeeded(page); return; }
    seedingRef.current = true;
    (async () => {
      const idMap = new Map<string, string>();
      for (const cat of local.categories) {
        try {
          const res = await categoryClient.createCategory({ targetType: protoTarget(page), name: cat.name, description: cat.description ?? "" });
          const serverId = (res.category as unknown as { id: string })?.id ?? "";
          if (serverId) idMap.set(cat.id, serverId);
        } catch { /* best-effort */ }
      }
      for (const [entityId, localCatId] of Object.entries(local.assignments)) {
        const serverCatId = idMap.get(localCatId);
        if (!serverCatId) continue;
        try {
          await categoryClient.assignToCategory({ categoryId: serverCatId, entityId, targetType: protoTarget(page) });
        } catch { /* ignore */ }
      }
      markSeeded(page);
      seedingRef.current = false;
    })();
  }, [page, data, isLoading, options?.noSeed]);

  const state: CategoryState = useMemo(() => {
    const categories = (data?.categories ?? []).map(dtoToCategory).sort((a, b) => a.order - b.order);
    const assignments: Record<string, string> = {};
    for (const a of data?.assignments ?? []) {
      assignments[a.entityId] = a.categoryId;
    }
    try { saveCategoryState(page, { categories, assignments }); } catch { /* ignore */ }
    return { categories, assignments };
  }, [data, page]);

  const toggleCollapsed = useCallback((categoryId: string) => {
    setCollapsedState((prev) => {
      const next = new Set(prev);
      if (next.has(categoryId)) next.delete(categoryId); else next.add(categoryId);
      saveCollapsedState(page, next);
      return next;
    });
  }, [page]);

  const createCategory = useCallback((name: string, description?: string) => {
    createMut.mutate({ name, description });
    return undefined;
  }, [createMut]);

  const renameCategory = useCallback((categoryId: string, newName: string) => {
    updateMut.mutate({ id: categoryId, name: newName });
  }, [updateMut]);

  const deleteCategory = useCallback((categoryId: string) => {
    deleteMut.mutate(categoryId);
  }, [deleteMut]);

  const updateDescription = useCallback((categoryId: string, description: string) => {
    updateMut.mutate({ id: categoryId, description });
  }, [updateMut]);

  const assignItem = useCallback((entityId: string, categoryId: string) => {
    if (!categoryId) unassignMut.mutate(entityId);
    else assignMut.mutate({ categoryId, entityId });
  }, [assignMut, unassignMut]);

  const ensureSeeded = useCallback((_entityIds: string[]) => {}, []);

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
    isLoading,
  };
}

export function getItemsForCategory(state: CategoryState, allItemIds: string[]): { categorized: Map<string, string[]>; uncategorized: string[] } {
  const categorized = new Map<string, string[]>();
  const uncategorized: string[] = [];
  for (const id of allItemIds) {
    const catId = state.assignments[id];
    if (catId) {
      const list = categorized.get(catId);
      if (list) list.push(id); else categorized.set(catId, [id]);
    } else uncategorized.push(id);
  }
  return { categorized, uncategorized };
}
