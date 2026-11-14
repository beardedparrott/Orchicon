import { describe, expect, it } from "vitest";
import {
  dropSupersededMirror,
  hasLiveTextItems,
  mirrorRowProseLifted,
  mirrorRowSuperseded,
  orderInjectedBeforeReply,
} from "./ask-live-merge";

// The double-draw this module exists to prevent was MEASURED, not assumed: against the shared
// fixture plane (which streams TextChunk AND seeds the mirror row) the GUI painted the reply as
// TWO <p> elements of 76 chars each. So the decision below is pinned rather than left inline in a
// route the test setup cannot render.

describe("mirrorRowSuperseded", () => {
  const IN_FLIGHT = "msg-acked";

  it("suppresses the in-flight row while the live stream is drawing it", () => {
    expect(mirrorRowSuperseded({ id: IN_FLIGHT }, IN_FLIGHT, true, true)).toBe(true);
  });

  it("keeps the row when this client has no live stream (reload, second tab, turn started elsewhere)", () => {
    // This is the case the mirror EXISTS for: empty live items must fall back to the row, never blank.
    expect(mirrorRowSuperseded({ id: IN_FLIGHT }, IN_FLIGHT, false, true)).toBe(false);
    expect(mirrorRowSuperseded({ id: IN_FLIGHT }, IN_FLIGHT, true, false)).toBe(false);
  });

  it("keeps the row when no acked id is known, because the mirror cannot be identified", () => {
    expect(mirrorRowSuperseded({ id: IN_FLIGHT }, null, true, true)).toBe(false);
  });

  it("never suppresses any OTHER message", () => {
    expect(mirrorRowSuperseded({ id: "msg-user" }, IN_FLIGHT, true, true)).toBe(false);
    expect(mirrorRowSuperseded({ id: "msg-older" }, IN_FLIGHT, true, true)).toBe(false);
  });
});

describe("dropSupersededMirror", () => {
  const msgs = [{ id: "u1" }, { id: "msg-acked" }];

  it("drops exactly the in-flight row and preserves order", () => {
    expect(dropSupersededMirror(msgs, "msg-acked", true, true)).toEqual([{ id: "u1" }]);
  });

  it("is a copy, not the caller's array, when it changes something", () => {
    const out = dropSupersededMirror(msgs, "msg-acked", true, true);
    expect(out).not.toBe(msgs);
    expect(msgs).toHaveLength(2);
  });

  it("returns every row when there is nothing to supersede", () => {
    expect(dropSupersededMirror(msgs, "msg-acked", false, true)).toEqual(msgs);
    expect(dropSupersededMirror(msgs, "msg-acked", true, false)).toEqual(msgs);
    expect(dropSupersededMirror(msgs, null, true, true)).toEqual(msgs);
  });
});

describe("mirrorRowSuperseded — a row carrying TOOL CALLS is never dropped", () => {
  const IN_FLIGHT = "msg-acked";

  it("keeps the in-flight row when it carries a tool call, even though its text duplicates the live copy", () => {
    // THE REPORTED BUG. The row's TEXT is a duplicate of the live stream, which is why the supersession
    // rule dropped it — but the live stream carries no tool calls, and a recorded `ask_user` call is what
    // renders the clarifying CARD (MessageBubble reads message.toolCalls). Dropping the row therefore
    // dropped the card: the operator sat on a live tab seeing "Orchicon is weighing… · no output for 123s"
    // while the card waited unrendered, and only a page reload showed it — because a fresh load has no
    // live items, so the row rendered.
    expect(
      mirrorRowSuperseded({ id: IN_FLIGHT, toolCalls: [{ id: "tc-1" }] }, IN_FLIGHT, true, true),
    ).toBe(false);
  });

  it("still drops a row that is ONLY the live text", () => {
    expect(mirrorRowSuperseded({ id: IN_FLIGHT, toolCalls: [] }, IN_FLIGHT, true, true)).toBe(true);
    expect(mirrorRowSuperseded({ id: IN_FLIGHT }, IN_FLIGHT, true, true)).toBe(true);
  });

  it("keeps a tool-carrying row through the list helper too", () => {
    const rows = [
      { id: "u1", toolCalls: [] },
      { id: IN_FLIGHT, toolCalls: [{ id: "tc-1" }] },
    ];
    expect(dropSupersededMirror(rows, IN_FLIGHT, true, true)).toEqual(rows);
  });
});

