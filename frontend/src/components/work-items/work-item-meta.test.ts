// Unit tests for the work-item presentation metadata (ADR-7).
//
// Every kind badge / status pill carries a light-palette and a
// dark-palette class set. The naive single-string
// `text-purple-800 dark:text-purple-300` variant was unreadable in the
// app's default config (zinc theme + dark mode): zinc is a light theme
// with no dark-palette overrides, so the page keeps a light background
// while the `.dark` class activates the `dark:` text color — 1.13:1
// contrast. These tests pin the split so no `dark:` variant leaks back
// into the meta and both palettes stay readable.

import { describe, expect, it } from "vitest";

import {
  WorkItemKind,
  WorkItemStatus,
  WorkItem,
  RecurringSchedule,
} from "@/api/gen/orchicon/api/v1/work_item_pb";
import {
  kindMeta,
  statusMeta,
  columnForStatus,
  allowedStatusesForKind,
  isTerminal,
  isRecurringItem,
  showRecurringBadge,
  MANUALLY_UNMOVABLE_STATUSES,
  STATUS_FILTER_OPTIONS,
} from "@/components/work-items/work-item-meta";

describe("kind meta palette variants", () => {
  // The proto enum includes UNSPECIFIED=0, which falls back to token
  // classes (`bg-muted text-muted-foreground`) — identical in both
  // palettes by design. Named kinds each have distinct light/dark sets.
  const namedKinds = Object.values(WorkItemKind).filter(
    (k): k is number => typeof k === "number" && k !== 0,
  );

  it("exposes distinct light and dark badge classes", () => {
    for (const kind of namedKinds) {
      const meta = kindMeta(kind);
      expect(meta.badge, `kind ${kind} light badge`).toBeTruthy();
      expect(meta.badgeDark, `kind ${kind} dark badge`).toBeTruthy();
      expect(meta.badge).not.toBe(meta.badgeDark);
    }
  });

  it("contains no raw `dark:` Tailwind variants (they keyed off the wrong flag)", () => {
    for (const kind of namedKinds) {
      expect(kindMeta(kind).badge).not.toMatch(/dark:/);
      expect(kindMeta(kind).badgeDark).not.toMatch(/dark:/);
    }
    // The unknown fallback must also avoid `dark:` variants.
    expect(kindMeta(0).badge).not.toMatch(/dark:/);
  });

  it("light variants use deep text hues, dark variants use light text hues", () => {
    const epic = kindMeta(WorkItemKind.EPIC);
    expect(epic.badge).toContain("text-purple-800");
    expect(epic.badgeDark).toContain("text-purple-300");
  });
});

describe("status meta palette variants", () => {
  it("exposes distinct light and dark pill classes without `dark:` variants", () => {
    for (const status of Object.values(WorkItemStatus).filter(
      (s): s is number => typeof s === "number",
    )) {
      const meta = statusMeta(status);
      expect(meta.pill, `status ${status} light pill`).toBeTruthy();
      expect(meta.pillDark, `status ${status} dark pill`).toBeTruthy();
      expect(meta.pill).not.toMatch(/dark:/);
      expect(meta.pillDark).not.toMatch(/dark:/);
    }
  });

  it("title labels are title-case and match the board column headers", () => {
    // Filter dropdowns, "Move to…" menus, and result toasts use the
    // title label so status names match the board column headers (the
    // pill stays lowercase). Regression guard for the mixed-case UI.
    const labels = Object.values(WorkItemStatus)
      .filter((s): s is number => typeof s === "number")
      .map((s) => statusMeta(s).titleLabel);
    for (const label of labels) {
      expect(label).toBeTruthy();
      expect(label[0]).toBe(label[0]!.toUpperCase());
    }
    expect(statusMeta(WorkItemStatus.PENDING).titleLabel).toBe("Pending");
    expect(statusMeta(WorkItemStatus.SUCCEEDED).titleLabel).toBe("Succeeded");
    expect(statusMeta(WorkItemStatus.READY).label).toBe("ready");
  });
});

