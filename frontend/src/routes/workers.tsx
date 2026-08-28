import { useEffect, useMemo, useState } from "react";
import { Link, createRoute } from "@tanstack/react-router";
import { Trash2, SearchX, FolderPlus, GripVertical, PencilLine } from "lucide-react";
import { useDraggable } from "@dnd-kit/core";

import {
  useBatchDeleteWorkers,
  useBulkUpdateWorkerModel,
  useListWorkers,
} from "@/api/workers";
import { WorkerStatus, type Worker, WorkerVersionStatus } from "@/api/gen/orchicon/api/v1/worker_pb";
import type { WorkerListItem } from "@/api/gen/orchicon/api/v1/worker_service_pb";
import { formatModelRef, versionStatusLabel, versionStatusTone } from "@/lib/worker-display";
import type { BulkUpdateWorkerModelResult } from "@/api/gen/orchicon/api/v1/worker_service_pb";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { BulkChangeWorkerModelDialog } from "@/components/BulkChangeWorkerModelDialog";
import { CategoryFolder } from "@/components/CategoryFolder";
import { CategoryDndContext } from "@/components/CategoryDndContext";
import { CreateCategoryDialog } from "@/components/CreateCategoryDialog";
import {
  useCategoryPreferences,
  getItemsForCategory,
  type Category,
} from "@/lib/category-store";
import { cn } from "@/lib/utils";
import { Route as rootRoute } from "@/routes/__root";

export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/workers",
  component: WorkersPage,
});

