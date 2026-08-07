// Pure dependency-presentation helpers (design §5.2, ADR-3).
//
// Dependencies are DAG edges (BLOCKS / DEPENDS_ON / RELATES_TO),
// distinct from parent/child links. This module derives a pure
// presentation index from the server graph — no business logic, just
// derived server state (AGENTS.md invariant #1). All rules here are
// ADVISORY: the server (TaskReconciler) stays authoritative.

import type {
  DependencyGraph,
  WorkItem,
  WorkItemDependency,
} from "@/api/gen/orchicon/api/v1/work_item_pb";

import { DependencyType, isTerminal } from "@/components/work-items/work-item-meta";

// Re-export for callers; single source of truth lives in work-item-meta.
export { isTerminal };

/**
 * Index of which items are blocked (depend on an unfinished item) and
 * which items block others (are depended on by an unfinished item).
 *
 * - `blockedBy.get(id)` → the non-terminal items this item depends on.
 * - `blocks.get(id)`    → the non-terminal items that depend on this item.
 *
 * Only BLOCKS / DEPENDS_ON edges block; RELATES_TO never does. An edge to
 * a terminal item (succeeded/failed/cancelled) stops blocking because the
 * blocker already finished.
 */
export function computeBlockState(
  nodes: WorkItem[] | undefined,
  edges: WorkItemDependency[] | undefined,
): {
  blockedBy: Map<string, WorkItem[]>;
  blocks: Map<string, WorkItem[]>;
} {
  const blockedBy = new Map<string, WorkItem[]>();
  const blocks = new Map<string, WorkItem[]>();

  const nodeById = new Map<string, WorkItem>((nodes ?? []).map((n) => [n.id, n]));

  for (const edge of edges ?? []) {
    if (edge.type !== DependencyType.BLOCKS && edge.type !== DependencyType.DEPENDS_ON) {
      continue; // RELATES_TO never blocks
    }
    const blocker = nodeById.get(edge.fromId);
    const dependent = nodeById.get(edge.toId);
    if (!blocker || !dependent) continue;
    if (isTerminal(blocker.status)) continue; // finished blockers don't block

    const deps = blockedBy.get(edge.toId);
    if (deps) deps.push(blocker);
    else blockedBy.set(edge.toId, [blocker]);

    const blockersList = blocks.get(edge.fromId);
    if (blockersList) blockersList.push(dependent);
    else blocks.set(edge.fromId, [dependent]);
  }

  return { blockedBy, blocks };
}

/** Titles of the items blocking `id`, for tooltips/toasts. */
export function blockingTitles(
  blockedBy: Map<string, WorkItem[]>,
  id: string,
  limit = 3,
): string {
  const items = blockedBy.get(id) ?? [];
  const titles = items.slice(0, limit).map((i) => i.title);
  const rest = items.length - titles.length;
  if (rest > 0) titles.push(`+${rest} more`);
  return titles.join(", ");
}

// ---------------------------------------------------------------------------
// Client-side filtering helpers (design §5.4). These exist so the page
// shell can compute ONE visible set shared by the filter bar's
// select-all/count and the active view. Kind, status AND search are all
// applied client-side over the full fetched set (pageSize 1000) so the
// tree hierarchy stays intact — a server-side filter would return only
// the matching rows, orphaning their children/parents under invisible
// rows and breaking the tree (a searched task would lose its epic).
// ---------------------------------------------------------------------------

/**
 * Free-text match mirroring the server's search semantics
 * (`title ILIKE %q% OR description ILIKE %q%`, case-insensitive —
 * internal/db/work_item.go). Client-side so search results keep their
 * ancestors in the tree.
 */
export function matchesSearch(item: WorkItem, search: string): boolean {
  const q = search.trim().toLowerCase();
  if (!q) return true;
  return (
    item.title.toLowerCase().includes(q) ||
    (item.description ?? "").toLowerCase().includes(q)
  );
}

/**
 * Filter the items by kind/status/search. OR within the kind and status
 * groups, AND across groups.
 *
 * An EMPTY `kinds` or `statuses` array matches NOTHING (ADR-WI-6): the
 * page defaults both to every option selected, so an empty selection
 * means the user actively unchecked everything and expects an empty
 * view — not an unfiltered one. The caller (the page shell) is
 * responsible for passing the full option list as the default.
 */
export function filterItemsByKindStatus(
  items: WorkItem[] | undefined,
  kinds: number[],
  statuses: number[],
  search = "",
): WorkItem[] {
  return (items ?? []).filter(
    (i) =>
      matchesSearch(i, search) &&
      kinds.includes(i.kind) &&
      statuses.includes(i.status),
  );
}

export interface TreeData {
  /** items that pass the kind/status/search filters (the "matches") */
  matches: WorkItem[];
  /** matches + their ancestors, so filtered results are reachable under
   *  (possibly non-matching) parents — file-explorer behavior */
  treeItems: WorkItem[];
  /** ids of ancestor-only rows (non-matches shown as containers) */
  ancestorIds: Set<string>;
}

export function buildTreeData(
  items: WorkItem[] | undefined,
  kinds: number[],
  statuses: number[],
  search = "",
): TreeData {
  const all = items ?? [];
  const matches = filterItemsByKindStatus(all, kinds, statuses, search);
  const byId = new Map(all.map((i) => [i.id, i]));
  const ancestors = new Map<string, WorkItem>();

  for (const item of matches) {
    let parentId = item.parentId;
    let guard = 0;
    while (parentId && byId.has(parentId) && guard++ < 10) {
      const parent = byId.get(parentId)!;
      if (!ancestors.has(parent.id)) ancestors.set(parent.id, parent);
      parentId = parent.parentId;
    }
  }

  // treeItems = matches + ancestor-only rows (ancestors that are also
  // matches must not be duplicated).
  const seen = new Set(matches.map((i) => i.id));
  const extra: WorkItem[] = [];
  for (const ancestor of ancestors.values()) {
    if (!seen.has(ancestor.id)) {
      extra.push(ancestor);
      seen.add(ancestor.id);
    }
  }

  return {
    matches,
    treeItems: [...matches, ...extra],
    ancestorIds: new Set(ancestors.keys()),
  };
}

export type BlockState = ReturnType<typeof computeBlockState>;
export type { DependencyGraph };
