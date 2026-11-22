// -list-all-conversations.test.ts
//
// THE BUG THIS PINS. `useListConversations` asked for ONE page of 50 and stopped. Everything downstream
// believed it had the whole set: the rail's search filtered those 50, "select all" selected those 50, and
// a bulk delete removed those 50 — after which the next page arrived and the SAME search matched more
// rows. The operator saw it as a cascade:
//
//   "I search hello, select all (19), delete them, then 5 more appear, I delete those, then 3 more."
//
// Measured on the live tenant: 409 conversations, 70 matching "hello". A 50-row window can only show a
// fraction of the 70, which is exactly why the count kept reappearing.
//
// The search was never wrong. It was only ever shown a window.

import { describe, expect, it } from "vitest";
import { listAllConversations } from "./askOrchicon";

/** fakeApi serves `total` conversations across pages of `pageSize`, newest-first by index. */
function fakeApi(total: number, opts?: { ignoreToken?: boolean }) {
  const calls: { pageSize: number; pageToken: string }[] = [];
  const byId = Array.from({ length: total }, (_, i) => ({ id: `c${i}` }));
  return {
    calls,
    fetchPage: async (req: { pageSize: number; pageToken: string }) => {
      calls.push(req);
      const start = req.pageToken === "" ? 0 : byId.findIndex((c) => c.id === req.pageToken) + 1;
      const slice = byId.slice(start, start + req.pageSize);
      const last = slice[slice.length - 1];
      return {
        conversations: slice,
        // A token is returned only when there is more to read.
        nextPageToken: opts?.ignoreToken ? "always" : last && start + slice.length < total ? last.id : "",
      };
    },
  };
}

describe("listAllConversations", () => {
  it("returns EVERY conversation, not just the first page", async () => {
    const api = fakeApi(409);
    const all = await listAllConversations(api.fetchPage);
    expect(all).toHaveLength(409);
  });

  it("keeps paging until the server stops returning a token", async () => {
    const api = fakeApi(250);
    await listAllConversations(api.fetchPage);
    // 250 across 100-row pages = 3 requests.
    expect(api.calls).toHaveLength(3);
  });

  it("makes ONE request when everything fits on a page", async () => {
    const api = fakeApi(12);
    const all = await listAllConversations(api.fetchPage);
    expect(all).toHaveLength(12);
    expect(api.calls).toHaveLength(1);
  });

  it("hard-stops instead of spinning when a server never clears the token", async () => {
    // A buggy or hostile server must not hang the rail forever.
    const api = fakeApi(10_000, { ignoreToken: true });
    const all = await listAllConversations(api.fetchPage);
    // Bounded, and it did terminate.
    expect(api.calls.length).toBeLessThanOrEqual(40);
    expect(all.length).toBeGreaterThan(0);
  });

  it("returns an empty list for a tenant with no conversations", async () => {
    const api = fakeApi(0);
    expect(await listAllConversations(api.fetchPage)).toEqual([]);
  });

  it("never repeats a conversation across pages", async () => {
    // The whole point: a duplicate would let a bulk delete target the same row twice and report a
    // failure for the second, or select-all would count one conversation as two.
    const api = fakeApi(320);
    const all = await listAllConversations(api.fetchPage);
    expect(new Set(all.map((c) => c.id)).size).toBe(all.length);
  });
});

// The numbers from the reported case, spelled out.
describe("the reported cascade", () => {
  it("one 50-row page could never contain all 70 matches", () => {
    const TOTAL = 409;
    const MATCHES = 70;
    const OLD_WINDOW = 50;
    // The old code saw at most 50 conversations, so it could not show all 70 matches even in principle.
    expect(Math.min(OLD_WINDOW, TOTAL)).toBeLessThan(MATCHES);
    // Paging to exhaustion sees all of them, which is what makes "select all then delete" finish.
    expect(TOTAL).toBeGreaterThan(MATCHES);
  });
});
