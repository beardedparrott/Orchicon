// Work item presentation metadata — the single source of truth for
// kind/status labels, theme-safe hue classes, board column mapping, and
// the advisory status-transition matrix used by the board drop gate.
//
// Per design-notes/complete-ui-and-functionality-overhaul-of-work-item-page.md
// (ADR-7): no new global tokens. All colors are component-level Tailwind
// alpha variants of the standard hues. Each badge/pill carries a
// light-palette (`badge`/`pill`) and a dark-palette (`badgeDark`/
// `pillDark`) class set; the badge components pick the set that matches
// the ACTUAL palette (see work-item-badges.tsx `useDarkPalette`).
//
// The naive `text-purple-800 dark:text-purple-300` single-string variant
// was unreadable in the app's default config (zinc theme + dark mode):
// zinc is a LIGHT theme with no dark-palette overrides, so the page
// keeps a light background while Tailwind's `.dark` class activates the
// `dark:` text color — light text on a light background (1.13:1, WCAG
// fail). Keying off the resolved theme's mode instead of the `.dark`
// class fixes every light-theme-in-dark-mode combination.

import {
  DependencyType,
  WorkItemKind,
  WorkItemStatus,
  type WorkItem,
} from "@/api/gen/orchicon/api/v1/work_item_pb";

export { DependencyType };

// ---------------------------------------------------------------------------
// Kinds (enum values from work_item.proto — do not re-invent)
// ---------------------------------------------------------------------------

export interface KindMeta {
  label: string; // "Epic"
  shortLabel: string; // "E" (square badge)
  /** light-palette badge classes (text ≥4.5:1) */
  badge: string;
  /** dark-palette badge classes (text ≥4.5:1) */
  badgeDark: string;
  /** solid dot color used for indent guides / timeline dots */
  dot: string;
  /** kind-colored left accent bar on cards */
  accentBar: string;
}

const KIND_META: Record<number, KindMeta> = {
  [WorkItemKind.EPIC]: {
    label: "Epic",
    shortLabel: "E",
    badge: "bg-purple-500/15 text-purple-800",
    badgeDark: "bg-purple-500/15 text-purple-300",
    dot: "bg-purple-500",
    accentBar: "border-l-purple-500",
  },
  [WorkItemKind.FEATURE]: {
    label: "Feature",
    shortLabel: "F",
    badge: "bg-indigo-500/15 text-indigo-800",
    badgeDark: "bg-indigo-500/15 text-indigo-300",
    dot: "bg-indigo-500",
    accentBar: "border-l-indigo-500",
  },
  [WorkItemKind.TASK]: {
    label: "Task",
    shortLabel: "T",
    badge: "bg-blue-500/15 text-blue-800",
    badgeDark: "bg-blue-500/15 text-blue-300",
    dot: "bg-blue-500",
    accentBar: "border-l-blue-500",
  },
  [WorkItemKind.SUBTASK]: {
    label: "Subtask",
    shortLabel: "S",
    badge: "bg-cyan-500/15 text-cyan-800",
    badgeDark: "bg-cyan-500/15 text-cyan-300",
    dot: "bg-cyan-500",
    accentBar: "border-l-cyan-500",
  },
  // Recovery kinds are schedulable like tasks; they share amber/rose hues
  // and the "R" badge letter (design: recovery badge style).
  [WorkItemKind.RECOVERY_STOP]: {
    label: "Recovery: Stop",
    shortLabel: "R",
    badge: "bg-amber-500/15 text-amber-800",
    badgeDark: "bg-amber-500/15 text-amber-300",
    dot: "bg-amber-500",
    accentBar: "border-l-amber-500",
  },
  [WorkItemKind.RECOVERY_SUMMARIZE_RESTART]: {
    label: "Recovery: Summarize & Restart",
    shortLabel: "R",
    badge: "bg-amber-500/15 text-amber-800",
    badgeDark: "bg-amber-500/15 text-amber-300",
    dot: "bg-amber-500",
    accentBar: "border-l-amber-500",
  },
  [WorkItemKind.RECOVERY_HUMAN_ESCALATION]: {
    label: "Recovery: Human Escalation",
    shortLabel: "R",
    badge: "bg-rose-500/15 text-rose-800",
    badgeDark: "bg-rose-500/15 text-rose-300",
    dot: "bg-rose-500",
    accentBar: "border-l-rose-500",
  },
  [WorkItemKind.RECOVERY_RETRY_N]: {
    label: "Recovery: Retry",
    shortLabel: "R",
    badge: "bg-amber-500/15 text-amber-800",
    badgeDark: "bg-amber-500/15 text-amber-300",
    dot: "bg-amber-500",
    accentBar: "border-l-amber-500",
  },
};

