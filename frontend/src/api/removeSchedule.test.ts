// REMOVING A SCHEDULE MUST CLEAR BOTH CLOCKS, AND BE REACHABLE FROM ONE ROW.
//
// The operator: "Deleting schedules from the schedules screen in the GUI and TUI seems to no
// longer work." Reproduced on this branch as case (c) of the work item: the RPC succeeded and the
// row came back, because the request cleared recurring_schedule/next_run_at only — a one-shot
// schedule is driven by scheduled_start_at, and the item stayed in the status=SCHEDULED query.
// On the GUI the remove action was also gated behind the multi-select gesture: there was no
// per-row control, so a row click navigated and sent nothing at all (case (a)).
//
// These assertions are SOURCE-LEVEL, deliberately: both failures are a missing request field and a
// missing per-row control inside large components, and catching them through rendering would need
// a full router + query harness for rules decidable from the source (the same reasoning as
// components/work-items/autoStartReachable.test.ts).

import { readFileSync } from "node:fs";
import { resolve } from "node:path";

import { describe, expect, it } from "vitest";

const read = (rel: string) => readFileSync(resolve(process.cwd(), rel), "utf8");

/** Strip comments so a MENTION in a rationale is not read as code. */
function code(src: string): string {
  return src
    .replace(/\{\/\*[\s\S]*?\*\/\}/g, "")
    .replace(/\/\*[\s\S]*?\*\//g, "")
    .replace(/^\s*\/\/.*$/gm, "");
}

describe("the GUI removes a schedule whole", () => {
  it("the request clears scheduled_start_at as well as the recurring schedule", () => {
    const c = code(read("src/api/workItems.ts"));
    expect(
      c,
      "clearScheduledStartAt is missing — a one-shot schedule would survive its own removal",
    ).toContain("clearScheduledStartAt: true");
    expect(c).toContain("new RecurringSchedule()");
  });

  it("a scheduled/recurring item is returned to pending, so the row leaves the scheduled query", () => {
    const c = code(read("src/api/workItems.ts"));
    expect(c).toContain("WorkItemStatus.SCHEDULED");
    expect(c).toContain("WorkItemStatus.RECURRING");
    expect(c).toContain("status: WorkItemStatus.PENDING");
  });

  it("a single row offers the remove control (no multi-select gesture required)", () => {
    const c = code(read("src/routes/schedules.tsx"));
    expect(
      c,
      "no per-row remove control — the only way to remove one schedule is to discover the tick-then-bar gesture",
    ).toContain("aria-label={`Remove schedule from ${");
    expect(c).toContain("onRemove([item.id])");
  });

  it("the bulk action is available in the Upcoming view too (not only History)", () => {
    const c = code(read("src/routes/schedules.tsx"));
    expect(c).toContain('view === "history" || view === "upcoming"');
  });
});
