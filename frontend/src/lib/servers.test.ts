/**
 * servers.test.ts — the setup states a row can render, and the one that must never look like progress.
 */
import { describe, expect, it } from "vitest";

import {
  SETUP_CANCELLED,
  SETUP_DONE,
  SETUP_FAILED,
  SETUP_RUNNING,
  SETUP_STALE,
  isSetupActive,
  setupLabel,
  type SetupProgress,
} from "./servers";

/** progress builds a record with the fields a test does not care about filled in. */
function progress(over: Partial<SetupProgress>): SetupProgress {
  return {
    instance: "dev",
    state: SETUP_RUNNING,
    phase: "",
    label: "",
    percent: 0,
    detail: "",
    started_at: "",
    updated_at: "",
    pid: 0,
    error: "",
    ...over,
  };
}

describe("isSetupActive", () => {
  it("counts a RUNNING setup as active, so the page polls", () => {
    expect(isSetupActive(progress({ state: SETUP_RUNNING }))).toBe(true);
  });

  // A STALE record must keep the row "active" too. It is not work in progress, but it is work the operator
  // has to act on, and a row that stops polling the moment setup dies would never show the recovery.
  it("counts a STALE setup as active", () => {
    expect(isSetupActive(progress({ state: SETUP_STALE }))).toBe(true);
  });

  it("does not count a finished, failed or cancelled setup as active", () => {
    for (const state of [SETUP_DONE, SETUP_FAILED, SETUP_CANCELLED]) {
      expect(isSetupActive(progress({ state }))).toBe(false);
    }
    expect(isSetupActive(null)).toBe(false);
    expect(isSetupActive(undefined)).toBe(false);
  });
});

describe("setupLabel", () => {
  it("shows the step while it is working", () => {
    expect(
      setupLabel(progress({ state: SETUP_RUNNING, label: "Downloading PostgreSQL" })),
    ).toBe("Downloading PostgreSQL");
  });

  it("falls back to a generic line rather than an empty one", () => {
    expect(setupLabel(progress({ state: SETUP_RUNNING, label: "" }))).toBe("Setting up…");
  });

  it("states the failure and its cause", () => {
    expect(
      setupLabel(progress({ state: SETUP_FAILED, error: "port 5432 is held" })),
    ).toBe("Setup failed: port 5432 is held");
    // An error with no message still says it failed — a blank row is not an option.
    expect(setupLabel(progress({ state: SETUP_FAILED, error: "" }))).toBe("Setup failed");
  });

  // THE ONE THAT MATTERS. A detached install that was killed cannot write "failed", so the record is left
  // claiming to be running. Without this state the row shows a progress bar that never moves and never
  // explains itself — the operator's "setup stopped responding" is the whole point of computing it.
  it("says a dead setup stopped responding rather than showing progress", () => {
    const label = setupLabel(progress({ state: SETUP_STALE }));
    expect(label).toContain("stopped responding");
    expect(label).toContain("retry");
    // It must NOT read as progress.
    expect(label).not.toContain("Setting up");
  });

  it("says a finished setup is complete", () => {
    expect(setupLabel(progress({ state: SETUP_DONE }))).toBe("Setup complete");
  });

  it("renders nothing for no record", () => {
    expect(setupLabel(null)).toBe("");
    expect(setupLabel(undefined)).toBe("");
  });
});
