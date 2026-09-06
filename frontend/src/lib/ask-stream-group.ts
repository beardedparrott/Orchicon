// ask-stream-group.ts — order-preserving coalescing for Ask Orchicon live
// stream items (extracted from routes/ask-orchicon.tsx).
//
// The old grouper buffered ALL text into one textBuf and ALL reasoning into
// one reasoningBuf, so an interleaved arrival text(A) → reasoning(B) →
// text(C) rendered as [all-text][all-reasoning] — interleaving could not
// survive by construction. This grouper coalesces only CONSECUTIVE
// same-kind chunks and emits each run in arrival order, so the render
// iterates the grouped array in order: [text A][reasoning B][text C].
//
// Chunk keys are stream sequence ids (`st-<seq>` / `sr-<seq>` from a
// per-conversation monotonic counter), never Math.random(): stable keys
// mean re-renders never reshuffle bubbles.

export type StreamItem =
  | { kind: "user"; text: string; at: number; key: string }
  | { kind: "text"; text: string; at: number; key: string; phase?: string }
  | { kind: "reasoning"; text: string; at: number; key: string; phase?: string }
  | { kind: "error"; text: string; at: number; key: string };

// groupStreamItems coalesces only CONSECUTIVE same-kind text/reasoning
// chunks, preserving arrival order. A phase change also breaks a run (a new
// turn/segment starts a new bubble). user/error items always break runs.
export function groupStreamItems(items: StreamItem[]): StreamItem[] {
  const out: StreamItem[] = [];
  let buf = "";
  let bufAt = 0;
  let bufKey = "";
  let bufPhase = "";
  let bufKind: "text" | "reasoning" | null = null;

  const flush = () => {
    if (!buf || !bufKind) return;
    if (bufKind === "text") {
      out.push({ kind: "text", text: buf, at: bufAt, key: bufKey, phase: bufPhase });
    } else {
      out.push({ kind: "reasoning", text: buf, at: bufAt, key: bufKey, phase: bufPhase });
    }
    buf = "";
    bufKind = null;
  };

  for (const item of items) {
    if (item.kind === "text" || item.kind === "reasoning") {
      const phase = item.phase ?? "";
      if (bufKind !== item.kind || (bufPhase && phase !== bufPhase)) {
        flush();
      }
      if (!bufKind) {
        bufKey = item.key;
        bufPhase = phase;
        bufKind = item.kind;
      }
      buf += item.text;
      bufAt = item.at;
    } else {
      flush();
      out.push(item);
    }
  }
  flush();
  return out;
}

// nextChunkKey builds a stable per-conversation sequence key for a live
// chunk. The caller holds a monotonic counter ref (seqRef) per
// conversation and passes the next value — no Math.random(), so React
// reconciliation never reshuffles bubbles on re-render.
export function nextChunkKey(prefix: "st" | "sr", seq: number): string {
  return `${prefix}-${seq}`;
}
