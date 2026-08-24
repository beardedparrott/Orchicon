import { describe, expect, it } from "vitest";

import { emptyWarnings, parseBudgetDefaults, buildBudgetDefaults, defaultCompactTiers } from "@/lib/budget-defaults";

describe("parseBudgetDefaults", () => {
  it("reads gates and leaves the ladder blank when the stored budget has no warnings block", () => {
    // A budget with gates only (no `warnings`). No hardcoded message copy is
    // seeded — the API is expected to return the ladder from the DB, and any
    // missing slot is simply blank in the form.
    const parsed = parseBudgetDefaults('{"tokens":500000,"cost_usd":0.25,"compact_max_turns":30}');

    expect(parsed.tokens).toBe("500000");
    expect(parsed.costUsd).toBe("0.25");
    expect(parsed.compactMaxTurns).toBe("30");

    expect(parsed.warnings.tokens.msgs).toEqual(["", "", ""]);
    expect(parsed.warnings.tokens.fracs).toEqual(["", "", ""]);
    expect(parsed.warnings.costUsd.msgs).toEqual(["", "", ""]);
  });

  it("returns a fully-empty form for an empty or unparseable budget", () => {
    expect(parseBudgetDefaults(undefined).warnings.tokens.msgs).toEqual(["", "", ""]);
    expect(parseBudgetDefaults("").warnings.tokens.msgs).toEqual(["", "", ""]);
    expect(parseBudgetDefaults("not-json").warnings.tokens.msgs).toEqual(["", "", ""]);
    // No compact_tiers in the payload → falls back to the built-in default.
    expect(parseBudgetDefaults(undefined).compactTiers).toEqual(defaultCompactTiers);
    expect(parseBudgetDefaults("{}").compactTiers).toEqual(defaultCompactTiers);
  });

  it("reads the per-tier compaction policy verbatim", () => {
    const parsed = parseBudgetDefaults('{"compact_tiers":[true,false,true]}');
    expect(parsed.compactTiers).toEqual([true, false, true]);
  });

  it("reads explicitly-configured messages and thresholds verbatim", () => {
    const parsed = parseBudgetDefaults(
      JSON.stringify({
        warnings: {
          fractions: { tokens: ["0.15", "0.3", "0.5"] },
          messages: { tokens: ["a", "b", "c"] },
        },
      }),
    );

    expect(parsed.warnings.tokens.fracs).toEqual(["0.15", "0.3", "0.5"]);
    expect(parsed.warnings.tokens.msgs).toEqual(["a", "b", "c"]);
    // An omitted dimension is left blank (no fallback copy).
    expect(parsed.warnings.costUsd.msgs).toEqual(["", "", ""]);
  });

  it("keeps a blank slot inside a configured messages array as-is", () => {
    const parsed = parseBudgetDefaults(
      JSON.stringify({ warnings: { messages: { tokens: ["custom", "", "final"] } } }),
    );

    expect(parsed.warnings.tokens.msgs[0]).toBe("custom");
    expect(parsed.warnings.tokens.msgs[1]).toBe("");
    expect(parsed.warnings.tokens.msgs[2]).toBe("final");
  });
});

describe("buildBudgetDefaults", () => {
  it("round-trips gates and the ladder into the stored JSON", () => {
    const form = parseBudgetDefaults(
      JSON.stringify({
        tokens: 500000,
        cost_usd: 0.25,
        compact_max_turns: 30,
        warnings: {
          fractions: { tokens: ["0.25", "0.5", "0.75"] },
          messages: { tokens: ["w", "e", "f"] },
        },
      }),
    );
    const rebuilt = buildBudgetDefaults(
      form.tokens,
      form.costUsd,
      form.wallClockSeconds,
      form.toolCallCount,
      form.compactMaxTurns,
      form.compactTiers,
      form.warnings,
    );

    const parsed2 = parseBudgetDefaults(rebuilt);
    expect(parsed2.tokens).toBe("500000");
    expect(parsed2.costUsd).toBe("0.25");
    expect(parsed2.compactMaxTurns).toBe("30");
    expect(parsed2.compactTiers).toEqual(defaultCompactTiers);
    expect(parsed2.warnings.tokens.fracs).toEqual(["0.25", "0.5", "0.75"]);
    expect(parsed2.warnings.tokens.msgs).toEqual(["w", "e", "f"]);
  });

  it("always emits compact_tiers (policy has no 'unset' — it is a stable choice)", () => {
    const out = JSON.parse(buildBudgetDefaults("", "", "", "", "", defaultCompactTiers, emptyWarnings));
    expect(out.compact_tiers).toEqual([false, true, true]);
    // The warnings block is still omitted when all thresholds/messages are blank.
    expect(out.warnings).toBeUndefined();
  });
});
