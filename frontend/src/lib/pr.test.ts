// Unit tests for the PR-link resolution helper (parallel board view).
//
// The link resolves only from the authoritative worker-authored pr_url/pr_state
// and renders only for completed (terminal) runs. There is no synthesized
// pull/new/{branch} fallback.

import { describe, expect, it } from "vitest";

import { prLinkForRun, prStateLabel, isTerminalExecutionStatus, type PrRun } from "@/lib/pr";

describe("prLinkForRun", () => {
  it("prefers the authored PR url on a completed run", () => {
    const run: PrRun = {
      prUrl: "https://github.com/a/b/pull/12",
      prState: "open",
      completed: true,
    };
    const link = prLinkForRun(run);
    expect(link).toEqual({ href: "https://github.com/a/b/pull/12", label: "PR open" });
  });

  it("renders a merged chip for a completed run", () => {
    const run: PrRun = {
      prUrl: "https://github.com/a/b/pull/12",
      prState: "merged",
      completed: true,
    };
    expect(prLinkForRun(run)).toEqual({
      href: "https://github.com/a/b/pull/12",
      label: "PR merged",
    });
  });

  it("returns null for a completed run with no authored pr_url (no fallback)", () => {
    expect(prLinkForRun({ completed: true })).toBeNull();
    expect(prLinkForRun({ completed: true, prState: "merged" })).toBeNull();
  });

  it("returns null for a non-completed (in-flight) run even with a pr_url", () => {
    const run: PrRun = { prUrl: "https://github.com/a/b/pull/12", prState: "open" };
    expect(prLinkForRun(run)).toBeNull();
    expect(prLinkForRun({ prUrl: "https://github.com/a/b/pull/12", completed: false })).toBeNull();
  });

  it("never emits a pull/new compare-page link", () => {
    const link = prLinkForRun({ prUrl: "https://github.com/a/b/pull/12", completed: true });
    expect(link?.href).not.toContain("pull/new");
    expect(link?.href).not.toContain("/pull/new/");
  });
});

describe("prStateLabel", () => {
  it("maps known states", () => {
    expect(prStateLabel("open")).toBe("PR open");
    expect(prStateLabel("merged")).toBe("PR merged");
    expect(prStateLabel("draft")).toBe("PR draft");
    expect(prStateLabel("closed")).toBe("PR closed");
    expect(prStateLabel("none")).toBe("No PR");
  });
  it("falls back for unknown/empty", () => {
    expect(prStateLabel(undefined)).toBe("View PR");
    expect(prStateLabel("weird")).toBe("View PR");
  });
});

describe("isTerminalExecutionStatus", () => {
  it("marks terminal execution statuses", () => {
    expect(isTerminalExecutionStatus(7)).toBe(true); // TERMINATED
    expect(isTerminalExecutionStatus(8)).toBe(true); // FAILED_TO_START
    expect(isTerminalExecutionStatus(9)).toBe(true); // SUCCEEDED
    expect(isTerminalExecutionStatus(10)).toBe(true); // FAILED
  });
  it("marks in-flight statuses as non-terminal", () => {
    expect(isTerminalExecutionStatus(0)).toBe(false); // UNSPECIFIED
    expect(isTerminalExecutionStatus(1)).toBe(false); // DISPATCHING
    expect(isTerminalExecutionStatus(2)).toBe(false); // RUNNING
    expect(isTerminalExecutionStatus(3)).toBe(false); // HEALTHY
    expect(isTerminalExecutionStatus(6)).toBe(false); // TERMINATING
    expect(isTerminalExecutionStatus(undefined)).toBe(false);
  });
});
