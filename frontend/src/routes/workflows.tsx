import { useEffect, useMemo, useState } from "react";
import { Link, createRoute } from "@tanstack/react-router";
import { Trash2, SearchX, FolderPlus, GripVertical } from "lucide-react";
import { useDraggable } from "@dnd-kit/core";

import { useBatchDeleteWorkflows, useListWorkflows } from "@/api/workflows";
import { WorkflowStatus, type Workflow } from "@/api/gen/orchicon/api/v1/workflow_pb";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Input } from "@/components/ui/input";
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
  path: "/workflows",
  component: WorkflowsPage,
});

function WorkflowsPage() {
  const [search, setSearch] = useState("");
  const [status, setStatus] = useState("all");
  const [sortBy, setSortBy] = useState("created_at");
  const [sortOrder, setSortOrder] = useState("asc");
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [showCreateCategory, setShowCreateCategory] = useState(false);

  const statusFilter =
    status === "all" ? undefined : (Number(status) as WorkflowStatus);

  const {
    data: workflows,
    isLoading,
    error,
  } = useListWorkflows({
    search,
    status: statusFilter,
    sortBy,
    sortOrder,
  });
  const batchDelete = useBatchDeleteWorkflows();

  const prefs = useCategoryPreferences("workflows");
  const { ensureSeeded } = prefs;

  // Seed all workflows into categories on first load
  const workflowIds = useMemo(
    () => (workflows ? workflows.map((w) => w.id) : []),
    [workflows],
  );
  useEffect(() => {
    if (workflowIds.length > 0) {
      ensureSeeded(workflowIds);
    }
  }, [workflowIds, ensureSeeded]);

  // Group workflows by category
  const { categorized, uncategorized } = useMemo(() => {
    if (!workflows)
      return { categorized: new Map<string, string[]>(), uncategorized: [] };
    const ids = workflows.map((w) => w.id);
    return getItemsForCategory(prefs.state, ids);
  }, [prefs.state, workflows]);

  // Build ordered list of categories with their workflows
  const categoryGroups = useMemo(() => {
    if (!workflows) return [];
    const workflowMap = new Map(workflows.map((w) => [w.id, w]));
    const groups: { category: Category; items: typeof workflows }[] = [];

    const sortedCategories = [...prefs.state.categories].sort(
      (a, b) => a.order - b.order,
    );

    for (const cat of sortedCategories) {
      const itemIds = categorized.get(cat.id) ?? [];
      const items = itemIds
        .map((id) => workflowMap.get(id))
        .filter((w): w is NonNullable<typeof w> => w != null);
      groups.push({ category: cat, items });
    }

    // Uncategorized group
    const uncategorizedItems = uncategorized
      .map((id) => workflowMap.get(id))
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
  }, [workflows, categorized, uncategorized, prefs.state.categories]);

  const toggleSelect = (id: string) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

  const toggleSelectAll = () => {
    if (!workflows) return;
    if (selected.size === workflows.length) {
      setSelected(new Set());
    } else {
      setSelected(new Set(workflows.map((w) => w.id)));
    }
  };

  const handleBatchDelete = () => {
    if (selected.size === 0) return;
    const count = selected.size;
    if (
      !window.confirm(
        `Delete ${count} workflow${count === 1 ? "" : "s"}? This cannot be undone.`,
      )
    )
      return;
    batchDelete.mutate(Array.from(selected), {
      onSuccess: () => setSelected(new Set()),
    });
  };

  const existingCategoryNames = prefs.state.categories.map((c) => c.name);

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight flex items-center gap-2"><span className="inline-flex h-2 w-2 rounded-full bg-sky-400 animate-pulse motion-reduce:animate-none" /> Workflows</h1>
          <p className="text-sm text-muted-foreground">
            Composable execution plans. Drag Workers onto a canvas, wire
            steps together, and run the DAG.
          </p>
        </div>
        <Button asChild>
          <Link to="/workflows/new">New Workflow</Link>
        </Button>
      </div>

      <div className="flex flex-wrap items-center gap-4 rounded-2xl glass-panel p-3 border border-white/10">
        <Input
          placeholder="Search workflows…"
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          className="max-w-xs"
        />
        <select
          value={status}
          onChange={(e) => setStatus(e.target.value)}
          className="h-9 rounded-xl glass-input px-3 text-sm"
        >
          <option value="all">All</option>
          <option value="1">Draft</option>
          <option value="2">Published</option>
          <option value="3">Deprecated</option>
        </select>
        <select
          value={sortBy}
          onChange={(e) => setSortBy(e.target.value)}
          className="h-9 rounded-xl glass-input px-3 text-sm"
        >
          <option value="created_at">Created</option>
          <option value="name">Name</option>
          <option value="status">Status</option>
        </select>
        <select
          value={sortOrder}
          onChange={(e) => setSortOrder(e.target.value)}
          className="h-9 rounded-xl glass-input px-3 text-sm"
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
          <Button
            variant="destructive"
            size="sm"
            onClick={handleBatchDelete}
            disabled={batchDelete.isPending}
          >
            <Trash2 aria-hidden="true" className="mr-1 h-3.5 w-3.5" />
            Delete {selected.size} selected
          </Button>
        )}
      </div>

      {isLoading && <p className="text-sm text-muted-foreground">Loading…</p>}
      {error && (
        <p className="text-sm text-destructive">
          Failed to load workflows: {String(error)}
        </p>
      )}

      {workflows && workflows.length === 0 && (
        <Card>
          <CardHeader>
            <CardTitle className="flex items-center gap-2">
              <SearchX aria-hidden="true" className="h-5 w-5 text-muted-foreground" />
              No workflows yet
            </CardTitle>
            <CardDescription>
              Create a workflow and open the visual editor to drag Workers
              onto a canvas, wire step dependencies, and run the DAG.
            </CardDescription>
          </CardHeader>
        </Card>
      )}

      {workflows && workflows.length > 0 && (
        <>
          <div className="flex items-center gap-2 px-2 py-1">
            <input
              type="checkbox"
              checked={
                workflows.length > 0 && selected.size === workflows.length
              }
              onChange={toggleSelectAll}
              className="h-4 w-4 rounded border-input"
            />
            <span className="text-xs text-muted-foreground">
              {selected.size > 0
                ? `${selected.size} of ${workflows.length} selected`
                : `${workflows.length} workflow${workflows.length === 1 ? "" : "s"}`}
            </span>
          </div>
          <div>
            <CategoryDndContext
              categories={prefs.state.categories}
              onAssign={prefs.assignItem}
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
                >
                  <div className="space-y-1">
                    {items.map((w) => (
                      <WorkflowRow
                        key={w.id}
                        workflow={w}
                        checked={selected.has(w.id)}
                        onToggleSelect={() => toggleSelect(w.id)}
                        onDelete={() => {
                          if (window.confirm("Delete this workflow?")) {
                            batchDelete.mutate([w.id]);
                          }
                        }}
                      />
                    ))}
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
    </div>
  );
}

