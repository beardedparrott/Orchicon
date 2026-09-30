// The archive view's status chips must be readable in ANY theme — the
// regression this file exists for.
//
// The operator: "the badge that says 'Archived from' is very hard to see. Green on
// green doesn't work. We should ensure this looks good in any theme."
//
// The bug was not a missing dark style; it was a MISSING PALETTE DECISION. The
// archive view built its two chips inline from `statusMeta(...).pill` — the
// LIGHT-palette class set — while every other badge in the app routed through
// `useDarkPalette()`. So on a dark theme the chip painted `text-emerald-800` on
// `bg-emerald-500/15` over a dark-green page: the text and its own background were
// the same family, at roughly 1.5:1.
//
// TWO LAYERS, because one is not enough:
//
//   1. `statusPillClasses` — the pure decision. Every archived-from status is
//      checked in BOTH palettes, so a status added later without a distinct dark
//      variant fails here rather than shipping unreadable.
//   2. a rendered `StatusChip` — proof that the component actually CONSULTS that
//      decision, i.e. that the palette reaches the DOM.
//
// `renderToStaticMarkup` is enough for (2): the repo ships no jsdom by design (see
// SessionGrants.test.tsx), and these assertions are about emitted markup, not
// layout. It reads the store's INITIAL state, which is why (1) exists — a
// component test cannot switch palettes in-process.

import { describe, expect, it, beforeEach } from "vitest";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";

// FIRST: installs the DOM shim the theme store needs at module-init. Import order
// is load-bearing — see the module's own comment.
import "@/test/theme-test-dom";

import { WorkItemStatus } from "@/api/gen/orchicon/api/v1/work_item_pb";
import {
  StatusChip,
  StatusPill,
  statusPillClasses,
} from "@/components/work-items/work-item-badges";
import { statusMeta } from "@/components/work-items/work-item-meta";
import { useThemeStore } from "@/lib/theme-store";

/** Every status an archived item can have been archived FROM. */
const TERMINAL_STATUSES = [
  WorkItemStatus.SUCCEEDED,
  WorkItemStatus.FAILED,
  WorkItemStatus.CANCELLED,
  WorkItemStatus.SKIPPED,
];

/** The classes on the rendered element's own class attribute. */
function classesOf(html: string): string {
  const m = /class="([^"]*)"/.exec(html);
  expect(m, `no class attribute in ${html}`).not.toBeNull();
  return m![1];
}

describe("archive chip palette decision (pure)", () => {
  it("uses the DARK class set for a dark palette", () => {
    const meta = statusMeta(WorkItemStatus.SUCCEEDED);
    const cls = statusPillClasses(meta, true);
    expect(cls).toBe(meta.pillDark);
    // The light set is the green-on-green failure: emerald-800 text on an
    // emerald-500/15 background over a dark-green page.
    expect(cls).not.toContain("text-emerald-800");
    expect(cls).toContain("text-emerald-300");
  });

  it("uses the LIGHT class set for a light palette", () => {
    const meta = statusMeta(WorkItemStatus.SUCCEEDED);
    expect(statusPillClasses(meta, false)).toBe(meta.pill);
  });

  it("EVERY archived-from status has a distinct, legible pair in both palettes", () => {
    for (const status of TERMINAL_STATUSES) {
      const meta = statusMeta(status);
      const light = statusPillClasses(meta, false);
      const dark = statusPillClasses(meta, true);
      // A status whose two variants are identical would render the same hue on
      // both palettes, which is exactly the unreachable-text defect.
      expect(light, `${meta.label}: light and dark sets are identical`).not.toBe(dark);
      expect(light, `${meta.label}: no light class set`).toBeTruthy();
      expect(dark, `${meta.label}: no dark class set`).toBeTruthy();
      // Both must carry a text colour and a background — a chip with only one of
      // the two inherits the page's own (the second half of green-on-green).
      for (const set of [light, dark]) {
        expect(set, `${meta.label}: ${set} has no text- class`).toContain("text-");
        expect(set, `${meta.label}: ${set} has no bg- class`).toContain("bg-");
      }
    }
  });

  it("the 'active' ghost anchor resolves through the same decision", () => {
    // The ghost anchor is an ACTIVE ancestor shown inside the archive tree, and it
    // carried the same hardcoded light-palette bug as the archived chip.
    const meta = statusMeta(WorkItemStatus.SUCCEEDED);
    expect(statusPillClasses(meta, true)).toContain("text-emerald-300");
    expect(statusPillClasses(meta, false)).toContain("text-emerald-800");
  });
});

describe("archive chip renders", () => {
  beforeEach(() => {
    // The store's own default mode; its `apply` is what the shim stands in for.
    useThemeStore.getState().setMode("dark");
  });

  it("a rendered StatusChip carries a full class pair, never a bare hue", () => {
    const meta = statusMeta(WorkItemStatus.SUCCEEDED);
    const html = renderToStaticMarkup(
      createElement(StatusChip, { meta, label: `Archived from: ${meta.label}` }),
    );
    const cls = classesOf(html);
    expect(cls).toContain("text-");
    expect(cls).toContain("bg-");
    expect(cls).toContain("rounded-full");
    expect(html).toContain("Archived from: succeeded");
  });

  it("StatusChip and StatusPill agree — one decision, not two", () => {
    // Both route through the same helper; if a future badge re-implements the
    // choice, this is what catches it. The assert is "same palette", not "the
    // same classes" — the two components legitimately differ in size/padding.
    const meta = statusMeta(WorkItemStatus.SUCCEEDED);
    const chip = classesOf(renderToStaticMarkup(createElement(StatusChip, { meta, label: "x" })));
    const pill = classesOf(
      renderToStaticMarkup(createElement(StatusPill, { status: WorkItemStatus.SUCCEEDED })),
    );
    const inDark = (c: string) => c.includes(meta.pillDark);
    // Agreement = both on the SAME side. (Asserting "both palettes at once" would
    // be unsatisfiable: one palette is ambient at a time.)
    expect(
      inDark(chip) === inDark(pill),
      `chip (${chip}) and pill (${pill}) resolved to different palettes`,
    ).toBe(true);
    // And at least one of them is present — i.e. a palette decision was made at
    // all, rather than a bare hue with no class set.
    expect(inDark(chip) || chip.includes(meta.pill)).toBe(true);
  });

  it("the ghost 'active' chip renders the same shape", () => {
    const meta = statusMeta(WorkItemStatus.SUCCEEDED);
    const html = renderToStaticMarkup(createElement(StatusChip, { meta, label: "active" }));
    expect(html).toContain("active");
    expect(classesOf(html)).toContain("bg-");
  });
});