function WorkersPage() {
  const [search, setSearch] = useState("");
  const [status, setStatus] = useState("all");
  const [sortBy, setSortBy] = useState("created_at");
  const [sortOrder, setSortOrder] = useState("asc");
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [showCreateCategory, setShowCreateCategory] = useState(false);
  const [showChangeModel, setShowChangeModel] = useState(false);
  const [changeModelResults, setChangeModelResults] = useState<
    BulkUpdateWorkerModelResult[] | null
  >(null);

  const statusFilter =
    status === "all" ? undefined : (Number(status) as WorkerStatus);
  const {
    data: workers,
    isLoading,
    error,
  } = useListWorkers({ search, status: statusFilter, sortBy, sortOrder });
  const batchDelete = useBatchDeleteWorkers();
  const bulkUpdateModel = useBulkUpdateWorkerModel();

  const prefs = useCategoryPreferences("workers");
  const { ensureSeeded } = prefs;

  // Seed all workers into categories on first load
  const workerIds = useMemo(
    () => (workers ? workers.map((it) => (it as WorkerListItem).worker!.id) : []),
    [workers],
  );
  useEffect(() => {
    if (workerIds.length > 0) {
      ensureSeeded(workerIds);
    }
  }, [workerIds, ensureSeeded]);

  // Group workers by category
  const { categorized, uncategorized } = useMemo(() => {
    if (!workers)
      return { categorized: new Map<string, string[]>(), uncategorized: [] };
    const ids = workers.map((it) => (it as WorkerListItem).worker!.id);
    return getItemsForCategory(prefs.state, ids);
  }, [prefs.state, workers]);

  // Build ordered list of categories with their workers
  const categoryGroups = useMemo(() => {
    if (!workers) return [];
    const workerMap = new Map(workers.map((it) => [(it as WorkerListItem).worker!.id, it as WorkerListItem]));
    const groups: { category: Category; items: typeof workers }[] = [];

    const sortedCategories = [...prefs.state.categories].sort(
      (a, b) => a.order - b.order,
    );

    for (const cat of sortedCategories) {
      const itemIds = categorized.get(cat.id) ?? [];
      const items = itemIds
        .map((id) => workerMap.get(id))
        .filter((w): w is NonNullable<typeof w> => w != null);
      groups.push({ category: cat, items });
    }

    // Uncategorized group
    const uncategorizedItems = uncategorized
      .map((id) => workerMap.get(id))
      .filter((w): w is NonNullable<typeof w> => w != null);
    if (uncategorizedItems.length > 0) {
      groups.push({
        category: {
          id: "uncategorized",
          name: "Uncategorized",
          order: Infinity,
        },
        items: uncategorizedItems,
      });
    }

    return groups;
  }, [workers, categorized, uncategorized, prefs.state.categories]);

  const toggleSelect = (id: string) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

  const toggleSelectAll = () => {
    if (!workers) return;
    if (selected.size === workers.length) {
      setSelected(new Set());
    } else {
      setSelected(new Set(workers.map((it) => (it as WorkerListItem).worker!.id)));
    }
  };

  const handleBatchDelete = () => {
    if (selected.size === 0) return;
    const count = selected.size;
    if (
      !window.confirm(
        `Delete ${count} worker${count === 1 ? "" : "s"}? This cannot be undone.`,
      )
    )
      return;
    batchDelete.mutate(Array.from(selected), {
      onSuccess: () => setSelected(new Set()),
    });
  };

  const handleChangeModelOpen = () => {
    if (selected.size === 0) return;
    setChangeModelResults(null);
    setShowChangeModel(true);
  };

  const handleChangeModelApply = (input: { workerIds: string[]; modelRef: string }) => {
    bulkUpdateModel.mutate(input, {
      onSuccess: (res) => {
        setChangeModelResults(res.results);
        // Clear selection only after the user dismisses the summary (the
        // dialog stays open in result-summary mode).
      },
    });
  };

  const handleChangeModelClose = () => {
    setShowChangeModel(false);
    setChangeModelResults(null);
    // Successful apply → clear selection so the bulk bar disappears.
    if (!bulkUpdateModel.isError && changeModelResults !== null) {
      setSelected(new Set());
    }
  };

  const existingCategoryNames = prefs.state.categories.map((c) => c.name);

  return (
    <div className="space-y-6">
      <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
        <div className="min-w-0">
          <h1 className="text-2xl font-semibold tracking-tight flex items-center gap-2"><span className="inline-flex h-2 w-2 rounded-full bg-cyan-400 animate-pulse motion-reduce:animate-none" /> Workers</h1>
          <p className="text-sm text-muted-foreground">
            Reusable, versioned execution profiles. A Worker declares what is
            permitted; the adapter advertises what is possible.
          </p>
        </div>
        <Button asChild>
          <Link to="/workers/new">New Worker</Link>
        </Button>
      </div>

      <div className="flex flex-wrap items-center gap-2 rounded-2xl glass-panel p-3 border border-white/10">
        <Input
          placeholder="Search workers…"
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          className="h-11 sm:h-9 min-h-[44px] w-full sm:w-64"
        />
        <select
          value={status}
          onChange={(e) => setStatus(e.target.value)}
          className="h-11 sm:h-9 min-h-[44px] rounded-xl glass-input px-3 text-sm"
        >
          <option value="all">All</option>
          <option value="1">Draft</option>
          <option value="2">Published</option>
          <option value="3">Deprecated</option>
          <option value="4">Retired</option>
        </select>
        <select
          value={sortBy}
          onChange={(e) => setSortBy(e.target.value)}
          className="h-11 sm:h-9 min-h-[44px] rounded-xl glass-input px-3 text-sm"
        >
          <option value="created_at">Created</option>
          <option value="name">Name</option>
          <option value="status">Status</option>
        </select>
        <select
          value={sortOrder}
          onChange={(e) => setSortOrder(e.target.value)}
          className="h-11 sm:h-9 min-h-[44px] rounded-xl glass-input px-3 text-sm"
        >
          <option value="asc">Asc</option>
          <option value="desc">Desc</option>
        </select>
        <Button
          variant="outline"
          size="sm"
          onClick={() => setShowCreateCategory(true)}
        >
          <FolderPlus aria-hidden="true" className="mr-1 h-3.5 w-3.5" />
          New Category
        </Button>
        {selected.size > 0 && (
          <>
            <Button
              variant="outline"
              size="sm"
              onClick={handleChangeModelOpen}
            >
              <PencilLine aria-hidden="true" className="mr-1 h-3.5 w-3.5" />
              Change model…
            </Button>
            <Button
              variant="destructive"
              size="sm"
              onClick={handleBatchDelete}
              disabled={batchDelete.isPending}
            >
              <Trash2 aria-hidden="true" className="mr-1 h-3.5 w-3.5" />
              Delete {selected.size} selected
            </Button>
          </>
        )}
      </div>

      {isLoading && <p className="text-sm text-muted-foreground">Loading…</p>}
      {error && (
        <p className="text-sm text-destructive">
          Failed to load workers: {String(error)}
        </p>
      )}

      {workers && workers.length === 0 && (
        <Card>
          <CardHeader>
            <CardTitle className="flex items-center gap-2">
              <SearchX aria-hidden="true" className="h-5 w-5 text-muted-foreground" />
              No workers yet
            </CardTitle>
            <CardDescription>
              Create a worker to define a reusable execution profile with
              permissions, budgets, and a system prompt.
            </CardDescription>
          </CardHeader>
        </Card>
      )}

      {workers && workers.length > 0 && (
        <>
          <div className="flex items-center gap-2 px-2 py-1">
            <input
              type="checkbox"
              checked={
                workers.length > 0 && selected.size === workers.length
              }
              onChange={toggleSelectAll}
              className="h-4 w-4 rounded border-input"
            />
            <span className="text-xs text-muted-foreground">
              {selected.size > 0
                ? `${selected.size} of ${workers.length} selected`
                : `${workers.length} worker${workers.length === 1 ? "" : "s"}`}
            </span>
          </div>
          <div>
            <CategoryDndContext
              categories={prefs.state.categories}
              onAssign={prefs.assignItem}
              onReorder={prefs.reorderCategories}
            >
              {categoryGroups.map(({ category, items }) => (
                <CategoryFolder
                  key={category.id}
                  category={category}
                  count={items.length}
                  isCollapsed={
                    category.id === "uncategorized"
                      ? prefs.collapsed.has("uncategorized")
                      : prefs.collapsed.has(category.id)
                  }
                  onToggle={() => prefs.toggleCollapsed(category.id)}
                  onRename={(newName) =>
                    prefs.renameCategory(category.id, newName)
                  }
                  onDelete={() => {
                    if (
                      window.confirm(
                        `Delete "${category.name}"? Items will move to Uncategorized.`,
                      )
                    ) {
                      prefs.deleteCategory(category.id);
                    }
                  }}
                  onUpdateDescription={(desc) =>
                    prefs.updateDescription(category.id, desc)
                  }
                  droppableId={category.id}
                  sortable={prefs.state.categories.length > 1}
                >
                  <div className="space-y-1">
                    {items.map((it) => {
                      const w = (it as WorkerListItem).worker!;
                      return (
                      <WorkerRow
                        key={w.id}
                        item={it as WorkerListItem}
                        checked={selected.has(w.id)}
                        onToggleSelect={() => toggleSelect(w.id)}
                        onDelete={() => {
                          if (window.confirm("Delete this worker?")) {
                            batchDelete.mutate([w.id]);
                          }
                        }}
                      />
                    )})}
                  </div>
                </CategoryFolder>
              ))}
            </CategoryDndContext>
          </div>
        </>
      )}

      <CreateCategoryDialog
        open={showCreateCategory}
        onOpenChange={setShowCreateCategory}
        onCreate={(name, description) =>
          prefs.createCategory(name, description)
        }
        existingNames={existingCategoryNames}
      />

      <BulkChangeWorkerModelDialog
        open={showChangeModel}
        onOpenChange={(open) => {
          if (!open) handleChangeModelClose();
          else setShowChangeModel(true);
        }}
        selectedIds={Array.from(selected)}
        workers={(workers ?? []).map((it) => (it as WorkerListItem).worker!) as Worker[]}
        onSubmit={handleChangeModelApply}
        isPending={bulkUpdateModel.isPending}
        error={bulkUpdateModel.error as Error | null}
        results={changeModelResults}
      />
    </div>
  );
}