describe("hasLiveTextItems", () => {
  it("counts text and reasoning, ignores anything else", () => {
    expect(hasLiveTextItems([{ kind: "text" }])).toBe(true);
    expect(hasLiveTextItems([{ kind: "reasoning" }])).toBe(true);
    expect(hasLiveTextItems([{ kind: "error" }, { kind: "user" }])).toBe(false);
    expect(hasLiveTextItems([])).toBe(false);
  });
});

// THE REPLY WAS DRAWN TWICE. A row carrying tool calls is never superseded (it is the only thing
// that can render the ask_user card), so it stayed in the transcript WITH its prose — and the live
// stream was already drawing that same prose. The operator: "while the model is responding/thinking,
// it prints the message twice." Measured in their screenshot: 802 chars above 958, the second a
// strict superset of the first, because the row is a 250ms-old snapshot of the live text.
describe("mirrorRowProseLifted", () => {
  const IN_FLIGHT = "msg-acked";

  it("lifts the in-flight row's prose while the live stream draws it", () => {
    expect(mirrorRowProseLifted({ id: IN_FLIGHT }, IN_FLIGHT, true, true)).toBe(true);
  });

  it("lifts it EVEN WHEN the row carries tool calls — that is the case the bug was in", () => {
    // mirrorRowSuperseded deliberately keeps such a row (for its card); its prose is still a duplicate.
    const withTools = { id: IN_FLIGHT, toolCalls: [{ id: "call-1" }] };
    expect(mirrorRowSuperseded(withTools, IN_FLIGHT, true, true)).toBe(false);
    expect(mirrorRowProseLifted(withTools, IN_FLIGHT, true, true)).toBe(true);
  });

  it("keeps the row's prose whenever the row is the only copy on screen", () => {
    // No live stream (reload / second tab / turn elsewhere) or no live text yet: the row IS the view,
    // so lifting its prose would blank the turn.
    expect(mirrorRowProseLifted({ id: IN_FLIGHT }, IN_FLIGHT, false, true)).toBe(false);
    expect(mirrorRowProseLifted({ id: IN_FLIGHT }, IN_FLIGHT, true, false)).toBe(false);
  });

  it("never touches another message's prose", () => {
    expect(mirrorRowProseLifted({ id: "msg-older" }, IN_FLIGHT, true, true)).toBe(false);
    expect(mirrorRowProseLifted({ id: IN_FLIGHT }, null, true, true)).toBe(false);
  });
});

