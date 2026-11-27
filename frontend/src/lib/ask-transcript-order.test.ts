import { readFileSync } from "node:fs";

import { describe, expect, it } from "vitest";

/**
 * THE TRANSCRIPT'S READING ORDER, PINNED IN BOTH CLIENTS.
 *
 * A turn has a shape: REASON -> ACT -> REPLY, and the clarifying card last (it is what the operator
 * acts on). The tool rows were inserted AFTER the assistant's prose in both clients, which inverts the
 * turn — the operator saw their bash calls listed BELOW the reply, as though the work happened after
 * the answer:
 *
 *	"All of your last bash calls are listed below your response to me. I would think they should be
 *	 above that and your final message is what should be at the bottom. This seems to be in the TUI
 *	 and the GUI"
 *
 * This is a source-order assertion because the transcript is assembled in two different languages from
 * a server payload; what would silently regress is the ORDER of the two blocks, which is exactly what
 * these check.
 */
describe("tool rows precede the reply in the GUI transcript", () => {
  it("places ask-tool-rows before AssistantBubble", () => {
    const src = readFileSync("src/routes/ask-orchicon.tsx", "utf8");
    // THE DURABLE MESSAGE RENDERER, not the live streaming one. The file has TWO AssistantBubble
    // call sites: the trailing live fallback (~2769) and the per-message row (~3565). Comparing
    // against the first would pass or fail for the wrong reason — which it did on my first attempt.
    const tools = src.indexOf('data-testid="ask-tool-rows"');
    expect(tools, "ask-tool-rows must exist in the GUI transcript").toBeGreaterThan(0);
    // The reply bubble in the SAME renderer: the first AssistantBubble AFTER the tool rows marker.
    const bubble = src.indexOf("<AssistantBubble", tools);
    expect(bubble, "the durable message renderer must draw an AssistantBubble").toBeGreaterThan(0);
    expect(
      tools,
      "tool rows must be drawn BEFORE the assistant bubble — a turn is reason -> act -> reply",
    ).toBeLessThan(bubble);
  });
});

describe("tool rows precede the reply in the TUI transcript", () => {
  it("appends toolRowsForMessage before the reply item", () => {
    const src = readFileSync("../internal/tui/chat/controller.go", "utf8");
    const tools = src.indexOf("items = append(items, toolRowsForMessage(m)...)");
    // The reply item is the one carrying the message Key.
    const reply = src.indexOf('Key:       "m-" + m.GetId(),');
    expect(tools, "toolRowsForMessage must be appended").toBeGreaterThan(0);
    expect(reply, "the reply item must be appended").toBeGreaterThan(0);
    expect(
      tools,
      "the TUI must append tool rows BEFORE the reply — the same order the GUI draws",
    ).toBeLessThan(reply);
  });

  it("keeps the clarifying CARD last in both clients", () => {
    const go = readFileSync("../internal/tui/chat/controller.go", "utf8");
    const card = go.indexOf('"-ask",');
    const tools = go.indexOf("items = append(items, toolRowsForMessage(m)...)");
    expect(card, "the card append must exist").toBeGreaterThan(0);
    expect(card, "the card must come AFTER the tool rows — it is what the operator acts on").toBeGreaterThan(tools);
  });
});
