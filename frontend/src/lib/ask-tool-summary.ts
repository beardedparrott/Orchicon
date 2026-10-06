// ask-tool-summary.ts — the GUI's half of the ONE tool counter both clients draw (the Go half is
// internal/toolclass). Pinned to the same bytes by internal/toolclass/testdata/rollup_fixture.json,
// read verbatim here AND by internal/toolclass/summarize_test.go — so the two clients cannot drift.
//
// It is a PORT of internal/toolclass/{toolclass.go,summarize.go}: the counting rule, the fixed
// modify→read→bash→other order, the singular/plural labels, the inclusive window boundary, the
// skipped zero/absent stamp, the future-stamp clamp and the load-bearing empty string are all that
// package's, restated here so the web client renders the identical sentence the TUI renders for the
// same ledger.

/** The rolling window the activity line counts over. Mirrors toolclass.DefaultWindow (30s) — a NAMED
 *  constant because the Go caller and this file must count the SAME span. */
export const TOOL_SUMMARY_WINDOW_MS = 30_000;

export type ToolClass = "modify" | "read" | "bash" | "other" | "ignore";

const MODIFY_TOOLS = new Set(["write", "edit", "batch_write"]);
const READ_TOOLS = new Set([
  "read",
  "batch_read",
  "grep",
  "batch_grep",
  "list",
  "glob",
  "todoread",
]);
const BASH_TOOLS = new Set(["bash", "shell"]);

/** Mirrors toolclass.Classify: case-insensitive, whitespace-tolerant, `orchicon_` stripped,
 *  `permission.` -> ignore, ask_user (3 spellings) -> ignore, todowrite -> ignore, the empty name ->
 *  ignore, and UNKNOWN -> other (the load-bearing default: a name nobody has heard of is real work
 *  and must be COUNTED, not silently dropped). */
export function classifyTool(toolName: string): ToolClass {
  let n = (toolName ?? "").toLowerCase().trim();
  // A consent decision is a record OF a decision, not work (tool_ledger.go): counting an approval
  // would let a "yes" inflate the tool tally.
  if (n.startsWith("permission.")) return "ignore";
  // `orchicon_` is a spelling of the same name, not a different tool.
  if (n.startsWith("orchicon_")) n = n.slice("orchicon_".length);
  // ask_user is a CARD, not work — all three spellings the clients use.
  if (n === "ask_user" || n === "askuser" || n === "ask_user_question") return "ignore";
  // todowrite is session bookkeeping, not operator-meaningful work: excluded EXPLICITLY (not by the
  // default) so a permanent counter does not appear on every line. Mirrors toolclass.go's D4 guard.
  if (n === "todowrite") return "ignore";
  if (MODIFY_TOOLS.has(n)) return "modify";
  if (READ_TOOLS.has(n)) return "read";
  if (BASH_TOOLS.has(n)) return "bash";
  // An empty name is not a call anybody made: never manufacture a count.
  if (n === "") return "ignore";
  // Everything else is REAL WORK, counted as `other`: Orchicon product tools (`list_projects`),
  // MCP tools (`mcp__*`) and any name added later. This is the default the under-report was missing.
  return "other";
}

/** One tool call the line can count: the name to classify and the epoch-MILLISECOND instant it was
 *  issued. 0 means "no timestamp" and is SKIPPED, never treated as the epoch (mirrors toolclass.Call). */
export interface ToolCallStamp {
  toolName: string;
  atMs: number;
}

/** The minimum a message must carry to be counted — structural, so this can be tested without
 *  building a protobuf. `issuedAtUnixMs` is a `bigint` at runtime (protoInt64, generated field). */
export interface CountableMessage {
  toolCalls?: readonly {
    functionName?: string;
    issuedAtUnixMs?: number | bigint;
  }[];
}

/** Collect the counted calls off the ListMessages page the transcript poll ALREADY fetched — the same
 *  page internal/tui/chat/pageToolCalls reads on the Go side. No new RPC, no new state.
 *
 *  `Number(...)` IS LOAD-BEARING: the generated field is protoInt64, i.e. a BigInt, and Math on a
 *  BigInt throws while a bare comparison against a number silently misbehaves. Mirrors the existing
 *  coercion of Heartbeat.serverTimeUnixMs on the two heartbeat arms. */
