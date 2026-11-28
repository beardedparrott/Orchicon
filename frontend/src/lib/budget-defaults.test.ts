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
      form.maxToolOutputBytes,
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

  // The tool-output cap rides the budget document with its sibling tuning knobs, so it must survive a
  // parse -> build round trip exactly like compact_max_turns does.
  it("round-trips max_tool_output_bytes", () => {
    const form = parseBudgetDefaults(JSON.stringify({ max_tool_output_bytes: 32768 }));
    expect(form.maxToolOutputBytes).toBe("32768");
    const rebuilt = buildBudgetDefaults(
      "", "", "", "", "",   // tokens, cost, wall clock, tool calls, compact turns
      form.maxToolOutputBytes,
      defaultCompactTiers,
      emptyWarnings,
    );
    expect(JSON.parse(rebuilt).max_tool_output_bytes).toBe(32768);
  });

  // An ABSENT cap must not be written at all: absent means "fall back to the env var / built-in
  // default", which is a different state from a stored 0 (an explicit zero-byte cap).
  // Context editing is ONE knob: keep the newest N tool results. It must round-trip, and an empty value
  // must write NOTHING — absence means "off", which is a different state from a stored 0.
  it("round-trips context_editing.keep_tool_calls", () => {
    const form = parseBudgetDefaults(JSON.stringify({ context_editing: { keep_tool_calls: 20 } }));
    expect(form.keepToolCalls).toBe("20");
    const rebuilt = buildBudgetDefaults(
      "", "", "", "", "", "", defaultCompactTiers, emptyWarnings,
      undefined,
      { keepToolCalls: form.keepToolCalls },
    );
    expect(JSON.parse(rebuilt).context_editing).toEqual({ keep_tool_calls: 20 });
  });

  it("omits context_editing entirely when unset or invalid", () => {
    for (const val of ["", "0", "not-a-number"]) {
      const out = JSON.parse(
        buildBudgetDefaults("", "", "", "", "", "", defaultCompactTiers, emptyWarnings, undefined, {
          keepToolCalls: val,
        }),
      );
      expect(out.context_editing).toBeUndefined();
    }
  });

  it("omits max_tool_output_bytes when unset", () => {
    const out = JSON.parse(buildBudgetDefaults("", "", "", "", "", "", defaultCompactTiers, emptyWarnings));
    expect(out.max_tool_output_bytes).toBeUndefined();
  });

  it("always emits compact_tiers (policy has no 'unset' — it is a stable choice)", () => {
    const out = JSON.parse(buildBudgetDefaults("", "", "", "", "", "", defaultCompactTiers, emptyWarnings));
    expect(out.compact_tiers).toEqual([false, true, true]);
    // The warnings block is still omitted when all thresholds/messages are blank.
    expect(out.warnings).toBeUndefined();
  });
});
