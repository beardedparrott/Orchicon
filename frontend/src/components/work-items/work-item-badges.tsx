// Theme-safe work-item badges — the single source of truth for
// KindBadge / StatusPill (previously copy-pasted with hardcoded
// light-theme colors across work-items.tsx, schedules.tsx and
// work-items_.$id.tsx; those `bg-purple-100 text-purple-800` classes
// were unreadable in dark themes — WCAG AA).
//
// Colors come from work-item-meta.ts (ADR-7): translucent alpha variants
// of the standard hues that hold up in both light and dark themes.

import { cn } from "@/lib/utils";

import { kindMeta, statusMeta } from "@/components/work-items/work-item-meta";

/**
 * Compact square kind badge ("E", "F", "T", "S", "R") — the Jira-style
 * scan badge used on tree rows and board cards.
 */
export function KindBadge({ kind, className }: { kind: number; className?: string }) {
  const meta = kindMeta(kind);
  return (
    <span
      aria-hidden
      className={cn(
        "inline-flex h-5 w-5 shrink-0 items-center justify-center rounded text-xs font-bold",
        meta.badge,
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
  const meta = kindMeta(kind);
  return (
    <span
      className={cn(
        "inline-flex items-center gap-1.5 rounded-full px-2 py-0.5 text-xs font-medium",
        meta.badge,
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
  const meta = statusMeta(status);
  return (
    <span
      className={cn(
        "inline-flex shrink-0 items-center gap-1.5 rounded-full px-2 py-0.5 text-xs font-medium",
        meta.pill,
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
