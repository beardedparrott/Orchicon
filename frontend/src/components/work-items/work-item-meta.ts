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

export { DependencyType, type WorkItem };

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
  { value: WorkItemKind.EPIC, label: "Epic" },
  { value: WorkItemKind.FEATURE, label: "Feature" },
  { value: WorkItemKind.TASK, label: "Task" },
  { value: WorkItemKind.SUBTASK, label: "Subtask" },
  { value: WorkItemKind.RECOVERY_STOP, label: "Recovery: Stop" },
  { value: WorkItemKind.RECOVERY_SUMMARIZE_RESTART, label: "Recovery: Summarize & Restart" },
  { value: WorkItemKind.RECOVERY_HUMAN_ESCALATION, label: "Recovery: Human Escalation" },
  { value: WorkItemKind.RECOVERY_RETRY_N, label: "Recovery: Retry" },
];

/** Every kind value offered by the type filter — the default selection
 *  (all selected = "show everything"; an empty selection = show nothing,
 *  ADR-WI-6). Recovery kinds are included so recovery work items stay
 *  visible by default and are filterable. */
export const ALL_KIND_VALUES = KIND_FILTER_OPTIONS.map((o) => o.value);

// ---------------------------------------------------------------------------
// Statuses (enum values from work_item.proto — do not re-invent)
// ---------------------------------------------------------------------------

export interface StatusMeta {
  /** Lowercase label ("ready") for inline pill/badge text. */
  label: string;
  /** Title-case label ("Ready") matching the board column headers —
   *  used by the filter dropdown, "Move to…" menus, and result toasts so
   *  status names are consistent across the page. */
  titleLabel: string;
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
    titleLabel: "Pending",
    pill: "bg-gray-500/15 text-gray-700",
    pillDark: "bg-gray-500/15 text-gray-300",
    dot: "bg-gray-400",
  },
  [WorkItemStatus.READY]: {
    label: "ready",
    titleLabel: "Ready",
    pill: "bg-blue-500/15 text-blue-800",
    pillDark: "bg-blue-500/15 text-blue-300",
    dot: "bg-blue-500",
  },
  [WorkItemStatus.ASSIGNED]: {
    label: "assigned",
    titleLabel: "Assigned",
    pill: "bg-amber-500/15 text-amber-800",
    pillDark: "bg-amber-500/15 text-amber-300",
    dot: "bg-amber-500",
  },
  [WorkItemStatus.RUNNING]: {
    label: "running",
    titleLabel: "Running",
    pill: "bg-green-500/15 text-green-800",
    pillDark: "bg-green-500/15 text-green-300",
    dot: "bg-green-500",
  },
  [WorkItemStatus.CHECKPOINTING]: {
    label: "checkpointing",
    titleLabel: "Checkpointing",
    pill: "bg-sky-500/15 text-sky-800",
    pillDark: "bg-sky-500/15 text-sky-300",
    dot: "bg-sky-500",
  },
  // PAUSED: an operator park, not a fault and not progress. Amber, because it is a deliberate
  // wait — visually distinct from checkpointing's blue (in-flight) and from failure's red.
  [WorkItemStatus.PAUSED]: {
    label: "paused",
    titleLabel: "Paused",
    pill: "bg-amber-500/15 text-amber-800",
    pillDark: "bg-amber-500/15 text-amber-300",
    dot: "bg-amber-500",
  },
  [WorkItemStatus.SUCCEEDED]: {
    label: "succeeded",
    titleLabel: "Succeeded",
    pill: "bg-emerald-500/15 text-emerald-800",
    pillDark: "bg-emerald-500/15 text-emerald-300",
    dot: "bg-emerald-500",
  },
  [WorkItemStatus.FAILED]: {
    label: "failed",
    titleLabel: "Failed",
    pill: "bg-red-500/15 text-red-800",
    pillDark: "bg-red-500/15 text-red-300",
    dot: "bg-red-500",
  },
  [WorkItemStatus.CANCELLED]: {
    label: "cancelled",
    titleLabel: "Cancelled",
    pill: "bg-gray-500/15 text-gray-600",
    pillDark: "bg-gray-500/15 text-gray-400",
    dot: "bg-gray-400",
  },
  [WorkItemStatus.RECOVERING]: {
    label: "recovering",
    titleLabel: "Recovering",
    pill: "bg-orange-500/15 text-orange-800",
    pillDark: "bg-orange-500/15 text-orange-300",
    dot: "bg-orange-500",
  },
  [WorkItemStatus.SCHEDULED]: {
    label: "scheduled",
    titleLabel: "Scheduled",
    pill: "bg-purple-500/15 text-purple-800",
    pillDark: "bg-purple-500/15 text-purple-300",
    dot: "bg-purple-500",
  },
  // Fuchsia — visually distinct from every other status hue (the earlier
  // teal collided with the SUCCEEDED emerald family at a glance). Same
  // hue as the RecurringBadge so the pill and the badge read as one
  // recurring concept.
  [WorkItemStatus.RECURRING]: {
    label: "recurring",
    titleLabel: "Recurring",
    pill: "bg-fuchsia-500/15 text-fuchsia-800",
    pillDark: "bg-fuchsia-500/15 text-fuchsia-300",
    dot: "bg-fuchsia-500",
  },
  // Teal — visually distinct from every other pill (teal is otherwise
  // unused by statuses/kinds). System-managed: set by the reconcilers when
  // an armed/on-deck item cannot dispatch because an upstream dependency is
  // not terminal-success. Distinct from pending/scheduled so operators see
  // WHY nothing is dispatching.
  [WorkItemStatus.BLOCKED]: {
    label: "blocked",
    titleLabel: "Blocked",
    pill: "bg-teal-500/15 text-teal-800",
    pillDark: "bg-teal-500/15 text-teal-300",
    dot: "bg-teal-500",
  },
  // Yellow — distinct from every other status hue (amber is ASSIGNED's,
  // emerald is SUCCEEDED's). System-managed terminal-success: set by the
  // reconcilers when a bound run completes with every active step
  // terminal-success but at least one step skipped. Satisfies dependency
  // edges exactly like SUCCEEDED (never blocks dependents).
  [WorkItemStatus.SKIPPED]: {
    label: "skipped",
    titleLabel: "Skipped",
    pill: "bg-yellow-500/15 text-yellow-800",
    pillDark: "bg-yellow-500/15 text-yellow-300",
    dot: "bg-yellow-500",
  },
  // Slate — visually distinct from every other status. User-initiated
  // terminal status: the item is hidden from every normal view and only
  // visible in the dedicated Archive view, where it can be restored.
  [WorkItemStatus.ARCHIVED]: {
    label: "archived",
    titleLabel: "Archived",
    pill: "bg-slate-500/15 text-slate-800",
    pillDark: "bg-slate-500/15 text-slate-300",
    dot: "bg-slate-500",
  },
  // Lime — visually distinct from every other status hue (amber is
  // ASSIGNED's, yellow is SKIPPED's). System-managed: set only by the
  // automation spawn path (a recurring fire with outputs_mode=idea) and
  // by Idea Cloud promotion. An idea is an automation-produced item
  // awaiting triage, excluded from every normal work-item view.
  [WorkItemStatus.IDEA]: {
    label: "idea",
    titleLabel: "Idea",
    pill: "bg-lime-500/15 text-lime-800",
    pillDark: "bg-lime-500/15 text-lime-300",
    dot: "bg-lime-500",
  },
};

