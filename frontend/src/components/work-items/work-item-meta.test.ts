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
} from "@/api/gen/orchicon/api/v1/work_item_pb";
import {
  kindMeta,
  statusMeta,
  columnForStatus,
  allowedStatusesForKind,
  isTerminal,
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
});

describe("board column mapping and transition matrix (regression guards)", () => {
  it("checkpointing/recovering render in Running, scheduled in Pending", () => {
    expect(columnForStatus(WorkItemStatus.CHECKPOINTING)).toBe(WorkItemStatus.RUNNING);
    expect(columnForStatus(WorkItemStatus.RECOVERING)).toBe(WorkItemStatus.RUNNING);
    expect(columnForStatus(WorkItemStatus.SCHEDULED)).toBe(WorkItemStatus.PENDING);
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

  it("terminal statuses are succeeded/failed/cancelled only", () => {
    expect(isTerminal(WorkItemStatus.SUCCEEDED)).toBe(true);
    expect(isTerminal(WorkItemStatus.FAILED)).toBe(true);
    expect(isTerminal(WorkItemStatus.CANCELLED)).toBe(true);
    expect(isTerminal(WorkItemStatus.RUNNING)).toBe(false);
    expect(isTerminal(WorkItemStatus.READY)).toBe(false);
  });
});