function WorkerRow({
  item,
  checked,
  onToggleSelect,
  onDelete,
}: {
  item: WorkerListItem;
  checked: boolean;
  onToggleSelect: () => void;
  onDelete: () => void;
}) {
  const worker = item.worker!;
  // Drag handle is a SIBLING of the <Link>, so clicking the handle can never
  // open the item; only the Card (<Link>) navigates. dnd-kit's listeners live
  // on the handle span, the measured node is the row.
  const { attributes, listeners, setNodeRef, isDragging } = useDraggable({
    id: worker.id,
  });
  return (
    <div
      ref={setNodeRef}
      className={cn("group flex items-center gap-2", isDragging && "z-10 opacity-50")}
    >
      <span
        {...attributes}
        {...listeners}
        aria-label={`Drag ${worker.name} to a category`}
        className="mt-0.5 shrink-0 cursor-grab text-muted-foreground opacity-0 transition-opacity group-hover:opacity-100 active:cursor-grabbing"
      >
        <GripVertical aria-hidden="true" className="h-4 w-4" />
      </span>
      <input
        type="checkbox"
        checked={checked}
        onChange={onToggleSelect}
        onPointerDown={(e) => e.stopPropagation()}
        className="ml-1 h-4 w-4 shrink-0 rounded border-input"
      />
      <Link
        to="/workers/$id"
        params={{ id: worker.id }}
        className="min-w-0 flex-1"
      >
        <Card className="transition-colors hover:bg-accent">
          <CardContent className="flex flex-col gap-2 p-4 sm:flex-row sm:items-center sm:justify-between">
            <div className="flex min-w-0 items-center gap-3">
              <WorkerStatusBadge status={worker.status} />
              <div className="min-w-0 flex-1 overflow-hidden">
                <p className="truncate text-sm font-medium">
                  {worker.name}
                </p>
                <p className="break-all font-mono text-xs text-muted-foreground">
                  {worker.slug}
                </p>
              </div>
            </div>
            <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-muted-foreground sm:shrink-0">
              {worker.purpose && (
                <span className="max-w-[200px] truncate">
                  {worker.purpose}
                </span>
              )}
              <span>v{worker.currentVersion}</span>
              <span className="max-w-[180px] truncate font-mono" title={item.activeModelRef || undefined}>{formatModelRef(item.activeModelRef)}</span>
              <VersionStatusBadge status={item.activeVersionStatus} />
            </div>
          </CardContent>
        </Card>
      </Link>
      <button
        onClick={onDelete}
        onPointerDown={(e) => e.stopPropagation()}
        className="rounded px-1.5 py-0.5 text-xs font-medium text-muted-foreground opacity-0 transition-all group-hover:opacity-100 hover:bg-accent hover:text-destructive shrink-0"
        title="Delete worker"
      >
        ✕
      </button>
    </div>
  );
}

function VersionStatusBadge({ status }: { status: WorkerVersionStatus }) {
  const label = versionStatusLabel(status);
  // Hide badge when no version yet (UNSPECIFIED)
  if (status === WorkerVersionStatus.UNSPECIFIED) return null;
  return (
    <span className={cn("rounded-full px-2 py-0.5 text-xs font-medium", versionStatusTone(status))}>
      {label}
    </span>
  );
}

function WorkerStatusBadge({ status }: { status: number }) {
  const label = STATUS_LABELS[status] ?? "unknown";
  return (
    <span
      className={cn(
        "rounded-full px-2 py-0.5 text-xs font-medium",
        STATUS_STYLES[status] ?? "bg-muted text-muted-foreground",
      )}
    >
      {label}
    </span>
  );
}

const STATUS_LABELS: Record<number, string> = {
  1: "draft",
  2: "published",
  3: "deprecated",
  4: "retired",
};

const STATUS_STYLES: Record<number, string> = {
  1: "bg-blue-100 text-blue-800",
  2: "bg-green-100 text-green-800",
  3: "bg-yellow-100 text-yellow-800",
  4: "bg-gray-200 text-gray-700",
};
