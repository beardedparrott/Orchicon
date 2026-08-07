// Filter bar for the Work Items page (design §4/§5.4, ADR-1).
//
// Holds every AGENTS.md-mandated list-page control — search input,
// filter/sort dropdowns, select-all checkbox, selection count, bulk
// action — plus the Tree|Board segmented toggle and the Dependency
// Graph link. The select-all + count + bulk delete are shared between
// the two views (the parent computes the tri-state over the visible
// filtered set).
//
// Status/Type filters are multi-select checkbox dropdowns
// (design-notes/visual-and-functional-tweaks-to-work-items-page.md,
// ADR-WI-2/ADR-WI-6): OR within a group, AND across groups, empty = all.

import { Columns3, FolderTree, Search, Trash2 } from "lucide-react";

import type { Project } from "@/api/gen/orchicon/api/v1/project_pb";
import {
  BOARD_COLUMNS,
  KIND_FILTER_OPTIONS,
  MANUALLY_UNMOVABLE_STATUSES,
  STATUS_FILTER_OPTIONS,
} from "@/components/work-items/work-item-meta";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { MultiSelect } from "@/components/ui/multi-select";
import { cn } from "@/lib/utils";

export type WorkItemsView = "tree" | "board";

/** Target statuses offered by the bulk "Move to…" select — every board
 *  column except the system-managed ones (Running etc. are only set by
 *  workflows). Per-item advisory gates still apply to the selected set. */
const BULK_MOVE_OPTIONS = BOARD_COLUMNS.filter(
  (c) => !MANUALLY_UNMOVABLE_STATUSES.has(c.status),
).map((c) => ({ value: c.status, label: c.label }));

export interface WorkItemsFilterBarProps {
  projects?: Project[];
  projectId: string;
  onProjectChange: (id: string) => void;
  search: string;
  onSearchChange: (value: string) => void;
  statuses: number[];
  onStatusFilterChange: (next: number[]) => void;
  kinds: number[];
  onKindFilterChange: (next: number[]) => void;
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
  /** Bulk "Move to…" — operates on the selected set (ADR-WI-5). */
  onMoveSelected: (targetStatus: number) => void;
  movePending: boolean;
}

export function WorkItemsFilterBar({
  projects,
  projectId,
  onProjectChange,
  search,
  onSearchChange,
  statuses,
  onStatusFilterChange,
  kinds,
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
  onMoveSelected,
  movePending,
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

      <MultiSelect
        label="Filter by status"
        options={STATUS_FILTER_OPTIONS}
        selected={new Set(statuses)}
        onChange={(next) => onStatusFilterChange(Array.from(next))}
        emptyLabel="No statuses"
      />

      <MultiSelect
        label="Filter by type"
        options={KIND_FILTER_OPTIONS}
        selected={new Set(kinds)}
        onChange={(next) => onKindFilterChange(Array.from(next))}
        emptyLabel="No types"
      />

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
          <>
            <select
              value=""
              disabled={movePending}
              aria-label="Move selected to…"
              onChange={(e) => {
                const status = Number(e.target.value);
                if (status) onMoveSelected(status);
                e.target.value = "";
              }}
              className="h-6 max-w-[7.5rem] cursor-pointer rounded border border-input bg-background px-1 text-[11px] text-muted-foreground focus-visible:ring-2 focus-visible:ring-ring"
            >
              <option value="" disabled>
                Move to…
              </option>
              {BULK_MOVE_OPTIONS.map((o) => (
                <option key={o.value} value={o.value}>
                  {o.label}
                </option>
              ))}
            </select>
            <Button
              variant="destructive"
              size="sm"
              onClick={onDeleteSelected}
              disabled={deletePending}
            >
              <Trash2 className="mr-1 h-3.5 w-3.5" />
              Delete {selectedCount} selected
            </Button>
          </>
        )}
      </div>
    </div>
  );
}
