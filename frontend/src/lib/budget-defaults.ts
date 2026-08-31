// ─── Budget warning defaults (mirror of the backend budget ladder) ────────
//
// The Go execution engine applies a unified warn → escalate → abort ladder
// per budget dimension (tokens / cost / tool-call / time). Each dimension
// has three thresholds plus three escalating message templates.
//
// The thresholds and messages are now DATABASE settings (the typed
// budget_ladder columns on tenant_settings), not code constants. The Settings
// API returns the effective ladder on the default_budget_overrides field, and
// the form below reads/writes those values directly — there is no hardcoded
// message copy here, so the UI can never drift from what the backend/DB
// actually sends.

export interface DimWarnings {
  fracs: [string, string, string]; // warn / escalate / final thresholds (0..1)
  msgs: [string, string, string]; // warn / escalate / final messages
}

export interface BudgetWarnings {
  tokens: DimWarnings;
  costUsd: DimWarnings;
  toolCallCount: DimWarnings;
  wallClockSeconds: DimWarnings;
}

// Dimension key → the JSON key used in the stored budget (snake_case).
const DIM_JSON_KEY: Record<keyof BudgetWarnings, string> = {
  tokens: "tokens",
  costUsd: "cost_usd",
  toolCallCount: "tool_call_count",
  wallClockSeconds: "wall_clock_seconds",
};

export const emptyWarnings: BudgetWarnings = {
  tokens: { fracs: ["", "", ""], msgs: ["", "", ""] },
  costUsd: { fracs: ["", "", ""], msgs: ["", "", ""] },
  toolCallCount: { fracs: ["", "", ""], msgs: ["", "", ""] },
  wallClockSeconds: { fracs: ["", "", ""], msgs: ["", "", ""] },
};

// Per-tier compaction policy [warn, escalate, final]: whether a budget-ladder
// tier ALSO triggers a context compact (or only injects its warning message).
// Mirrors the backend default — warn does NOT compact (the lossy collapse
// interrupts the worker mid-flight), escalate + final do.
export type CompactTiers = [boolean, boolean, boolean];
export const defaultCompactTiers: CompactTiers = [false, true, true];

export interface BudgetDefaultsForm {
  tokens: string;
  costUsd: string;
  wallClockSeconds: string;
  toolCallCount: string;
  compactMaxTurns: string;
  compactTiers: CompactTiers;
  warnings: BudgetWarnings;
}

// Parse the tenant default_budget_overrides JSON into form strings + warning
// config. Every value comes straight from the API payload (which is built from
// the DB columns) — no hardcoded fallback copy.
export function parseBudgetDefaults(raw?: string): BudgetDefaultsForm {
  const empty: BudgetDefaultsForm = {
    tokens: "",
    costUsd: "",
    wallClockSeconds: "",
    toolCallCount: "",
    compactMaxTurns: "",
    compactTiers: defaultCompactTiers,
    warnings: emptyWarnings,
  };
  if (!raw) return empty;
  try {
    const m = JSON.parse(raw);
    const w = m.warnings ?? {};
    const read = (dim: keyof BudgetWarnings): DimWarnings => {
      const key = DIM_JSON_KEY[dim];
      const f = w.fractions?.[key];
      const ms = w.messages?.[key];
      return {
        fracs: [
          f?.[0] != null ? String(f[0]) : "",
          f?.[1] != null ? String(f[1]) : "",
          f?.[2] != null ? String(f[2]) : "",
        ],
        msgs: [
          ms?.[0] != null ? String(ms[0]) : "",
          ms?.[1] != null ? String(ms[1]) : "",
          ms?.[2] != null ? String(ms[2]) : "",
        ],
      };
    };
    // Read the per-tier compaction policy, defaulting to the built-in
    // [false, true, true] when the stored budget has no compact_tiers array.
    const ct = m.compact_tiers;
    const compactTiers: CompactTiers =
      Array.isArray(ct) && ct.length === 3
        ? [Boolean(ct[0]), Boolean(ct[1]), Boolean(ct[2])]
        : defaultCompactTiers;
    return {
      tokens: m.tokens != null ? String(m.tokens) : "",
      costUsd: m.cost_usd != null ? String(m.cost_usd) : "",
      wallClockSeconds: m.wall_clock_seconds != null ? String(m.wall_clock_seconds) : "",
      toolCallCount: m.tool_call_count != null ? String(m.tool_call_count) : "",
      compactMaxTurns: m.compact_max_turns != null ? String(m.compact_max_turns) : "",
      compactTiers,
      warnings: {
        tokens: read("tokens"),
        costUsd: read("costUsd"),
        toolCallCount: read("toolCallCount"),
        wallClockSeconds: read("wallClockSeconds"),
      },
    };
  } catch {
    return empty;
  }
}

// Build the default_budget_overrides JSON from the form fields. Empty
// fields are omitted so the worker/tenant fall back to built-in defaults.
export function buildBudgetDefaults(
  tokens: string,
  costUsd: string,
  wallClockSeconds: string,
  toolCallCount: string,
  compactMaxTurns: string,
  compactTiers: CompactTiers,
  warnings: BudgetWarnings,
): string {
  const out: Record<string, number | object> = {};
  if (tokens !== "") out.tokens = Number(tokens);
  if (costUsd !== "") out.cost_usd = Number(costUsd);
  if (wallClockSeconds !== "") out.wall_clock_seconds = Number(wallClockSeconds);
  if (toolCallCount !== "") out.tool_call_count = Number(toolCallCount);
  if (compactMaxTurns !== "") out.compact_max_turns = Number(compactMaxTurns);
  out.compact_tiers = [...compactTiers];

  const fracs: Record<string, number[]> = {};
  const msgs: Record<string, string[]> = {};
  const dimFrac = [
    ["tokens", "tokens"],
    ["cost_usd", "costUsd"],
    ["tool_call_count", "toolCallCount"],
    ["wall_clock_seconds", "wallClockSeconds"],
  ] as const;
  for (const [key, field] of dimFrac) {
    const f = warnings[field].fracs;
    if (f.some((x) => x !== "")) {
      fracs[key] = f.map((x) => Number(x) || 0);
    }
    const m = warnings[field].msgs;
    if (m.some((x) => x !== "")) {
      msgs[key] = m;
    }
  }
  if (Object.keys(fracs).length || Object.keys(msgs).length) {
    const warningsObj: Record<string, object> = {};
    if (Object.keys(fracs).length) warningsObj.fractions = fracs;
    if (Object.keys(msgs).length) warningsObj.messages = msgs;
    out.warnings = warningsObj;
  }
  return JSON.stringify(out);
}