export function statusMeta(status: number): StatusMeta {
  return (
    STATUS_META[status] ?? {
      label: "unknown",
      titleLabel: "Unknown",
      pill: "bg-muted text-muted-foreground",
      pillDark: "bg-muted text-muted-foreground",
      dot: "bg-muted",
    }
  );
}

/** Map a canonical status string (as stored server-side, e.g. "succeeded",
 *  and as echoed in WorkItem.archivedFromStatus) to its numeric enum value
 *  for statusMeta(). Returns WorkItemStatus.UNSPECIFIED for unknown strings
 *  so callers fall back to the "unknown" presentation. */
export function statusMetaFromString(status: string): StatusMeta {
  switch (status) {
    case "pending":
      return statusMeta(WorkItemStatus.PENDING);
    case "ready":
      return statusMeta(WorkItemStatus.READY);
    case "assigned":
      return statusMeta(WorkItemStatus.ASSIGNED);
    case "running":
      return statusMeta(WorkItemStatus.RUNNING);
    case "checkpointing":
      return statusMeta(WorkItemStatus.CHECKPOINTING);
    case "paused":
      return statusMeta(WorkItemStatus.PAUSED);
    case "succeeded":
      return statusMeta(WorkItemStatus.SUCCEEDED);
    case "failed":
      return statusMeta(WorkItemStatus.FAILED);
    case "cancelled":
      return statusMeta(WorkItemStatus.CANCELLED);
    case "recovering":
      return statusMeta(WorkItemStatus.RECOVERING);
    case "scheduled":
      return statusMeta(WorkItemStatus.SCHEDULED);
    case "recurring":
      return statusMeta(WorkItemStatus.RECURRING);
    case "blocked":
      return statusMeta(WorkItemStatus.BLOCKED);
    case "skipped":
      return statusMeta(WorkItemStatus.SKIPPED);
    case "archived":
      return statusMeta(WorkItemStatus.ARCHIVED);
    case "idea":
      return statusMeta(WorkItemStatus.IDEA);
    default:
      return statusMeta(WorkItemStatus.UNSPECIFIED);
  }
}

