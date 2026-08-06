// Shared work-item card + blocked chip (design §5.3, ADR-3).
//
// `WorkItemCard` is the Jira-style board card: kind accent bar, checkbox,
// kind badge, blocked chip, status pill, title link, priority/age meta
// row. The Tree view reuses `BlockedChip` in its rows.
//
// Drag & drop is server-confirmed (ADR-5): while the status mutation is
// pending the card renders `moving` (reduced opacity + spinner) and stays
// in its origin column until the refetch lands.

import { Link } from "@tanstack/react-router";
import { Link2, Loader2 } from "lucide-react";
import type { ReactNode } from "react";

import type { WorkItem } from "@/api/gen/orchicon/api/v1/work_item_pb";
import { KindBadge, StatusPill } from "@/components/work-items/work-item-badges";
import { kindMeta, priorityLabel, relativeAge } from "@/components/work-items/work-item-meta";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import { cn } from "@/lib/utils";

/** Dependency chain icon. Amber chip + count when actually blocked; a
 *  muted gray icon (with "has dependencies" tooltip) when the item has
 *  edges in the DAG but nothing blocking it right now. */
export function BlockedChip({
  blockedBy,
  id,
  depsCount = 0,
  className,
}: {
  blockedBy?: Map<string, WorkItem[]>;
  id: string;
  depsCount?: number;
  className?: string;
}) {
  const blockers = blockedBy?.get(id) ?? [];
  const titles = blockers.map((b) => b.title).join(", ");

  if (blockers.length > 0) {
    return (
      <Tooltip>
        <TooltipTrigger asChild>
          <span
            tabIndex={0}
            aria-label={`Blocked by ${titles}`}
            className={cn(
              "inline-flex items-center gap-0.5 rounded-full bg-amber-500/15 px-1.5 py-0.5 text-[11px] font-semibold text-amber-800 focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-1 dark:text-amber-300",
              className,
            )}
          >
            <Link2 aria-hidden className="h-3 w-3" />
            {blockers.length}
          </span>
        </TooltipTrigger>
        <TooltipContent>Blocked by: {titles}</TooltipContent>
      </Tooltip>
    );
  }

  if (depsCount > 0) {
    return (
      <Tooltip>
        <TooltipTrigger asChild>
          <span
            tabIndex={0}
            aria-label="Has dependencies"
            className={cn(
              "inline-flex items-center rounded-full px-1.5 py-0.5 text-muted-foreground focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-1",
              className,
            )}
          >
            <Link2 aria-hidden className="h-3 w-3" />
          </span>
        </TooltipTrigger>
        <TooltipContent>Has dependencies (none blocking)</TooltipContent>
      </Tooltip>
    );
  }

  return null;
}

export function WorkItemCard({
  item,
  selected,
  onToggleSelect,
  blockedBy,
  depsCount = 0,
  moving = false,
  actions,
}: {
  item: WorkItem;
  selected: boolean;
  onToggleSelect: (id: string) => void;
  blockedBy?: Map<string, WorkItem[]>;
  depsCount?: number;
  /** transient "moving…" state while a board drop mutation is pending */
  moving?: boolean;
  /** optional trailing controls (e.g. the board's "Move to…" menu) */
  actions?: ReactNode;
}) {
  const meta = kindMeta(item.kind);
  const priority = priorityLabel(item.priority);
  const age = relativeAge(item.createdAt);

  return (
    <div
      className={cn(
        "group relative rounded-md border border-l-4 bg-card p-3 transition-all",
        "hover:-translate-y-0.5 hover:shadow-md motion-reduce:translate-y-0 motion-reduce:transition-none",
        meta.accentBar,
        selected && "border-l-indigo-500 bg-accent/60 ring-1 ring-ring",
        moving && "opacity-60",
      )}
      aria-busy={moving}
    >
      <div className="flex items-center gap-2">
        <input
          type="checkbox"
          checked={selected}
          onChange={() => onToggleSelect(item.id)}
          // Stop the drag listeners on the sortable wrapper from
          // hijacking clicks on this control.
          onPointerDown={(e) => e.stopPropagation()}
          onKeyDown={(e) => e.stopPropagation()}
          className="h-3.5 w-3.5 shrink-0 cursor-pointer rounded border-input"
          aria-label={`Select ${item.title}`}
        />
        <KindBadge kind={item.kind} />
        <span className="ml-auto flex shrink-0 items-center gap-1.5">
          <BlockedChip blockedBy={blockedBy} id={item.id} depsCount={depsCount} />
          {moving ? (
            <Loader2 className="h-3.5 w-3.5 animate-spin text-muted-foreground" />
          ) : (
            <StatusPill status={item.status} />
          )}
        </span>
      </div>
      <Link
        to="/work-items/$id"
        params={{ id: item.id }}
        className="mt-2 block text-sm font-medium leading-snug line-clamp-2 text-foreground focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-1 hover:underline"
      >
        {item.title}
      </Link>
      {(priority || age || actions) && (
        <div className="mt-1.5 flex items-center gap-1.5 text-xs text-muted-foreground">
          {priority && <span className="font-mono tabular-nums">{priority}</span>}
          {priority && age && <span aria-hidden>·</span>}
          {age && <span>{age}</span>}
          {actions && <span className="ml-auto">{actions}</span>}
        </div>
      )}
    </div>
  );
}