describe("recurring status meta", () => {
  it("uses fuchsia (distinct from every other status hue)", () => {
    const recurring = statusMeta(WorkItemStatus.RECURRING);
    expect(recurring.pill).toContain("text-fuchsia-800");
    expect(recurring.pillDark).toContain("text-fuchsia-300");
    expect(recurring.pill).toContain("bg-fuchsia-500/15");
    // The RecurringBadge shares the same hue family — they must not drift.
    expect(recurring.pill).not.toContain("teal");
  });
});

describe("isRecurringItem", () => {
  const base = { id: "wi_1", kind: WorkItemKind.TASK } as const;

  it("is true for an item in the recurring status", () => {
    expect(isRecurringItem(new WorkItem({ ...base, status: WorkItemStatus.RECURRING }))).toBe(true);
  });

  it("stays true while an occurrence fires (running status keeps the schedule)", () => {
    const firing = new WorkItem({
      ...base,
      status: WorkItemStatus.RUNNING,
      recurringSchedule: new RecurringSchedule({ frequency: "daily", interval: 1 }),
    });
    expect(isRecurringItem(firing)).toBe(true);
  });

  it("is false for ordinary items", () => {
    expect(isRecurringItem(new WorkItem({ ...base, status: WorkItemStatus.PENDING }))).toBe(false);
    expect(isRecurringItem(new WorkItem({ ...base, status: WorkItemStatus.SCHEDULED }))).toBe(false);
  });
});

describe("showRecurringBadge", () => {
  const base = { id: "wi_1", kind: WorkItemKind.TASK } as const;

  it("is false at rest — the RECURRING status pill is the single recurrence signal", () => {
    const recurring = new WorkItem({
      ...base,
      status: WorkItemStatus.RECURRING,
      recurringSchedule: new RecurringSchedule({ frequency: "daily", interval: 1 }),
    });
    expect(showRecurringBadge(recurring)).toBe(false);
  });

  it("is true mid-run — the pill reads 'running', so the badge is the only marker", () => {
    const firing = new WorkItem({
      ...base,
      status: WorkItemStatus.RUNNING,
      recurringSchedule: new RecurringSchedule({ frequency: "daily", interval: 1 }),
    });
    expect(showRecurringBadge(firing)).toBe(true);
  });

  it("is false for ordinary items", () => {
    expect(showRecurringBadge(new WorkItem({ ...base, status: WorkItemStatus.PENDING }))).toBe(false);
    expect(showRecurringBadge(new WorkItem({ ...base, status: WorkItemStatus.SCHEDULED }))).toBe(false);
    expect(showRecurringBadge(new WorkItem({ ...base, status: WorkItemStatus.RUNNING }))).toBe(false);
  });
});