// ---------------------------------------------------------------------------------------------
// orderInjectedBeforeReply — an interjected message must render BEFORE the reply it prompted.
//
// The operator: "the interjection on opencode adapter prints the user message AFTER the response. It
// should print before and stay before the response from the model that comes after."
//
// created_at cannot express this: the assistant row is created when its turn STARTS, the interjection is
// persisted mid-turn, so created_at puts the operator's message after the answer it prompted.
// ---------------------------------------------------------------------------------------------
describe("orderInjectedBeforeReply", () => {
  const user = (id: string, injected = false) => ({ id, role: "user", injected });
  const assistant = (id: string) => ({ id, role: "assistant", injected: false });

  it("moves an interjection in front of the reply it was injected into", () => {
    // Arrival/created_at order: user asked, assistant started, operator interjected mid-turn.
    const rows = [user("u1"), assistant("a1"), user("u2", true)];
    expect(orderInjectedBeforeReply(rows).map((m) => m.id)).toEqual(["u1", "u2", "a1"]);
  });

  it("keeps it before that reply for the rest of the conversation", () => {
    const rows = [user("u1"), assistant("a1"), user("u2", true), user("u3"), assistant("a2")];
    expect(orderInjectedBeforeReply(rows).map((m) => m.id)).toEqual([
      "u1",
      "u2",
      "a1",
      "u3",
      "a2",
    ]);
  });

  it("leaves a normal conversation untouched", () => {
    const rows = [user("u1"), assistant("a1"), user("u2"), assistant("a2")];
    expect(orderInjectedBeforeReply(rows).map((m) => m.id)).toEqual(["u1", "a1", "u2", "a2"]);
  });

  it("preserves the order of several interjections in one turn", () => {
    const rows = [user("u1"), assistant("a1"), user("u2", true), user("u3", true)];
    expect(orderInjectedBeforeReply(rows).map((m) => m.id)).toEqual(["u1", "u2", "u3", "a1"]);
  });

  it("leaves an interjection with no preceding turn where it is", () => {
    // No assistant row before it means no turn to precede (nothing has started), so moving it would
    // reorder it past unrelated history for no reason.
    const rows = [user("u1"), user("u2", true)];
    expect(orderInjectedBeforeReply(rows).map((m) => m.id)).toEqual(["u1", "u2"]);
  });

  it("does not treat a normal user message as injected", () => {
    const rows = [user("u1"), assistant("a1"), user("u2")];
    expect(orderInjectedBeforeReply(rows).map((m) => m.id)).toEqual(["u1", "a1", "u2"]);
  });
});

// --- anchorLiveReply: the streaming reply belongs BELOW the interjection that prompted it -------------
//
// REPORTED: "if I send an interjecting message and then refresh the browser, my last message sits at the
// bottom and your text streams above it instead of below it". The live text was drawn as a trailing section
// after the whole transcript, so a durable interjection took its place in the timeline and the streaming
// reply was appended underneath it.
import { anchorLiveReply } from "./ask-live-merge";

type Row = { id: string };

describe("anchorLiveReply", () => {
  it("puts the live reply immediately after the row it belongs to", () => {
    // The reported shape: the interjection is durable, and a reply is still streaming into the turn it joined.
    const durable: Row[] = [
      { id: "assistant-turn" },
      { id: "user-interjection" },
      { id: "earlier-history" },
    ];
    const live: Row[] = [{ id: "live-1" }, { id: "live-2" }];

    const { messages, inserted } = anchorLiveReply(durable, live, "assistant-turn");

    expect(inserted).toBe(true);
    expect(messages.map((m) => m.id)).toEqual([
      "assistant-turn",
      "live-1",
      "live-2",
      "user-interjection",
      "earlier-history",
    ]);
  });

  it("keeps the interjection ABOVE nothing — it stays where the durable order put it", () => {
    const { messages } = anchorLiveReply(
      [{ id: "a" }, { id: "user" }],
      [{ id: "live-1" }],
      "a",
    );
    const userAt = messages.findIndex((m) => m.id === "user");
    const liveAt = messages.findIndex((m) => m.id === "live-1");
    expect(liveAt).toBeLessThan(userAt);
  });

  it("does not move anything when there is no anchor (no live turn)", () => {
    const durable: Row[] = [{ id: "a" }, { id: "b" }];
    const { messages, inserted } = anchorLiveReply(durable, [], "a");
    expect(inserted).toBe(false);
    expect(messages).toEqual(durable);
  });

  it("does not move anything when the anchor row has not arrived yet", () => {
    const durable: Row[] = [{ id: "a" }, { id: "b" }];
    const { messages, inserted } = anchorLiveReply(durable, [{ id: "live-1" }], "not-there");
    expect(inserted).toBe(false);
    expect(messages).toEqual(durable);
  });

  it("appends at the end when the anchor is the last row", () => {
    const { messages } = anchorLiveReply([{ id: "a" }, { id: "turn" }], [{ id: "live-1" }], "turn");
    expect(messages.map((m) => m.id)).toEqual(["a", "turn", "live-1"]);
  });

  it("is pure: the input arrays are untouched", () => {
    const durable: Row[] = [{ id: "a" }, { id: "b" }];
    const live: Row[] = [{ id: "live-1" }];
    anchorLiveReply(durable, live, "a");
    expect(durable.map((m) => m.id)).toEqual(["a", "b"]);
    expect(live.map((m) => m.id)).toEqual(["live-1"]);
  });
});

