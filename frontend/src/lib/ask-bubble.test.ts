import { describe, expect, it } from "vitest";
import { bubbleKindFor } from "./ask-bubble";

// The bug this pins: a `system` row — the platform's own record that the context was
// compacted — fell through to the assistant's bubble, so the transcript read as though
// the model had said its history had been summarized.

describe("bubbleKindFor", () => {
  it("routes a platform notice to the notice shell, not the assistant's", () => {
    expect(bubbleKindFor({ role: "system" })).toBe("notice");
  });

  it("routes the operator's own messages to the user shell", () => {
    expect(bubbleKindFor({ role: "user" })).toBe("user");
  });

  it("routes a failed turn to the error shell", () => {
    expect(bubbleKindFor({ role: "assistant", metadata: { error: "reply timed out" } })).toBe("error");
  });

  // THE BUG THIS PINS. The error shell renders the error text INSTEAD of the message content, so
  // routing a stopped turn there deleted a reply the server had already persisted. Stopping a turn
  // wiped the operator's whole visible turn.
  it("keeps a STOPPED turn's reply rather than routing it to the error shell", () => {
    expect(
      bubbleKindFor({ role: "assistant", metadata: { stopped: "Turn stopped by the user." } }),
    ).toBe("assistant");
  });

  it("still routes a genuine failure to the error shell", () => {
    expect(bubbleKindFor({ role: "assistant", metadata: { error: "provider status 429" } })).toBe(
      "error",
    );
  });

  it("prefers the stopped shell over error if a row somehow carries both", () => {
    // A stopped turn must never be presented as a failure; the stop is the operator's own action.
    expect(
      bubbleKindFor({
        role: "assistant",
        metadata: { error: "Turn stopped by the user.", stopped: "Turn stopped by the user." },
      }),
    ).toBe("assistant");
  });

  it("routes a plain assistant reply to the assistant shell", () => {
    expect(bubbleKindFor({ role: "assistant" })).toBe("assistant");
    expect(bubbleKindFor({ role: "assistant", metadata: { error: "" } })).toBe("assistant");
  });

  it("still treats an unknown role as the assistant's, which is the correct default here", () => {
    // Every other row the service writes IS the model's reply, so the default stays. The
    // fix is that `system` is NAMED above it, not that the default changed.
    expect(bubbleKindFor({ role: "tool" })).toBe("assistant");
  });
});