export function kindMeta(kind: number): KindMeta {
  return (
    KIND_META[kind] ?? {
      label: "Unknown",
      shortLabel: "?",
      badge: "bg-muted text-muted-foreground",
      badgeDark: "bg-muted text-muted-foreground",
      dot: "bg-muted",
      accentBar: "border-l-muted",
    }
  );
}

/** Lowercase kind label ("epic", "task", …) for inline text. */
export function kindLabel(kind: number): string {
  return kindMeta(kind).label.toLowerCase();
}

export const KIND_FILTER_OPTIONS = [
  { value: String(WorkItemKind.EPIC), label: "Epic" },
  { value: String(WorkItemKind.FEATURE), label: "Feature" },
  { value: String(WorkItemKind.TASK), label: "Task" },
  { value: String(WorkItemKind.SUBTASK), label: "Subtask" },
];

// ---------------------------------------------------------------------------
// Statuses (enum values from work_item.proto — do not re-invent)
// ---------------------------------------------------------------------------

export interface StatusMeta {
  label: string;
  /** light-palette pill classes (text ≥4.5:1) */
  pill: string;
  /** dark-palette pill classes (text ≥4.5:1) */
  pillDark: string;
  /** small dot shown next to counts / column headers */
  dot: string;
}

const STATUS_META: Record<number, StatusMeta> = {
  [WorkItemStatus.PENDING]: {
    label: "pending",
    pill: "bg-gray-500/15 text-gray-700",
    pillDark: "bg-gray-500/15 text-gray-300",
    dot: "bg-gray-400",
  },
  [WorkItemStatus.READY]: {
    label: "ready",
    pill: "bg-blue-500/15 text-blue-800",
    pillDark: "bg-blue-500/15 text-blue-300",
    dot: "bg-blue-500",
  },
  [WorkItemStatus.ASSIGNED]: {
    label: "assigned",
    pill: "bg-amber-500/15 text-amber-800",
    pillDark: "bg-amber-500/15 text-amber-300",
    dot: "bg-amber-500",
  },
  [WorkItemStatus.RUNNING]: {
    label: "running",
    pill: "bg-green-500/15 text-green-800",
    pillDark: "bg-green-500/15 text-green-300",
    dot: "bg-green-500",
  },
  [WorkItemStatus.CHECKPOINTING]: {
    label: "checkpointing",
    pill: "bg-sky-500/15 text-sky-800",
    pillDark: "bg-sky-500/15 text-sky-300",
    dot: "bg-sky-500",
  },
  [WorkItemStatus.SUCCEEDED]: {
    label: "succeeded",
    pill: "bg-emerald-500/15 text-emerald-800",
    pillDark: "bg-emerald-500/15 text-emerald-300",
    dot: "bg-emerald-500",
  },
  [WorkItemStatus.FAILED]: {
    label: "failed",
    pill: "bg-red-500/15 text-red-800",
    pillDark: "bg-red-500/15 text-red-300",
    dot: "bg-red-500",
  },
  [WorkItemStatus.CANCELLED]: {
    label: "cancelled",
    pill: "bg-gray-500/15 text-gray-600",
    pillDark: "bg-gray-500/15 text-gray-400",
    dot: "bg-gray-400",
  },
  [WorkItemStatus.RECOVERING]: {
    label: "recovering",
    pill: "bg-orange-500/15 text-orange-800",
    pillDark: "bg-orange-500/15 text-orange-300",
    dot: "bg-orange-500",
  },
  [WorkItemStatus.SCHEDULED]: {
    label: "scheduled",
    pill: "bg-purple-500/15 text-purple-800",
    pillDark: "bg-purple-500/15 text-purple-300",
    dot: "bg-purple-500",
  },
};

export function statusMeta(status: number): StatusMeta {
  return (
    STATUS_META[status] ?? {
      label: "unknown",
      pill: "bg-muted text-muted-foreground",
      pillDark: "bg-muted text-muted-foreground",
      dot: "bg-muted",
    }
  );
}

export const STATUS_FILTER_OPTIONS = [
  { value: String(WorkItemStatus.PENDING), label: "pending" },
  { value: String(WorkItemStatus.READY), label: "ready" },
  { value: String(WorkItemStatus.ASSIGNED), label: "assigned" },
  { value: String(WorkItemStatus.RUNNING), label: "running" },
  { value: String(WorkItemStatus.SUCCEEDED), label: "succeeded" },
  { value: String(WorkItemStatus.FAILED), label: "failed" },
  { value: String(WorkItemStatus.CANCELLED), label: "cancelled" },
  { value: String(WorkItemStatus.RECOVERING), label: "recovering" },
  { value: String(WorkItemStatus.SCHEDULED), label: "scheduled" },
];

// ---------------------------------------------------------------------------
// Terminal statuses — used to decide whether a dependency edge still blocks
// its dependent (a blocked item whose blocker already finished is not
// blocked anymore).
// ---------------------------------------------------------------------------

