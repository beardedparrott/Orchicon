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

import { kindMeta, statusMeta } from "@/components/work-items/work-item-meta";
import { useDarkPalette } from "@/components/work-items/use-dark-palette";

/**
 * Compact square kind badge ("E", "F", "T", "S", "R") — the Jira-style
 * scan badge used on tree rows and board cards.
 */
export function KindBadge({ kind, className }: { kind: number; className?: string }) {
  const isDark = useDarkPalette();
  const meta = kindMeta(kind);
  return (
    <span
      aria-hidden
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
      <span aria-hidden className={cn("h-1.5 w-1.5 rounded-full", meta.dot)} />
      {meta.label}
    </span>
  );
}

/** Rounded status pill ("pending", "running", …) — theme-safe. */
export function StatusPill({ status, className }: { status: number; className?: string }) {
  const isDark = useDarkPalette();
  const meta = statusMeta(status);
  return (
    <span
      className={cn(
        "inline-flex shrink-0 items-center gap-1.5 rounded-full px-2 py-0.5 text-xs font-medium",
        isDark ? meta.pillDark : meta.pill,
        className,
      )}
    >
      <span aria-hidden className={cn("h-1.5 w-1.5 rounded-full", meta.dot)} />
      {meta.label}
    </span>
  );
}

/** Small solid kind dot (timelines, timeline indent guides). */
export function KindDot({ kind, className }: { kind: number; className?: string }) {
  return (
    <span
      aria-hidden
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
 *  within its parent — never from display order. Shows the true chain
 *  position even when the board/tree display sort reorders the cards. */
export function PositionBadge({ position, className }: { position: number; className?: string }) {
  return (
    <span
      aria-label={`Sequential Order #${position}`}
      title={`Sequential Order #${position}`}
      className={cn(
        "inline-flex h-4 shrink-0 items-center justify-center rounded-full border border-amber-500/70 bg-amber-500/15 px-1.5 text-[10px] font-bold tabular-nums text-amber-700",
        className,
      )}
    >
      Sequential Order #{position}
    </span>
  );
}
