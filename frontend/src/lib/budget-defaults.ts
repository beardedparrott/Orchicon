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
  maxToolOutputBytes: string;
  compactTiers: CompactTiers;
  warnings: BudgetWarnings;
  // Context editing (clearing consumed tool results) — separate from compaction.
  clearEnabled: boolean;
  clearTriggerFrac: string;
  clearKeepRecent: string;
  clearMinBytes: string;
  clearBandWidth: string;
}

/** The `context_editing` block as the form carries it. */
export interface ContextEditingDraft {
  enabled: boolean;
  triggerFrac: string;
  keepRecent: string;
  minBytes: string;
  bandWidth: string;
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
    maxToolOutputBytes: "",
    compactTiers: defaultCompactTiers,
    warnings: emptyWarnings,
    clearEnabled: false,
    clearTriggerFrac: "",
    clearKeepRecent: "",
    clearMinBytes: "",
    clearBandWidth: "",
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
    const ce = m.context_editing ?? {};
    const numStr = (v: unknown) => (v != null ? String(v) : "");
    return {
      clearEnabled: ce.enabled === true,
      clearTriggerFrac: numStr(ce.trigger_frac),
      clearKeepRecent: numStr(ce.keep_recent),
      clearMinBytes: numStr(ce.min_bytes),
      clearBandWidth: numStr(ce.band_width),
      tokens: m.tokens != null ? String(m.tokens) : "",
      costUsd: m.cost_usd != null ? String(m.cost_usd) : "",
      wallClockSeconds: m.wall_clock_seconds != null ? String(m.wall_clock_seconds) : "",
      toolCallCount: m.tool_call_count != null ? String(m.tool_call_count) : "",
      compactMaxTurns: m.compact_max_turns != null ? String(m.compact_max_turns) : "",
      maxToolOutputBytes: m.max_tool_output_bytes != null ? String(m.max_tool_output_bytes) : "",
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

// Build the default_budget_overrides JSON from the form fields. Empty fields
// are omitted, and an omitted ceiling is NO ceiling — there is no built-in
// fallback for any of them, so the operator's silence means "unlimited".
/**
 * Top-level budget-document keys this form does NOT render but must PRESERVE across a save.
 *
 * buildBudgetDefaults rebuilds the whole document from its own fields, so any key it does not know is
 * dropped the moment an operator presses Save. Every key in this document is a decision that costs money
 * or changes what a worker sees, so a form silently deleting one is the worst failure shape available:
 * nothing errors, the setting just stops applying, and the next run behaves differently for no visible
 * reason. `context_editing` is exactly that case (an agent configures it through update_settings; nothing
 * in this form renders it).
 *
 * Exported so a test can assert parse -> build round-trips every key, rather than trusting this comment.
 */
export const UNMODELED_BUDGET_KEYS = ["context_editing"] as const;

export function buildBudgetDefaults(
  tokens: string,
  costUsd: string,
  wallClockSeconds: string,
  toolCallCount: string,
  compactMaxTurns: string,
  maxToolOutputBytes: string,
  compactTiers: CompactTiers,
  warnings: BudgetWarnings,
  /** The raw document the form was seeded from — unmodeled keys are carried forward from it. */
  preserveSource?: string,
  /** Context editing (clearing). Omitted = the block is not written at all. */
  clearing?: ContextEditingDraft,
): string {
  const out: Record<string, number | object> = {};
  if (tokens !== "") out.tokens = Number(tokens);
  if (costUsd !== "") out.cost_usd = Number(costUsd);
  if (wallClockSeconds !== "") out.wall_clock_seconds = Number(wallClockSeconds);
  if (toolCallCount !== "") out.tool_call_count = Number(toolCallCount);
  if (compactMaxTurns !== "") out.compact_max_turns = Number(compactMaxTurns);
  if (maxToolOutputBytes !== "") out.max_tool_output_bytes = Number(maxToolOutputBytes);
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

  // CONTEXT EDITING. Written only when the operator has engaged with it, so an untouched tenant keeps a
  // clean document and the built-in "off" default applies.
  if (clearing) {
    const engaged =
      clearing.enabled ||
      [clearing.triggerFrac, clearing.keepRecent, clearing.minBytes, clearing.bandWidth].some(
        (v) => v.trim() !== "",
      );
    if (engaged) {
      const ce: Record<string, number | boolean> = { enabled: clearing.enabled };
      const num = (v: string) => {
        const n = Number(v);
        return v.trim() !== "" && Number.isFinite(n) ? n : undefined;
      };
      const tf = num(clearing.triggerFrac);
      if (tf !== undefined) ce.trigger_frac = tf;
      const kr = num(clearing.keepRecent);
      if (kr !== undefined) ce.keep_recent = kr;
      const mb = num(clearing.minBytes);
      if (mb !== undefined) ce.min_bytes = mb;
      const bw = num(clearing.bandWidth);
      if (bw !== undefined) ce.band_width = bw;
      out.context_editing = ce;
    }
  }

  // CARRY UNMODELED BLOCKS FORWARD. An unparseable source is not fatal: the form's own fields are still
  // saved, and losing an unreadable blob is no worse than the state it was in.
  if (preserveSource) {
    try {
      const src = JSON.parse(preserveSource);
      for (const key of UNMODELED_BUDGET_KEYS) {
        if (src?.[key] != null) out[key] = src[key];
      }
    } catch {
      /* leave the preserved blocks out rather than refusing the save */
    }
  }
  return JSON.stringify(out);
}