export const STATUS_FILTER_OPTIONS = [
  { value: WorkItemStatus.PENDING, label: "Pending" },
  { value: WorkItemStatus.READY, label: "Ready" },
  { value: WorkItemStatus.ASSIGNED, label: "Assigned" },
  { value: WorkItemStatus.RUNNING, label: "Running" },
  { value: WorkItemStatus.CHECKPOINTING, label: "Checkpointing" },
  // PAUSED IS A REAL STATUS THE SERVER WRITES. Leaving it out of this list is not a cosmetic
  // omission: ALL_STATUS_VALUES derives from these options, the DEFAULT filter selects every
  // option, and filterItemsByKindStatus is `statuses.includes(i.status)`. A paused row therefore
  // matched nothing and simply disappeared from every view — reported the moment pause started
  // working. Same defect class as ARCHIVED below, which is already documented here.
  { value: WorkItemStatus.PAUSED, label: "Paused" },
  { value: WorkItemStatus.SUCCEEDED, label: "Succeeded" },
  { value: WorkItemStatus.FAILED, label: "Failed" },
  { value: WorkItemStatus.CANCELLED, label: "Cancelled" },
  { value: WorkItemStatus.RECOVERING, label: "Recovering" },
  { value: WorkItemStatus.SCHEDULED, label: "Scheduled" },
  { value: WorkItemStatus.RECURRING, label: "Recurring" },
  { value: WorkItemStatus.BLOCKED, label: "Blocked" },
  { value: WorkItemStatus.SKIPPED, label: "Skipped" },
  // ARCHIVED IS A REAL STATUS THE SERVER WRITES, and leaving it out of this
  // list was a live bug rather than a tidy-up: db.ArchiveWorkItem sets
  // status='archived', so every archived row carries this value, and
  // filterItemsByKindStatus (which is `statuses.includes(i.status)`) matched
  // NONE of them. The archive view counted 0 visible items and its header
  // select-all therefore selected nothing — the operator saw "1 work item"
  // with 81 archived rows on screen.
  //
  // It belongs here even though the archive view does not apply the filter
  // (buildArchiveTreeData reads the archived set directly): the filter
  // vocabulary is the set of statuses this UI knows about, and a status the
  // database can return must not be one the UI cannot name.
  { value: WorkItemStatus.ARCHIVED, label: "Archived" },
];

/** Every status value offered by the status filter — the default
 *  selection (all selected = "show everything"; an empty selection =
 *  show nothing). Checkpointing is included so transient workflow items
 *  stay visible by default and are filterable. */
/**
 * STATUSES DELIBERATELY ABSENT FROM THE FILTER, with the reason.
 *
 * A status the server can write but the filter cannot select is invisible: the default filter
 * selects every option, and filtering is `statuses.includes(i.status)`. `paused` shipped that way
 * and the operator's paused item vanished. `archived` shipped that way once too.
 *
 * IDEA is different and stays out ON PURPOSE, for a reason the item list does not depend on: idea
 * items are excluded from the normal Work Items scope entirely (they are system-managed and only
 * become queryable there via promote_idea). Offering a filter that can never match would be its own
 * small lie.
 *
 * A status added in future must land in STATUS_FILTER_OPTIONS or here — the test in
 * work-item-meta.test.ts enumerates the whole wire enum, so silence is not an option.
 */
export const STATUSES_NOT_FILTERABLE: readonly number[] = [WorkItemStatus.IDEA];

export const ALL_STATUS_VALUES = STATUS_FILTER_OPTIONS.map((o) => o.value);

// ---------------------------------------------------------------------------
// Terminal statuses — used to decide whether a dependency edge still blocks
// its dependent (a blocked item whose blocker already finished is not
// blocked anymore).
// ---------------------------------------------------------------------------

