import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";

import { describeEngineStatus, type ContainerEngineStatus } from "@/api/engine";

// AC5: availability must be legible WHERE EXECUTION MODE IS CHOSEN, so the sentence the badge
// renders is part of the contract, not decoration. These are the three states the plane's
// /enginez reports (internal/api/engine_status.go) plus the two degenerate ones.
function status(over: Partial<ContainerEngineStatus>): ContainerEngineStatus {
  return {
    engine: "podman",
    minimum: "4.0.0",
    available: false,
    state: "unavailable",
    ...over,
  };
}

describe("describeEngineStatus", () => {
  it("says nothing is known yet before the first answer", () => {
    expect(describeEngineStatus(undefined)).toBe("Checking whether containers are available…");
  });

  it("ready names the engine and the version the plane proved", () => {
    const text = describeEngineStatus(
      status({ available: true, state: "ready", version: "5.2.1", machine: "running" }),
    );
    // The machine state is redundant when it is running and absent on Linux — neither is noise
    // the operator needs next to the execution-mode select.
    expect(text).toBe("Containers ready (podman 5.2.1)");
  });

  it("ready mentions a machine state that is not the happy one", () => {
    const text = describeEngineStatus(
      status({ available: true, state: "ready", version: "5.2.1", machine: "stopped" }),
    );
    expect(text).toBe("Containers ready (podman 5.2.1, machine stopped)");
  });

  it("provisioning is its own sentence, not a spinner", () => {
    expect(describeEngineStatus(status({ state: "provisioning" }))).toBe(
      "Provisioning containers — podman is being installed…",
    );
  });

  it("unavailable carries the REASON and what happens instead", () => {
    const text = describeEngineStatus(
      status({ reason: "podman 3.4.4 is older than the minimum 4.0.0" }),
    );
    expect(text).toBe(
      "Containers unavailable — podman 3.4.4 is older than the minimum 4.0.0. Runs will execute locally.",
    );
  });

  it("unavailable without a reason still says runs execute locally", () => {
    expect(describeEngineStatus(status({}))).toBe(
      "Containers unavailable — no container engine. Runs will execute locally.",
    );
  });
});

// THE DEV SERVER MUST FORWARD /enginez. Without the proxy rule, fetch("/enginez") is answered
// by the SPA's index.html — HTTP 200 with HTML — so the badge tells the operator availability
// is unknown on a host where containers were just proven to work. Cheap source assertion
// because vite.config.ts is read by Vite, not imported by any test.
describe("vite dev proxy", () => {
  it("forwards /enginez to the control plane", () => {
    const src = fs.readFileSync(path.join(__dirname, "../../vite.config.ts"), "utf8");
    expect(src).toMatch(/"\/enginez"\s*:\s*\{/);
  });
});
