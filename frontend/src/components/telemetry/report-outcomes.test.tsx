import { describe, expect, it } from "vitest";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";

import { ReportModels, ReportOutcomes } from "./report-outcomes";
import { TelemetryReportModel, TelemetryReportOutcomes } from "@/api/gen/orchicon/api/v1/telemetry_service_pb";

// The outcome roll-up is the answer to "did the work land?", so what is pinned
// here is that it reports every state it is given, tells aborted apart from
// failed, and does not invent a row for a state that never occurred.
//
// The counter vocabulary is the part that was easy to get wrong: workflow_runs
// success is 'completed' (there is no 'succeeded'), and 'aborted' is cancelled
// work, not a failure. Both are asserted through the rendered surface, not
// through the server, because a panel that renders "Failed 0" next to a
// nonzero aborted count is exactly the misreport this guards.

function outcomes(partial: Partial<Record<keyof TelemetryReportOutcomes, number>>): TelemetryReportOutcomes {
  return Object.assign(new TelemetryReportOutcomes(), partial);
}

function render(node: Parameters<typeof renderToStaticMarkup>[0]): string {
  return renderToStaticMarkup(node);
}

describe("ReportOutcomes", () => {
  it("shows completed, failed, cancelled and open work", () => {
    const html = render(
      createElement(ReportOutcomes, {
        outcomes: outcomes({
          workSucceeded: 12,
          workFailed: 3,
          workCancelled: 2,
          workOpen: 7,
          workflowsCompleted: 4,
          workflowsFailed: 1,
          workflowsRunning: 2,
        }),
      }),
    );
    expect(html).toContain("Work items");
    expect(html).toContain("Completed");
    expect(html).toContain("12");
    expect(html).toContain("Failed");
    expect(html).toContain("Cancelled");
    expect(html).toContain("Still open");
    expect(html).toContain("Workflow runs");
    expect(html).toContain("Running");
  });

  it("does not render a row for a state that never happened", () => {
    // A permanent "0 Skipped" row is noise, and implies a state to watch for.
    const html = render(
      createElement(ReportOutcomes, {
        outcomes: outcomes({ workSucceeded: 5, workOpen: 1, workflowsCompleted: 1 }),
      }),
    );
    expect(html).not.toContain("Skipped");
    expect(html).not.toContain("Aborted");
  });

  it("surfaces skipped and aborted once they are non-zero", () => {
    const html = render(
      createElement(ReportOutcomes, {
        outcomes: outcomes({
          workSucceeded: 1,
          workSkipped: 4,
          workflowsCompleted: 1,
          workflowsAborted: 3,
        }),
      }),
    );
    expect(html).toContain("Skipped");
    expect(html).toContain("never ran");
    expect(html).toContain("Aborted");
  });

  it("says nothing finished rather than showing a row of zeroes", () => {
    const html = render(createElement(ReportOutcomes, { outcomes: outcomes({}) }));
    expect(html).toContain("Nothing finished in this window.");
  });

  it("renders a usable empty state when the report carries no outcomes at all", () => {
    // A nil outcomes message must not crash the panel or claim work happened.
    const html = render(createElement(ReportOutcomes, {}));
    expect(html).toContain("Nothing finished in this window.");
  });
});

describe("ReportModels", () => {
  const alpha = Object.assign(new TelemetryReportModel(), {
    model: "model-alpha",
    totalTokens: 1000,
    costUsd: 0.75,
    executions: 3,
  });
  const beta = Object.assign(new TelemetryReportModel(), {
    model: "model-beta",
    totalTokens: 200,
    costUsd: 0.25,
    executions: 1,
  });

  it("lists models with cost share that adds up", () => {
    const html = render(createElement(ReportModels, { models: [alpha, beta] }));
    expect(html).toContain("model-alpha");
    expect(html).toContain("model-beta");
    expect(html).toContain("75.0%");
    expect(html).toContain("25.0%");
  });

  it("does not divide by zero when every model costs nothing", () => {
    const free = Object.assign(new TelemetryReportModel(), { model: "free", totalTokens: 10, costUsd: 0 });
    const html = render(createElement(ReportModels, { models: [free] }));
    expect(html).toContain("free");
    expect(html).toContain("—");
    expect(html).not.toContain("NaN");
  });

  it("explains an empty window rather than rendering an empty table", () => {
    const html = render(createElement(ReportModels, { models: [] }));
    expect(html).toContain("No model usage recorded in this window.");
    expect(html).not.toContain("<table");
  });
});
