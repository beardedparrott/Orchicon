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
