// Theme-safe work-item badges — the single source of truth for
// KindBadge / StatusPill (previously copy-pasted with hardcoded
// light-theme colors across work-items.tsx, schedules.tsx and
// work-items_.$id.tsx; those `bg-purple-100 text-purple-800` classes
// were unreadable in dark themes — WCAG AA).
//
// Colors come from work-item-meta.ts (ADR-7): translucent alpha variants
// of the standard hues with a light-palette and a dark-palette class
// set each. Which set is active is decided by `useDarkPalette`, which
// reads the RESOLVED theme's mode — NOT the global `.dark` class. The
// app's default config is zinc (a light theme) + dark mode: the `.dark`
// class is present but the palette stays light, so `dark:` variants made
// badge text light-on-light (1.13:1). Only dark themes define a dark
// palette, so that is the flag the variants key off.

import { cn } from "@/lib/utils";

import { kindMeta, statusMeta, type StatusMeta } from "@/components/work-items/work-item-meta";
import { useDarkPalette } from "@/components/work-items/use-dark-palette";

/**
 * Resolve a StatusMeta's pill classes for the ACTIVE palette.
 *
 * This is the ONE place the light/dark choice is made, so a badge cannot pick
 * the wrong set. It exists because one did: the archive view's "Archived from"
 * chip read `original.pill` directly (the LIGHT-palette classes), so on a dark
 * theme it painted `text-emerald-800` on `bg-emerald-500/15` over a dark-green
 * page — green on green, which is exactly how the operator reported it. Every
 * other badge in the app already went through `useDarkPalette`; the chips built
 * inline did not, because the decision was copyable rather than shared.
 */
export function useStatusPillClasses(meta: StatusMeta): string {
  return statusPillClasses(meta, useDarkPalette());
}

/**
 * The palette decision as a PURE function — the hook above is only the binding of
 * this to the theme store.
 *
 * Split out so the decision itself is directly testable: React's static renderer
 * reads zustand's INITIAL state (useSyncExternalStore's server snapshot), so a
 * component-level test cannot switch palettes in-process. The rule "which class
 * set does a chip use" is what must not regress, and a pure function lets every
 * status be checked in both palettes rather than one example in one theme.
 */
export function statusPillClasses(meta: StatusMeta, isDarkPalette: boolean): string {
  return isDarkPalette ? meta.pillDark : meta.pill;
}

/**
 * Compact square kind badge ("E", "F", "T", "S", "R") — the Jira-style
 * scan badge used on tree rows and board cards.
 */
export function KindBadge({ kind, className }: { kind: number; className?: string }) {
  const isDark = useDarkPalette();
  const meta = kindMeta(kind);
  return (
    <span
      aria-hidden="true"
      className={cn(
        "inline-flex h-5 w-5 shrink-0 items-center justify-center rounded text-xs font-bold",
        isDark ? meta.badgeDark : meta.badge,
        className,
      )}
    >
      {meta.shortLabel}
    </span>
  );
}

/**
 * Full-label kind pill ("Epic", "Feature", …) used on the detail page
 * header, where the full kind name matters.
 */
export function KindPill({ kind, className }: { kind: number; className?: string }) {
  const isDark = useDarkPalette();
  const meta = kindMeta(kind);
  return (
    <span
      className={cn(
        "inline-flex items-center gap-1.5 rounded-full px-2 py-0.5 text-xs font-medium",
        isDark ? meta.badgeDark : meta.badge,
        className,
      )}
    >
      <span aria-hidden="true" className={cn("h-1.5 w-1.5 rounded-full", meta.dot)} />
      {meta.label}
    </span>
  );
}

/** Rounded status pill ("pending", "running", …) — theme-safe. */
export function StatusPill({ status, className }: { status: number; className?: string }) {
  const meta = statusMeta(status);
  const pill = useStatusPillClasses(meta);
  return (
    <span
      className={cn(
        "inline-flex shrink-0 items-center gap-1.5 rounded-full px-2 py-0.5 text-xs font-medium",
        pill,
        className,
      )}
    >
      <span aria-hidden="true" className={cn("h-1.5 w-1.5 rounded-full", meta.dot)} />
      {meta.label}
    </span>
  );
}

/**
 * Small status chip with caller-supplied text — the archive view's "Archived
 * from: succeeded" / "active" markers.
 *
 * It takes a resolved StatusMeta rather than a numeric enum because the archive
 * view renders `WorkItem.archivedFromStatus`, which arrives as a canonical
 * STRING; forcing it through a number would mean re-inventing that mapping here.
 * The palette comes from `useStatusPillClasses`, so these chips are theme-safe
 * by construction (see that hook for the bug this closes).
 */
