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

import { useBatchDeleteWorkItems, useGetDependencyGraph, useListWorkItems } from "@/api/workItems";
import { useListProjects } from "@/api/projects";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { TooltipProvider } from "@/components/ui/tooltip";
import { computeBlockState, buildTreeData, filterItemsByKindStatus } from "@/components/work-items/dependency-utils";
import {
  useWorkItemSelection,
  visibleSelectionState,
} from "@/components/work-items/use-work-item-selection";
import {
  WorkItemsBoard,
} from "@/components/work-items/work-items-board";
import {
  WorkItemsFilterBar,
  type WorkItemsView,
} from "@/components/work-items/work-items-filter-bar";
import { useDebouncedValue } from "@/components/work-items/use-debounced-value";
import { WorkItemsTree } from "@/components/work-items/work-items-tree";
import { cn } from "@/lib/utils";
import { Route as rootRoute } from "@/routes/__root";

export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/work-items",
  component: WorkItemsPage,
});

function WorkItemsPage() {
  const { data: projects } = useListProjects();
  const [projectId, setProjectId] = useState<string>("");
  const [view, setView] = useState<WorkItemsView>("tree");
  const [search, setSearch] = useState("");
  const debouncedSearch = useDebouncedValue(search, 300);
  const [statusFilter, setStatusFilter] = useState<string>("");
  const [kindFilter, setKindFilter] = useState<string>("");
  const [sortBy, setSortBy] = useState("created_at");
  const [sortOrder, setSortOrder] = useState("desc");

  const hasProjects = projects && projects.length > 0;
  const batchDelete = useBatchDeleteWorkItems();

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
  });
  const { data: graph } = useGetDependencyGraph(projectId, { refetchInterval: 5_000 });

  const blockState = useMemo(
    () => computeBlockState(graph?.nodes, graph?.edges),
    [graph],
  );

  // Client-side filtering (design §5.4): kind + status + search compose
  // over the full fetched set so the tree hierarchy stays intact.
  const filteredItems = useMemo(
    () => filterItemsByKindStatus(items, kindFilter, statusFilter, debouncedSearch),
    [items, kindFilter, statusFilter, debouncedSearch],
  );
  const treeData = useMemo(
    () => buildTreeData(items, kindFilter, statusFilter, debouncedSearch),
    [items, kindFilter, statusFilter, debouncedSearch],
  );

  // Selection (design §5.1): cascade in the tree, flat on the board,
  // cleared whenever the visible set changes. Tree/Board share the Set.
  const resetKey = [projectId, statusFilter, kindFilter, debouncedSearch, sortBy, sortOrder].join("|");
  const childrenOf = useCallback(
    (parentId: string) =>
      view === "tree" ? treeData.treeItems.filter((i) => i.parentId === parentId) : [],
    [view, treeData.treeItems],
  );
  const { selected, toggle, toggleAll, setSelected } = useWorkItemSelection(childrenOf, resetKey);

  // The header select-all + count operate over the MATCHES (the items
  // that actually pass the filters), never the dimmed ancestor container
  // rows — otherwise "select all" under a type filter would mark
  // filtered-out epics/features for bulk delete.
  const visibleItems = view === "tree" ? treeData.matches : filteredItems;
  const visibleIds = useMemo(() => visibleItems.map((i) => i.id), [visibleItems]);
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

  const hasQuery = statusFilter !== "" || kindFilter !== "" || debouncedSearch !== "";

  return (
    <div className="space-y-6">
      <div className="flex flex-wrap items-center justify-between gap-4">
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
        search={search}
        onSearchChange={setSearch}
        statusFilter={statusFilter}
        onStatusFilterChange={setStatusFilter}
        kindFilter={kindFilter}
        onKindFilterChange={setKindFilter}
        sortBy={sortBy}
        onSortByChange={setSortBy}
        sortOrder={sortOrder}
        onSortOrderChange={setSortOrder}
        view={view}
        onViewChange={setView}
        visibleCount={visibleItems.length}
        selectedCount={selected.size}
        allChecked={allChecked}
        allIndeterminate={allIndeterminate}
        onToggleAll={handleToggleAll}
        onDeleteSelected={handleBatchDelete}
        deletePending={batchDelete.isPending}
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
        <>
          {!projectId && (
            <p className="text-xs text-muted-foreground">
              Dependency state (blocked/blocking chips) is per-project — select a
              project above to see it.
            </p>
          )}
          <TooltipProvider delayDuration={200}>
            {view === "tree" ? (
              <WorkItemsTree
                treeItems={treeData.treeItems}
                matchIds={new Set(treeData.matches.map((i) => i.id))}
                ancestorIds={treeData.ancestorIds}
                filterActive={hasQuery}
                blockState={blockState}
                selected={selected}
                onToggleSelect={toggle}
                isLoading={isLoading}
                error={error}
                hasQuery={hasQuery}
              />
            ) : (
              <WorkItemsBoard
                projectId={projectId}
                items={filteredItems}
                blockState={blockState}
                selected={selected}
                onToggleSelect={toggle}
                isLoading={isLoading}
                error={error}
                hasQuery={hasQuery}
              />
            )}
          </TooltipProvider>
        </>
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
      className="flex items-center gap-2 rounded-lg border bg-card px-3 py-2"
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
