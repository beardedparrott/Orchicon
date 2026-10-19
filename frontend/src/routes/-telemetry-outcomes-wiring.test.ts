// WHERE the outcome counters are wired on the Telemetry page. The counters are
// computed once on the server and asserted once in report-outcomes.test.tsx, but
// a surface that forgets to RENDER them silently loses them exactly where the
// operator looks first — so both call sites are pinned by source assertion (the
// repo idiom for route-level contracts; see -local-mode-disclosure-wiring.test.ts).
//
// THE BUG THIS PINS. The Overview tab ("the dashboard") is the page's default
// tab, and it read GetDashboard: cost, tokens, execution count, providers. Every
// new outcome counter — completed / failed / cancelled / open, and the workflow
// pass/fail split — was served by the API and rendered on Reports, so the
// operator's first screen still said nothing about whether any work finished.
// "There aren't any new metrics I can see on the dashboard."
import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";

const page = fs.readFileSync(path.join(__dirname, "telemetry.tsx"), "utf8");

describe("the telemetry dashboard shows work outcomes", () => {
  it("reads the report that carries the outcome counters", () => {
    expect(page).toContain("useGetTelemetryReport");
    expect(page).toContain("const outcomes = report?.outcomes");
  });

  it("renders every work-outcome counter on the Overview tab", () => {
    // Each card must name the counter it shows, not just exist.
    for (const [label, field] of [
      ["Work completed", "workSucceeded"],
      ["Work failed", "workFailed"],
      ["Work cancelled", "workCancelled"],
      ["Work still open", "workOpen"],
    ] as const) {
      expect(page).toContain(label);
      expect(page).toContain(`outcomes?.${field}`);
    }
  });

  it("keeps the workflow-run outcomes too, with aborted distinct from failed", () => {
    expect(page).toContain("outcomes?.workflowsCompleted");
    expect(page).toContain("outcomes?.workflowsFailed");
    // Aborted is cancelled work. Folding it into Failed reports it as a failure,
    // which is the misreport the server-side counter exists to prevent.
    expect(page).toContain("outcomes?.workflowsAborted");
    // The sentence wraps in the rendered card, so assert its distinctive stem.
    expect(page).toMatch(/aborted is cancelled work, not a\s+failure/);
  });

  it("states the window the counters cover", () => {
    // A count with no window is not an answer. GetDashboard is lifetime and the
    // report defaults to 24h, so an unlabelled number on the same page would
    // mean two different things in two places.
    expect(page).toContain("windowLabel");
    expect(page).toContain("report?.windowStart");
  });

  it("renders the counters on the OVERVIEW panel, not only under Reports", () => {
    const overview = page.indexOf("function OverviewPanel");
    // The CALL, not the import (which sits at the top of the file).
    const reportsUse = page.indexOf("useGetTelemetryReport({");
    const credits = page.indexOf("function CreditsPanel");
    expect(overview).toBeGreaterThan(-1);
    expect(credits).toBeGreaterThan(overview);
    // The report hook and the counters live between the Overview panel's start
    // and the next panel, i.e. inside Overview.
    expect(reportsUse).toBeGreaterThan(overview);
    expect(reportsUse).toBeLessThan(credits);
    expect(page.indexOf("Work completed")).toBeLessThan(credits);
  });
});