export function StatusChip({
  meta,
  label,
  className,
  title,
}: {
  meta: StatusMeta;
  label: string;
  className?: string;
  title?: string;
}) {
  const pill = useStatusPillClasses(meta);
  return (
    <span
      title={title}
      className={cn(
        "inline-flex shrink-0 items-center gap-1 rounded-full px-1.5 py-0.5 text-[10px] font-medium",
        pill,
        className,
      )}
    >
      <span aria-hidden="true" className={cn("h-1 w-1 rounded-full", meta.dot)} />
      {label}
    </span>
  );
}

/** Small solid kind dot (timelines, timeline indent guides). */
export function KindDot({ kind, className }: { kind: number; className?: string }) {
  return (
    <span
      aria-hidden="true"
      className={cn("h-2.5 w-2.5 shrink-0 rounded-full", kindMeta(kind).dot, className)}
    />
  );
}

/**
 * "multi-workflow" chip — the color-coded label shown on a sequence parent
 * (a work item with children running sequentially, each with its own
 * workflow) and its children (architecture-notes/
 * sequential-multi-workflow-runs.md §4). Indigo so it reads distinct from
 * the status pills / kind badges.
 */
export function MultiWorkflowChip({
  label = "multi-workflow",
  className,
}: {
  label?: string;
  className?: string;
}) {
  const isDark = useDarkPalette();
  return (
    <span
      className={cn(
        "inline-flex shrink-0 items-center gap-1 rounded-full px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wide",
        isDark ? "bg-indigo-500/20 text-indigo-300" : "bg-indigo-500/15 text-indigo-800",
        className,
      )}
    >
      {label}
    </span>
  );
}

/** Sequential-order badge for a sequence child, derived from sort_order rank
 *  within its parent — never from display order. Styled to match the status
 *  pill (pending's gray), so it reads consistently on the tree/board. The
 *  label defaults to "#N"; the tree passes a fuller "Sequential Order #N". */
export function PositionBadge({
  position,
  className,
  label,
}: {
  position: number;
  className?: string;
  /** display text; defaults to "#N" */
  label?: string;
}) {
  const isDark = useDarkPalette();
  const text = label ?? `#${position}`;
  return (
    <span
      aria-label={text}
      title={text}
      className={cn(
        "inline-flex shrink-0 items-center gap-1.5 rounded-full px-2 py-0.5 text-xs font-medium",
        isDark ? "bg-gray-500/15 text-gray-300" : "bg-gray-500/15 text-gray-700",
        className,
      )}
    >
      {text}
    </span>
  );
}

/** Compact "recurring" badge — a fuchsia marker for work items that carry a
 *  recurring schedule, so they read distinct from the gray sequential badge
 *  and the status pills. Rendered next to the sequential-order badge on
 *  recurring sequence parents/children and on ordinary recurring cards.
 *  Shares the fuchsia hue of the RECURRING status pill (work-item-meta.ts)
 *  so the badge and the pill read as one recurring concept. */
export function RecurringBadge({
  label = "recurring",
  className,
}: {
  label?: string;
  className?: string;
}) {
  const isDark = useDarkPalette();
  return (
    <span
      className={cn(
        "inline-flex shrink-0 items-center gap-1.5 rounded-full px-2 py-0.5 text-xs font-medium",
        isDark ? "bg-fuchsia-500/15 text-fuchsia-300" : "bg-fuchsia-500/15 text-fuchsia-800",
        className,
      )}
    >
      <span aria-hidden="true" className="h-1.5 w-1.5 rounded-full bg-fuchsia-500" />
      {label}
    </span>
  );
}

/** Workflow run status badge — the numeric WorkflowRunStatus as a colored
 *  pill. Shared by the workflow editor, the run view, and the Schedules
 *  History view (the numeric run status is what a run-driven History shows,
 *  distinct from the work item's own StatusPill). */
export function RunStatusBadge({ status }: { status: number }) {
  const labels: Record<number, string> = {
    1: "pending",
    2: "running",
    3: "completed",
    4: "failed",
    5: "aborted",
    6: "paused",
  };
  const styles: Record<number, string> = {
    1: "bg-gray-200 text-gray-700 dark:bg-gray-800 dark:text-gray-300",
    2: "bg-blue-100 text-blue-800 dark:bg-blue-950/60 dark:text-blue-200",
    3: "bg-emerald-100 text-emerald-800 dark:bg-emerald-950/60 dark:text-emerald-200",
    4: "bg-rose-100 text-rose-800 dark:bg-rose-950/60 dark:text-rose-200",
    5: "bg-gray-300 text-gray-700 dark:bg-gray-700 dark:text-gray-200",
    6: "bg-amber-100 text-amber-800 dark:bg-amber-950/60 dark:text-amber-200",
  };
  return (
    <span
      className={cn(
        "rounded-full px-2 py-0.5 text-[10px] font-medium",
        styles[status] ?? "bg-muted text-muted-foreground",
      )}
    >
      {labels[status] ?? "unknown"}
    </span>
  );
}