export const TERMINAL_STATUSES = new Set<number>([
  WorkItemStatus.SUCCEEDED,
  WorkItemStatus.SKIPPED,
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
// scheduled/recurring/blocked → Pending, skipped → Succeeded. The card
// still shows its REAL status pill (blocked keeps its distinct teal pill
// while sitting in the Pending column; skipped keeps its yellow pill in
// the Succeeded column).
export function columnForStatus(status: number): number {
  if (status === WorkItemStatus.CHECKPOINTING) return WorkItemStatus.RUNNING;
  if (status === WorkItemStatus.RECOVERING) return WorkItemStatus.RUNNING;
  // PAUSED has no column of its own. It sits under RUNNING because that is where the work was and
  // where Resume will put it back; its amber pill still says "paused", so the column does not
  // misrepresent it. Without this it falls through to a status that has no column and the card
  // vanishes from the board entirely.
  if (status === WorkItemStatus.PAUSED) return WorkItemStatus.RUNNING;
  if (status === WorkItemStatus.SCHEDULED) return WorkItemStatus.PENDING;
  if (status === WorkItemStatus.RECURRING) return WorkItemStatus.PENDING;
  if (status === WorkItemStatus.BLOCKED) return WorkItemStatus.PENDING;
  if (status === WorkItemStatus.SKIPPED) return WorkItemStatus.SUCCEEDED;
  return status;
}

// ---------------------------------------------------------------------------
// System-managed statuses — users cannot manually drag items into these.
// Running is set by the TaskReconciler when a workflow executes;
// Checkpointing and Recovering are transient states within a workflow.
// Blocked is set by the reconcilers when an upstream dependency is
// unsatisfied (system-managed — operators can't drag INTO it; it clears
// automatically). Skipped is NOT in this set: it is operator-settable like
// succeeded/cancelled, through the same status control, and the sequence
// engine consumes it as terminal-success and passes over it when arming
// the next child. (A skip applied to an item with an active bound run is
// still reverted when that run completes — the reconciler writes the run
// outcome unconditionally, same as any other manually-set status.)
// ---------------------------------------------------------------------------
export const MANUALLY_UNMOVABLE_STATUSES = new Set<number>([
  WorkItemStatus.RUNNING,
  WorkItemStatus.CHECKPOINTING,
  WorkItemStatus.RECOVERING,
  WorkItemStatus.BLOCKED,
]);

// ---------------------------------------------------------------------------
// Advisory status-transition matrix (ADR-3 / design §5.4). The server stays
// authoritative; this only prevents obviously-wrong drops in the UI.
// Epics/Features accept: pending, ready, assigned, succeeded, cancelled.
// Tasks/Subtasks (and recovery kinds) accept any board column, plus
// Skipped — which has no dedicated board column of its own (it renders in
// the Succeeded column via columnForStatus) but is still a normal,
// operator-settable target for the row status menu (MoveToMenu).
// ---------------------------------------------------------------------------

const ALL_BOARD_STATUSES = [...BOARD_COLUMNS.map((c) => c.status), WorkItemStatus.SKIPPED];

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

/** True when a work item participates in a recurring schedule. The
 *  persistent `recurringSchedule` field is the stable signal — the status
 *  flips to `running` (and back to `recurring`) while an occurrence fires,
 *  so a status-only check would make the badge blink off mid-run. */
export function isRecurringItem(item: WorkItem): boolean {
  return item.recurringSchedule != null || item.status === WorkItemStatus.RECURRING;
}

/** Recurring marker — rendered only when the status pill is NOT already
 *  "recurring" (i.e. mid-run, where the pill reads "running"). At rest the
 *  fuchsia RECURRING status pill is the single recurrence signal, so the
 *  badge would only duplicate it. */
export function showRecurringBadge(item: WorkItem): boolean {
  return isRecurringItem(item) && item.status !== WorkItemStatus.RECURRING;
}

// ---------------------------------------------------------------------------
// Auto-start requires a bound workflow (the client half of the workflow-first rule)
// ---------------------------------------------------------------------------

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

/**
 * STATUS_DOMAIN_STRINGS maps a wire enum value to the domain string the SERVER stores.
 *
 * IT EXISTS BECAUSE THREE PLACES HAND-BUILT THIS MAP AND EACH MISSED STATUSES.
 *
 * The work item detail page's telemetry payload carried its own inline Record that omitted
 * checkpointing, blocked, skipped, archived — and, when `paused` was added, paused too. The result
 * was the operator opening a paused item and being told its status was "unknown" while the list view
 * showed "paused" correctly: two maps, two answers.
 *
 * Derived from the enum by lowercasing the key, so a status added to the proto is covered here
 * automatically. The explicit list is the one exception: WorkItemStatus keys are the proto names,
 * and the domain strings are the same words lowercased — verified by the test in
 * work-item-meta.test.ts, which asserts the round trip for EVERY value.
 */
export const STATUS_DOMAIN_STRINGS: Record<number, string> = Object.fromEntries(
  Object.entries(WorkItemStatus)
    .filter(([k, v]) => typeof v === "number" && k !== "UNSPECIFIED")
    .map(([k, v]) => [v as number, k.toLowerCase()]),
);

/** statusDomainString is the domain string for a wire status, never "unknown" for a known value. */
export function statusDomainString(status: number): string {
  return STATUS_DOMAIN_STRINGS[status] ?? "unknown";
}
