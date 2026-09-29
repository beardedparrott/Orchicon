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

// ---------------------------------------------------------------------------
// Archive view tree (option A — archived-only tree with active ghost
// anchors). Archived subtrees are self-contained (an archived parent's
// children are archived too — the block-don't-cascade archive rule holds
// in the data), but an archived item's parent can be ACTIVE: the sequence
// engine / operators finish and archive a leaf while its epic stays open.
// Walking the parentId chain needs BOTH partitions, so callers pass the
// active items separately (a second, lazily-enabled query) rather than
// mixing archive semantics into the one archived-only list query.
// ---------------------------------------------------------------------------

export interface ArchiveTreeData {
  /** archived items + any active ancestors needed to connect them */
  treeItems: WorkItem[];
  /** ids of the active "ghost anchor" rows (non-restorable, non-selectable) */
  activeAnchorIds: Set<string>;
}

/**
 * Builds the archive view's tree: every archived item, plus — for any
 * archived item whose ancestor chain crosses into an ACTIVE item — the
 * active ancestors needed to render that connection as muted anchor rows.
 * `activeItems` only needs to cover items that might be ancestors of the
 * archived set (the page passes the full active list for the project).
 */
export function buildArchiveTreeData(
  archivedItems: WorkItem[] | undefined,
  activeItems: WorkItem[] | undefined,
): ArchiveTreeData {
  const archived = archivedItems ?? [];
  const activeById = new Map((activeItems ?? []).map((i) => [i.id, i]));
  const archivedById = new Map(archived.map((i) => [i.id, i]));

  const activeAnchors = new Map<string, WorkItem>();
  for (const item of archived) {
    let parentId = item.parentId;
    let guard = 0;
    while (parentId && guard++ < 10) {
      if (archivedById.has(parentId)) {
        // Archived ancestor: already in treeItems, keep walking above it.
        parentId = archivedById.get(parentId)!.parentId;
        continue;
      }
      const activeParent = activeById.get(parentId);
      if (!activeParent) break; // parent not resolvable (deleted, or out of scope)
      if (!activeAnchors.has(activeParent.id)) activeAnchors.set(activeParent.id, activeParent);
      parentId = activeParent.parentId;
    }
  }

  return {
    treeItems: [...archived, ...activeAnchors.values()],
    activeAnchorIds: new Set(activeAnchors.keys()),
  };
}

/**
 * Orders ids child-first (bottom-up) by parentId depth within `itemsById`,
 * so a subtree restore issues child requests before their parent — a
 * clean sequence with no parent momentarily "restored" ahead of children
 * still archived. Ids outside `itemsById` sort first (depth 0 is safest
 * last, so unknown-depth items don't block behind a real chain).
 */
export function bottomUpOrder(ids: string[], itemsById: Map<string, WorkItem>): string[] {
  const depthOf = (id: string): number => {
    let depth = 0;
    let current = itemsById.get(id);
    let guard = 0;
    while (current?.parentId && itemsById.has(current.parentId) && guard++ < 10) {
      depth++;
      current = itemsById.get(current.parentId);
    }
    return depth;
  };
  return [...ids].sort((a, b) => depthOf(b) - depthOf(a));
}