// --- THE LIVE REPLY'S TRANSCRIPT POSITION ---------------------------------------------------------
//
// REPORTED: "When you are watching the live stream, the reasoning and assistant text is being printed
// ABOVE the user message already sent. If you refresh the page, it puts it in the right order."
//
// THE MECHANISM. anchorLiveReply merges the streaming reply into the durable list by IDENTITY — right
// after the anchor row — which is the correct decision. But it merges CHAT MESSAGES: the live blocks are
// built from stream chunks, so they carry no createdAt at all. The route then stamps every row from
// `createdAt`, so every live block sorts at epoch 0, and interleave's sort floats the whole streaming
// reply to the TOP of the transcript — above messages sent before it. A refresh has no live items, so no
// epoch-0 blocks, so the order looks right: exactly the reported asymmetry.
import { interleave } from "./ask-consent";
import { liveBlockAt } from "./ask-live-merge";

type LiveRow = { id: string; at: number };

describe("a live reply must sort at its ANCHOR, not at epoch 0", () => {
  // The literal shapes the route produces: a real user row, the acked assistant row (server createdAt),
  // and a live block (stream chunk — no createdAt, so stamped 0).
  const userLiveRow: LiveRow = { id: "user-goal", at: 1_000 };
  const ackedLiveRow: LiveRow = { id: "msg-acked", at: 2_000 };
  const liveLiveRow: LiveRow = { id: "live-st-1", at: 0 };

  it("the ANCHORED order survives interleave: the reply stays after its anchor", () => {
    // anchorLiveReply put live-st-1 immediately after msg-acked, by identity. interleave preserves that
    // order now, so the in-flight reply can no longer be floated above the operator's message by a key.
    const slots = interleave([userLiveRow, ackedLiveRow, liveLiveRow], []);
    expect(slots.map((s) => (s.block as LiveRow).id)).toEqual(["user-goal", "msg-acked", "live-st-1"]);
  });

  it("a card raised MID-TURN lands at the point it was asked, live reply or not", () => {
    // `at` is what a card is INSERTED against, so a live block carrying a lie (0) would compare wrongly
    // against it. With the block's key stamped from its anchor, the card lands after the live reply — the
    // point it was actually asked — and does so whether or not the live block was stamped, because
    // interleave preserves the anchored order.
    const asks = [{ key: "per_1", at: 3_000 }] as unknown as Parameters<typeof interleave>[1];

    const unstamped = interleave([userLiveRow, ackedLiveRow, liveLiveRow], asks);
    expect(unstamped.map((s) => (s.isAsk ? "ASK" : (s.block as LiveRow).id))).toEqual([
      "user-goal",
      "msg-acked",
      "live-st-1",
      "ASK",
    ]);

    const stamped = interleave(
      [userLiveRow, ackedLiveRow, { ...liveLiveRow, at: liveBlockAt(ackedLiveRow.at, liveLiveRow.at) }],
      asks,
    );
    expect(stamped.map((s) => (s.isAsk ? "ASK" : (s.block as LiveRow).id))).toEqual([
      "user-goal",
      "msg-acked",
      "live-st-1",
      "ASK",
    ]);
  });

  it("falls back to the block's own arrival when no anchor timestamp is known", () => {
    // A live turn whose anchor row carried no createdAt (0) must not lose its position entirely.
    expect(liveBlockAt(0, 500)).toBe(500);
  });
});
