// Cascade selection for the work-item tree (design §5.1, ADR-2).
//
// Fixes the reported bug: the old header select-all did a flat
// `setSelected(all ids)`, so unchecking an epic after "select all"
// left every descendant checked. Here, checking/unchecking a node
// applies to its ENTIRE subtree, parent checkboxes are tri-state, and
// the header select-all is tri-state over the visible filtered set.
//
// Selection clears whenever the visible set changes (project, status
// filter, type filter, search, or sort) via `resetKey` — the Tree and
// Board views share the same selection, so it survives a view toggle
// (the visible set is unchanged).

import { useCallback, useEffect, useRef, useState } from "react";

import type { WorkItem } from "@/api/gen/orchicon/api/v1/work_item_pb";

/**
 * All ids in the subtree rooted at `id` (node + descendants), using
 * `childrenOf` to walk the tree. Pure — exported for tests.
 */
export function subtreeIds(
  id: string,
  childrenOf: (parentId: string) => WorkItem[],
): string[] {
  const out: string[] = [id];
  const queue = [id];
  while (queue.length > 0) {
    const current = queue.shift()!;
    const children = childrenOf(current);
    for (const child of children) {
      out.push(child.id);
      queue.push(child.id);
    }
  }
  return out;
}

/** Tri-state result for a checkbox: "checked" | "indeterminate" | "unchecked". */
export function subtreeSelectionState(
  ids: string[],
  selected: Set<string>,
): "checked" | "indeterminate" | "unchecked" {
  if (ids.length === 0) return "unchecked";
  let count = 0;
  for (const id of ids) if (selected.has(id)) count++;
  if (count === 0) return "unchecked";
  if (count === ids.length) return "checked";
  return "indeterminate";
}

/** Header select-all state over a visible set of ids. */
export function visibleSelectionState(
  visibleIds: string[],
  selected: Set<string>,
): { allChecked: boolean; allIndeterminate: boolean } {
  if (visibleIds.length === 0) return { allChecked: false, allIndeterminate: false };
  let count = 0;
  for (const id of visibleIds) if (selected.has(id)) count++;
  if (count === 0) return { allChecked: false, allIndeterminate: false };
  if (count === visibleIds.length) return { allChecked: true, allIndeterminate: false };
  return { allChecked: false, allIndeterminate: true };
}

export function useWorkItemSelection(
  childrenOf: (parentId: string) => WorkItem[],
  resetKey: string,
) {
  const [selected, setSelected] = useState<Set<string>>(new Set());

  // Keep a ref so toggle/toggleAll see the latest tree without
  // re-creating their identities on every render.
  const childrenOfRef = useRef(childrenOf);
  childrenOfRef.current = childrenOf;

  // Clear the selection whenever the visible set can change.
  useEffect(() => {
    setSelected(new Set());
  }, [resetKey]);

  /** Toggle a node and its entire subtree. */
  const toggle = useCallback((id: string) => {
    setSelected((prev) => {
      const subtree = subtreeIds(id, childrenOfRef.current);
      const state = subtreeSelectionState(subtree, prev);
      const next = new Set(prev);
      if (state === "checked") {
        subtree.forEach((i) => next.delete(i));
      } else {
        subtree.forEach((i) => next.add(i));
      }
      return next;
    });
  }, []);

  /** Toggle-select exactly the given visible ids (header checkbox). */
  const toggleAll = useCallback((visibleIds: string[]) => {
    setSelected((prev) => {
      const { allChecked } = visibleSelectionState(visibleIds, prev);
      return allChecked ? new Set() : new Set(visibleIds);
    });
  }, []);

  return { selected, toggle, toggleAll, setSelected };
}