export const TERMINAL_STATUSES = new Set<number>([
  WorkItemStatus.SUCCEEDED,
  WorkItemStatus.FAILED,
  WorkItemStatus.CANCELLED,
]);

export function isTerminal(status: number): boolean {
  return TERMINAL_STATUSES.has(status);
}

// ---------------------------------------------------------------------------
// Board columns (one per server status — the drop target maps 1:1 to a
// WorkItemStatus value; ADR-8).
// ---------------------------------------------------------------------------

export interface BoardColumn {
  status: number;
  label: string;
}

export const BOARD_COLUMNS: BoardColumn[] = [
  { status: WorkItemStatus.PENDING, label: "Pending" },
  { status: WorkItemStatus.READY, label: "Ready" },
  { status: WorkItemStatus.ASSIGNED, label: "Assigned" },
  { status: WorkItemStatus.RUNNING, label: "Running" },
  { status: WorkItemStatus.SUCCEEDED, label: "Succeeded" },
  { status: WorkItemStatus.FAILED, label: "Failed" },
  { status: WorkItemStatus.CANCELLED, label: "Cancelled" },
];

// Statuses without a dedicated column render in the column of their
// closest active status: checkpointing/recovering → Running,
// scheduled → Pending. The card still shows its REAL status pill.
export function columnForStatus(status: number): number {
  if (status === WorkItemStatus.CHECKPOINTING) return WorkItemStatus.RUNNING;
  if (status === WorkItemStatus.RECOVERING) return WorkItemStatus.RUNNING;
  if (status === WorkItemStatus.SCHEDULED) return WorkItemStatus.PENDING;
  return status;
}

// ---------------------------------------------------------------------------
// System-managed statuses — users cannot manually drag items into these.
// Running is set by the TaskReconciler when a workflow executes;
// Checkpointing and Recovering are transient states within a workflow.
// ---------------------------------------------------------------------------
export const MANUALLY_UNMOVABLE_STATUSES = new Set<number>([
  WorkItemStatus.RUNNING,
  WorkItemStatus.CHECKPOINTING,
  WorkItemStatus.RECOVERING,
]);

// ---------------------------------------------------------------------------
// Advisory status-transition matrix (ADR-3 / design §5.4). The server stays
// authoritative; this only prevents obviously-wrong drops in the UI.
// Epics/Features accept: pending, ready, assigned, succeeded, cancelled.
// Tasks/Subtasks (and recovery kinds) accept any board column.
// ---------------------------------------------------------------------------

const ALL_BOARD_STATUSES = BOARD_COLUMNS.map((c) => c.status);

export const ALLOWED_STATUSES_PER_KIND: Record<number, number[]> = {
  [WorkItemKind.EPIC]: [
    WorkItemStatus.PENDING,
    WorkItemStatus.READY,
    WorkItemStatus.ASSIGNED,
    WorkItemStatus.SUCCEEDED,
    WorkItemStatus.CANCELLED,
  ],
  [WorkItemKind.FEATURE]: [
    WorkItemStatus.PENDING,
    WorkItemStatus.READY,
    WorkItemStatus.ASSIGNED,
    WorkItemStatus.SUCCEEDED,
    WorkItemStatus.CANCELLED,
  ],
  [WorkItemKind.TASK]: ALL_BOARD_STATUSES,
  [WorkItemKind.SUBTASK]: ALL_BOARD_STATUSES,
  [WorkItemKind.RECOVERY_STOP]: ALL_BOARD_STATUSES,
  [WorkItemKind.RECOVERY_SUMMARIZE_RESTART]: ALL_BOARD_STATUSES,
  [WorkItemKind.RECOVERY_HUMAN_ESCALATION]: ALL_BOARD_STATUSES,
  [WorkItemKind.RECOVERY_RETRY_N]: ALL_BOARD_STATUSES,
};

export function allowedStatusesForKind(kind: number): number[] {
  return ALLOWED_STATUSES_PER_KIND[kind] ?? ALL_BOARD_STATUSES;
}

// ---------------------------------------------------------------------------
// Small shared helpers
// ---------------------------------------------------------------------------

/** "P3" for priority > 0, otherwise a muted "—" (rendered by the card). */
export function priorityLabel(priority: number): string {
  return priority > 0 ? `P${priority}` : "";
}

/** Relative age of a work item from its created_at ("just now", "2d ago"). */
export function relativeAge(createdAt?: WorkItem["createdAt"], now = Date.now()): string {
  if (!createdAt) return "";
  const ms = Number(createdAt.seconds) * 1000;
  const diff = now - ms;
  if (diff < 0) return "now";
  const minutes = Math.floor(diff / 60_000);
  if (minutes < 1) return "just now";
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h ago`;
  const days = Math.floor(hours / 24);
  if (days < 30) return `${days}d ago`;
  const months = Math.floor(days / 30);
  if (months < 12) return `${months}mo ago`;
  return `${Math.floor(months / 12)}y ago`;
}
