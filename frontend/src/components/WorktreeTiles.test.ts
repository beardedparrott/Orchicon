import { describe, expect, it } from "vitest";

import { worktreeTileItems } from "./WorktreeTiles";

/**
 * WHY THESE EXIST.
 *
 * The operator aborted run 01M8CSC92YWEZSC2FTRWC2K5A7, saw "Worktree: Pruned", and reasonably read
 * it as "my work is gone". It was not: the BRANCH was alive on origin at a94256ecb, and only the
 * disposable checkout had been reaped.
 *
 * Two different losses share the `pruned` status and the tile did not distinguish them. These tests
 * pin the distinction, because "the work survived" is exactly the thing an operator needs to know
 * and exactly the thing a one-word status hid.
 */
describe("worktreeTileItems", () => {
  it("says the WORKTREE went and the BRANCH stayed, rather than a bare 'Pruned'", () => {
    const items = worktreeTileItems("pruned", "6-antigravity-ask", "");
    const worktree = items.find((i) => i.label === "Worktree");
    const branch = items.find((i) => i.label === "Branch");

    expect(worktree?.value).toContain("Reaped");
    expect(branch?.value).toContain("6-antigravity-ask");
    expect(branch?.value).toContain("kept");
    // The old lie must not come back.
    expect(items.some((i) => i.value === "Pruned")).toBe(false);
  });

  it("records honestly when no branch survived", () => {
    const items = worktreeTileItems("pruned", "", "");
    expect(items.find((i) => i.label === "Branch")?.value).toBe("None recorded");
  });

  it("still shows path AND branch when the worktree is live", () => {
    const items = worktreeTileItems("ready", "feat/x", "/tmp/wt");
    expect(items.map((i) => i.label)).toEqual(["Worktree", "Branch", "Path"]);
  });
});
