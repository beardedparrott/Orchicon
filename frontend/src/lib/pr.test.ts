// Unit tests for the PR-link resolution helper (parallel board view).
//
// Two source types: the authoritative worker-authored pr_url/pr_state and
// the deterministic pull/new/{branch} fallback derived from the project's
// repo_slug. Both must be provider-free on the read path.

import { describe, expect, it } from "vitest";

import { prLinkForRun, prStateLabel, type PrRun } from "@/lib/pr";

describe("prLinkForRun", () => {
  it("prefers the authored PR url", () => {
    const run: PrRun = {
      prUrl: "https://github.com/a/b/pull/12",
      prState: "open",
      worktreeStatus: "ready",
      worktreeBranch: "feat-x-abc",
    };
    const link = prLinkForRun(run, "a/b");
    expect(link).toEqual({ href: "https://github.com/a/b/pull/12", label: "PR open", isFallback: false });
  });

  it("falls back to a deterministic pull/new link for a ready git-backed run", () => {
    const run: PrRun = { worktreeStatus: "ready", worktreeBranch: "feat-x-abc" };
    const link = prLinkForRun(run, "beardedparrott/Orchicon");
    expect(link).toEqual({
      href: "https://github.com/beardedparrott/Orchicon/pull/new/feat-x-abc",
      label: "No PR yet",
      isFallback: true,
    });
  });

  it("prefers the per-run repoSlug over the card-level slug", () => {
    const run: PrRun = {
      worktreeStatus: "ready",
      worktreeBranch: "feat-x-abc",
      repoSlug: "other/repo",
    };
    const link = prLinkForRun(run, "a/b");
    expect(link?.href).toBe("https://github.com/other/repo/pull/new/feat-x-abc");
  });

  it("returns null when there is no branch and no authored url", () => {
    expect(prLinkForRun({ worktreeStatus: "pending" }, "a/b")).toBeNull();
  });

  it("does not fall back when the worktree is not ready", () => {
    expect(prLinkForRun({ worktreeStatus: "skipped", worktreeBranch: "x" }, "a/b")).toBeNull();
  });

  it("does not fall back without a repo slug", () => {
    expect(prLinkForRun({ worktreeStatus: "ready", worktreeBranch: "x" }, undefined)).toBeNull();
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
