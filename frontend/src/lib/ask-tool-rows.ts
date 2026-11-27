// ask-tool-rows.ts — DURABLE tool calls become transcript rows (the GUI half of
// internal/tui/chat/toolrows.go).
//
// THE DEFECT THIS FIXES. MessageBubble drew a message's prose, its reasoning and its recorded ask card,
// and DROPPED its tool calls. Tool rows existed only while the SSE stream was live (the stream's
// tool-call arm creates them), so every tool call a turn had made VANISHED as soon as the turn finalized
// and the client re-read its page.
//
// It is invisible on the native/opencode adapters, whose recorded calls are mostly one-line reads, and
// it is the WHOLE TRANSCRIPT for an antigravity turn: agy reports its work AS tool calls (`run_command`,
// `view_file`, `read_url_content`), and those rows are the operator's only record of what it did.
// Reloading an agy conversation showed the model's reply with no evidence of the work behind it.
//
// WHY THE CALL AND ITS RESULT ARE PAIRED. They are separate ledger rows keyed by tool_call_id. The row
// wants both — the subject comes from the CALL's arguments, the output from the RESULT — so they are
// joined by id and drawn as one row, exactly as the TUI pairs them and as ToolCard already draws them in
// the execution panes.

import type { ParsedTool } from "@/components/executions/sessionItems";
import { isAskUserToolCall } from "@/components/ask/AskCard";

/** The minimum a recorded message must expose for its tool calls to become rows. */
export interface ToolCallMessage {
  id?: string;
  createdAt?: { seconds?: bigint | number } | Date | number;
  toolCalls?: ReadonlyArray<{
    id?: string;
    functionName?: string;
    arguments?: string;
  }> | null;
  toolResults?: ReadonlyArray<{
    toolCallId?: string;
    output?: string;
    isError?: boolean;
  }> | null;
}

/** DurableToolRow is one tool call as the transcript draws it. */
export type DurableToolRow = ParsedTool;

/**
 * toolCallRendered reports whether a recorded tool call becomes a row of its own.
 *
 * TWO KINDS OF CALL ARE NOT ROWS:
 *   - an `ask_user` call renders as its CARD (askCardOf/the TUI's parseAskUserCall); a row beside the
 *     card would show the operator a duplicate of the question they are being asked;
 *   - a `permission.*` ledger row is the RESOLUTION of a consent card and is already read by
 *     lib/ask-consent's settleFromLedger. As a row it would say "permission" and nothing else.
 */
export function toolCallRendered(functionName: string): boolean {
  const n = (functionName ?? "").trim().toLowerCase();
  if (n === "") return false;
  if (isAskUserToolCall(functionName)) return false;
  return !n.startsWith("permission.");
}

/** atMs reads whatever the message carries as its instant, tolerating the three shapes this platform
 *  records (a Date, a protobuf Timestamp, or a plain millisecond number). Absent means 0, which the
 *  renderers draw as "no time" rather than as epoch 0. */
function atMs(createdAt: ToolCallMessage["createdAt"]): number {
  if (createdAt == null) return 0;
  if (createdAt instanceof Date) return createdAt.getTime();
  if (typeof createdAt === "number") return createdAt;
  const secs = createdAt.seconds;
  if (typeof secs === "bigint") return Number(secs) * 1000;
  if (typeof secs === "number") return secs * 1000;
  return 0;
}

/**
 * durableToolRows returns one row per recorded tool call, in ledger order, with its result paired.
 *
 * Empty for a message with no calls, and for one whose calls are all cards or permission ledger rows —
 * so a caller can render the list unconditionally.
 */
export function durableToolRows(message: ToolCallMessage | null | undefined): DurableToolRow[] {
  const calls = message?.toolCalls ?? [];
  if (!message || calls.length === 0) return [];

  // INDEX THE RESULTS ONCE: a page holds hundreds of calls, and a scan per call is a quadratic that
  // only shows up on the longest conversations.
  const byId = new Map<string, { output?: string; isError?: boolean }>();
  for (const r of message.toolResults ?? []) {
    if (r?.toolCallId) byId.set(r.toolCallId, r);
  }
  const orphanResults = (message.toolResults ?? []).filter((r) => r && !r.toolCallId);

  const at = atMs(message.createdAt);
  const rows: DurableToolRow[] = [];
  let orphan = 0;
  for (const c of calls) {
    if (!c || !toolCallRendered(c.functionName ?? "")) continue;
    const id = c.id ?? "";
    let output = "";
    let failed = false;
    const paired = id ? byId.get(id) : undefined;
    if (paired) {
      output = paired.output ?? "";
      failed = paired.isError === true;
    } else if (!id) {
      // A ledger row recorded without an id still has an output beside it; the index-ordered orphan
      // gives it back rather than dropping what the tool said.
      const orphanResult = orphanResults[orphan];
      output = orphanResult?.output ?? "";
      failed = orphanResult?.isError === true;
      orphan += 1;
    }
    rows.push({
      id,
      toolName: c.functionName ?? "",
      input: c.arguments ?? "",
      output,
      at,
      // THE RESULT'S ERROR FLAG, so the row can draw a refusal in full (see ToolCard).
      failed,
    });
  }
  return rows;
}

/** toolRowKey is the row's stable React key: the call's id, or its position when the ledger recorded
 *  none (two id-less calls must not collide). */
export function toolRowKey(row: DurableToolRow, index: number): string {
  return row.id ? `tc-${row.id}` : `tc-${row.toolName}-${index}`;
}
