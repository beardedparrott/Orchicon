// Source guard for the native Telemetry page.
//
// The page used to be a 640 px Grafana iframe. These assertions keep it a
// first-party view: no embed, no Grafana URL helper, no stale
// "starts automatically" copy, and all five states wired through the shared
// state component.

import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

import { describe, expect, it } from "vitest";

function read(relative: string): string {
  return readFileSync(fileURLToPath(new URL(relative, import.meta.url)), "utf8");
}

const route = read("./telemetry.tsx");
const states = read("../components/telemetry/states.tsx");
const api = read("../api/telemetry.ts");
const clients = read("../api/clients.ts");

describe("telemetry route is native", () => {
  it("has no Grafana embed and no iframe", () => {
    expect(route).not.toMatch(/<iframe/i);
    expect(route).not.toMatch(/grafana/i);
    expect(clients).not.toMatch(/GRAFANA_UI_URL/);
  });

  it("tabs are a real tablist with keyboard support", () => {
    expect(route).toContain('role="tablist"');
    expect(route).toContain('role="tab"');
    expect(route).toContain("aria-selected");
    expect(route).toContain('role="tabpanel"');
    expect(route).toMatch(/ArrowRight/);
    expect(route).toMatch(/ArrowLeft/);
  });

  it("renders reports and the three graph panels", () => {
    expect(route).toContain("ReportsPanel");
    expect(route).toContain("TracesPanel");
    expect(route).toContain("MetricsGraphPanel");
    expect(route).toContain("LogsPanel");
  });

  it("drives every tab from one shared filter bar", () => {
    expect(route).toContain("<TelemetryFilters");
    expect(route).toContain("setTelemetryAttribution");
  });
});

describe("stale copy is corrected", () => {
  it("no longer claims the stack starts automatically", () => {
    expect(route.toLowerCase()).not.toContain("starts automatically");
    expect(states.toLowerCase()).not.toContain("starts automatically");
  });

  it("describes the services as host-resident processes", () => {
    expect(states).toContain("host-resident");
    expect(states).toContain("orchicon dev start");
  });
});

describe("all five states exist", () => {
  it.each(["loading", "empty", "empty-filtered", "degraded", "error", "denied"])(
    "renders the %s state",
    (kind) => {
      expect(states).toContain(`case "${kind}"`);
    },
  );

  it("names the degraded backend from degradedReason", () => {
    expect(states).toContain("degradedReason");
    expect(api).toContain("degradedReason");
  });

  it("maps permission-denied from the RPC code", () => {
    expect(states).toContain("Code.PermissionDenied");
  });
});

describe("filters and reports are wired to the wire contract", () => {
  it("sends the identity + window + search filters", () => {
    for (const field of ["projectId", "executionId", "workItemId", "start", "end"]) {
      expect(api).toContain(field);
    }
    expect(api).toContain("search:");
  });

  it("reads the report RPC with a group-by", () => {
    expect(api).toContain("getTelemetryReport");
    expect(api).toContain("groupBy");
  });
});
