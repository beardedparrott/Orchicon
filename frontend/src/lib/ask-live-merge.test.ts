import { describe, expect, it } from "vitest";
import {
  dropSupersededMirror,
  hasLiveTextItems,
  mirrorRowSuperseded,
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
