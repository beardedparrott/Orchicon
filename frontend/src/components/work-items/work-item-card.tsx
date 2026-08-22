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
import { ExternalLink, GitBranch, Link2, Loader2 } from "lucide-react";
import type { ReactNode } from "react";

import { WorkItemStatus, type WorkItem } from "@/api/gen/orchicon/api/v1/work_item_pb";
import {
  KindBadge,
  RecurringBadge,
  StatusPill,
} from "@/components/work-items/work-item-badges";
import { showRecurringBadge, kindMeta, priorityLabel, relativeAge } from "@/components/work-items/work-item-meta";
import { useDarkPalette } from "@/components/work-items/use-dark-palette";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import { prLinkForRun, type PrRun } from "@/lib/pr";
import { cn } from "@/lib/utils";

/** Dependency chain icon. Amber chip + count when actually blocked; a
 *  muted gray icon (with "has dependencies" tooltip) when the item has
 *  edges in the DAG but nothing blocking it right now.
 *
 *  When the item's status IS blocked, the server is the authority
 *  (AGENTS invariant #1): the chip renders from `item.blockedBy` (the
 *  blocking edges the server computed at read time), falling back to the
 *  client-derived advisory map only as a presentation hint. */
export function BlockedChip({
  item,
  blockedBy,
  id,
  depsCount = 0,
  className,
}: {
  /** the work item the chip annotates (server `blockedBy` is authoritative
   *  when it is blocked) */
  item?: WorkItem;
  blockedBy?: Map<string, WorkItem[]>;
  id: string;
  depsCount?: number;
  className?: string;
}) {
  const serverBlockers = item?.status === WorkItemStatus.BLOCKED ? item.blockedBy ?? [] : [];
  const isDark = useDarkPalette();

  if (serverBlockers.length > 0) {
    const titles = serverBlockers.map((b) => b.title).join(", ");
    return (
      <Tooltip>
        <TooltipTrigger asChild>
          <span
            tabIndex={0}
            aria-label={`Blocked by ${titles}`}
            className={cn(
              "inline-flex items-center gap-0.5 rounded-full bg-amber-500/15 px-1.5 py-0.5 text-[11px] font-semibold focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-1",
              isDark ? "text-amber-300" : "text-amber-800",
              className,
            )}
          >
            <Link2 aria-hidden className="h-3 w-3" />
            {serverBlockers.length}
          </span>
        </TooltipTrigger>
        <TooltipContent>Blocked by: {titles}</TooltipContent>
      </Tooltip>
    );
  }

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
              "inline-flex items-center gap-0.5 rounded-full bg-amber-500/15 px-1.5 py-0.5 text-[11px] font-semibold focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-1",
              isDark ? "text-amber-300" : "text-amber-800",
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

/** Compact external-link chip to a completed run's actual authored PR.
 *  Theme-aware (dark palette via useDarkPalette); always tinted primary.
 *  Renders nothing for in-flight runs or runs with no captured PR. */
export function PrLinkChip({ run }: { run: PrRun }) {
  const link = prLinkForRun(run);
  if (!link) return null;
  return (
    <a
      href={link.href}
      target="_blank"
      rel="noreferrer"
      onClick={(e) => e.stopPropagation()}
      title={link.href}
      className={cn(
        "inline-flex shrink-0 items-center gap-1 rounded-full px-1.5 py-0.5 text-[11px] font-semibold no-underline hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-1",
        "bg-primary/10 text-primary hover:bg-primary/15",
      )}
    >
      <ExternalLink className="h-3 w-3" aria-hidden />
      {link.label}
    </a>
  );
}

/** Compact "run" footer for a board/list card: one line per active run with
 *  its branch (reusing the F1 worktree vocabulary for visual consistency)
 *  plus a PR link chip, and a "+N concurrent" indicator when more than one
 *  run is active so parallelism is visible at a glance. Renders nothing when
 *  there are no runs. Keeps the F2 BlockedChip rendering unaffected (it
 *  lives in the card header row). */
export function RunFooter({ runs }: { runs?: PrRun[] }) {
  if (!runs || runs.length === 0) return null;
  // Dedupe by branch (a run's branch is unique); fall back to prUrl.
  const seen = new Set<string>();
  const unique = runs.filter((r) => {
    const k = r.worktreeBranch || r.prUrl || "";
    if (!k || seen.has(k)) return false;
    seen.add(k);
    return true;
  });
  const showConcurrency = unique.length > 1;
  return (
    <div className="mt-1.5 flex min-w-0 flex-wrap items-center gap-1.5 text-xs">
      {unique.map((r, i) => (
        <span key={r.worktreeBranch || r.prUrl || i} className="inline-flex min-w-0 items-center gap-1.5">
          {r.worktreeBranch && (
            <span className="inline-flex shrink-0 items-center gap-1 font-mono text-muted-foreground">
              <GitBranch className="h-3 w-3 shrink-0" aria-hidden />
              <span className="max-w-[9rem] truncate" title={r.worktreeBranch}>
                {r.worktreeBranch}
              </span>
            </span>
          )}
          <PrLinkChip run={r} />
        </span>
      ))}
      {showConcurrency && (
        <span className="shrink-0 rounded-full bg-accent px-1.5 py-0.5 text-[11px] font-semibold text-accent-foreground">
          +{unique.length - 1} concurrent
        </span>
      )}
    </div>
  );
}

export function WorkItemCard({
  item,
  selected,
  onToggleSelect,
  blockedBy,
  depsCount = 0,
  moving = false,
  actions,
  badge,
  runs,
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
  /** optional leading chip next to the kind badge (e.g. chain position) */
  badge?: ReactNode;
  /** optional run footer data — branch/worktree/PR per active run */
  runs?: PrRun[];
}) {
  const meta = kindMeta(item.kind);
  const priority = priorityLabel(item.priority);
  const age = relativeAge(item.createdAt);

  return (
    <div
      className={cn(
        "group relative min-w-0 rounded-md border border-l-4 bg-card p-3 transition-all",
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
        {badge}
        {showRecurringBadge(item) && <RecurringBadge />}
        <span className="ml-auto flex shrink-0 items-center gap-1.5">
          <BlockedChip item={item} blockedBy={blockedBy} id={item.id} depsCount={depsCount} />
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
        className="mt-2 block min-w-0 break-words [overflow-wrap:anywhere] text-sm font-medium leading-snug line-clamp-2 text-foreground focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-1 hover:underline"
      >
        {item.title}
      </Link>
      {(priority || age || actions) && (
        <div className="mt-1.5 flex min-w-0 items-center gap-1.5 text-xs text-muted-foreground">
          {priority && <span className="shrink-0 font-mono tabular-nums">{priority}</span>}
          {priority && age && <span aria-hidden className="shrink-0">·</span>}
          {age && <span className="min-w-0 truncate">{age}</span>}
          {actions && <span className="ml-auto shrink-0">{actions}</span>}
        </div>
      )}
      <RunFooter runs={runs} />
    </div>
  );
}
