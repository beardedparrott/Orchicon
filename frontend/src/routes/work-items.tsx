// Work items page (design-notes/complete-ui-and-functionality-overhaul-of-work-item-page.md).
// Thin route shell: owns filter state, the shared selection, and the
// list/graph queries; delegates rendering to the shared work-items
// module. Provides the Tree view (Epic → Feature → Task → Subtask
// hierarchy with cascade selection) and the Kanban board (dnd-kit drag &
// drop, server-confirmed status transitions).
//
// Auto-refresh (design §5.5): the list + graph queries poll every 5s,
// pause while the tab is hidden (TanStack default
// refetchIntervalInBackground=false), refetch on window focus, and the
// LiveRefreshIndicator in the header makes it visible.

import { createRoute, Link } from "@tanstack/react-router";
import { useCallback, useEffect, useMemo, useState } from "react";

import {
  useBatchArchiveWorkItems,
  useBatchDeleteWorkItems,
  useGetDependencyGraph,
  useListWorkItems,
  useReorderWorkItems,
  useRestoreWorkItem,
} from "@/api/workItems";
import { useListProjects } from "@/api/projects";
import { useListExecutions } from "@/api/executions";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { TooltipProvider } from "@/components/ui/tooltip";
import { useToast } from "@/components/ui/toast";
import { useBatchMoveWorkItems } from "@/components/work-items/batch-move";
import { useBatchRunWorkItems } from "@/components/work-items/batch-run";
import { computeBlockState, buildTreeData, filterItemsByKindStatus } from "@/components/work-items/dependency-utils";
import {
  KIND_FILTER_OPTIONS,
  STATUS_FILTER_OPTIONS,
} from "@/components/work-items/work-item-meta";
import {
  useWorkItemSelection,
  visibleSelectionState,
} from "@/components/work-items/use-work-item-selection";
import {
  WorkItemsBoard,
} from "@/components/work-items/work-items-board";
import {
  WorkItemsFilterBar,
} from "@/components/work-items/work-items-filter-bar";
import { WorkItemsArchiveView } from "@/components/work-items/work-items-archive-view";
import { useDebouncedValue } from "@/components/work-items/use-debounced-value";
import { WorkItemsTree } from "@/components/work-items/work-items-tree";
import { useWorkItemsPreferences, parentIds } from "@/components/work-items/work-items-preferences";
import { cn } from "@/lib/utils";
import { RecurringFilter } from "@/api/gen/orchicon/api/v1/work_item_service_pb";
import { isTerminalExecutionStatus, type PrRun } from "@/lib/pr";
import { Route as rootRoute } from "@/routes/__root";

export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/work-items",
  component: WorkItemsPage,
});

