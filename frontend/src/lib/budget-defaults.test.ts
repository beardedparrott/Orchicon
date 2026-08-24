import { describe, expect, it } from "vitest";

import { DEFAULT_WARN_MSGS, emptyWarnings, parseBudgetDefaults, buildBudgetDefaults } from "@/lib/budget-defaults";

describe("parseBudgetDefaults", () => {
  it("pre-fills the built-in warning messages when the stored budget has no warnings block", () => {
    // The shape dev/prod currently persist — no `warnings` key.
    const parsed = parseBudgetDefaults('{"tokens":500000,"cost_usd":0.25,"compact_max_turns":30}');

    expect(parsed.tokens).toBe("500000");
    expect(parsed.costUsd).toBe("0.25");
    expect(parsed.compactMaxTurns).toBe("30");

    // The GUI must show the real copy that the ladder will send, not blanks.
    expect(parsed.warnings.tokens.msgs).toEqual(DEFAULT_WARN_MSGS.tokens);
    expect(parsed.warnings.costUsd.msgs).toEqual(DEFAULT_WARN_MSGS.costUsd);
    expect(parsed.warnings.toolCallCount.msgs).toEqual(DEFAULT_WARN_MSGS.toolCallCount);
    expect(parsed.warnings.wallClockSeconds.msgs).toEqual(DEFAULT_WARN_MSGS.wallClockSeconds);

    // Thresholds stay blank so the backend keeps its default fractions.
    expect(parsed.warnings.tokens.fracs).toEqual(["", "", ""]);
  });

  it("pre-fills the built-in copy when the budget is empty or unparseable", () => {
    expect(parseBudgetDefaults(undefined).warnings.tokens.msgs).toEqual(DEFAULT_WARN_MSGS.tokens);
    expect(parseBudgetDefaults("").warnings.tokens.msgs).toEqual(DEFAULT_WARN_MSGS.tokens);
    expect(parseBudgetDefaults("not-json").warnings.tokens.msgs).toEqual(DEFAULT_WARN_MSGS.tokens);
  });

  it("keeps explicitly-configured messages and pre-fills only the dimensions that are omitted", () => {
    const parsed = parseBudgetDefaults(
      JSON.stringify({ warnings: { messages: { tokens: ["a", "b", "c"] } } }),
    );

    expect(parsed.warnings.tokens.msgs).toEqual(["a", "b", "c"]);
    // cost is not configured → falls back to the built-in copy.
    expect(parsed.warnings.costUsd.msgs).toEqual(DEFAULT_WARN_MSGS.costUsd);
  });

  it("fall-backs a blank slot inside a configured messages array to the built-in copy", () => {
    const parsed = parseBudgetDefaults(
      JSON.stringify({ warnings: { messages: { tokens: ["custom", "", "final"] } } }),
    );

    expect(parsed.warnings.tokens.msgs[0]).toBe("custom");
    expect(parsed.warnings.tokens.msgs[1]).toBe(DEFAULT_WARN_MSGS.tokens[1]);
    expect(parsed.warnings.tokens.msgs[2]).toBe("final");
  });
});

describe("buildBudgetDefaults", () => {
  it("round-trips pre-filled default messages into the stored JSON", () => {
    const parsed = parseBudgetDefaults('{"tokens":500000,"cost_usd":0.25,"compact_max_turns":30}');
    const rebuilt = buildBudgetDefaults(
      parsed.tokens,
      parsed.costUsd,
      parsed.wallClockSeconds,
      parsed.toolCallCount,
      parsed.compactMaxTurns,
      parsed.warnings,
    );

    const parsed2 = parseBudgetDefaults(rebuilt);
    expect(parsed2.tokens).toBe("500000");
    expect(parsed2.costUsd).toBe("0.25");
    expect(parsed2.compactMaxTurns).toBe("30");
    expect(parsed2.warnings.tokens.msgs).toEqual(DEFAULT_WARN_MSGS.tokens);
  });

  it("omits the warnings block when all messages and thresholds are blank", () => {
    expect(JSON.parse(buildBudgetDefaults("", "", "", "", "", emptyWarnings))).toEqual({});
  });
});
