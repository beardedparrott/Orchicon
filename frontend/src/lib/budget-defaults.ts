// ─── Budget warning defaults (mirror of the backend budget ladder) ────────
//
// The Go execution engine applies a unified warn → escalate → abort ladder
// per budget dimension (tokens / cost / tool-call / time). Each dimension
// has three thresholds plus three escalating message templates. When a
// budget entry omits `warnings.messages` (or leaves a slot blank), the
// backend falls back to its built-in copy — defaultWarnMsgs() in
// internal/opencode/compact.go.
//
// The Settings GUI needs to SHOW the operator exactly what will be sent, so
// it seeds blank message slots with the built-in copy below. KEEP THIS IN
// SYNC with defaultWarnMsgs() in internal/opencode/compact.go.

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

export const DEFAULT_WARN_MSGS: Record<keyof BudgetWarnings, [string, string, string]> = {
  tokens: [
    "WARNING: You have used {pct}% of your token budget. Your output is too large and you are spending too many tokens. " +
      "STOP expanding the work. Tighten your approach: batch your tool calls, stop narrating, and deliver only the minimal delta. " +
      "If you do not reduce your token spend immediately, your session will be KILLED.",
    "CRITICAL: You have used {pct}% of your token budget. You are burning through tokens dangerously fast. " +
      "Consolidate EVERY remaining tool call into a single batch and finish the deliverable NOW. " +
      "Your session will be KILLED if you keep spending at this rate.",
    "FINAL WARNING: You have used {pct}% of your token budget. This is your last chance. " +
      "You must complete your work in the next minimal number of tool calls or your session will be KILLED. " +
      "Stop all exploration. Finish now.",
  ],
  costUsd: [
    "WARNING: You have used {pct}% of your cost budget. You are spending too much money. " +
      "STOP the expensive work and consolidate. Batch tool calls. Deliver the minimum. " +
      "Reduce your spend immediately or your session will be KILLED.",
    "CRITICAL: You have used {pct}% of your cost budget. You are on pace to blow past it. " +
      "Use only the cheapest possible tool calls, do not re-derive anything, and finish NOW. " +
      "Your session will be KILLED if you keep spending.",
    "FINAL WARNING: You have used {pct}% of your cost budget. This is your last warning. " +
      "Complete your work in the next minimal tool calls or your session will be KILLED.",
  ],
  toolCallCount: [
    "WARNING: YOU ARE CALLING TOOLS TOO OFTEN. BATCH YOUR TOOL CALLS TOGETHER OR YOU WILL RISK YOUR SESSION BEING KILLED.",
    "CRITICAL: YOU ARE STILL CALLING TOOLS TOO OFTEN. STOP the micro tool calls. You MUST batch them together " +
      "into a single round-trip. Your session will be KILLED if you keep splitting your calls.",
    "FINAL WARNING: YOUR TOOL CALL LIMIT IS ALMOST REACHED. YOU HAVE ONLY A HANDFUL OF TOOL CALLS LEFT. " +
      "You MUST finish your work in the next tool calls or your session WILL BE KILLED. " +
      "Batch everything. Finish now.",
  ],
  wallClockSeconds: [
    "WARNING: IT HAS BEEN {pct}% OF YOUR TIME BUDGET. YOU NEED TO WORK QUICKLY AND FINISH YOUR WORK TO AVOID EXCEEDING BUDGET — YOUR SESSION WILL BE KILLED.",
    "CRITICAL: YOU ARE RUNNING OUT OF TIME ({pct}% ELAPSED). STOP the slow path: batch your remaining tool calls and finish NOW. " +
      "Your session will be KILLED if you do not finish quickly.",
    "FINAL WARNING: {pct}% OF YOUR TIME IS GONE. You have almost no time left. " +
      "Complete your work in the next tool calls. Your session will be KILLED at the time limit.",
  ],
};

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

// warnings seeded with the built-in message copy (blank thresholds so the
// backend keeps its default fractions). Used when the stored budget is
// empty/unparseable so the GUI never shows a wall of blanks.
function defaultWarningsWithMessages(): BudgetWarnings {
  return {
    tokens: { fracs: ["", "", ""], msgs: [...DEFAULT_WARN_MSGS.tokens] },
    costUsd: { fracs: ["", "", ""], msgs: [...DEFAULT_WARN_MSGS.costUsd] },
    toolCallCount: { fracs: ["", "", ""], msgs: [...DEFAULT_WARN_MSGS.toolCallCount] },
    wallClockSeconds: { fracs: ["", "", ""], msgs: [...DEFAULT_WARN_MSGS.wallClockSeconds] },
  };
}

export interface BudgetDefaultsForm {
  tokens: string;
  costUsd: string;
  wallClockSeconds: string;
  toolCallCount: string;
  compactMaxTurns: string;
  warnings: BudgetWarnings;
}

// Parse the tenant default_budget_overrides JSON into form strings + warning
// config. Blank message slots are seeded with the built-in copy so the
// operator sees the message that will actually be sent.
export function parseBudgetDefaults(raw?: string): BudgetDefaultsForm {
  const empty: BudgetDefaultsForm = {
    tokens: "",
    costUsd: "",
    wallClockSeconds: "",
    toolCallCount: "",
    compactMaxTurns: "",
    warnings: defaultWarningsWithMessages(),
  };
  if (!raw) return empty;
  try {
    const m = JSON.parse(raw);
    const w = m.warnings ?? {};
    const read = (dim: keyof BudgetWarnings): DimWarnings => {
      const key = DIM_JSON_KEY[dim];
      const f = w.fractions?.[key];
      const ms = w.messages?.[key];
      const defaults = DEFAULT_WARN_MSGS[dim];
      return {
        fracs: [
          f?.[0] != null ? String(f[0]) : "",
          f?.[1] != null ? String(f[1]) : "",
          f?.[2] != null ? String(f[2]) : "",
        ],
        msgs: [
          ms?.[0] != null && ms[0] !== "" ? String(ms[0]) : defaults[0],
          ms?.[1] != null && ms[1] !== "" ? String(ms[1]) : defaults[1],
          ms?.[2] != null && ms[2] !== "" ? String(ms[2]) : defaults[2],
        ],
      };
    };
    return {
      tokens: m.tokens != null ? String(m.tokens) : "",
      costUsd: m.cost_usd != null ? String(m.cost_usd) : "",
      wallClockSeconds: m.wall_clock_seconds != null ? String(m.wall_clock_seconds) : "",
      toolCallCount: m.tool_call_count != null ? String(m.tool_call_count) : "",
      compactMaxTurns: m.compact_max_turns != null ? String(m.compact_max_turns) : "",
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
  warnings: BudgetWarnings,
): string {
  const out: Record<string, number | object> = {};
  if (tokens !== "") out.tokens = Number(tokens);
  if (costUsd !== "") out.cost_usd = Number(costUsd);
  if (wallClockSeconds !== "") out.wall_clock_seconds = Number(wallClockSeconds);
  if (toolCallCount !== "") out.tool_call_count = Number(toolCallCount);
  if (compactMaxTurns !== "") out.compact_max_turns = Number(compactMaxTurns);

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