function WorkItemsPage() {
  const { data: projects } = useListProjects();
  const [projectId, setProjectId] = useState<string>("");

  // Persisted view preferences (ADR-WI-1/ADR-WI-3/ADR-WI-6): the last
  // default view, per-project filters, and per-project expand sets all
  // survive navigation and reload via localStorage.
  const {
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
    expandAll,
    collapseAll,
  } = useWorkItemsPreferences(projectId);

  const debouncedSearch = useDebouncedValue(filters.search, 300);
  const { statuses, kinds, sortBy, sortOrder } = filters;

  const hasProjects = projects && projects.length > 0;
  const batchDelete = useBatchDeleteWorkItems();
  const batchArchive = useBatchArchiveWorkItems();
  const { moveItems, isPending: movePending } = useBatchMoveWorkItems(projectId);
  const runWorkItems = useBatchRunWorkItems(projectId);
  const toast = useToast();
  const reorder = useReorderWorkItems();
  const restoreWorkItem = useRestoreWorkItem(projectId);
  const handleRestore = (id: string) => {
    restoreWorkItem.mutate(id, {
      onSuccess: (item) => {
        toast.success(`Restored "${item.title}" to the active views.`);
      },
      onError: (e) => {
        toast.error(`Failed to restore: ${String(e)}`);
      },
    });
  };
  const handleReorder = (parentId: string, childIds: string[]) => {
    // The RPC requires the siblings' project — derive it from the items
    // themselves so reorder works in the "All projects" view too (the
    // page's projectId is empty there).
    const sibling = (items ?? []).find((i) => i.id === childIds[0]);
    const pid = sibling?.projectId ?? projectId;
    if (!pid) {
      toast.error("Cannot reorder: the work item has no project.");
      return;
    }
    reorder.mutate({ projectId: pid, parentId, childIds });
  };

  // Server state (design §3): list + DAG, both auto-refreshed. The shell
  // owns the queries so the filter bar's select-all/count and the shared
  // selection see exactly the same data as the active view.
  //
  // Search is intentionally NOT sent to the server: a server-side search
  // returns only the matching rows, orphaning a searched task's epic and
  // leaving an empty tree. We fetch the full page (pageSize 1000) and
  // apply search + kind + status client-side (design §5.4) so filtered
  // results keep their ancestors (file-explorer behavior).
  const {
    data: items,
    isLoading,
    error,
    dataUpdatedAt,
    isFetching,
  } = useListWorkItems(projectId, {
    sortBy: sortBy || undefined,
    sortOrder: sortOrder || undefined,
    // The archive view is the ONLY caller that opts in to archived items;
    // every active view (tree/board) leaves this false so archived items
    // never surface in them.
    includeArchived: view === "archive",
    recurringFilter: RecurringFilter.EXCLUDE_RECURRING,
  });
  const { data: graph } = useGetDependencyGraph(projectId, { refetchInterval: 5_000 });

  // Run/PR visibility (parallel board view): executions carry the run's
  // branch/worktree and PR surface (mirrored at dispatch), so group them by
  // work item (taskId) to render a per-run footer on the board/list cards.
  // Reuses useListExecutions (MVP path) — no new RPC. Polling matches the
  // board's 5s rhythm so concurrent runs stay fresh.
  const { data: executions } = useListExecutions({
    projectId: projectId || undefined,
  });
  const runsByItem = useMemo(() => {
    const m = new Map<string, PrRun[]>();
    for (const e of executions ?? []) {
      if (!e.taskId) continue;
      const pr: PrRun = {
        prUrl: e.prUrl || undefined,
        prState: e.prState || undefined,
        worktreeBranch: e.worktreeBranch || undefined,
        completed: isTerminalExecutionStatus(e.status),
      };
      const list = m.get(e.taskId);
      if (list) list.push(pr);
      else m.set(e.taskId, [pr]);
    }
    return m;
  }, [executions]);

  const blockState = useMemo(
    () => computeBlockState(graph?.nodes, graph?.edges),
    [graph],
  );

  // Client-side filtering (design §5.4): kind + status + search compose
  // over the full fetched set so the tree hierarchy stays intact. OR
  // within kind/status groups, AND across groups, empty = all (ADR-WI-6).
  const filteredItems = useMemo(
    () => filterItemsByKindStatus(items, kinds, statuses, debouncedSearch),
    [items, kinds, statuses, debouncedSearch],
  );
  const treeData = useMemo(
    () => buildTreeData(items, kinds, statuses, debouncedSearch),
    [items, kinds, statuses, debouncedSearch],
  );

  // A filter is "active" only when the selection differs from the full
  // option list (the default = show everything) or a search is typed. An
  // all-selected type/status filter is NOT a query: it must not dim rows,
  // auto-expand the tree, or say "No matching work items" (ADR-WI-6).
  const hasQuery =
    debouncedSearch !== "" ||
    kinds.length !== KIND_FILTER_OPTIONS.length ||
    statuses.length !== STATUS_FILTER_OPTIONS.length;

  // The header select-all + count operate over the MATCHES (the items
  // that actually pass the filters), never the dimmed ancestor container
  // rows — otherwise "select all" under a type filter would mark
  // filtered-out epics/features for bulk delete.
  const visibleItems = view === "tree" ? treeData.matches : filteredItems;
  const visibleIds = useMemo(() => visibleItems.map((i) => i.id), [visibleItems]);
  const visibleIdsSet = useMemo(() => new Set(visibleIds), [visibleIds]);

  // Selection (design §5.1): cascade selection — when a filter is active
  // the parent toggle is scoped to the currently-viewable descendants
  // only (visibleIdsSet + hasQuery), so hidden items are never selected.
  // With no filter, the full subtree is used (AC5).
  const resetKey = [projectId, statuses.join(","), kinds.join(","), debouncedSearch, sortBy, sortOrder].join("|");
  const childrenOf = useCallback(
    (parentId: string) => (items ?? []).filter((i) => i.parentId === parentId),
    [items],
  );
  const { selected, toggle, toggleAll, setSelected } = useWorkItemSelection(childrenOf, visibleIdsSet, hasQuery, resetKey);
  const { allChecked, allIndeterminate } = visibleSelectionState(visibleIds, selected);

  const handleToggleAll = () => toggleAll(visibleIds);

  const handleBatchDelete = () => {
    if (selected.size === 0) return;
    const count = selected.size;
    if (!window.confirm(`Permanently delete ${count} work item${count === 1 ? "" : "s"}? This cannot be undone.`)) return;
    batchDelete.mutate(Array.from(selected), {
      onSuccess: () => setSelected(new Set()),
    });
  };

  // Bulk "Archive" (mirrors handleBatchDelete): confirm naming the count,
  // fan out the archive fan-out, clear the selection on completion, and
  // report the archived/skipped split. The server is authoritative for the
  // two gates (terminal-only, no children), so skipped items are the ones
  // the server rejected — reported as one generic bucket (design D2).
  const handleBatchArchive = () => {
    if (selected.size === 0) return;
    const count = selected.size;
    if (
      !window.confirm(
        `Archive ${count} work item${count === 1 ? "" : "s"}? Only terminal items without children can be archived; the rest are skipped.`,
      )
    ) return;
    batchArchive.mutate(Array.from(selected), {
      onSuccess: (res) => {
        setSelected(new Set());
        if (res.skipped > 0) {
          toast.success(
            `Archived ${res.archived}, skipped ${res.skipped} (non-terminal or has children)`,
          );
        } else {
          toast.success(`Archived ${res.archived}`);
        }
      },
    });
  };

  // Bulk "Move to…" (ADR-WI-5): the selection toolbar's keyboard path for
  // multi-move, sharing the exact gates + mutation path with board
  // multi-drag.
  const itemsById = useMemo(() => new Map((items ?? []).map((i) => [i.id, i])), [items]);

  // Ids that have at least one direct child — used to detect sequence
  // parents (a task/subtask with children runs its children in chain order)
  // so the Run button can start them without a workflow binding.
  const parentIdSet = useMemo(
    () => new Set((items ?? []).filter((i) => i.parentId !== "").map((i) => i.parentId)),
    [items],
  );

  const handleMoveSelected = (targetStatus: number) => {
    if (selected.size === 0) return;
    void moveItems(Array.from(selected), targetStatus, { itemsById, blockState });
  };

  // Bulk "Run" (ADR-WI-9): act on the visible-selected set only — the header
  // select-all and parent cascade can mark filtered-out descendants, but Run
  // must only touch the currently-visible rows (AC-3). The label uses the
  // visible-selected count so it never over-promises (AC-1).
  const visibleSelectedCount = useMemo(
    () => [...selected].filter((id) => visibleIdsSet.has(id)).length,
    [selected, visibleIdsSet],
  );
  const handleRunSelected = () => {
    if (selected.size === 0) return;
    const runIds = [...selected].filter((id) => visibleIdsSet.has(id));
    void runWorkItems.runSelected(runIds, { itemsById, parentIdSet });
  };



  // Expand/Collapse all (ADR-WIT-4): the parent ids come from the FULL
  // items list (not the filtered set) so collapsing a filtered-out
  // ancestor is harmless. The buttons are disabled when the action is
  // already the default state for the active view.
  const parentIDs = useMemo(() => parentIds(items ?? []), [items]);
  const hasParents = parentIDs.length > 0;
  const expandAllDisabled =
    !hasParents ||
    (view === "board"
      ? boardCollapsed.size === 0
      : hasQuery
        ? treeCollapsed.size === 0
        : parentIDs.every((p) => treeExpanded.has(p)));
  const collapseAllDisabled =
    !hasParents ||
    (view === "board"
      ? false
      : hasQuery
        ? parentIDs.every((p) => treeCollapsed.has(p))
        : treeExpanded.size === 0);
  const handleExpandAll = () => expandAll(view, hasQuery, parentIDs);
  const handleCollapseAll = () => collapseAll(view, hasQuery, parentIDs);

  return (
    <div className="flex flex-col gap-6 min-h-0" style={{ minHeight: "calc(100vh - 64px)" }}>
      <div className="flex flex-wrap items-center justify-between gap-4 shrink-0">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">Work Items</h1>
          <p className="text-sm text-muted-foreground">
            The work hierarchy: Project → Epic → Feature → Task → Subtask.
            Dependencies form a DAG between items.
          </p>
        </div>
        <div className="flex items-center gap-2">
          <LiveRefreshIndicator lastUpdated={dataUpdatedAt} isFetching={isFetching} />
          <Button asChild>
            <Link to="/work-items/new" search={{ projectId: projectId ?? "", parentId: "" }}>
              New Work Item
            </Link>
          </Button>
        </div>
      </div>

      <WorkItemsFilterBar
        projects={projects}
        projectId={projectId}
        onProjectChange={setProjectId}
        search={filters.search}
        onSearchChange={(value) => setFilters({ search: value })}
        statuses={statuses}
        onStatusFilterChange={(next) => setFilters({ statuses: next })}
        kinds={kinds}
        onKindFilterChange={(next) => setFilters({ kinds: next })}
        sortBy={sortBy}
        onSortByChange={(value) =>
          // Chain order ("") is always ascending — reset a leftover desc
          // direction so the server's `ORDER BY sort_order NULLS LAST,
          // created_at ASC` (not a reversed chain) applies.
          setFilters(value === "" ? { sortBy: value, sortOrder: "asc" } : { sortBy: value })
        }
        sortOrder={sortOrder}
        onSortOrderChange={(value) => setFilters({ sortOrder: value })}
        view={view}
        onViewChange={setView}
        onExpandAll={handleExpandAll}
        onCollapseAll={handleCollapseAll}
        expandAllDisabled={expandAllDisabled}
        collapseAllDisabled={collapseAllDisabled}
        visibleCount={visibleItems.length}
        selectedCount={selected.size}
        allChecked={allChecked}
        allIndeterminate={allIndeterminate}
        onToggleAll={handleToggleAll}
        onDeleteSelected={handleBatchDelete}
        deletePending={batchDelete.isPending}
        onArchiveSelected={handleBatchArchive}
        archivePending={batchArchive.isPending}
        onMoveSelected={handleMoveSelected}
        movePending={movePending}
        onRunSelected={handleRunSelected}
        runPending={runWorkItems.isPending}
        visibleSelectedCount={visibleSelectedCount}
      />

      {!hasProjects && (
        <Card>
          <CardHeader>
            <CardTitle>No project selected</CardTitle>
            <CardDescription>
              Create a project first to start adding work items.
            </CardDescription>
          </CardHeader>
        </Card>
      )}

      {hasProjects && (
        <div className="flex flex-1 min-h-0 flex-col">
          {!projectId && (
            <p className="text-xs text-muted-foreground shrink-0">
              Dependency state (blocked/blocking chips) is per-project — select a
              project above to see it.
            </p>
          )}
          <TooltipProvider delayDuration={200}>
            {view === "archive" ? (
              <WorkItemsArchiveView
                items={items}
                isLoading={isLoading}
                error={error}
                onRestore={handleRestore}
                restorePending={restoreWorkItem.isPending}
              />
            ) : view === "tree" ? (
              <WorkItemsTree
                treeItems={treeData.treeItems}
                allItems={items}
                matchIds={new Set(treeData.matches.map((i) => i.id))}
                ancestorIds={treeData.ancestorIds}
                filterActive={hasQuery}
                expandedIds={treeExpanded}
                onToggleExpand={toggleTreeExpanded}
                collapsedIds={treeCollapsed}
                onToggleCollapse={toggleTreeCollapsed}
                blockState={blockState}
                selected={selected}
                onToggleSelect={toggle}
                onReorder={handleReorder}
                isLoading={isLoading}
                error={error}
                hasQuery={hasQuery}
                runsByItem={runsByItem}
              />
            ) : (
              <WorkItemsBoard
                projectId={projectId}
                items={filteredItems}
                allItems={items}
                blockState={blockState}
                selected={selected}
                onToggleSelect={toggle}
                collapsedIds={boardCollapsed}
                onToggleCollapse={toggleBoardCollapsed}
                isLoading={isLoading}
                error={error}
                hasQuery={hasQuery}
                runsByItem={runsByItem}
              />
            )}
          </TooltipProvider>
        </div>
      )}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Live refresh indicator (design §4/§5.5 — reuse of the Schedules
// LiveClock pattern): pulsing dot + last-refresh time, paused while the
// tab is hidden (the same visibilitychange logic the Schedules page
// uses for its ticker).
// ---------------------------------------------------------------------------

function LiveRefreshIndicator({
  lastUpdated,
  isFetching,
}: {
  lastUpdated?: number;
  isFetching: boolean;
}) {
  const now = useNow(1000);
  const d = new Date(lastUpdated ?? now);
  const time = d.toLocaleTimeString([], {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    hour12: false,
  });
  return (
    <div
      role="timer"
      aria-label={`Auto-refreshes every 5 seconds. Last refreshed at ${time}`}
      className="flex items-center gap-2 rounded-2xl glass-panel px-3 py-2"
    >
      <span
        className={cn(
          "h-2 w-2 animate-pulse rounded-full motion-reduce:animate-none",
          isFetching ? "bg-sky-500" : "bg-emerald-500",
        )}
      />
      <span className="font-mono text-xs font-medium tabular-nums text-foreground">
        Live {time}
      </span>
    </div>
  );
}

// One page-level `now` ticker for the live indicator; pauses when the tab
// is hidden (browsers throttle background timers anyway; this makes it
// explicit and cheap). Mirrors Schedules' useNow.
function useNow(intervalMs = 1000) {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    let timer: ReturnType<typeof setInterval> | undefined;
    const start = () => {
      if (timer) return;
      setNow(Date.now());
      timer = setInterval(() => setNow(Date.now()), intervalMs);
    };
    const stop = () => {
      if (timer) {
        clearInterval(timer);
        timer = undefined;
      }
    };
    const onVisibility = () => {
      if (document.hidden) stop();
      else start();
    };
    start();
    document.addEventListener("visibilitychange", onVisibility);
    return () => {
      stop();
      document.removeEventListener("visibilitychange", onVisibility);
    };
  }, [intervalMs]);
  return now;
}
