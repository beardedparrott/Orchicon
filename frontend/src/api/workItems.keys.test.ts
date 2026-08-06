// Regression tests for the work-item query-key builder.
//
// The page's list query lives under a key that includes the
// `{search, sortBy, sortOrder}` opts object. Mutations invalidate with
// `workItemKeys.list(projectId)` and TanStack Query matches by prefix
// (`partialMatchKey(query.queryKey, queryKey)` — it iterates over the
// FILTER key's elements). A filter key that itself ended in `undefined`
// at the `opts` slot could never match the real opts object, so
// mutations silently failed to invalidate and the board card only
// settled on the 5s poll. `workItemKeys.list` must therefore return a
// TRUE prefix of the full key.

import { describe, expect, it } from "vitest";
import { partialMatchKey } from "@tanstack/query-core";

import { workItemKeys } from "@/api/workItems";

describe("workItemKeys.list prefix semantics", () => {
  const projectId = "projA";
  const opts = { search: undefined, sortBy: "created_at", sortOrder: "desc" };

  it("drops trailing undefined params so the bare key is a true prefix", () => {
    expect(workItemKeys.list(projectId)).toEqual(["work-items", "list", projectId]);
  });

  it("keeps parentId/status/opts when provided", () => {
    expect(workItemKeys.list(projectId, "parent1")).toEqual([
      "work-items",
      "list",
      projectId,
      "parent1",
    ]);
    expect(workItemKeys.list(projectId, "parent1", 3)).toEqual([
      "work-items",
      "list",
      projectId,
      "parent1",
      3,
    ]);
    expect(workItemKeys.list(projectId, undefined, undefined, opts)).toEqual([
      "work-items",
      "list",
      projectId,
      opts,
    ]);
  });

  it("the mutation invalidation key prefix-matches the active page query key", () => {
    // This is the exact pair that regressed: the active query key holds
    // the opts object while the invalidation key is the bare prefix.
    const activeKey = workItemKeys.list(projectId, undefined, undefined, opts);
    const invalidateKey = workItemKeys.list(projectId);
    expect(partialMatchKey(activeKey, invalidateKey)).toBe(true);
  });
});