function WorkflowRow({
  workflow,
  checked,
  onToggleSelect,
  onDelete,
}: {
  workflow: Workflow;
  checked: boolean;
  onToggleSelect: () => void;
  onDelete: () => void;
}) {
  // Drag handle is a SIBLING of the <Link>, so clicking the handle can never
  // open the item; only the Card (<Link>) navigates. dnd-kit's listeners live
  // on the handle span, the measured node is the row.
  const { attributes, listeners, setNodeRef, isDragging } = useDraggable({
    id: workflow.id,
  });
  return (
    <div
      ref={setNodeRef}
      className={cn("group flex items-center gap-2", isDragging && "z-10 opacity-50")}
    >
      <span
        {...attributes}
        {...listeners}
        aria-label={`Drag ${workflow.name} to a category`}
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
        to="/workflows/$id"
        params={{ id: workflow.id }}
        className="min-w-0 flex-1"
      >
        <Card className="transition-colors hover:bg-accent">
          <CardContent className="flex flex-col gap-2 p-4 sm:flex-row sm:items-center sm:justify-between">
            <div className="flex min-w-0 items-center gap-3">
              <StatusBadge status={workflow.status} />
              <div className="min-w-0 flex-1 overflow-hidden">
                <p className="truncate text-sm font-medium">
                  {workflow.name}
                </p>
                <div className="flex items-center gap-2">
                  <span
                    className={cn(
                      "rounded-full px-1.5 py-0.5 text-[10px] font-medium",
                      workflow.projectId
                        ? "bg-blue-100 text-blue-700 dark:bg-blue-950/60 dark:text-blue-300"
                        : "bg-purple-100 text-purple-700 dark:bg-purple-950/60 dark:text-purple-300",
                    )}
                  >
                    {workflow.projectId ? "One-Shot" : "Template"}
                  </span>
                </div>
              </div>
            </div>
            <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-muted-foreground sm:shrink-0">
              <span>v{workflow.currentVersion || "—"}</span>
            </div>
          </CardContent>
        </Card>
      </Link>
      <button
        onClick={onDelete}
        onPointerDown={(e) => e.stopPropagation()}
        className="rounded px-1.5 py-0.5 text-xs font-medium text-muted-foreground opacity-0 transition-all group-hover:opacity-100 hover:bg-accent hover:text-destructive shrink-0"
        title="Delete workflow"
      >
        ✕
      </button>
    </div>
  );
}

function StatusBadge({ status }: { status: number }) {
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
};

const STATUS_STYLES: Record<number, string> = {
  1: "bg-blue-100 text-blue-800",
  2: "bg-green-100 text-green-800",
  3: "bg-yellow-100 text-yellow-800",
};
