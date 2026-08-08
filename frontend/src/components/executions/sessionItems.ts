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
//     transcript's latest part, and group consecutive live chunks of the
//     same kind (text, reasoning) into one growing bubble so a turn reads
//     as one message instead of a burst of tiny ones.
export interface ParsedTool {
  id: string;
  toolName: string;
  input: string;
  output: string;
  at: number;
}

export type ChatItem =
  | { kind: "user"; text: string; source: string; at: number; key: string }
  | { kind: "text"; text: string; at: number; key: string; live?: boolean }
  | { kind: "tool"; tool: ParsedTool; key: string }
  | { kind: "reasoning"; text: string; at: number; key: string; live?: boolean }
  | { kind: "error"; text: string; at: number; key: string }
  | { kind: "artifact"; name: string; type: string; content: string; at: number; key: string }
  | { kind: "session"; sessionId: string; serveUrl: string; at: number; key: string };

export function itemAt(i: ChatItem): number {
  return i.kind === "tool" ? i.tool.at : i.at;
}

/**
 * mergeSessionItems merges the durable transcript history with the live
 * event stream for a RUNNING execution. Returns the transcript history
 * plus only the live events not yet reflected in it, with consecutive
 * live text / reasoning chunks grouped into single growing bubbles.
 */
export function mergeSessionItems(history: ChatItem[], live: ChatItem[]): ChatItem[] {
  const lastHistoryAt = history.reduce((m, i) => Math.max(m, itemAt(i)), 0);
  const fresh = live.filter((i) => itemAt(i) >= lastHistoryAt);
  const merged = [...history, ...fresh].sort((a, b) => itemAt(a) - itemAt(b));

  const out: ChatItem[] = [];
  let runKind: "text" | "reasoning" | "" = "";
  const run: ChatItem[] = [];
  const flush = () => {
    if (run.length === 0) return;
    if (run.length > 1 && (runKind === "text" || runKind === "reasoning")) {
      const texts = run as { kind: "text" | "reasoning"; text: string; at: number; key: string; live?: boolean }[];
      out.push({
        kind: runKind,
        text: texts.map((t) => t.text).join(""),
        at: texts[texts.length - 1].at,
        key: texts[0].key,
        live: true,
      } as ChatItem);
    } else {
      out.push(...run);
    }
    run.length = 0;
    runKind = "";
  };
  for (const item of merged) {
    if ((item.kind === "text" || item.kind === "reasoning") && item.live) {
      if (item.kind !== runKind && run.length > 0) flush();
      if (run.length === 0) runKind = item.kind;
      run.push(item);
    } else {
      flush();
      out.push(item);
    }
  }
  flush();
  return out;
}
