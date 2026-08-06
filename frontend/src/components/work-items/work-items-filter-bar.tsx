// Filter bar for the Work Items page (design §4/§5.4, ADR-1).
//
// Holds every AGENTS.md-mandated list-page control — search input,
// filter/sort dropdowns, select-all checkbox, selection count, bulk
// action — plus the Tree|Board segmented toggle and the Dependency
// Graph link. The select-all + count + bulk delete are shared between
// the two views (the parent computes the tri-state over the visible
// filtered set).

import { Link } from "@tanstack/react-router";
import { Columns3, FolderTree, Search, Trash2 } from "lucide-react";

import type { Project } from "@/api/gen/orchicon/api/v1/project_pb";
import {
  KIND_FILTER_OPTIONS,
  STATUS_FILTER_OPTIONS,
} from "@/components/work-items/work-item-meta";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";

export type WorkItemsView = "tree" | "board";

export interface WorkItemsFilterBarProps {
  projects?: Project[];
  projectId: string;
  onProjectChange: (id: string) => void;
  search: string;
  onSearchChange: (value: string) => void;
  statusFilter: string;
  onStatusFilterChange: (value: string) => void;
  kindFilter: string;
  onKindFilterChange: (value: string) => void;
  sortBy: string;
  onSortByChange: (value: string) => void;
  sortOrder: string;
  onSortOrderChange: (value: string) => void;
  view: WorkItemsView;
  onViewChange: (view: WorkItemsView) => void;
  visibleCount: number;
  selectedCount: number;
  allChecked: boolean;
  allIndeterminate: boolean;
  onToggleAll: () => void;
  onDeleteSelected: () => void;
  deletePending: boolean;
}

export function WorkItemsFilterBar({
  projects,
  projectId,
  onProjectChange,
  search,
  onSearchChange,
  statusFilter,
  onStatusFilterChange,
  kindFilter,
  onKindFilterChange,
  sortBy,
  onSortByChange,
  sortOrder,
  onSortOrderChange,
  view,
  onViewChange,
  visibleCount,
  selectedCount,
  allChecked,
  allIndeterminate,
  onToggleAll,
  onDeleteSelected,
  deletePending,
}: WorkItemsFilterBarProps) {
  const selectClass =
    "h-9 rounded-md border border-input bg-transparent px-3 text-sm shadow-sm focus-visible:ring-2 focus-visible:ring-ring";

  return (
    <div className="flex flex-wrap items-center gap-3 shrink-0">
      {/* ── Row 1: project + search + filters + sort ── */}
      <select
        value={projectId}
        onChange={(e) => onProjectChange(e.target.value)}
        disabled={!projects || projects.length === 0}
        aria-label="Project"
        className={selectClass}
      >
        <option value="">All projects</option>
        {projects && projects.length > 0 ? (
          projects.map((p) => (
            <option key={p.id} value={p.id}>
              {p.name}
            </option>
          ))
        ) : (
          <option value="" disabled>
            No projects available
          </option>
        )}
      </select>

      <div className="relative min-w-[160px] flex-1">
        <Search
          aria-hidden
          className="pointer-events-none absolute left-2.5 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-muted-foreground"
        />
        <Input
          placeholder="Search title or description…"
          value={search}
          onChange={(e) => onSearchChange(e.target.value)}
          className="h-9 w-full pl-8"
        />
      </div>

      <select
        value={statusFilter}
        onChange={(e) => onStatusFilterChange(e.target.value)}
        aria-label="Filter by status"
        className={selectClass}
      >
        <option value="">All statuses</option>
        {STATUS_FILTER_OPTIONS.map((s) => (
          <option key={s.value} value={s.value}>
            {s.label}
          </option>
        ))}
      </select>

      <select
        value={kindFilter}
        onChange={(e) => onKindFilterChange(e.target.value)}
        aria-label="Filter by type"
        className={selectClass}
      >
        <option value="">All types</option>
        {KIND_FILTER_OPTIONS.map((k) => (
          <option key={k.value} value={k.value}>
            {k.label}
          </option>
        ))}
      </select>

      <select
        value={sortBy}
        onChange={(e) => onSortByChange(e.target.value)}
        aria-label="Sort by"
        className={selectClass}
      >
        <option value="created_at">Sort: created</option>
        <option value="title">Sort: title</option>
        <option value="priority">Sort: priority</option>
      </select>

      <select
        value={sortOrder}
        onChange={(e) => onSortOrderChange(e.target.value)}
        aria-label="Sort order"
        className={selectClass}
      >
        <option value="desc">Desc</option>
        <option value="asc">Asc</option>
      </select>

      {/* ── Row 2: view toggle · graph link · selection + bulk ── */}
      <div className="flex rounded-md border" role="group" aria-label="View">
        <button
          type="button"
          aria-pressed={view === "tree"}
          className={cn(
            "flex items-center gap-1.5 rounded-l-md px-3 py-1.5 text-sm font-medium transition-colors focus-visible:ring-2 focus-visible:ring-ring",
            view === "tree"
              ? "bg-accent text-accent-foreground"
              : "text-muted-foreground hover:bg-accent/50",
          )}
          onClick={() => onViewChange("tree")}
        >
          <FolderTree aria-hidden className="h-3.5 w-3.5" />
          Tree
        </button>
        <button
          type="button"
          aria-pressed={view === "board"}
          className={cn(
            "flex items-center gap-1.5 rounded-r-md px-3 py-1.5 text-sm font-medium transition-colors focus-visible:ring-2 focus-visible:ring-ring",
            view === "board"
              ? "bg-accent text-accent-foreground"
              : "text-muted-foreground hover:bg-accent/50",
          )}
          onClick={() => onViewChange("board")}
        >
          <Columns3 aria-hidden className="h-3.5 w-3.5" />
          Board
        </button>
      </div>

      {projectId && (
        <Button variant="outline" size="sm" asChild>
          <Link to="/work-items/graph" search={{ projectId }}>
            Dependency Graph
          </Link>
        </Button>
      )}

      <div className="ml-auto flex items-center gap-2">
        <input
          type="checkbox"
          checked={allChecked}
          ref={(el) => {
            if (el) el.indeterminate = allIndeterminate;
          }}
          onChange={onToggleAll}
          className="h-4 w-4 cursor-pointer rounded border-input"
          aria-label="Select all visible work items"
        />
        <span aria-live="polite" className="text-xs text-muted-foreground">
          {selectedCount > 0
            ? `${selectedCount} of ${visibleCount} selected`
            : `${visibleCount} work item${visibleCount === 1 ? "" : "s"}`}
        </span>
        {selectedCount > 0 && (
          <Button
            variant="destructive"
            size="sm"
            onClick={onDeleteSelected}
            disabled={deletePending}
          >
            <Trash2 className="mr-1 h-3.5 w-3.5" />
            Delete {selectedCount} selected
          </Button>
        )}
      </div>
    </div>
  );
}
