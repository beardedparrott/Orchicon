// mergeEdits resume/dedup contract tests (diff pipeline, AC 6).
//
// mergeEdits is the client-side half of the stream discipline: the durable
// fetch is a superset of everything up to ~2s ago, so live events append
// only when their sequence exceeds the durable max, and dedupe by id
// (the stream's event_id == the ledger row id) so a reconnect never
// double-applies. A mid-stream reconnect loses nothing and duplicates
// nothing.
import { describe, expect, it } from "vitest";
import { mergeEdits } from "@/api/fileEdits";
import type { FileEdit } from "@/api/gen/orchicon/api/v1/file_edit_pb";

function edit(id: string, seq: bigint, path = "a.txt"): FileEdit {
  return { id, seq, path } as FileEdit;
}

describe("mergeEdits", () => {
  it("appends live events beyond the durable max (ordered delivery)", () => {
    const durable = [edit("r1", 1n), edit("r2", 2n)];
    const live = [edit("r3", 3n), edit("r4", 4n)];
    const merged = mergeEdits(durable, live);
    expect(merged.map((e) => e.id)).toEqual(["r1", "r2", "r3", "r4"]);
  });

  it("drops a late lower-seq live event (covered by next durable refetch)", () => {
    const durable = [edit("r1", 1n), edit("r2", 2n)];
    const live = [edit("r4", 4n), edit("r3", 3n)];
    // Live delivery is seq-ordered per connection; a late lower-seq event
    // (r3 arriving after r4 advanced the running max) is dropped — the next
    // durable refetch covers it. This pins the documented discipline.
    const merged = mergeEdits(durable, live);
    expect(merged.map((e) => e.id)).toEqual(["r1", "r2", "r4"]);
  });

  it("dedupes reconnect replays by id (event_id == row id)", () => {
    const durable = [edit("r1", 1n), edit("r2", 2n)];
    // Reconnect replays r2 (already in durable) plus the new r3.
    const live = [edit("r2", 2n), edit("r3", 3n)];
    const merged = mergeEdits(durable, live);
    expect(merged.map((e) => e.id)).toEqual(["r1", "r2", "r3"]);
  });

  it("drops stale live events at or below the durable max", () => {
    const durable = [edit("r1", 1n), edit("r2", 2n)];
    const live = [edit("stale", 1n, "old.txt")];
    const merged = mergeEdits(durable, live);
    expect(merged.map((e) => e.id)).toEqual(["r1", "r2"]);
  });

  it("returns durable alone when the stream delivered nothing", () => {
    const durable = [edit("r1", 1n)];
    expect(mergeEdits(durable, [])).toEqual(durable);
  });

  it("returns live alone when the durable fetch was empty", () => {
    const merged = mergeEdits([], [edit("r1", 1n)]);
    expect(merged.map((e) => e.id)).toEqual(["r1"]);
  });
});
