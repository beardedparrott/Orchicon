// sessionItems.compacted.test.ts — a mid-run collapse must be a ROW in the run's transcript.
//
// The `compacted` session part has been written by the opencode and claude adapters for a long time and by the
// native engine since this change; neither renderer drew it, so the one event that changes the history the
// provider receives had no row in the transcript it rewrote. Reported as a worker execution failing on
// `400 The reasoning_content in the thinking mode must be passed back to the API` after 30 tool rounds, with
// nothing on the run page to explain it.
import { describe, expect, it } from "vitest";
import { compactedNotice } from "./sessionItems";

describe("compactedNotice", () => {
  it("renders the native engine's payload with what it evicted and why it fired", () => {
    const text = compactedNotice({
      step: 29,
      reason: "budget:tokens:escalate",
      messages_before: 61,
      messages_after: 12,
      tool_rounds_evicted: 27,
      fresh_tokens: 268000,
      cost_usd: 0.31,
    });
    expect(text).toContain("Context compacted mid-run at step 29");
    expect(text).toContain("61 messages collapsed to 12");
    expect(text).toContain("27 tool round(s) were moved to the offload file");
    expect(text).toContain("Trigger: budget:tokens:escalate");
    expect(text).toContain("carries the collapsed history");
  });

  it("renders the opencode and claude payload shapes too", () => {
    expect(
      compactedNotice({ step: 12, reason: "window_pressure", provider: "opencode-go", model: "deepseek-v4.1-flash" }),
    ).toContain("Provider: opencode-go (deepseek-v4.1-flash)");
    expect(compactedNotice({ source: "claude_auto_compact" })).toContain("Source: claude_auto_compact");
    expect(compactedNotice({ step: 4, reason: "manual", provider: "anthropic" })).toContain(
      "Provider: anthropic",
    );
  });

  // A part that names nothing recognisable STILL renders: losing the fact that context was collapsed because
  // an adapter used different field names is exactly how it went unseen.
  it("renders an opaque payload rather than dropping it", () => {
    expect(compactedNotice({})).toBe("");
    expect(compactedNotice({ scope: "session" })).toBe("");
    expect(compactedNotice({ step: 3 })).toContain("at step 3");
  });

  it("tolerates numeric fields arriving as strings", () => {
    expect(compactedNotice({ step: "7", messages_before: "40", messages_after: "9" })).toContain(
      "40 messages collapsed to 9",
    );
  });
});
