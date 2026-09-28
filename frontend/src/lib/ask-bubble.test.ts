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
