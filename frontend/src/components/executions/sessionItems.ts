// Pure helpers for the session chat pane's RUNNING-view merge.
//
// While an execution is live the pane must show the FULL conversation
// (joining mid-run shows everything already said) plus the newest events
// still streaming in, without duplicating anything:
//
//   - the runner records the session transcript and flushes it every ~2s,
//     so `history` is a superset of everything up to ~2s ago;
//   - the live event stream carries the same events instantly (assistant
//     text as 40-char chunks, reasoning as 40-char chunks);
//   - so we render `history` + only the live events NEWER than the
//     transcript's latest part.
//
// Grouping is by "phase", not by consecutive adjacency: a phase is one
// continuous assistant generation period (an opencode step / one "think"
// span). Every reasoning chunk emitted within a phase forms ONE growing
// reasoning bubble and every text chunk forms ONE growing text bubble —
// regardless of how interleaved-streaming models interleave them.
// Interleaving (reasoning → text → reasoning…) used to produce a run of
// chopped fragments; phase-keyed grouping coalesces them.
export interface ParsedTool {
  id: string;
  toolName: string;
  input: string;
  output: string;
  at: number;
}

export type ChatItem =
  | { kind: "user"; text: string; source: string; at: number; key: string }
  | { kind: "text"; text: string; at: number; key: string; live?: boolean; phase?: string }
  | { kind: "tool"; tool: ParsedTool; key: string }
  | { kind: "reasoning"; text: string; at: number; key: string; live?: boolean; phase?: string }
  | { kind: "error"; text: string; at: number; key: string }
  | { kind: "artifact"; name: string; type: string; content: string; at: number; key: string }
  | { kind: "session"; sessionId: string; serveUrl: string; at: number; key: string };

export function itemAt(i: ChatItem): number {
  return i.kind === "tool" ? i.tool.at : i.at;
}

// --- phase-keyed grouping ------------------------------------------------
//
// Each producer assigns a stable "phase" key to its text/reasoning items:
//   - transcript history parts get `step-N` (N = opencode step counter,
//     incremented on step_start / step_finish; stable because seq is);
//   - live event chunks get `live-N` (a monotonic synthetic counter,
//     incremented on tool/error boundaries — the events array is
//     monotonic, so the keys are stable across renders).
// Items without a key (legacy/untagged producers) fall back to `live`/
// `hist` so history and live chunks never merge by accident.
interface PhaseGroupable {
  kind: string;
  phase?: string;
  live?: boolean;
}

function itemPhase<T extends PhaseGroupable>(item: T): string {
  return item.phase ?? (item.live ? "live" : "hist");
}

/**
 * groupPhaseGroups splits a stream into one group per (kind, phase),
 * sealing the current phase on boundary items (tool, error, user
 * message, step boundary…) and whenever the phase key changes. Groups
 * are emitted in first-appearance order, so reasoning-before-text stays
 * reasoning-before-text.
 *
 * `absorb` names kinds that do NOT seal the phase: such an item is
 * appended to the open text group of its phase when one exists (used by
 * RuntimeSessionPane for artifacts, which render inline inside the
 * assistant bubble), otherwise it is emitted on its own.
 */
export function groupPhaseGroups<T extends PhaseGroupable>(
  items: T[],
  absorb: ReadonlySet<string> = new Set(),
): T[][] {
  const out: T[][] = [];
  let text: T[] = [];
  let reasoning: T[] = [];
  let order: ("text" | "reasoning")[] = [];
  let phase: string | null = null;

  const flush = () => {
    for (const k of order) out.push(k === "text" ? text : reasoning);
    text = [];
    reasoning = [];
    order = [];
    phase = null;
  };

  for (const item of items) {
    const p = itemPhase(item);
    if (item.kind === "text") {
      if (phase !== null && p !== phase) flush();
      if (phase === null) phase = p;
      if (!order.includes("text")) order.push("text");
      text.push(item);
    } else if (item.kind === "reasoning") {
      if (phase !== null && p !== phase) flush();
      if (phase === null) phase = p;
      if (!order.includes("reasoning")) order.push("reasoning");
      reasoning.push(item);
    } else if (absorb.has(item.kind)) {
      if (text.length > 0) text.push(item);
      else out.push([item]);
    } else {
      flush();
      out.push([item]);
    }
  }
  flush();
  return out;
}

/**
 * groupByPhase merges each (kind, phase) group into one growing ChatItem:
 * text is concatenated, `at` is the last chunk's, `key` the first's, and
 * `live` is true when any chunk is live. Boundary items pass through
 * unchanged. This is the ChatItem-typed wrapper around groupPhaseGroups.
 */
export function groupByPhase(items: ChatItem[]): ChatItem[] {
  return groupPhaseGroups(items).map((g) => {
    if (g.length <= 1) return g[0];
    const kind = g[0].kind;
    if (kind !== "text" && kind !== "reasoning") return g[0];
    const run = g as { text: string; at: number; key: string; live?: boolean; phase?: string }[];
    return {
      kind,
      text: run.map((t) => t.text).join(""),
      at: run[run.length - 1].at,
      key: run[0].key,
      live: run.some((t) => t.live),
      phase: run[0].phase,
    } as ChatItem;
  });
}

/**
 * mergeSessionItems merges the durable transcript history with the live
 * event stream for a RUNNING execution. Returns the transcript history
 * plus only the live events not yet reflected in it, with text /
 * reasoning chunks grouped per phase into single growing bubbles.
 */
export function mergeSessionItems(history: ChatItem[], live: ChatItem[]): ChatItem[] {
  const lastHistoryAt = history.reduce((m, i) => Math.max(m, itemAt(i)), 0);
  const fresh = live.filter((i) => itemAt(i) >= lastHistoryAt);
  const merged = [...history, ...fresh].sort((a, b) => itemAt(a) - itemAt(b));
  return groupByPhase(merged);
}
