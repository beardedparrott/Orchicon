import { describe, it, expect } from "vitest";
import type { PermissionAsk } from "@/api/gen/orchicon/api/v1/ask_orchicon_service_pb";
import { applyAskChunk, interleave, type AskItem } from "./ask-consent";

const ask = (askId: string): PermissionAsk =>
  ({
    askId,
    conversationId: "conv-1",
    sessionId: "ses-1",
    tool: "write",
    command: "",
    targets: ["/p/sibling/notes.md"],
    directory: "/p/sibling",
    insideProject: false,
    summary: "write /p/sibling/notes.md",
    denyEntriesBelow: [],
  }) as PermissionAsk;

type Msg = { at: number; id: string };
const msg = (id: string, at: number): Msg => ({ at, id });

describe("interleave", () => {
  // THE BUG THIS FIXES. The operator: "the permission blocks in the GUI are still
  // remaining at the bottom at the end of a turn which makes no sense. They should
  // be in the conversation and move up just like any other conversation block."
  //
  // The cards were a separate list rendered AFTER every message, so a card asked
  // in the middle of a conversation sat at the bottom forever.
  it("places a card BETWEEN the messages it happened between", () => {
    const asks = applyAskChunk([], ask("per_1"), 150) as AskItem[];
    const blocks = [msg("m1", 100), msg("m2", 200), msg("m3", 300)];

    const out = interleave(blocks, asks);

    expect(out.map((s) => (s.isAsk ? "ASK" : (s.block as Msg).id))).toEqual([
      "m1",
      "ASK",
      "m2",
      "m3",
    ]);
  });

  // The operator's words: they must "move up just like any other conversation
  // block". A card asked early does not stay pinned to the bottom as the
  // conversation grows past it.
  it("moves a settled card up as later messages arrive", () => {
    const asks = applyAskChunk([], ask("per_1"), 150) as AskItem[];

    const before = interleave([msg("m1", 100)], asks);
    expect(before[before.length - 1].isAsk).toBe(true);

    const after = interleave([msg("m1", 100), msg("m2", 200), msg("m3", 300)], asks);
    expect(after.map((s) => (s.isAsk ? "ASK" : (s.block as Msg).id))).toEqual([
      "m1",
      "ASK",
      "m2",
      "m3",
    ]);
    // Specifically: NO LONGER last.
    expect(after[after.length - 1].isAsk).toBe(false);
  });

  it("keeps a card at the end when it is genuinely the newest thing", () => {
    const asks = applyAskChunk([], ask("per_1"), 400) as AskItem[];
    const out = interleave([msg("m1", 100), msg("m2", 200)], asks);
    expect(out[out.length - 1].isAsk).toBe(true);
  });

  it("orders several cards against each other and the messages", () => {
    let asks = applyAskChunk([], ask("per_1"), 150);
    asks = applyAskChunk(asks, ask("per_2"), 250);
    const out = interleave([msg("m1", 100), msg("m2", 200), msg("m3", 300)], asks);
    expect(out.map((s) => (s.isAsk ? "ASK" : (s.block as Msg).id))).toEqual([
      "m1",
      "ASK",
      "m2",
      "ASK",
      "m3",
    ]);
  });

  // Same-instant blocks must not swap on a re-render (a poll re-runs this on
  // every tick), so the order has to be deterministic rather than
  // implementation-defined.
  it("is deterministic for same-instant blocks", () => {
    const asks = applyAskChunk([], ask("per_1"), 100) as AskItem[];
    const a = interleave([msg("m1", 100)], asks);
    const b = interleave([msg("m1", 100)], asks);
    expect(a.map((s) => (s.isAsk ? "ASK" : (s.block as Msg).id))).toEqual(
      b.map((s) => (s.isAsk ? "ASK" : (s.block as Msg).id)),
    );
  });

  it("returns the messages untouched when there are no asks", () => {
    const out = interleave([msg("m1", 100), msg("m2", 200)], undefined);
    expect(out.map((s) => (s.block as Msg).id)).toEqual(["m1", "m2"]);
  });
});

describe("applyAskChunk stamps the card's arrival time", () => {
  // Without a timestamp the interleave cannot place the card at all. The bug was
  // a MISSING timestamp on the Go side; the equivalent here would be a card with
  // at === 0, which sorts first — i.e. the same wrong end.
  it("records the arrival time it was given", () => {
    const asks = applyAskChunk([], ask("per_1"), 1234) as AskItem[];
    expect(asks[0].at).toBe(1234);
  });

  it("defaults to now when no time is supplied", () => {
    const before = Date.now();
    const asks = applyAskChunk([], ask("per_1")) as AskItem[];
    expect(asks[0].at).toBeGreaterThanOrEqual(before);
  });
});