describe("board column mapping and transition matrix (regression guards)", () => {
  it("checkpointing/recovering render in Running, scheduled/recurring in Pending", () => {
    expect(columnForStatus(WorkItemStatus.CHECKPOINTING)).toBe(WorkItemStatus.RUNNING);
    expect(columnForStatus(WorkItemStatus.RECOVERING)).toBe(WorkItemStatus.RUNNING);
    expect(columnForStatus(WorkItemStatus.SCHEDULED)).toBe(WorkItemStatus.PENDING);
    expect(columnForStatus(WorkItemStatus.RECURRING)).toBe(WorkItemStatus.PENDING);
    expect(columnForStatus(WorkItemStatus.RUNNING)).toBe(WorkItemStatus.RUNNING);
  });

  it("epics/features accept pending|ready|assigned|succeeded|cancelled", () => {
    expect(allowedStatusesForKind(WorkItemKind.EPIC)).toEqual([
      WorkItemStatus.PENDING,
      WorkItemStatus.READY,
      WorkItemStatus.ASSIGNED,
      WorkItemStatus.SUCCEEDED,
      WorkItemStatus.CANCELLED,
    ]);
    expect(allowedStatusesForKind(WorkItemKind.FEATURE)).toEqual([
      WorkItemStatus.PENDING,
      WorkItemStatus.READY,
      WorkItemStatus.ASSIGNED,
      WorkItemStatus.SUCCEEDED,
      WorkItemStatus.CANCELLED,
    ]);
  });

  it("tasks/subtasks accept every board column", () => {
    const task = allowedStatusesForKind(WorkItemKind.TASK);
    expect(task).toContain(WorkItemStatus.RUNNING);
    expect(task).toContain(WorkItemStatus.FAILED);
  });

  it("terminal statuses are succeeded/skipped/failed/cancelled only", () => {
    expect(isTerminal(WorkItemStatus.SUCCEEDED)).toBe(true);
    expect(isTerminal(WorkItemStatus.SKIPPED)).toBe(true);
    expect(isTerminal(WorkItemStatus.FAILED)).toBe(true);
    expect(isTerminal(WorkItemStatus.CANCELLED)).toBe(true);
    expect(isTerminal(WorkItemStatus.RUNNING)).toBe(false);
    expect(isTerminal(WorkItemStatus.READY)).toBe(false);
    expect(isTerminal(WorkItemStatus.RECURRING)).toBe(false);
    // The server gate unblocks only on SUCCEEDED/SKIPPED; a blocked item
    // whose blocker failed/cancelled stays blocked (named,
    // operator-resolvable). The advisory TERMINAL_STATUSES intentionally
    // diverges — keep it.
    expect(isTerminal(WorkItemStatus.BLOCKED)).toBe(false);
  });
});

describe("blocked status (blocked/waiting state)", () => {
  it("uses teal — distinct from every other status pill", () => {
    const blocked = statusMeta(WorkItemStatus.BLOCKED);
    expect(blocked.pill).toContain("text-teal-800");
    expect(blocked.pillDark).toContain("text-teal-300");
    expect(blocked.pill).toContain("bg-teal-500/15");
    expect(blocked.label).toBe("blocked");
    expect(blocked.titleLabel).toBe("Blocked");
    // Distinct from pending/scheduled so operators see why nothing is
    // dispatching.
    expect(blocked.pill).not.toBe(statusMeta(WorkItemStatus.PENDING).pill);
    expect(blocked.pill).not.toBe(statusMeta(WorkItemStatus.SCHEDULED).pill);
  });

  it("renders in the Pending board column (real pill stays teal)", () => {
    expect(columnForStatus(WorkItemStatus.BLOCKED)).toBe(WorkItemStatus.PENDING);
  });

  it("is system-managed (not manually movable) and filterable", () => {
    expect(MANUALLY_UNMOVABLE_STATUSES.has(WorkItemStatus.BLOCKED)).toBe(true);
    expect(STATUS_FILTER_OPTIONS.map((o) => o.value)).toContain(WorkItemStatus.BLOCKED);
  });
});

describe("skipped status (terminal-success, skip-status/depends_on interplay)", () => {
  it("uses yellow — distinct from every other status pill", () => {
    const skipped = statusMeta(WorkItemStatus.SKIPPED);
    expect(skipped.pill).toContain("text-yellow-800");
    expect(skipped.pillDark).toContain("text-yellow-300");
    expect(skipped.pill).toContain("bg-yellow-500/15");
    expect(skipped.label).toBe("skipped");
    expect(skipped.titleLabel).toBe("Skipped");
    // Distinct from succeeded (emerald) and assigned (amber).
    expect(skipped.pill).not.toBe(statusMeta(WorkItemStatus.SUCCEEDED).pill);
    expect(skipped.pill).not.toBe(statusMeta(WorkItemStatus.ASSIGNED).pill);
  });

  it("renders in the Succeeded board column (real pill stays yellow)", () => {
    expect(columnForStatus(WorkItemStatus.SKIPPED)).toBe(WorkItemStatus.SUCCEEDED);
  });

  it("is system-managed (not manually movable) and filterable", () => {
    expect(MANUALLY_UNMOVABLE_STATUSES.has(WorkItemStatus.SKIPPED)).toBe(true);
    expect(STATUS_FILTER_OPTIONS.map((o) => o.value)).toContain(WorkItemStatus.SKIPPED);
  });
});