export function toolCallsFromMessages(
  messages: readonly CountableMessage[] | undefined,
): ToolCallStamp[] {
  const out: ToolCallStamp[] = [];
  for (const m of messages ?? []) {
    for (const c of m.toolCalls ?? []) {
      out.push({
        toolName: c?.functionName ?? "",
        atMs: Number(c?.issuedAtUnixMs ?? 0),
      });
    }
  }
  return out;
}

function countLabel(n: number, singular: string, plural: string): string {
  return n === 1 ? `1 ${singular}` : `${n} ${plural}`;
}

interface Counted {
  modifies: number;
  reads: number;
  bashes: number;
  others: number;
  newest: number;
  counted: boolean;
}

function countWindow(
  calls: readonly ToolCallStamp[],
  nowMs: number,
  windowMs: number,
): Counted {
  // A non-positive window is a caller slip, not a request for a blank line — mirror summarize.go.
  const window = windowMs > 0 ? windowMs : TOOL_SUMMARY_WINDOW_MS;
  const r: Counted = { modifies: 0, reads: 0, bashes: 0, others: 0, newest: 0, counted: false };
  for (const c of calls) {
    if (!c || c.atMs === 0) continue; // no timestamp: skipped, never placed at the epoch
    let ageMs = nowMs - c.atMs;
    if (ageMs < 0) ageMs = 0; // clock skew / future stamp: clamp, never drop the work
    if (ageMs > window) continue; // INCLUSIVE on the left: exactly `window` old IS counted
    switch (classifyTool(c.toolName)) {
      case "modify":
        r.modifies++;
        break;
      case "read":
        r.reads++;
        break;
      case "bash":
        r.bashes++;
        break;
      case "other":
        r.others++;
        break;
      default:
        continue;
    }
    // Track the NEWEST COUNTED entry explicitly rather than seeding a 0 sentinel and taking the
    // max: a malformed negative stamp would then leave `newest` at 0 and the trailing age would be
    // measured from the epoch, not from the entry that was just counted.
    if (!r.counted || c.atMs > r.newest) r.newest = c.atMs;
    r.counted = true;
  }
  return r;
}

/** The count phrase WITHOUT the trailing age — what a screen reader is told, and what a narrow pane
 *  is allowed to keep. `""` when nothing was counted, never "0 modifies". */
export function toolCountPhrase(
  calls: readonly ToolCallStamp[],
  nowMs: number,
  windowMs: number,
): string {
  const r = countWindow(calls, nowMs, windowMs);
  if (!r.counted) return "";
  // Ordering is FIXED (modify, read, bash) so the line does not reorder as counts change.
  const parts: string[] = [];
  if (r.modifies > 0) parts.push(countLabel(r.modifies, "modify", "modifies"));
  if (r.reads > 0) parts.push(countLabel(r.reads, "read", "reads"));
  if (r.bashes > 0) parts.push(countLabel(r.bashes, "bash", "bash")); // "bash" deliberately invariable
  // The catch-all goes LAST: the three NAMED buckets keep their fixed leading order so the line
  // never reorders as counts change (AC6).
  if (r.others > 0) parts.push(countLabel(r.others, "other tool", "other tools"));
  return parts.join(" · ");
}

/** The full summary, the EXACT string toolclass.Summarize renders for the same ledger:
 *  "5 modifies · 2 reads · 3 bash · newest call 30s ago". Empty input, JSON-less input or
 *  all-ignored calls return "" — load-bearing, because the caller APPENDS this to an existing line
 *  and "0 modifies" would be a false claim that work is happening. Pinned by rollup_fixture.json. */
export function summarizeToolCalls(
  calls: readonly ToolCallStamp[],
  nowMs: number,
  windowMs: number,
): string {
  const phrase = toolCountPhrase(calls, nowMs, windowMs);
  if (phrase === "") return "";
  const r = countWindow(calls, nowMs, windowMs);
  let ageMs = nowMs - r.newest;
  if (ageMs < 0) ageMs = 0;
  // The age rounds with the activity line's own rule: Round-to-nearest SECOND
  // (`int(d.Round(time.Second)/time.Second)` in internal/tui/app.go), not floor.
  const ageSecs = Math.round(ageMs / 1000);
  // `newest call Ns ago` NAMES what is being aged: a bare `last Ns` read as "the window is Ns long"
  // (AC7) — the operator's own misreading of `last 12s` against a 30s window.
  return `${phrase} · newest call ${ageSecs}s ago`;
}
